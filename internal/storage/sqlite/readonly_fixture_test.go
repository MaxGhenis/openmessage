package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// This file holds the fixtures shared by the read-only attach tests: a
// seeded store at any schema version, and the client read inventory, which is
// every store call internal/v2read makes (the only consumer of the v2 store
// in the transportless MCP client and openmessage read/status).

const readOnlyFixtureTimeMS int64 = 1_900_000_000_000

var fixtureBridgeKeys = []string{"google_messages", "whatsmeow", "signal"}

type fixtureMessageSpec struct {
	conversation int
	body         string
	occurredAtMS int64
	outgoing     bool
}

type clientFixtureSpec struct {
	accounts      int
	conversations int
	messages      []fixtureMessageSpec
}

// clientFixture names the rows a seeded fixture holds, so the inventory can
// query real IDs.
type clientFixture struct {
	version       int
	accountIDs    []string
	identityIDs   []string
	conversations []Conversation
	messageIDs    []string
	// messageConversation and messageAccount map each seeded message to
	// its conversation and account.
	messageConversation map[string]string
	messageAccount      map[string]string
	attachmentMsgID     string
	outgoingMsgID       string
	reactionMsgID       string
	outboxID            string
}

func defaultClientFixtureSpec() clientFixtureSpec {
	return clientFixtureSpec{
		accounts:      2,
		conversations: 3,
		messages: []fixtureMessageSpec{
			{conversation: 0, body: "hello from the older schema", occurredAtMS: readOnlyFixtureTimeMS - 5_000},
			{conversation: 0, body: "reply sent by me", occurredAtMS: readOnlyFixtureTimeMS - 4_000, outgoing: true},
			{conversation: 1, body: "héllo wörld 你好 👋", occurredAtMS: readOnlyFixtureTimeMS - 3_000},
			{conversation: 2, body: "third conversation", occurredAtMS: readOnlyFixtureTimeMS - 2_000},
		},
	}
}

// seedClientFixture writes spec through the store's own repositories, plus
// one raw outbox row: the outbox repository's enqueue SQL names columns from
// newer migrations, while the client only ever reads the state column.
func seedClientFixture(store *Store, version int, spec clientFixtureSpec) (clientFixture, error) {
	fixture := clientFixture{
		version:             version,
		messageConversation: map[string]string{},
		messageAccount:      map[string]string{},
	}
	now := func() time.Time { return time.UnixMilli(readOnlyFixtureTimeMS) }
	for i := 0; i < spec.accounts; i++ {
		accountID := fmt.Sprintf("account-%d", i)
		if err := store.UpsertAccount(Account{
			AccountID:   accountID,
			BridgeKey:   fixtureBridgeKeys[i%len(fixtureBridgeKeys)],
			DisplayName: "Account " + accountID,
			Mode:        AccountModeLive,
			Enabled:     true,
			ConfigJSON:  `{}`,
			CreatedAtMS: readOnlyFixtureTimeMS,
			UpdatedAtMS: readOnlyFixtureTimeMS,
		}); err != nil {
			return fixture, err
		}
		identityID := fmt.Sprintf("identity-%d", i)
		if err := store.UpsertIdentity(Identity{
			IdentityID:     identityID,
			AccountID:      accountID,
			Kind:           IdentityKind("e164"),
			CanonicalValue: fmt.Sprintf("+1555000000%d", i),
			RawValue:       fmt.Sprintf("+1 555 000 000%d", i),
			DisplayName:    fmt.Sprintf("Person %d", i),
			MetadataJSON:   `{}`,
			CreatedAtMS:    readOnlyFixtureTimeMS,
			UpdatedAtMS:    readOnlyFixtureTimeMS,
		}); err != nil {
			return fixture, err
		}
		fixture.accountIDs = append(fixture.accountIDs, accountID)
		fixture.identityIDs = append(fixture.identityIDs, identityID)
	}
	for c := 0; c < spec.conversations; c++ {
		account := c % spec.accounts
		conversation := Conversation{
			ConversationID:       fmt.Sprintf("conversation-%d", c),
			AccountID:            fixture.accountIDs[account],
			RemoteConversationID: fmt.Sprintf("remote-conversation-%d", c),
			Kind:                 ConversationKindDirect,
			Title:                fmt.Sprintf("Conversation %d with Person %d", c, account),
			NotificationMode:     NotificationModeAll,
			LastMessageAtMS:      readOnlyFixtureTimeMS - int64(c),
			MetadataJSON:         `{}`,
			CreatedAtMS:          readOnlyFixtureTimeMS,
			UpdatedAtMS:          readOnlyFixtureTimeMS,
		}
		if err := store.UpsertConversation(conversation); err != nil {
			return fixture, err
		}
		if err := store.ReplaceConversationParticipants(conversation.ConversationID, []ConversationParticipant{{
			AccountID:      conversation.AccountID,
			ConversationID: conversation.ConversationID,
			IdentityID:     fixture.identityIDs[account],
			Role:           ParticipantRoleMember,
			DisplayName:    fmt.Sprintf("Person %d", account),
			IsActive:       true,
		}}); err != nil {
			return fixture, err
		}
		fixture.conversations = append(fixture.conversations, conversation)
	}
	if spec.conversations == 0 {
		return fixture, nil
	}

	messages, err := NewMessageRepository(store, now)
	if err != nil {
		return fixture, err
	}
	for m, messageSpec := range spec.messages {
		conversation := fixture.conversations[messageSpec.conversation%len(fixture.conversations)]
		account := slices.Index(fixture.accountIDs, conversation.AccountID)
		message := Message{
			MessageID:       fmt.Sprintf("message-%d", m),
			ConversationID:  conversation.ConversationID,
			AccountID:       conversation.AccountID,
			RemoteMessageID: fmt.Sprintf("remote-message-%d", m),
			Direction:       MessageDirectionIncoming,
			Body:            messageSpec.body,
			State:           MessageStateActive,
			OccurredAtMS:    messageSpec.occurredAtMS,
		}
		if messageSpec.outgoing {
			message.Direction = MessageDirectionOutgoing
		} else {
			sender := fixture.identityIDs[account]
			message.SenderIdentityID = &sender
		}
		var attachments []MessageAttachment
		if fixture.attachmentMsgID == "" && !messageSpec.outgoing {
			size := int64(4096)
			attachments = append(attachments, MessageAttachment{
				Ordinal:   0,
				RemoteID:  "remote-media-" + message.MessageID,
				RemoteRef: []byte("opaque"),
				Filename:  "photo.png",
				MIME:      "image/png",
				SizeBytes: &size,
			})
			fixture.attachmentMsgID = message.MessageID
		}
		if err := messages.ImportMessage(context.Background(), MessageProjection{
			Message:     message,
			Attachments: attachments,
		}); err != nil {
			return fixture, fmt.Errorf("import %s: %w", message.MessageID, err)
		}
		fixture.messageIDs = append(fixture.messageIDs, message.MessageID)
		fixture.messageConversation[message.MessageID] = message.ConversationID
		fixture.messageAccount[message.MessageID] = message.AccountID
		if messageSpec.outgoing && fixture.outgoingMsgID == "" {
			fixture.outgoingMsgID = message.MessageID
			fixture.outboxID = "outbox-" + message.MessageID
			if _, err := store.db.Exec(`
				INSERT INTO outbox (
					outbox_id, account_id, conversation_id, kind, idempotency_key,
					payload_hash, operation, state, local_message_id,
					transport_request_id, result_remote_id, attempt_count,
					scheduled_for_ms, created_at_ms, updated_at_ms
				) VALUES (?, ?, ?, 'text', ?, 'payload-hash', 'send_text', 'confirmed', ?, ?, ?, 1, ?, ?, ?)
			`,
				fixture.outboxID,
				message.AccountID,
				message.ConversationID,
				"idempotency-"+message.MessageID,
				message.MessageID,
				"transport-"+message.MessageID,
				message.RemoteMessageID,
				readOnlyFixtureTimeMS,
				readOnlyFixtureTimeMS,
				readOnlyFixtureTimeMS,
			); err != nil {
				return fixture, fmt.Errorf("insert outbox row: %w", err)
			}
		}
		if version >= 10 && !messageSpec.outgoing && fixture.reactionMsgID == "" {
			reactions, err := NewReactionRepository(store, now)
			if err != nil {
				return fixture, err
			}
			reactor := fixture.identityIDs[account]
			if _, err := reactions.ApplyReaction(context.Background(), ReactionApply{
				AccountID:         message.AccountID,
				ConversationID:    message.ConversationID,
				MessageID:         message.MessageID,
				ReactorKey:        reactor,
				ReactorIdentityID: &reactor,
				ReactorLabel:      "Person",
				Emoji:             "👍",
				Action:            bridge.ReactionAdd,
				OccurredAtMS:      message.OccurredAtMS + 1,
			}); err != nil {
				return fixture, fmt.Errorf("apply reaction: %w", err)
			}
			fixture.reactionMsgID = message.MessageID
		}
	}
	return fixture, nil
}

// buildClientFixture builds a seeded store at version in a fresh directory
// and returns its path.
func buildClientFixture(t *testing.T, version int, spec clientFixtureSpec) (string, clientFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	var fixture clientFixture
	if err := BuildStoreAtVersion(path, version, func(store *Store) error {
		var err error
		fixture, err = seedClientFixture(store, version, spec)
		return err
	}); err != nil {
		t.Fatalf("BuildStoreAtVersion(%d): %v", version, err)
	}
	return path, fixture
}

// fixtureTemplates caches one migrated, unseeded store per schema version for
// a single test, so generated cases copy a template instead of re-running
// every migration (the dominant cost, especially under -race).
type fixtureTemplates struct {
	dir   string
	paths map[int]string
}

func newFixtureTemplates(t *testing.T) *fixtureTemplates {
	t.Helper()
	return &fixtureTemplates{dir: t.TempDir(), paths: map[int]string{}}
}

// build writes a seeded store at version into a fresh directory: a byte copy
// of the version's template, seeded through a writable handle that does not
// migrate.
func (f *fixtureTemplates) build(t *testing.T, version int, spec clientFixtureSpec) (string, clientFixture, error) {
	t.Helper()
	template, ok := f.paths[version]
	if !ok {
		template = filepath.Join(f.dir, fmt.Sprintf("template-%d.sqlite3", version))
		if err := BuildStoreAtVersion(template, version, nil); err != nil {
			return "", clientFixture{}, err
		}
		f.paths[version] = template
	}
	content, err := os.ReadFile(template)
	if err != nil {
		return "", clientFixture{}, err
	}
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", clientFixture{}, err
	}
	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return "", clientFixture{}, err
	}
	fixture, seedErr := seedClientFixture(&Store{db: db, schemaVersion: version}, version, spec)
	if err := errors.Join(seedErr, db.Close()); err != nil {
		return "", clientFixture{}, err
	}
	return path, fixture, nil
}

func mustFingerprint(t *testing.T, path string) StoreFingerprint {
	t.Helper()
	fingerprint, err := ReadStoreFingerprint(path)
	if err != nil {
		t.Fatalf("ReadStoreFingerprint(%s): %v", path, err)
	}
	return fingerprint
}

func assertFingerprintUnchanged(t *testing.T, label string, before, after StoreFingerprint) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		if before.Dump != after.Dump {
			t.Errorf("%s: logical dump changed:\nbefore:\n%s\nafter:\n%s", label, before.Dump, after.Dump)
		}
		before.Dump, after.Dump = "", ""
		t.Fatalf("%s: store fingerprint changed:\nbefore: %+v\nafter:  %+v", label, before, after)
	}
}

// assertOnlyWALSidecarsAdded checks that a read-only session created no file
// in the store's directory except the -wal and -shm sidecars.
func assertOnlyWALSidecarsAdded(t *testing.T, path string, before []string) {
	t.Helper()
	after := dirNames(t, filepath.Dir(path))
	base := filepath.Base(path)
	for _, name := range after {
		if slices.Contains(before, name) || name == base+"-wal" || name == base+"-shm" {
			continue
		}
		t.Fatalf("read-only session created %q (before: %v, after: %v)", name, before, after)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// clientRead is one entry of the client read inventory. run returns how many
// rows the call produced; a not-found result counts as a successful read of
// zero rows, because the SQL still ran against the schema.
type clientRead struct {
	name string
	run  func(ctx context.Context, store *Store, fixture clientFixture) (int, error)
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func fixtureConversationID(fixture clientFixture) string {
	if len(fixture.conversations) > 0 {
		return fixture.conversations[0].ConversationID
	}
	return "missing-conversation"
}

func notFoundAsEmpty(count int, err error) (int, error) {
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return count, err
}

// clientReadInventory is every store call internal/v2read makes, keyed as
// "<Source field>.<method>". TestClientReadInventoryCoversV2Read keeps this
// list equal to v2read's actual calls.
var clientReadInventory = []clientRead{
	{name: "store.ListAccounts", run: func(_ context.Context, store *Store, _ clientFixture) (int, error) {
		rows, err := store.ListAccounts()
		return len(rows), err
	}},
	{name: "store.ListConversationsByRecency", run: func(_ context.Context, store *Store, fixture clientFixture) (int, error) {
		rows, err := store.ListConversationsByRecency(firstOr(fixture.accountIDs, "missing-account"))
		return len(rows), err
	}},
	{name: "store.ListConversationsByRecencyAllAccounts", run: func(_ context.Context, store *Store, _ clientFixture) (int, error) {
		rows, err := store.ListConversationsByRecencyAllAccounts(50)
		return len(rows), err
	}},
	{name: "store.SearchConversationsByName", run: func(_ context.Context, store *Store, _ clientFixture) (int, error) {
		rows, err := store.SearchConversationsByName("Person", 50)
		return len(rows), err
	}},
	{name: "store.GetConversation", run: func(_ context.Context, store *Store, fixture clientFixture) (int, error) {
		_, err := store.GetConversation(fixtureConversationID(fixture))
		return notFoundAsEmpty(1, err)
	}},
	{name: "store.GetConversationByRemote", run: func(_ context.Context, store *Store, fixture clientFixture) (int, error) {
		accountID, remoteID := "missing-account", "missing-remote"
		if len(fixture.conversations) > 0 {
			accountID = fixture.conversations[0].AccountID
			remoteID = fixture.conversations[0].RemoteConversationID
		}
		_, err := store.GetConversationByRemote(accountID, remoteID)
		return notFoundAsEmpty(1, err)
	}},
	{name: "store.ListParticipants", run: func(_ context.Context, store *Store, fixture clientFixture) (int, error) {
		rows, err := store.ListParticipants(fixtureConversationID(fixture))
		return len(rows), err
	}},
	{name: "store.GetIdentity", run: func(_ context.Context, store *Store, fixture clientFixture) (int, error) {
		_, err := store.GetIdentity(firstOr(fixture.identityIDs, "missing-identity"))
		return notFoundAsEmpty(1, err)
	}},
	{name: "messages.ListMessagesByConversation", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewMessageRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		rows, err := repository.ListMessagesByConversation(ctx, fixtureConversationID(fixture), 0, "", 100)
		return len(rows), err
	}},
	{name: "messages.ListMessagesByConversationAfter", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewMessageRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		rows, err := repository.ListMessagesByConversationAfter(ctx, fixtureConversationID(fixture), 1, "", 100)
		return len(rows), err
	}},
	{name: "messages.ListMessagesAroundMessage", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewMessageRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		conversationID, messageID := fixtureConversationID(fixture), "missing-message"
		if len(fixture.messageIDs) > 0 {
			messageID = fixture.messageIDs[0]
			conversationID = fixture.messageConversation[messageID]
		}
		rows, err := repository.ListMessagesAroundMessage(ctx, conversationID, messageID, 5, 5)
		return notFoundAsEmpty(len(rows), err)
	}},
	{name: "messages.SearchMessages", run: func(ctx context.Context, store *Store, _ clientFixture) (int, error) {
		repository, err := NewMessageRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		rows, err := repository.SearchMessages(ctx, "e", SearchQuery{Limit: 100})
		return len(rows), err
	}},
	{name: "attachments.GetForDownload", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewMessageAttachmentRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		messageID := fixture.attachmentMsgID
		if messageID == "" {
			messageID = "missing-message"
		}
		_, err = repository.GetForDownload(ctx, messageID, 0)
		return notFoundAsEmpty(1, err)
	}},
	{name: "outbox.LatestStateForLocalMessage", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewOutboxRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		accountID, messageID := firstOr(fixture.accountIDs, "missing-account"), "missing-message"
		if fixture.outgoingMsgID != "" {
			messageID = fixture.outgoingMsgID
			accountID = fixture.messageAccount[messageID]
		}
		_, ok, err := repository.LatestStateForLocalMessage(ctx, accountID, messageID)
		if ok {
			return 1, err
		}
		return 0, err
	}},
	{name: "reactions.ReactionsForMessages", run: func(ctx context.Context, store *Store, fixture clientFixture) (int, error) {
		repository, err := NewReactionRepository(store, time.Now)
		if err != nil {
			return 0, err
		}
		// Always pass at least one ID: with none, the repository returns
		// without running SQL, and the tightness check would be vacuous.
		ids := append([]string{"missing-message"}, fixture.messageIDs...)
		rows, err := repository.ReactionsForMessages(ctx, ids)
		count := 0
		for _, list := range rows {
			count += len(list)
		}
		return count, err
	}},
}

func clientReadNames() []string {
	names := make([]string, 0, len(clientReadInventory))
	for _, read := range clientReadInventory {
		names = append(names, read.name)
	}
	return names
}

func findClientRead(t *testing.T, name string) clientRead {
	t.Helper()
	for _, read := range clientReadInventory {
		if read.name == name {
			return read
		}
	}
	t.Fatalf("client read %q is not in the inventory (have %s)", name, strings.Join(clientReadNames(), ", "))
	return clientRead{}
}
