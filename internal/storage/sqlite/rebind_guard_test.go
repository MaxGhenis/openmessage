package sqlite

import (
	"errors"
	"testing"
)

// ReassignConversationRemoteIDFrom checks the conversation's current binding
// inside its write transaction, so a caller that read the binding earlier can
// never overwrite a newer one (the PR #204 review's ingest race). Each case
// starts from: conversation-a bound to remote-conversation-a (the outbox's
// conversation) and conversation-b bound to remote-conversation-b.
func TestReassignConversationRemoteIDFromGuardsTheCurrentBinding(t *testing.T) {
	const later = repositoryTestTimeMS + 1000
	setup := func(t *testing.T) *Store {
		t.Helper()
		store := openRepositoryTestStore(t)
		seedMessageAccount(t, store, "account-a", "google")
		seedMessageConversation(t, store, "conversation-a", "account-a")
		seedMessageConversation(t, store, "conversation-b", "account-a")
		return store
	}
	binding := func(t *testing.T, store *Store, conversationID string) string {
		t.Helper()
		conversation, err := store.GetConversation(conversationID)
		if err != nil {
			t.Fatalf("GetConversation(%q): %v", conversationID, err)
		}
		return conversation.RemoteConversationID
	}

	t.Run("still bound to the expected ID moves and displaces the holder", func(t *testing.T) {
		store := setup(t)
		if err := store.ReassignConversationRemoteIDFrom(
			"account-a", "remote-conversation-a", "remote-conversation-b", "conversation-a", later,
		); err != nil {
			t.Fatalf("ReassignConversationRemoteIDFrom(): %v", err)
		}
		if got := binding(t, store, "conversation-a"); got != "remote-conversation-b" {
			t.Fatalf("conversation-a bound to %q, want remote-conversation-b", got)
		}
		if got := binding(t, store, "conversation-b"); got != displacedRemoteID("remote-conversation-b", "conversation-b") {
			t.Fatalf("conversation-b bound to %q, want the displaced marker", got)
		}
	})

	t.Run("already at the target ID is a no-op", func(t *testing.T) {
		store := setup(t)
		if err := store.ReassignConversationRemoteIDFrom(
			"account-a", "remote-somewhere-else", "remote-conversation-a", "conversation-a", later,
		); err != nil {
			t.Fatalf("ReassignConversationRemoteIDFrom(): %v", err)
		}
		if got := binding(t, store, "conversation-a"); got != "remote-conversation-a" {
			t.Fatalf("conversation-a bound to %q, want it unchanged", got)
		}
	})

	t.Run("rebound since the caller looked changes nothing", func(t *testing.T) {
		store := setup(t)
		// The caller expected remote-stale; another writer has since bound
		// conversation-a to remote-conversation-a.
		err := store.ReassignConversationRemoteIDFrom(
			"account-a", "remote-stale", "remote-conversation-b", "conversation-a", later,
		)
		if !errors.Is(err, ErrConversationRebound) {
			t.Fatalf("ReassignConversationRemoteIDFrom() error = %v, want ErrConversationRebound", err)
		}
		if got := binding(t, store, "conversation-a"); got != "remote-conversation-a" {
			t.Fatalf("conversation-a bound to %q, want it unchanged", got)
		}
		if got := binding(t, store, "conversation-b"); got != "remote-conversation-b" {
			t.Fatalf("conversation-b bound to %q, want its holder left in place", got)
		}
	})

	t.Run("missing conversation is not found", func(t *testing.T) {
		store := setup(t)
		err := store.ReassignConversationRemoteIDFrom(
			"account-a", "remote-conversation-a", "remote-conversation-b", "conversation-missing", later,
		)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("ReassignConversationRemoteIDFrom() error = %v, want ErrNotFound", err)
		}
		if got := binding(t, store, "conversation-b"); got != "remote-conversation-b" {
			t.Fatalf("conversation-b bound to %q, want its holder left in place", got)
		}
	})

	t.Run("an empty expected binding is refused", func(t *testing.T) {
		store := setup(t)
		if err := store.ReassignConversationRemoteIDFrom(
			"account-a", " ", "remote-conversation-b", "conversation-a", later,
		); err == nil {
			t.Fatal("ReassignConversationRemoteIDFrom() accepted an empty expected binding")
		}
	})
}
