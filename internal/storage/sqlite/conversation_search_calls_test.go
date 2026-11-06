package sqlite

import (
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	modernsqlite "modernc.org/sqlite"
)

// countedIdentityMatch tests whether its two arguments hold the same text,
// and countedIdentityMatches counts its calls.
const countedIdentityMatch = "om_test_counted_identity_match"

var countedIdentityMatches atomic.Int64

func init() {
	modernsqlite.MustRegisterDeterministicScalarFunction(countedIdentityMatch, 2, func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedIdentityMatches.Add(1)
		if args[0] == args[1] {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// Invariant: the phone line and address searches call the Go matcher at most
// once for each identity, however many threads it joins. Alice and Bob share
// 30 threads with the owner, Carol has one and Dan has none, so a plan that
// tests each participant row calls the matcher 61 times. The route tool runs
// these searches once for each number on an address-book card that a name
// finds.
func TestSearchConversationsByIdentityCallsTheMatcherOncePerIdentity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	const account, at = "calls-account", int64(1000)
	if err := store.UpsertAccount(Account{
		AccountID: account, BridgeKey: "google_messages", DisplayName: account, Mode: AccountModeLive, Enabled: true,
		ConfigJSON: `{}`, CreatedAtMS: at, UpdatedAtMS: at,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	identities := []struct {
		name string
		self bool
	}{{"me", true}, {"alice", false}, {"bob", false}, {"carol", false}, {"dan", false}}
	for _, identity := range identities {
		if err := store.UpsertIdentity(Identity{
			IdentityID: "identity-" + identity.name, AccountID: account, Kind: IdentityKind("username"),
			CanonicalValue: identity.name, RawValue: identity.name, IsSelf: identity.self, MetadataJSON: `{}`, CreatedAtMS: at, UpdatedAtMS: at,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.name, err)
		}
	}
	seedThread := func(id string, lastMessageAt int64, members ...string) {
		t.Helper()
		if err := store.UpsertConversation(Conversation{
			ConversationID: id, AccountID: account, RemoteConversationID: "remote-" + id, Kind: ConversationKindGroup,
			NotificationMode: NotificationModeAll, LastMessageAtMS: lastMessageAt, MetadataJSON: `{}`, CreatedAtMS: at, UpdatedAtMS: at,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", id, err)
		}
		participants := []ConversationParticipant{}
		for _, member := range members {
			participants = append(participants, ConversationParticipant{
				AccountID: account, ConversationID: id, IdentityID: "identity-" + member, Role: ParticipantRoleMember, IsActive: true,
			})
		}
		if err := store.ReplaceConversationParticipants(id, participants); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", id, err)
		}
	}
	for index := 0; index < 30; index++ {
		seedThread(fmt.Sprintf("shared-%02d", index), at+int64(index), "me", "alice", "bob")
	}
	seedThread("carols", at+100, "me", "carol")

	for _, test := range []struct {
		value string
		limit int
		want  []string
	}{
		{"alice", 3, []string{"shared-29", "shared-28", "shared-27"}},
		{"bob", 2, []string{"shared-29", "shared-28"}},
		{"carol", 3, []string{"carols"}},
		{"dan", 3, []string{}},
		{"me", 3, []string{}},
		{"nobody", 3, []string{}},
	} {
		countedIdentityMatches.Store(0)
		conversations, err := store.searchConversationsByIdentity(countedIdentityMatch+"(?, i.canonical_value)", test.value, test.limit)
		if err != nil {
			t.Fatalf("searchConversationsByIdentity(%q): %v", test.value, err)
		}
		got := []string{}
		for _, conversation := range conversations {
			got = append(got, conversation.ConversationID)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("searchConversationsByIdentity(%q, %d) = %v, want %v", test.value, test.limit, got, test.want)
		}
		if calls := countedIdentityMatches.Load(); calls > int64(len(identities)) {
			t.Errorf("searchConversationsByIdentity(%q) called the matcher %d times, want at most once for each of %d identities", test.value, calls, len(identities))
		}
	}
}
