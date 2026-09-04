package v2read

import (
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func seedSearchFixture(t *testing.T, store *sqlite.Store) {
	t.Helper()
	seedSourceAccount(t, store, "search-account", "google_messages")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-group",
		AccountID:            "search-account",
		RemoteConversationID: "remote-group",
		Kind:                 sqlite.ConversationKindGroup,
		Title:                "Byte-exact group",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      300,
	})
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-direct",
		AccountID:            "search-account",
		RemoteConversationID: "remote-direct",
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      200,
	})
	if err := store.UpsertIdentity(sqlite.Identity{
		IdentityID:     "identity-alice",
		AccountID:      "search-account",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550000001",
		RawValue:       "+1 (555) 000-0001",
		DisplayName:    "Alice Example",
		MetadataJSON:   `{}`,
		CreatedAtMS:    sourceTestTimeMS,
		UpdatedAtMS:    sourceTestTimeMS,
	}); err != nil {
		t.Fatalf("UpsertIdentity(): %v", err)
	}
	if err := store.UpsertIdentity(sqlite.Identity{
		IdentityID:     "identity-self",
		AccountID:      "search-account",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550009999",
		RawValue:       "+15550009999",
		DisplayName:    "Me",
		IsSelf:         true,
		MetadataJSON:   `{}`,
		CreatedAtMS:    sourceTestTimeMS,
		UpdatedAtMS:    sourceTestTimeMS,
	}); err != nil {
		t.Fatalf("UpsertIdentity(self): %v", err)
	}
	if err := store.ReplaceConversationParticipants("conversation-direct", []sqlite.ConversationParticipant{{
		AccountID:      "search-account",
		ConversationID: "conversation-direct",
		IdentityID:     "identity-alice",
		Role:           sqlite.ParticipantRoleMember,
		DisplayName:    "Ali (nickname)",
		IsActive:       true,
	}, {
		AccountID:      "search-account",
		ConversationID: "conversation-direct",
		IdentityID:     "identity-self",
		Role:           sqlite.ParticipantRoleMember,
		IsActive:       true,
	}}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}
}

func TestSearchConversationsByMetadataMatchesTitleParticipantsAndAddresses(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSearchFixture(t, store)

	for name, query := range map[string]string{
		"identity display name, case-insensitive": "alice",
		"participant display name":                "nickname",
		"canonical address":                       "5550000001",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := source.SearchConversationsByMetadata(query, 10)
			if err != nil {
				t.Fatalf("search %q: %v", query, err)
			}
			assertConversationIDs(t, got, "conversation-direct")
			if !strings.Contains(got[0].Participants, "Ali (nickname)") {
				t.Fatalf("direct conversation participants = %q, want the matched peer", got[0].Participants)
			}
			// The account owner is flagged so readers can tell it from the peer.
			if !strings.Contains(got[0].Participants, `"name":"Me","number":"+15550009999","is_me":true`) {
				t.Fatalf("self participant not flagged is_me: %q", got[0].Participants)
			}
			if strings.Contains(got[0].Participants, `"Ali (nickname)","number":"+15550000001","is_me"`) {
				t.Fatalf("peer participant wrongly flagged is_me: %q", got[0].Participants)
			}
		})
	}

	group, err := source.SearchConversationsByMetadata("BYTE-EXACT", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertConversationIDs(t, group, "conversation-group")
}

func TestSearchConversationsByMetadataOrdersNewestFirstAndBoundsResults(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSearchFixture(t, store)
	// "e" appears in both the group title and Alice's name.
	all, err := source.SearchConversationsByMetadata("e", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertConversationIDs(t, all, "conversation-group", "conversation-direct")

	one, err := source.SearchConversationsByMetadata("e", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertConversationIDs(t, one, "conversation-group")

	none, err := source.SearchConversationsByMetadata("zzz-no-such-thread", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("no-match search returned %d rows", len(none))
	}
	empty, err := source.SearchConversationsByMetadata("   ", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("blank query returned %d rows", len(empty))
	}
}

func TestSearchConversationsByMetadataTreatsLikeMetacharactersLiterally(t *testing.T) {
	store, _, source := openSourceTestStore(t)
	seedSearchFixture(t, store)

	for _, query := range []string{"%", "_", `\`} {
		got, err := source.SearchConversationsByMetadata(query, 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(got) != 0 {
			t.Fatalf("metacharacter query %q matched %d rows; it must match literally", query, len(got))
		}
	}
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-percent",
		AccountID:            "search-account",
		RemoteConversationID: "remote-percent",
		Kind:                 sqlite.ConversationKindGroup,
		Title:                "100% done",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      100,
	})
	got, err := source.SearchConversationsByMetadata("%", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertConversationIDs(t, got, "conversation-percent")
}
