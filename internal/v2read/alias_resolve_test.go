package v2read

import (
	"database/sql"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

func TestResolveConversationReturnsCanonicalRowOrNotFound(t *testing.T) {
	store, _, _ := openSourceTestStore(t)
	seedSourceAccount(t, store, "signal-primary", "signal_cli")
	const remoteID = "signal:+15550000002"
	conversationID := v2keys.DeriveID("conversation", "signal-primary", remoteID)
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            "signal-primary",
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      400,
	})

	for _, key := range []string{
		conversationID,
		" " + conversationID + "\n",
		remoteID,
		"\t" + remoteID + " ",
		// Signal remote IDs normalize the address payload as they did at
		// cutover.
		"signal: +15550000002",
	} {
		conversation, err := ResolveConversation(store, key)
		if err != nil {
			t.Fatalf("ResolveConversation(%q): %v", key, err)
		}
		if conversation.ConversationID != conversationID || conversation.AccountID != "signal-primary" {
			t.Fatalf("ResolveConversation(%q) = %+v, want %q", key, conversation, conversationID)
		}
	}
	for _, key := range []string{"", "  ", "signal:+15550009999", v2keys.DeriveID("conversation", "signal-primary", "absent")} {
		if _, err := ResolveConversation(store, key); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("ResolveConversation(%q) error = %v, want ErrNotFound", key, err)
		}
	}
}

// TestResolveConversationMatchesReferenceAndReadPath checks, over random
// stores whose remote IDs collide across accounts and whose recency ties,
// that ResolveConversation agrees with a brute-force reading of the alias
// rule, and that the v2 read path (Source.GetConversation) serves the same
// conversation. The second check is what lets a write keyed by a
// caller-supplied ID land on the thread a read of that ID shows.
func TestResolveConversationMatchesReferenceAndReadPath(t *testing.T) {
	accounts := []struct {
		accountID string
		bridgeKey string
	}{
		{"google-primary", "google_messages"},
		{"signal-primary", "signal_cli"},
		{"whatsapp-primary", "whatsmeow"},
	}
	remotePool := []string{
		"thread-1",
		"thread-2",
		"signal:+15550000001",
		"signal-group:QUJD=",
		"15550000003@s.whatsapp.net",
	}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store, _, source := openSourceTestStore(t)
		var conversations []sqlite.Conversation
		platformOf := map[string]string{}
		for _, account := range accounts {
			seedSourceAccount(t, store, account.accountID, account.bridgeKey)
			platformOf[account.accountID] = platformForBridgeKey(account.bridgeKey)
			for _, remoteID := range remotePool {
				if random.Intn(2) == 0 {
					continue
				}
				conversation := sqlite.Conversation{
					ConversationID:       v2keys.DeriveID("conversation", account.accountID, remoteID),
					AccountID:            account.accountID,
					RemoteConversationID: remoteID,
					Kind:                 sqlite.ConversationKindDirect,
					NotificationMode:     sqlite.NotificationModeAll,
					// Few distinct values, so recency ties are common.
					LastMessageAtMS: int64(random.Intn(3)) * 100,
				}
				seedSourceConversation(t, store, conversation)
				conversations = append(conversations, conversation)
			}
		}

		// reference is the alias rule written out over the seeded rows.
		reference := func(key string) (string, bool) {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				return "", false
			}
			for _, conversation := range conversations {
				if conversation.ConversationID == trimmed {
					return trimmed, true
				}
			}
			var best *sqlite.Conversation
			for index := range conversations {
				conversation := &conversations[index]
				if conversation.RemoteConversationID != v2keys.NormalizeRemoteConversationID(platformOf[conversation.AccountID], trimmed) {
					continue
				}
				if best == nil || conversation.LastMessageAtMS > best.LastMessageAtMS ||
					(conversation.LastMessageAtMS == best.LastMessageAtMS && conversation.ConversationID < best.ConversationID) {
					best = conversation
				}
			}
			if best == nil {
				return "", false
			}
			return best.ConversationID, true
		}

		keys := []string{"", " ", "unknown-thread", "signal: +15550000001", "signal-group: QUJD="}
		for _, remoteID := range remotePool {
			keys = append(keys, remoteID, " "+remoteID+"\t")
		}
		for _, account := range accounts {
			for _, remoteID := range remotePool {
				keys = append(keys, v2keys.DeriveID("conversation", account.accountID, remoteID))
			}
		}
		for _, key := range keys {
			wantID, wantFound := reference(key)
			resolved, err := ResolveConversation(store, key)
			if wantFound != (err == nil) || (err != nil && !errors.Is(err, sqlite.ErrNotFound)) {
				t.Errorf("seed %d: ResolveConversation(%q) error = %v, want found=%v", seed, key, err, wantFound)
				return false
			}
			if wantFound && resolved.ConversationID != wantID {
				t.Errorf("seed %d: ResolveConversation(%q) = %q, reference %q", seed, key, resolved.ConversationID, wantID)
				return false
			}
			read, err := source.GetConversation(key)
			if !wantFound {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Errorf("seed %d: GetConversation(%q) = %+v, %v; want sql.ErrNoRows", seed, key, read, err)
					return false
				}
				continue
			}
			if err != nil || read.ConversationID != wantID {
				t.Errorf("seed %d: GetConversation(%q) = %+v, %v; want %q", seed, key, read, err, wantID)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
}
