package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// ledgerMutation is one way a stored ledger can differ from a clean prefix of
// the build's migrations.
type ledgerMutation int

const (
	mutationNone ledgerMutation = iota
	mutationFlipChecksum
	mutationRename
	mutationSwapPayload
	mutationDropMiddle
	mutationAppendNext
	mutationAppendGap
	ledgerMutationCount
)

func (m ledgerMutation) String() string {
	return [...]string{"none", "flip-checksum", "rename", "swap-payload", "drop-middle", "append-next", "append-gap"}[m]
}

// ledgerCase is a generated store ledger: the first n migrations of known,
// then one mutation, a user_version offset, and an application_id.
type ledgerCase struct {
	known        []migration
	min          int
	n            int
	mutation     ledgerMutation
	i, j         int
	uvDelta      int
	appOK        bool
	ledgerExists bool
}

func (c ledgerCase) String() string {
	return fmt.Sprintf("known=%d min=%d n=%d mutation=%s i=%d j=%d uvDelta=%d appOK=%v ledgerExists=%v",
		len(c.known), c.min, c.n, c.mutation, c.i, c.j, c.uvDelta, c.appOK, c.ledgerExists)
}

// Generate biases n toward the boundaries of the accepted window
// [min, len(known)] and mixes the real embedded migrations with synthetic
// lists of other lengths, so every sentinel and the accept path all occur.
func (ledgerCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := ledgerCase{}
	if r.Intn(2) == 0 {
		c.known = embeddedMigrations
		c.min = MinClientReadSchemaVersion
	} else {
		c.known = syntheticMigrations(r, 1+r.Intn(12))
		c.min = 1 + r.Intn(len(c.known))
	}
	k := len(c.known)
	candidates := []int{0, 1, c.min - 1, c.min, c.min + 1, k - 1, k, k + 1, k + 2, r.Intn(k + 3)}
	c.n = min(max(candidates[r.Intn(len(candidates))], 0), k+2)
	if r.Intn(5) < 2 {
		c.mutation = mutationNone
	} else {
		c.mutation = ledgerMutation(1 + r.Intn(int(ledgerMutationCount)-1))
	}
	c.i, c.j = r.Intn(k+3), r.Intn(k+3)
	c.uvDelta = []int{0, 0, 0, -1, 1}[r.Intn(5)]
	c.appOK = r.Intn(5) != 0
	c.ledgerExists = r.Intn(12) != 0
	return reflect.ValueOf(c)
}

func syntheticMigrations(r *rand.Rand, count int) []migration {
	migrations := make([]migration, count)
	for i := range migrations {
		migrations[i] = migration{
			version:        i + 1,
			name:           fmt.Sprintf("synthetic_%04d", i+1),
			checksumSHA256: fakeChecksum(fmt.Sprintf("synthetic-%d-%d", i+1, r.Int63())),
		}
	}
	return migrations
}

func fakeChecksum(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func flipHex(value string) string {
	replacement := byte('0')
	if value[0] == '0' {
		replacement = '1'
	}
	return string(replacement) + value[1:]
}

// build materializes the case. effective reports whether the mutation
// actually changed the ledger; inapplicable mutations degrade to none.
func (c ledgerCase) build() (snapshot ledgerSnapshot, effective bool) {
	k := len(c.known)
	applied := make([]appliedMigration, 0, c.n+1)
	for row := 0; row < c.n; row++ {
		if row < k {
			applied = append(applied, appliedMigration{
				version:        row + 1,
				name:           c.known[row].name,
				checksumSHA256: c.known[row].checksumSHA256,
			})
			continue
		}
		applied = append(applied, appliedMigration{
			version:        row + 1,
			name:           fmt.Sprintf("future_%04d", row+1),
			checksumSHA256: fakeChecksum(fmt.Sprintf("future-%d", row+1)),
		})
	}
	n := len(applied)
	switch c.mutation {
	case mutationFlipChecksum:
		if n > 0 {
			i := c.i % n
			applied[i].checksumSHA256 = flipHex(applied[i].checksumSHA256)
			effective = true
		}
	case mutationRename:
		if n > 0 {
			i := c.i % n
			applied[i].name = fmt.Sprintf("renamed_%04d", applied[i].version)
			effective = true
		}
	case mutationSwapPayload:
		if n >= 2 {
			i, j := c.i%n, c.j%n
			if i == j {
				j = (i + 1) % n
			}
			applied[i].name, applied[j].name = applied[j].name, applied[i].name
			applied[i].checksumSHA256, applied[j].checksumSHA256 = applied[j].checksumSHA256, applied[i].checksumSHA256
			effective = true
		}
	case mutationDropMiddle:
		if n >= 3 {
			i := 1 + c.i%(n-2)
			applied = append(applied[:i], applied[i+1:]...)
			effective = true
		}
	case mutationAppendNext:
		applied = append(applied, appliedMigration{
			version:        n + 1,
			name:           "unknown_next",
			checksumSHA256: fakeChecksum("unknown-next"),
		})
		effective = true
	case mutationAppendGap:
		applied = append(applied, appliedMigration{
			version:        n + 3,
			name:           "unknown_gap",
			checksumSHA256: fakeChecksum("unknown-gap"),
		})
		effective = true
	}

	userVersion := c.uvDelta
	if len(applied) > 0 {
		userVersion += applied[len(applied)-1].version
	}
	gotApplicationID := applicationID
	if !c.appOK {
		gotApplicationID = applicationID + 1
	}
	if !c.ledgerExists {
		applied = nil
	}
	return ledgerSnapshot{
		ledgerExists:  c.ledgerExists,
		applied:       applied,
		userVersion:   userVersion,
		applicationID: gotApplicationID,
	}, effective
}

// expected is the oracle, stated in terms of how the case was generated
// rather than by re-running the rule: a missing ledger or any effective
// mutation, pragma drift, or empty ledger is a mismatch, except that a
// ledger longer than the build's list is always a newer store; a clean
// prefix shorter than min is too old; anything else is accepted at n.
func (c ledgerCase) expected(snapshot ledgerSnapshot, effective bool) (class error, version int) {
	k := len(c.known)
	switch {
	case !c.ledgerExists:
		return ErrLedgerMismatch, 0
	case len(snapshot.applied) == 0:
		return ErrLedgerMismatch, 0
	case len(snapshot.applied) > k:
		return ErrSchemaNewer, snapshot.applied[len(snapshot.applied)-1].version
	case effective, c.uvDelta != 0, !c.appOK:
		return ErrLedgerMismatch, 0
	case c.n < c.min:
		return ErrSchemaTooOld, c.n
	default:
		return nil, c.n
	}
}

var schemaClasses = []error{ErrSchemaNewer, ErrSchemaTooOld, ErrLedgerMismatch}

func schemaClassOf(err error) (error, int) {
	var class error
	matches := 0
	for _, candidate := range schemaClasses {
		if errors.Is(err, candidate) {
			class = candidate
			matches++
		}
	}
	return class, matches
}

// TestClassifyLedgerProperty executes I3's decision rule for all generated
// ledgers: classifyLedger accepts exactly the clean prefixes whose length is
// within [min, len(known)], returns that length, and otherwise returns exactly
// one class sentinel, the one the generator predicts.
func TestClassifyLedgerProperty(t *testing.T) {
	outcomes := map[string]int{}
	property := func(c ledgerCase) bool {
		snapshot, effective := c.build()
		wantClass, wantVersion := c.expected(snapshot, effective)
		version, err := classifyLedger(snapshot, c.known, c.min)
		gotClass, matches := schemaClassOf(err)
		switch {
		case wantClass == nil && err != nil:
			t.Logf("%s: want accept at %d, got %v", c, wantVersion, err)
			return false
		case wantClass == nil && version != wantVersion:
			t.Logf("%s: accepted at %d, want %d", c, version, wantVersion)
			return false
		case wantClass != nil && (err == nil || matches != 1 || gotClass != wantClass):
			t.Logf("%s: want %v, got %v (class matches %d)", c, wantClass, err, matches)
			return false
		case (wantClass == ErrSchemaNewer || wantClass == ErrSchemaTooOld) && version != wantVersion:
			t.Logf("%s: %v reported version %d, want %d", c, wantClass, version, wantVersion)
			return false
		}
		if wantClass == nil {
			outcomes["accept"]++
		} else {
			outcomes[wantClass.Error()]++
		}
		return true
	}
	config := &quick.Config{MaxCount: 4000, Rand: rand.New(rand.NewSource(20260929))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"accept", ErrSchemaNewer.Error(), ErrSchemaTooOld.Error(), ErrLedgerMismatch.Error()} {
		if outcomes[outcome] < 50 {
			t.Fatalf("generator produced %d %q cases (all: %v); the property is close to vacuous", outcomes[outcome], outcome, outcomes)
		}
	}
}

// TestClassifyLedgerAgreesWithValidateDatabaseState is the I4 differential:
// the read-only client path (OpenReadOnly's snapshot read over a mode=ro
// connection, then classifyLedger) and the owner's path (readAppliedMigrations
// plus validateDatabaseState inside a writable transaction, as runMigrations
// does) must accept and reject the same stored ledgers when the client's
// minimum is 1, and classify rejections alike. Both read a real database file
// the case was written into. The one intended divergence, a database with no
// ledger at all (the owner provisions it, the client refuses), is excluded
// here and pinned in TestOpenReadOnlyRejectsTamperedStore.
func TestClassifyLedgerAgreesWithValidateDatabaseState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	iteration := 0
	agreements := map[bool]int{}
	property := func(c ledgerCase) bool {
		c.known = embeddedMigrations
		c.min = 1
		c.ledgerExists = true
		iteration++
		snapshot, _ := c.build()
		path := filepath.Join(dir, fmt.Sprintf("ledger-%d.sqlite3", iteration))
		if err := writeLedgerDatabase(path, snapshot); err != nil {
			t.Logf("%s: write ledger database: %v", c, err)
			return false
		}

		store, info, clientErr := openReadOnly(path, embeddedMigrations, 1)
		if store != nil {
			_ = store.Close()
		}

		owner, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Logf("open owner connection: %v", err)
			return false
		}
		defer owner.Close()
		tx, err := owner.BeginTx(ctx, nil)
		if err != nil {
			t.Logf("owner BeginTx: %v", err)
			return false
		}
		defer func() { _ = tx.Rollback() }()
		applied, ledgerExists, err := readAppliedMigrations(ctx, tx)
		if err != nil {
			t.Logf("owner readAppliedMigrations: %v", err)
			return false
		}
		ownerErr := validateDatabaseState(ctx, tx, ledgerExists, applied, embeddedMigrations)

		if (clientErr == nil) != (ownerErr == nil) {
			t.Logf("%s: client err = %v, owner err = %v", c, clientErr, ownerErr)
			return false
		}
		if clientErr == nil {
			if info.SchemaVersion != len(applied) {
				t.Logf("%s: client accepted at %d, owner ledger has %d rows", c, info.SchemaVersion, len(applied))
				return false
			}
		} else {
			clientClass, _ := schemaClassOf(clientErr)
			ownerClass, _ := schemaClassOf(ownerErr)
			if clientClass != ownerClass {
				t.Logf("%s: client class %v (%v), owner class %v (%v)", c, clientClass, clientErr, ownerClass, ownerErr)
				return false
			}
		}
		agreements[clientErr == nil]++
		return true
	}
	config := &quick.Config{MaxCount: 150, Rand: rand.New(rand.NewSource(29092026))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	if agreements[true] < 10 || agreements[false] < 10 {
		t.Fatalf("differential covered %d accepted and %d rejected ledgers; want both well represented", agreements[true], agreements[false])
	}
}

// writeLedgerDatabase writes snapshot's ledger, user_version and
// application_id into a new database file, using the ledger DDL from
// migration 0001.
func writeLedgerDatabase(path string, snapshot ledgerSnapshot) error {
	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	ddl := migration0001SQL[strings.Index(migration0001SQL, "CREATE TABLE schema_migrations"):]
	ddl = ddl[:strings.Index(ddl, ") STRICT;")+len(") STRICT;")]
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}
	for _, row := range snapshot.applied {
		if _, err := db.Exec(`
			INSERT INTO schema_migrations (version, name, checksum_sha256, applied_at_ms, app_version, execution_ms)
			VALUES (?, ?, ?, 1, 'test', 0)
		`, row.version, row.name, row.checksumSHA256); err != nil {
			return fmt.Errorf("insert ledger row %d: %w", row.version, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, snapshot.userVersion)); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`PRAGMA application_id = %d`, snapshot.applicationID))
	return err
}

// readOnlySessionCase is one generated client session: a store at a
// supported version with random content, a random sequence of inventory
// reads, and one attempted write.
type readOnlySessionCase struct {
	version int
	spec    clientFixtureSpec
	reads   []int
	write   int
}

func (c readOnlySessionCase) String() string {
	return fmt.Sprintf("version=%d accounts=%d conversations=%d messages=%d reads=%v write=%s",
		c.version, c.spec.accounts, c.spec.conversations, len(c.spec.messages), c.reads, readOnlyWrites[c.write].name)
}

var bodyAlphabet = []rune("abcdefghijklmnopqrstuvwxyz ABC 0123456789 éüßñ 你好世界 😀👍🏽❤️ ́‍'\"%_\\\n\t")

func randomBody(r *rand.Rand) string {
	length := r.Intn(40)
	runes := make([]rune, length)
	for i := range runes {
		runes[i] = bodyAlphabet[r.Intn(len(bodyAlphabet))]
	}
	return string(runes)
}

func (readOnlySessionCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := readOnlySessionCase{
		version: MinClientReadSchemaVersion + r.Intn(LatestSchemaVersion()-MinClientReadSchemaVersion+1),
		spec: clientFixtureSpec{
			accounts:      1 + r.Intn(3),
			conversations: r.Intn(7),
		},
		write: r.Intn(len(readOnlyWrites)),
	}
	if c.spec.conversations > 0 {
		for m := r.Intn(31); m > 0; m-- {
			c.spec.messages = append(c.spec.messages, fixtureMessageSpec{
				conversation: r.Intn(c.spec.conversations),
				body:         randomBody(r),
				occurredAtMS: 1 + r.Int63n(readOnlyFixtureTimeMS),
				outgoing:     r.Intn(3) == 0,
			})
		}
	}
	for n := 1 + r.Intn(20); n > 0; n-- {
		c.reads = append(c.reads, r.Intn(len(clientReadInventory)))
	}
	return reflect.ValueOf(c)
}

// readOnlyWrite is a mutation a client might attempt. Each succeeds on a
// writable handle to the same fixture
// (TestReadOnlyWritesSucceedOnWritableHandle), so a failure on the read-only
// handle is the read-only attach refusing it, nothing else.
type readOnlyWrite struct {
	name string
	run  func(ctx context.Context, store *Store, fixture clientFixture) error
}

var readOnlyWrites = []readOnlyWrite{
	{name: "UpsertAccount", run: func(_ context.Context, store *Store, _ clientFixture) error {
		return store.UpsertAccount(testReadOnlyAccount("account-new"))
	}},
	{name: "UpsertIdentity", run: func(_ context.Context, store *Store, fixture clientFixture) error {
		return store.UpsertIdentity(Identity{
			IdentityID:     "identity-new",
			AccountID:      fixture.accountIDs[0],
			Kind:           IdentityKind("e164"),
			CanonicalValue: "+15559999999",
			RawValue:       "+1 555 999 9999",
			MetadataJSON:   `{}`,
			CreatedAtMS:    readOnlyFixtureTimeMS,
			UpdatedAtMS:    readOnlyFixtureTimeMS,
		})
	}},
	{name: "UpsertConversation", run: func(_ context.Context, store *Store, fixture clientFixture) error {
		return store.UpsertConversation(Conversation{
			ConversationID:       "conversation-new",
			AccountID:            fixture.accountIDs[0],
			RemoteConversationID: "remote-conversation-new",
			Kind:                 ConversationKindDirect,
			NotificationMode:     NotificationModeAll,
			MetadataJSON:         `{}`,
			CreatedAtMS:          readOnlyFixtureTimeMS,
			UpdatedAtMS:          readOnlyFixtureTimeMS,
		})
	}},
	{name: "CreatePerson", run: func(_ context.Context, store *Store, _ clientFixture) error {
		return store.CreatePerson(Person{
			PersonID:    "person-new",
			DisplayName: "New Person",
			CreatedAtMS: readOnlyFixtureTimeMS,
			UpdatedAtMS: readOnlyFixtureTimeMS,
		})
	}},
	{name: "ImportMessage", run: func(ctx context.Context, store *Store, fixture clientFixture) error {
		conversation, err := ensureWritableConversation(store, fixture)
		if err != nil {
			return err
		}
		repository, err := NewMessageRepository(store, time.Now)
		if err != nil {
			return err
		}
		return repository.ImportMessage(ctx, MessageProjection{Message: Message{
			MessageID:       "message-new",
			ConversationID:  conversation.ConversationID,
			AccountID:       conversation.AccountID,
			RemoteMessageID: "remote-message-new",
			Direction:       MessageDirectionOutgoing,
			Body:            "must not land",
			State:           MessageStateActive,
			OccurredAtMS:    readOnlyFixtureTimeMS,
		}})
	}},
	{name: "ApplyReaction", run: func(ctx context.Context, store *Store, fixture clientFixture) error {
		if len(fixture.messageIDs) == 0 {
			return store.UpsertAccount(testReadOnlyAccount("account-new-reaction"))
		}
		repository, err := NewReactionRepository(store, time.Now)
		if err != nil {
			return err
		}
		messageID := fixture.messageIDs[0]
		_, err = repository.ApplyReaction(ctx, ReactionApply{
			AccountID:      fixture.messageAccount[messageID],
			ConversationID: fixture.messageConversation[messageID],
			MessageID:      messageID,
			ReactorKey:     "self",
			ReactorIsSelf:  true,
			ReactorLabel:   "me",
			Emoji:          "🎉",
			Action:         bridge.ReactionAdd,
			OccurredAtMS:   readOnlyFixtureTimeMS + 10,
		})
		return err
	}},
	{name: "PRAGMA user_version", run: func(ctx context.Context, store *Store, _ clientFixture) error {
		_, err := store.db.ExecContext(ctx, `PRAGMA user_version = 999`)
		return err
	}},
	{name: "raw DELETE", run: func(ctx context.Context, store *Store, _ clientFixture) error {
		_, err := store.db.ExecContext(ctx, `DELETE FROM accounts`)
		return err
	}},
}

// ensureWritableConversation returns a conversation to write into, which the
// write itself creates when the fixture has none (still a write that must be
// refused on the read-only handle).
func ensureWritableConversation(store *Store, fixture clientFixture) (Conversation, error) {
	if len(fixture.conversations) > 0 {
		return fixture.conversations[0], nil
	}
	conversation := Conversation{
		ConversationID:       "conversation-for-write",
		AccountID:            fixture.accountIDs[0],
		RemoteConversationID: "remote-conversation-for-write",
		Kind:                 ConversationKindDirect,
		NotificationMode:     NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          readOnlyFixtureTimeMS,
		UpdatedAtMS:          readOnlyFixtureTimeMS,
	}
	return conversation, store.UpsertConversation(conversation)
}

// TestOpenReadOnlyNeverMutatesStore executes I1, I6 and part of I3 over
// generated sessions: on a store at any supported version with random
// content, every inventory read succeeds, the attempted write fails with
// IsReadOnlyError, SchemaVersion reports the on-disk version, and after close
// the main file, user_version, ledger, schema cookie and logical dump are
// unchanged, with no file created beyond the WAL sidecars.
func TestOpenReadOnlyNeverMutatesStore(t *testing.T) {
	ctx := context.Background()
	templates := newFixtureTemplates(t)
	property := func(c readOnlySessionCase) bool {
		path, fixture, err := templates.build(t, c.version, c.spec)
		if err != nil {
			t.Logf("%s: build fixture: %v", c, err)
			return false
		}
		before, err := ReadStoreFingerprint(path)
		if err != nil {
			t.Logf("%s: fingerprint before: %v", c, err)
			return false
		}
		filesBefore := dirNames(t, filepath.Dir(path))

		store, info, err := OpenReadOnly(path)
		if err != nil {
			t.Logf("%s: OpenReadOnly: %v", c, err)
			return false
		}
		if store.SchemaVersion() != c.version || info.SchemaVersion != c.version || !store.ReadOnly() {
			t.Logf("%s: SchemaVersion()=%d info=%+v ReadOnly()=%v", c, store.SchemaVersion(), info, store.ReadOnly())
			_ = store.Close()
			return false
		}
		for _, index := range c.reads {
			read := clientReadInventory[index]
			if _, err := read.run(ctx, store, fixture); err != nil {
				t.Logf("%s: %s: %v", c, read.name, err)
				_ = store.Close()
				return false
			}
		}
		write := readOnlyWrites[c.write]
		if err := write.run(ctx, store, fixture); !IsReadOnlyError(err) {
			t.Logf("%s: write %s on the read-only store error = %v, want IsReadOnlyError", c, write.name, err)
			_ = store.Close()
			return false
		}
		if err := store.Close(); err != nil {
			t.Logf("%s: Close: %v", c, err)
			return false
		}

		after, err := ReadStoreFingerprint(path)
		if err != nil {
			t.Logf("%s: fingerprint after: %v", c, err)
			return false
		}
		if !reflect.DeepEqual(before, after) {
			t.Logf("%s: store changed:\nbefore: %+v\nafter:  %+v", c, before, after)
			return false
		}
		assertOnlyWALSidecarsAdded(t, path, filesBefore)
		return true
	}
	config := &quick.Config{MaxCount: 25, Rand: rand.New(rand.NewSource(10110))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// TestReadOnlyWritesSucceedOnWritableHandle keeps TestOpenReadOnlyNeverMutatesStore
// honest: every write in readOnlyWrites succeeds through a writable handle on
// the same kind of fixture, so its failure on a read-only handle can only be
// the read-only attach refusing it.
func TestReadOnlyWritesSucceedOnWritableHandle(t *testing.T) {
	ctx := context.Background()
	for _, write := range readOnlyWrites {
		specs := []clientFixtureSpec{defaultClientFixtureSpec()}
		if write.name == "ImportMessage" || write.name == "ApplyReaction" {
			// These take a different path on a fixture without messages.
			specs = append(specs, clientFixtureSpec{accounts: 1})
		}
		for _, spec := range specs {
			path, fixture := buildClientFixture(t, LatestSchemaVersion(), spec)
			before := mustFingerprint(t, path)
			db, err := sql.Open("sqlite", storeDSN(path))
			if err != nil {
				t.Fatalf("open writable: %v", err)
			}
			store := &Store{db: db, schemaVersion: LatestSchemaVersion()}
			if err := write.run(ctx, store, fixture); err != nil {
				t.Errorf("%s on a writable handle (conversations=%d): %v", write.name, spec.conversations, err)
			}
			_ = db.Close()
			after := mustFingerprint(t, path)
			if before.Dump == after.Dump && before.UserVersion == after.UserVersion {
				t.Errorf("%s on a writable handle (conversations=%d) changed nothing", write.name, spec.conversations)
			}
		}
	}
}
