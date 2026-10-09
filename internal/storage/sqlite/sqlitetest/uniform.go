package sqlitetest

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Uniform sizes a uniform store.
type Uniform struct {
	Accounts      int
	Conversations int // per account
	Participants  int // identities per account, all in every conversation
	MessagesPer   int // per conversation
}

// UniformStore is a store built by BuildUniformStore, reopened through the
// counting driver.
type UniformStore struct {
	Path string
	// ConversationIDs lists every conversation, account by account, oldest
	// first within an account.
	ConversationIDs []string
	Store           *sqlite.Store
	Counter         *Counter
}

// BuildUniformStore writes Accounts × Conversations group conversations, each
// holding every one of the account's Participants identities (the first is
// the account's self identity) and MessagesPer messages: even messages are
// incoming with a sender, odd ones outgoing with a confirmed outbox row; every
// third has an ordinal-0 attachment and every fourth a reaction. Every body
// holds "needle". It then reopens the file through OpenCounting.
func BuildUniformStore(t testing.TB, shape Uniform) *UniformStore {
	accounts, conversations, participants, messagesPer := shape.Accounts, shape.Conversations, shape.Participants, shape.MessagesPer
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	blank, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	if err := blank.Close(); err != nil {
		t.Fatal(err)
	}
	raw := OpenRaw(t, path)
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	var ids []string
	for a := 0; a < accounts; a++ {
		accountID := fmt.Sprintf("budget-%d", a)
		exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, ?, 1, 1)`,
			accountID, []string{"google_messages", "whatsmeow", "signal_cli"}[a%3])
		for p := 0; p < participants; p++ {
			exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, is_self, created_at_ms, updated_at_ms)
				VALUES (?, ?, 'e164', ?, ?, ?, ?, 1, 1)`,
				fmt.Sprintf("%s-id-%d", accountID, p), accountID, fmt.Sprintf("+1555%04d%03d", a, p),
				fmt.Sprintf("+1555%04d%03d", a, p), fmt.Sprintf("Person %d", p), BoolInt(p == 0))
		}
		for c := 0; c < conversations; c++ {
			conversationID := fmt.Sprintf("%s-conv-%04d", accountID, c)
			ids = append(ids, conversationID)
			exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms)
				VALUES (?, ?, ?, 'group', ?, 1, 1)`, conversationID, accountID, "remote-"+conversationID, 1+c)
			for p := 0; p < participants; p++ {
				exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES (?, ?, ?)`,
					accountID, conversationID, fmt.Sprintf("%s-id-%d", accountID, p))
			}
			for m := 0; m < messagesPer; m++ {
				messageID := fmt.Sprintf("%s-msg-%04d", conversationID, m)
				if m%2 == 0 {
					exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
						VALUES (?, ?, ?, ?, ?, 'incoming', 'hi needle', ?, 1, 1)`,
						messageID, conversationID, accountID, "r-"+messageID, fmt.Sprintf("%s-id-%d", accountID, m%participants), 1+m)
				} else {
					exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
						VALUES (?, ?, ?, ?, 'outgoing', 'sent needle', ?, 1, 1)`, messageID, conversationID, accountID, "r-"+messageID, 1+m)
					exec(`INSERT INTO outbox (outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation, state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms)
						VALUES (?, ?, ?, 'text', ?, 'h', 'send_text', 'confirmed', ?, ?, 1, 1, 1)`,
						"ob-"+messageID, accountID, conversationID, "k-"+messageID, messageID, "t-"+messageID)
				}
				if m%3 == 0 {
					exec(`INSERT INTO message_attachments (message_id, ordinal, mime, created_at_ms, updated_at_ms) VALUES (?, 0, 'image/png', 1, 1)`, messageID)
				}
				if m%4 == 0 {
					exec(`INSERT INTO reactions (message_id, reactor_key, account_id, conversation_id, reactor_label, emoji, occurred_at_ms, created_at_ms, updated_at_ms)
						VALUES (?, 'r', ?, ?, 'x', '👍', 1, 1, 1)`, messageID, accountID, conversationID)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store, counter, err := OpenCounting(path)
	if err != nil {
		t.Fatalf("OpenCounting(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter.Reset()
	return &UniformStore{Path: path, ConversationIDs: ids, Store: store, Counter: counter}
}
