package ingest

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Fictional numbers in the 555-01xx range reserved for examples.
const (
	stAccount = "google-primary"
	stSelf    = "+15550100000"
	stAda     = "+15550100001"
	stBea     = "+15550100002"
	stCal     = "+15550100003"
	stDee     = "+15550100004"
)

func stRef(kind string, peers ...string) bridge.ConversationRef {
	ref := bridge.ConversationRef{
		RemoteID: "remote-thread",
		Kind:     kind,
		Self:     []bridge.IdentityRef{{Kind: "e164", Canonical: stSelf}},
	}
	for _, peer := range peers {
		ref.Peers = append(ref.Peers, bridge.IdentityRef{Kind: "e164", Canonical: peer})
	}
	return ref
}

func stSnapshot(kind string, participants ...bridge.Participant) bridge.ConversationEvent {
	return bridge.ConversationEvent{RemoteConversationID: "remote-thread", Kind: kind, Participants: participants}
}

func stPeer(raw string) bridge.Participant {
	return bridge.Participant{Identity: bridge.IdentityRef{Raw: raw}, Active: true}
}

func stMe(raw string) bridge.Participant {
	return bridge.Participant{Identity: bridge.IdentityRef{Raw: raw, IsSelf: true}, Active: true}
}

func TestCheckSendTargetCases(t *testing.T) {
	tests := []struct {
		name     string
		ref      bridge.ConversationRef
		snapshot bridge.ConversationEvent
		want     bool
	}{
		{"same direct peer", stRef("direct", stAda), stSnapshot("direct", stMe(stSelf), stPeer(stAda)), false},
		{"direct thread now names someone else", stRef("direct", stAda), stSnapshot("direct", stMe(stSelf), stPeer(stBea)), true},
		{"own number listed without the self flag", stRef("direct", stAda), stSnapshot("direct", stPeer(stSelf), stPeer(stAda)), false},
		{"peer number formatted differently", stRef("direct", stAda), stSnapshot("direct", stPeer("+1 (555) 010-0001")), false},
		{"direct thread gained a second peer", stRef("direct", stAda), stSnapshot("direct", stPeer(stAda), stPeer(stBea)), true},
		{"group shares a member", stRef("group", stAda, stBea), stSnapshot("group", stPeer(stBea), stPeer(stCal)), false},
		{"group membership fully replaced", stRef("group", stAda, stBea), stSnapshot("group", stPeer(stCal), stPeer(stDee)), true},
		{"direct thread is a group on the phone", stRef("direct", stAda), stSnapshot("group", stPeer(stAda), stPeer(stBea)), true},
		{"group is a direct thread on the phone", stRef("group", stAda, stBea), stSnapshot("direct", stPeer(stAda)), true},
		{"no stored peers", stRef("direct"), stSnapshot("direct", stPeer(stBea)), false},
		{"no stored kind", stRef("", stAda), stSnapshot("direct", stPeer(stBea)), false},
		{"phone names only this account", stRef("direct", stAda), stSnapshot("direct", stMe(stSelf), stPeer(stSelf)), false},
		{"phone names no one", stRef("direct", stAda), stSnapshot("direct"), false},
		{"unkeyable entry beside the right peer", stRef("direct", stAda), stSnapshot("direct", stPeer("+"), stPeer(stAda)), false},
		{"only unkeyable or blank entries", stRef("direct", stAda), stSnapshot("direct", stPeer("+"), stPeer("  ")), false},
		{"another person flagged as self", stRef("direct", stAda), stSnapshot("direct", stMe(stBea), stPeer(stAda)), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CheckSendTarget(stAccount, bridge.PlatformGoogle, test.ref, test.snapshot)
			if got.Contradicts != test.want {
				t.Fatalf("CheckSendTarget() = %+v, want Contradicts %v", got, test.want)
			}
		})
	}
}

func TestCheckGoogleSendTargetDecodesThePhoneThread(t *testing.T) {
	conversation := &gmproto.Conversation{
		ConversationID: "remote-thread",
		Participants: []*gmproto.Participant{
			{ID: &gmproto.SmallInfo{Number: stSelf}, IsMe: true},
			{FormattedNumber: stBea},
			nil,
		},
	}
	check := CheckGoogleSendTarget(stAccount, stRef("direct", stAda), conversation)
	want := SendTargetCheck{
		Contradicts: true, StoredKind: "direct", RemoteKind: "direct",
		StoredPeers: 1, RemotePeers: 1, Shared: 0,
	}
	if check != want {
		t.Fatalf("CheckGoogleSendTarget() = %+v, want %+v", check, want)
	}
	conversation.IsGroupChat = true
	conversation.Participants = append(conversation.Participants, &gmproto.Participant{ID: &gmproto.SmallInfo{Number: stAda}})
	if check := CheckGoogleSendTarget(stAccount, stRef("group", stAda, stCal), conversation); check.Contradicts ||
		check.RemoteKind != "group" || check.RemotePeers != 2 || check.Shared != 1 {
		t.Fatalf("group CheckGoogleSendTarget() = %+v, want a consistent group sharing one member", check)
	}
}

// stCase is one generated send target: a stored conversation (kind, peers,
// this account's identities) and the phone's snapshot of its remote thread.
type stCase struct {
	refKind      string
	storedPeers  []string
	self         []bridge.IdentityRef
	snapshotKind string
	participants []bridge.Participant
}

var stPool = []string{stAda, stBea, stCal, stDee}

// stFormat writes a pool number the way the phone might: as stored, or
// formatted with spaces and punctuation that key to the same identity.
func stFormat(rng *rand.Rand, number string) string {
	if rng.Intn(3) != 0 {
		return number
	}
	digits := strings.TrimPrefix(number, "+1")
	return fmt.Sprintf("+1 (%s) %s-%s", digits[:3], digits[3:6], digits[6:])
}

func stGenerate(rng *rand.Rand, withUnkeyable bool) stCase {
	c := stCase{}
	switch rng.Intn(5) {
	case 0:
		c.refKind = ""
	case 1, 2:
		c.refKind = "direct"
	default:
		c.refKind = "group"
	}
	stored := rng.Intn(4)
	if c.refKind == "direct" && rng.Intn(4) != 0 {
		stored = 1
	}
	for _, index := range rng.Perm(len(stPool))[:stored] {
		c.storedPeers = append(c.storedPeers, stPool[index])
	}
	if rng.Intn(5) != 0 {
		c.self = append(c.self, bridge.IdentityRef{Kind: "e164", Canonical: stSelf})
	}
	if rng.Intn(3) == 0 {
		c.self = append(c.self, bridge.IdentityRef{Kind: "legacy_participant", Canonical: "legacy-participant:me"})
	}
	c.snapshotKind = "direct"
	if rng.Intn(2) == 0 {
		c.snapshotKind = "group"
	}
	if rng.Intn(4) == 0 {
		// The phone still agrees with the stored thread.
		if c.refKind != "" {
			c.snapshotKind = c.refKind
		}
		for _, peer := range c.storedPeers {
			c.participants = append(c.participants, stPeer(stFormat(rng, peer)))
		}
	}
	for i, n := 0, rng.Intn(6); i < n; i++ {
		switch pick := rng.Intn(10); {
		case pick < 6:
			c.participants = append(c.participants, stPeer(stFormat(rng, stPool[rng.Intn(len(stPool))])))
		case pick < 8:
			// The phone flags its own number, but not always.
			c.participants = append(c.participants, bridge.Participant{
				Identity: bridge.IdentityRef{Raw: stFormat(rng, stSelf), IsSelf: rng.Intn(2) == 0},
				Active:   true,
			})
		case pick < 9:
			c.participants = append(c.participants, stPeer("+15550100099"))
		default:
			if withUnkeyable {
				c.participants = append(c.participants, stPeer([]string{"+", "", "  "}[rng.Intn(3)]))
			} else {
				c.participants = append(c.participants, stPeer(stPool[0]))
			}
		}
	}
	return c
}

func (c stCase) ref() bridge.ConversationRef {
	return bridge.ConversationRef{RemoteID: "remote-thread", Kind: c.refKind, Peers: stRef("", c.storedPeers...).Peers, Self: c.self}
}

func (c stCase) snapshot() bridge.ConversationEvent {
	return stSnapshot(c.snapshotKind, c.participants...)
}

// stOracle is the rule in plain set terms, independent of CheckSendTarget's
// code: no evidence means no refusal; differing kinds refuse; a direct thread
// needs the same peer set and a group needs a shared member.
func stOracle(c stCase) SendTargetCheck {
	key := func(kind, canonical string) string { return kind + "\x1f" + canonical }
	stored := map[string]bool{}
	for _, peer := range c.storedPeers {
		stored[key("e164", peer)] = true
	}
	self := map[string]bool{}
	for _, identity := range c.self {
		self[key(identity.Kind, identity.Canonical)] = true
	}
	remote := map[string]bool{}
	for _, participant := range c.participants {
		raw := strings.TrimSpace(participant.Identity.Raw)
		if participant.Identity.IsSelf || raw == "" {
			continue
		}
		identity, err := v2keys.IdentityKey(stAccount, "google", raw)
		if err != nil {
			continue
		}
		if k := key(identity.Kind, identity.Canonical); !self[k] {
			remote[k] = true
		}
	}
	shared := 0
	for k := range remote {
		if stored[k] {
			shared++
		}
	}
	check := SendTargetCheck{
		StoredKind: c.refKind, RemoteKind: c.snapshotKind,
		StoredPeers: len(stored), RemotePeers: len(remote), Shared: shared,
	}
	switch {
	case c.refKind == "" || len(stored) == 0:
	case c.refKind != c.snapshotKind:
		check.Contradicts = true
	case len(remote) == 0:
	case c.refKind == "direct":
		check.Contradicts = shared != len(remote) || shared != len(stored)
	default:
		check.Contradicts = shared == 0
	}
	return check
}

// Invariants of the send-time check, over generated rosters:
//
//   - S1: the verdict and its counts equal the set-based oracle (stOracle).
//   - S2: the verdict does not depend on participant order or repetition.
//   - S3: a snapshot naming exactly the stored peers never refuses, whether
//     it writes each number as plain E.164 or as "+1 (555) 010-0001", and
//     whether or not it flags this account's own entry. A number without the
//     leading "+" keys as a username, a different identity, so S3 does not
//     cover it.
//   - S4: entries v2keys cannot key never change the verdict.
func TestCheckSendTargetPropertiesHold(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		c := stGenerate(rng, true)
		got := CheckSendTarget(stAccount, bridge.PlatformGoogle, c.ref(), c.snapshot())
		if want := stOracle(c); got != want {
			t.Logf("S1 seed %d: got %+v, want %+v (case %+v)", seed, got, want, c)
			return false
		}

		shuffled := c
		shuffled.participants = append([]bridge.Participant(nil), c.participants...)
		if len(shuffled.participants) > 0 {
			shuffled.participants = append(shuffled.participants, shuffled.participants[rng.Intn(len(shuffled.participants))])
		}
		rng.Shuffle(len(shuffled.participants), func(i, j int) {
			shuffled.participants[i], shuffled.participants[j] = shuffled.participants[j], shuffled.participants[i]
		})
		if again := CheckSendTarget(stAccount, bridge.PlatformGoogle, shuffled.ref(), shuffled.snapshot()); again != got {
			t.Logf("S2 seed %d: shuffled %+v, original %+v", seed, again, got)
			return false
		}

		mirror := c
		mirror.snapshotKind = c.refKind
		if mirror.snapshotKind == "" {
			mirror.snapshotKind = "direct"
		}
		mirror.participants = nil
		for _, peer := range c.storedPeers {
			mirror.participants = append(mirror.participants, stPeer(stFormat(rng, peer)))
		}
		mirror.participants = append(mirror.participants, bridge.Participant{
			Identity: bridge.IdentityRef{Raw: stSelf, IsSelf: true}, Active: true,
		})
		if len(c.self) > 0 && c.self[0].Canonical == stSelf {
			mirror.participants = append(mirror.participants, stPeer(stFormat(rng, stSelf)))
		}
		if check := CheckSendTarget(stAccount, bridge.PlatformGoogle, mirror.ref(), mirror.snapshot()); check.Contradicts {
			t.Logf("S3 seed %d: the check refused the stored roster itself: %+v", seed, check)
			return false
		}

		clean := c
		clean.participants = nil
		for _, participant := range c.participants {
			if _, err := v2keys.IdentityKey(stAccount, "google", participant.Identity.Raw); err == nil {
				clean.participants = append(clean.participants, participant)
			}
		}
		if without := CheckSendTarget(stAccount, bridge.PlatformGoogle, clean.ref(), clean.snapshot()); without != got {
			t.Logf("S4 seed %d: without unkeyable entries %+v, with %+v", seed, without, got)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261103))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// The send-time check and the history stale-binding check must agree: they
// apply one rule to the same stored thread and the same phone snapshot, but
// resolve identities differently. History looks each snapshot entry up in
// the store; the send check compares the natural keys the dispatcher passes
// (CheckSendTarget). Some stored identities here carry IDs that v2keys would
// not derive from their keys, as migrated ones do, and the account holds
// people outside the thread. The test leaves unkeyable entries out: history on
// this branch fails the snapshot on them (#207 changes that), while the send
// check ignores them (S4).
func TestCheckSendTargetAgreesWithHistoryStaleBindingCheck(t *testing.T) {
	store, worker := stOpenWorker(t)
	nowMS := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).UnixMilli()
	compared, contradicted := 0, 0
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		c := stGenerate(rng, false)
		if c.refKind == "" {
			c.refKind = "direct"
		}
		accountID := fmt.Sprintf("st-account-%d", compared)
		compared++
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: accountID, BridgeKey: "google", Mode: sqlite.AccountModeLive, Enabled: true,
			ConfigJSON: `{}`, CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}); err != nil {
			t.Fatalf("UpsertAccount(): %v", err)
		}
		identityID := func(kind, canonical string) string {
			if rng.Intn(3) == 0 {
				return fmt.Sprintf("migrated-%d-%x", compared, rng.Int63())
			}
			return v2keys.DeriveID("identity", accountID, kind+"\x1f"+canonical)
		}
		upsert := func(kind, canonical string, isSelf bool) string {
			id := identityID(kind, canonical)
			if err := store.UpsertIdentity(sqlite.Identity{
				IdentityID: id, AccountID: accountID, Kind: sqlite.IdentityKind(kind), CanonicalValue: canonical,
				RawValue: canonical, IsSelf: isSelf, MetadataJSON: `{}`, CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
			}); err != nil {
				t.Fatalf("UpsertIdentity(): %v", err)
			}
			return id
		}
		for _, identity := range c.self {
			upsert(identity.Kind, identity.Canonical, true)
		}
		peerIDs := map[string]string{}
		for _, number := range stPool {
			if rng.Intn(2) == 0 || containsString(c.storedPeers, number) {
				peerIDs[number] = upsert("e164", number, false)
			}
		}
		conversation := sqlite.Conversation{
			ConversationID: "st-conversation-" + accountID, AccountID: accountID,
			RemoteConversationID: "remote-thread", Kind: sqlite.ConversationKind(c.refKind),
			Title: "thread", NotificationMode: sqlite.NotificationModeAll, MetadataJSON: `{}`,
			CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("UpsertConversation(): %v", err)
		}
		participants := make([]sqlite.ConversationParticipant, 0, len(c.storedPeers)+2)
		for _, number := range c.storedPeers {
			participants = append(participants, sqlite.ConversationParticipant{
				AccountID: accountID, ConversationID: conversation.ConversationID,
				IdentityID: peerIDs[number], Role: sqlite.ParticipantRoleMember, IsActive: true,
			})
		}
		// A departed member and this account's own row prove nothing.
		for _, number := range stPool {
			if id, ok := peerIDs[number]; ok && !containsString(c.storedPeers, number) && rng.Intn(3) == 0 {
				participants = append(participants, sqlite.ConversationParticipant{
					AccountID: accountID, ConversationID: conversation.ConversationID,
					IdentityID: id, Role: sqlite.ParticipantRoleMember, IsActive: false,
				})
				break
			}
		}
		if err := store.ReplaceConversationParticipants(conversation.ConversationID, participants); err != nil {
			t.Fatalf("ReplaceConversationParticipants(): %v", err)
		}

		// The ref the dispatcher builds (messaging.conversationRef).
		ref := bridge.ConversationRef{RemoteID: conversation.RemoteConversationID, Kind: string(conversation.Kind)}
		peers, err := store.ListConversationPeerIdentities(accountID, conversation.ConversationID)
		if err != nil {
			t.Fatalf("ListConversationPeerIdentities(): %v", err)
		}
		self, err := store.ListSelfIdentities(accountID)
		if err != nil {
			t.Fatalf("ListSelfIdentities(): %v", err)
		}
		for _, identity := range peers {
			ref.Peers = append(ref.Peers, bridge.IdentityRef{Kind: string(identity.Kind), Canonical: identity.CanonicalValue})
		}
		for _, identity := range self {
			ref.Self = append(ref.Self, bridge.IdentityRef{Kind: string(identity.Kind), Canonical: identity.CanonicalValue})
		}

		send := CheckSendTarget(accountID, bridge.PlatformGoogle, ref, c.snapshot())
		history, err := worker.historySnapshotContradicts(accountID, bridge.PlatformGoogle, c.snapshot(), conversation)
		if err != nil {
			t.Fatalf("historySnapshotContradicts(): %v", err)
		}
		if send.Contradicts {
			contradicted++
		}
		if send.Contradicts != history {
			t.Logf("seed %d: send check %+v, history check %v (case %+v)", seed, send, history, c)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(1103))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	if contradicted == 0 || contradicted == compared {
		t.Fatalf("%d of %d generated cases contradicted; want a mix", contradicted, compared)
	}
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func stOpenWorker(t *testing.T) (*sqlite.Store, *Worker) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &Counters{}
	worker, err := NewWorker(WorkerConfig{
		Store:     store,
		Messages:  messages,
		Reactions: reactions,
		Counters:  counters,
		Logger:    zerolog.Nop(),
		Now:       now,
		Decoders: []DecoderRegistration{{
			Codec:    GoogleCodec,
			Platform: bridge.PlatformGoogle,
			Decoder:  NewGoogleDecoder(counters),
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	return store, worker
}

// The oracle's own sanity: it must reach every verdict branch, or S1 proves
// little.
func TestCheckSendTargetGeneratorReachesEveryBranch(t *testing.T) {
	rng := rand.New(rand.NewSource(20261103))
	branches := map[string]int{}
	for i := 0; i < 3000; i++ {
		c := stGenerate(rng, true)
		check := stOracle(c)
		var branch string
		switch {
		case c.refKind == "" || check.StoredPeers == 0:
			branch = "no stored roster"
		case c.refKind != c.snapshotKind:
			branch = "kind differs"
		case check.RemotePeers == 0:
			branch = "phone names no peer"
		case check.Contradicts:
			branch = c.refKind + " contradicts"
		default:
			branch = c.refKind + " consistent"
		}
		branches[branch]++
	}
	want := []string{"no stored roster", "kind differs", "phone names no peer",
		"direct contradicts", "direct consistent", "group contradicts", "group consistent"}
	var missing []string
	for _, branch := range want {
		if branches[branch] < 20 {
			missing = append(missing, fmt.Sprintf("%s (%d)", branch, branches[branch]))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("generator rarely reaches: %s", strings.Join(missing, ", "))
	}
}
