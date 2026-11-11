package sqlite

// Tests for MessageRepository.InsertHistoricalMessage: insert-only semantics
// for messages recovered from fetched history (DESIGN invariant I3). Reuses
// the messages_test.go helpers (openMessageTestRepository,
// newMessageTestClock, seedMessageProjectionGraph, seedMessageAccount,
// seedMessageIdentity, seedMessageConversation, messageTestMessage, pointer)
// and assertRowCount from store_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
)

func TestInsertHistoricalMessageInsertsNewRowWithAttachments(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	message := messageTestMessage("message-history-new", "conversation-a", "account-a", "remote-history-new", pointer("identity-a"))
	message.ReplyToRemoteID = pointer("remote-parent")
	size := int64(77)
	inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
		Message: message,
		Attachments: []MessageAttachment{
			{
				MessageID: "caller-supplied-parent-is-ignored",
				Ordinal:   0,
				RemoteID:  "media-0",
				RemoteRef: []byte(`{"v":1}`),
				Filename:  "photo.jpg",
				MIME:      "image/jpeg",
				SizeBytes: &size,
			},
			{Ordinal: 1, RemoteID: "media-1"},
		},
	})
	if err != nil {
		t.Fatalf("InsertHistoricalMessage(): %v", err)
	}
	if !inserted {
		t.Fatal("InsertHistoricalMessage() inserted = false for a new message")
	}

	got, err := repository.GetMessage(context.Background(), message.MessageID)
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	want := message
	want.CreatedAtMS = messageTestTimeMS
	want.UpdatedAtMS = messageTestTimeMS
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored message = %+v, want %+v", got, want)
	}

	attachments, err := NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	first, err := attachments.GetForDownload(context.Background(), message.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(0): %v", err)
	}
	if first.MessageID != message.MessageID || first.AccountID != "account-a" || first.RemoteID != "media-0" ||
		string(first.RemoteRef) != `{"v":1}` || first.Filename != "photo.jpg" || first.MIME != "image/jpeg" ||
		first.SizeBytes == nil || *first.SizeBytes != 77 || first.State != "pending" || first.BlobHash != nil {
		t.Fatalf("attachment 0 = %+v", first)
	}
	second, err := attachments.GetForDownload(context.Background(), message.MessageID, 1)
	if err != nil {
		t.Fatalf("GetForDownload(1): %v", err)
	}
	if second.MessageID != message.MessageID || second.RemoteID != "media-1" ||
		second.MIME != "application/octet-stream" || len(second.RemoteRef) != 0 || second.SizeBytes != nil {
		t.Fatalf("attachment 1 = %+v, want defaults applied", second)
	}
	assertRowCount(t, store.db, "messages", 1)
	assertRowCount(t, store.db, "message_attachments", 2)
	// Like ImportMessage, a historical insert needs and touches no inbox row.
	assertRowCount(t, store.db, "inbox", 0)
}

func TestInsertHistoricalMessageNeverUpdatesExistingRow(t *testing.T) {
	for _, test := range []struct {
		name            string
		secondMessageID string
	}{
		// Primary key and natural key both conflict.
		{name: "same message id", secondMessageID: "message-history-original"},
		// Only the (account, conversation, remote) natural key conflicts, as
		// when a re-derived or rebound ID differs from the stored row's.
		{name: "natural key only", secondMessageID: "message-history-other-id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newMessageTestClock(messageTestTimeMS)
			store, repository := openMessageTestRepository(t, clock.Now)
			seedMessageProjectionGraph(t, store)

			original := messageTestMessage("message-history-original", "conversation-a", "account-a", "remote-shared", pointer("identity-a"))
			inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
				Message:     original,
				Attachments: []MessageAttachment{{Ordinal: 0, RemoteID: "media-original", MIME: "image/png"}},
			})
			if err != nil || !inserted {
				t.Fatalf("InsertHistoricalMessage(original) = %v, %v; want true, nil", inserted, err)
			}
			messagesBefore := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`)
			attachmentsBefore := hinsDump(t, store, `SELECT * FROM message_attachments ORDER BY message_id, ordinal`)

			clock.Set(messageTestTimeMS + 5_000)
			changed := original
			changed.MessageID = test.secondMessageID
			changed.SenderIdentityID = nil
			changed.Direction = MessageDirectionOutgoing
			changed.Body = "fetched copy with different content"
			changed.ReplyToRemoteID = pointer("remote-other-parent")
			changed.State = MessageStateEdited
			changed.OccurredAtMS = original.OccurredAtMS + 9_999
			for _, state := range []MessageState{MessageStateEdited, MessageStateDeleted, MessageStateActive} {
				changed.State = state
				inserted, err = repository.InsertHistoricalMessage(context.Background(), MessageProjection{
					Message: changed,
					Attachments: []MessageAttachment{
						{Ordinal: 0, RemoteID: "media-replacement", MIME: "video/mp4"},
						{Ordinal: 1, RemoteID: "media-extra"},
					},
				})
				if err != nil {
					t.Fatalf("InsertHistoricalMessage(conflicting, state %s): %v", state, err)
				}
				if inserted {
					t.Fatalf("InsertHistoricalMessage(conflicting, state %s) inserted = true", state)
				}
			}

			// Byte-identical: every column including updated_at_ms, and the
			// attachment rows (no replacement, no extra ordinal).
			if after := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`); !reflect.DeepEqual(after, messagesBefore) {
				t.Fatalf("messages changed:\nbefore %q\n after %q", messagesBefore, after)
			}
			if after := hinsDump(t, store, `SELECT * FROM message_attachments ORDER BY message_id, ordinal`); !reflect.DeepEqual(after, attachmentsBefore) {
				t.Fatalf("attachments changed:\nbefore %q\n after %q", attachmentsBefore, after)
			}
			assertRowCount(t, store.db, "messages", 1)
			assertRowCount(t, store.db, "message_attachments", 1)
		})
	}
}

func TestInsertHistoricalMessagePrimaryKeyConflictInAnotherConversation(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	seedMessageConversation(t, store, "conversation-b", "account-a")

	existing := messageTestMessage("message-shared-pk", "conversation-a", "account-a", "remote-a", pointer("identity-a"))
	if inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
		Message:     existing,
		Attachments: []MessageAttachment{{Ordinal: 0, RemoteID: "media-a"}},
	}); err != nil || !inserted {
		t.Fatalf("InsertHistoricalMessage(existing) = %v, %v", inserted, err)
	}
	messagesBefore := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`)
	attachmentsBefore := hinsDump(t, store, `SELECT * FROM message_attachments ORDER BY message_id, ordinal`)

	clock.Set(messageTestTimeMS + 1_000)
	elsewhere := messageTestMessage("message-shared-pk", "conversation-b", "account-a", "remote-b", nil)
	elsewhere.Body = "a different message that happens to derive the same id"
	inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
		Message: elsewhere,
		// Attachments bind to message.MessageID, which here names the OTHER
		// conversation's row; recording them would graft media onto it.
		Attachments: []MessageAttachment{{Ordinal: 0, RemoteID: "media-b"}, {Ordinal: 3, RemoteID: "media-b3"}},
	})
	if err != nil {
		t.Fatalf("InsertHistoricalMessage(primary-key conflict): %v", err)
	}
	if inserted {
		t.Fatal("InsertHistoricalMessage(primary-key conflict) inserted = true")
	}
	if after := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`); !reflect.DeepEqual(after, messagesBefore) {
		t.Fatalf("messages changed:\nbefore %q\n after %q", messagesBefore, after)
	}
	if after := hinsDump(t, store, `SELECT * FROM message_attachments ORDER BY message_id, ordinal`); !reflect.DeepEqual(after, attachmentsBefore) {
		t.Fatalf("attachments changed:\nbefore %q\n after %q", attachmentsBefore, after)
	}
	if _, err := repository.GetMessageByRemote(context.Background(), "account-a", "conversation-b", "remote-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMessageByRemote(conversation-b) error = %v, want ErrNotFound", err)
	}
}

func TestInsertHistoricalMessageKeepsDeletedRowDeleted(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)

	tombstone := messageTestMessage("message-deleted", "conversation-a", "account-a", "remote-deleted", pointer("identity-a"))
	tombstone.State = MessageStateDeleted
	tombstone.Body = ""
	if err := repository.ImportMessage(context.Background(), MessageProjection{Message: tombstone}); err != nil {
		t.Fatalf("ImportMessage(tombstone): %v", err)
	}
	before := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`)

	clock.Set(messageTestTimeMS + 60_000)
	for _, messageID := range []string{"message-deleted", "message-deleted-rederived"} {
		resurrected := tombstone
		resurrected.MessageID = messageID
		resurrected.State = MessageStateActive
		resurrected.Body = "the original text the phone still had cached"
		inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{Message: resurrected})
		if err != nil {
			t.Fatalf("InsertHistoricalMessage(%s over tombstone): %v", messageID, err)
		}
		if inserted {
			t.Fatalf("InsertHistoricalMessage(%s over tombstone) inserted = true", messageID)
		}
	}
	got, err := repository.GetMessage(context.Background(), "message-deleted")
	if err != nil {
		t.Fatalf("GetMessage(): %v", err)
	}
	if got.State != MessageStateDeleted || got.Body != "" || got.UpdatedAtMS != messageTestTimeMS {
		t.Fatalf("tombstone after history insert = %+v, want still deleted and untouched", got)
	}
	if after := hinsDump(t, store, `SELECT * FROM messages ORDER BY message_id`); !reflect.DeepEqual(after, before) {
		t.Fatalf("messages changed:\nbefore %q\n after %q", before, after)
	}
}

func TestInsertHistoricalMessageConstraintViolationsStillError(t *testing.T) {
	store, repository := openMessageTestRepository(t, newMessageTestClock(messageTestTimeMS).Now)
	seedMessageProjectionGraph(t, store)
	seedMessageAccount(t, store, "account-b", "whatsapp")
	seedMessageIdentity(t, store, "identity-b", "account-b")
	seedMessageConversation(t, store, "conversation-b", "account-b")

	tests := []struct {
		name   string
		mutate func(*Message)
		want   []error
	}{
		{
			name:   "unknown conversation",
			mutate: func(m *Message) { m.ConversationID = "conversation-missing" },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation, ErrOrphanMessage, ErrOrphanMessageConversation},
		},
		{
			name:   "unknown sender identity",
			mutate: func(m *Message) { m.SenderIdentityID = pointer("identity-missing") },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation, ErrOrphanMessage, ErrOrphanMessageIdentity},
		},
		{
			name:   "cross-account conversation",
			mutate: func(m *Message) { m.ConversationID = "conversation-b" },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation, ErrCrossAccountMessage},
		},
		{
			name:   "cross-account sender",
			mutate: func(m *Message) { m.SenderIdentityID = pointer("identity-b") },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation, ErrCrossAccountMessage},
		},
		// ON CONFLICT DO NOTHING covers uniqueness only; CHECK constraints still
		// reject a malformed row.
		{
			name:   "non-positive occurred_at",
			mutate: func(m *Message) { m.OccurredAtMS = 0 },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation},
		},
		{
			name:   "unknown direction",
			mutate: func(m *Message) { m.Direction = MessageDirection("sideways") },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation},
		},
		{
			name:   "unknown state",
			mutate: func(m *Message) { m.State = MessageState("archived") },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation},
		},
		{
			name:   "blank remote id",
			mutate: func(m *Message) { m.RemoteMessageID = "  " },
			want:   []error{ErrInvalidMessage, ErrConstraintViolation},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := messageTestMessage(
				fmt.Sprintf("message-invalid-%d", index), "conversation-a", "account-a",
				fmt.Sprintf("remote-invalid-%d", index), pointer("identity-a"),
			)
			test.mutate(&message)
			inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
				Message:     message,
				Attachments: []MessageAttachment{{Ordinal: 0, RemoteID: "media"}},
			})
			if inserted {
				t.Fatal("InsertHistoricalMessage() inserted = true for an invalid row")
			}
			for _, want := range test.want {
				if !errors.Is(err, want) {
					t.Fatalf("InsertHistoricalMessage() error = %v, want %v", err, want)
				}
			}
		})
	}
	assertRowCount(t, store.db, "messages", 0)
	assertRowCount(t, store.db, "message_attachments", 0)

	// A failing attachment rolls the message insert back with it.
	message := messageTestMessage("message-bad-attachment", "conversation-a", "account-a", "remote-bad-attachment", nil)
	inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
		Message:     message,
		Attachments: []MessageAttachment{{Ordinal: 0, RemoteID: "ok"}, {Ordinal: -1, RemoteID: "bad"}},
	})
	if err == nil || inserted {
		t.Fatalf("InsertHistoricalMessage(bad attachment) = %v, %v; want false and an error", inserted, err)
	}
	assertRowCount(t, store.db, "messages", 0)
	assertRowCount(t, store.db, "message_attachments", 0)

	// Observed boundary: when the primary key already exists, SQLite resolves
	// the uniqueness conflict before checking foreign keys, so a conflicting
	// row with an unknown conversation is a silent no-op rather than an error.
	// Nothing is written, so nothing can be orphaned.
	existing := messageTestMessage("message-existing", "conversation-a", "account-a", "remote-existing", nil)
	if inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{Message: existing}); err != nil || !inserted {
		t.Fatalf("InsertHistoricalMessage(existing) = %v, %v", inserted, err)
	}
	orphanTwin := existing
	orphanTwin.ConversationID = "conversation-missing"
	if inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{Message: orphanTwin}); err != nil || inserted {
		t.Fatalf("InsertHistoricalMessage(pk conflict with unknown conversation) = %v, %v; want false, nil", inserted, err)
	}
	assertRowCount(t, store.db, "messages", 1)
}

// hinsCall is one generated InsertHistoricalMessage call.
type hinsCall struct {
	Message     Message
	Attachments []MessageAttachment
}

// hinsSequence is a generated sequence of calls over a small id space: two
// accounts, three conversations, four remote IDs, message IDs usually derived
// from the natural key (as the worker derives them) and sometimes drawn from a
// tiny shared "stray" pool that collides across keys and accounts.
type hinsSequence struct {
	Calls []hinsCall
}

var hinsConversations = []struct {
	accountID      string
	conversationID string
	senderID       string
}{
	{accountID: "account-a", conversationID: "conversation-a", senderID: "identity-a"},
	{accountID: "account-a", conversationID: "conversation-a2", senderID: "identity-a"},
	{accountID: "account-b", conversationID: "conversation-b", senderID: "identity-b"},
}

func (hinsSequence) Generate(r *rand.Rand, _ int) reflect.Value {
	states := []MessageState{MessageStateActive, MessageStateEdited, MessageStateDeleted}
	directions := []MessageDirection{MessageDirectionIncoming, MessageDirectionOutgoing}
	calls := make([]hinsCall, 1+r.Intn(24))
	for index := range calls {
		thread := hinsConversations[r.Intn(len(hinsConversations))]
		remoteID := fmt.Sprintf("remote-%d", r.Intn(4))
		messageID := "m|" + thread.conversationID + "|" + remoteID
		if r.Intn(5) == 0 {
			messageID = fmt.Sprintf("m-stray-%d", r.Intn(2))
		}
		message := Message{
			MessageID:       messageID,
			ConversationID:  thread.conversationID,
			AccountID:       thread.accountID,
			RemoteMessageID: remoteID,
			Direction:       directions[r.Intn(len(directions))],
			Body:            fmt.Sprintf("body-%d-%d", index, r.Intn(1000)),
			State:           states[r.Intn(len(states))],
			OccurredAtMS:    1 + r.Int63n(messageTestTimeMS),
		}
		if r.Intn(2) == 0 {
			message.SenderIdentityID = pointer(thread.senderID)
		}
		if r.Intn(3) == 0 {
			message.ReplyToRemoteID = pointer(fmt.Sprintf("reply-%d", r.Intn(3)))
		}
		var attachments []MessageAttachment
		for ordinal := range r.Intn(3) {
			attachments = append(attachments, MessageAttachment{
				Ordinal:  int64(ordinal),
				RemoteID: fmt.Sprintf("media-%d-%d", index, ordinal),
				MIME:     []string{"image/png", ""}[r.Intn(2)],
			})
		}
		calls[index] = hinsCall{Message: message, Attachments: attachments}
	}
	return reflect.ValueOf(hinsSequence{Calls: calls})
}

// Property (first writer wins): for any sequence, a call inserts exactly when
// neither its primary key nor its natural key is already held; each natural
// key is inserted at most once, and exactly once if it ends up stored; the
// final rows (every column) and attachments are exactly those of the inserting
// calls, so no later call ever changes a stored row.
func TestInsertHistoricalMessageFirstWriterWinsProperty(t *testing.T) {
	clock := newMessageTestClock(messageTestTimeMS)
	store, repository := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	seedMessageConversation(t, store, "conversation-a2", "account-a")
	seedMessageAccount(t, store, "account-b", "whatsapp")
	seedMessageIdentity(t, store, "identity-b", "account-b")
	seedMessageConversation(t, store, "conversation-b", "account-b")

	type naturalKey struct{ account, conversation, remote string }
	var conflictsSeen, strayCollisionsSeen int
	property := func(sequence hinsSequence) bool {
		if _, err := store.db.Exec(`DELETE FROM message_attachments`); err != nil {
			t.Errorf("reset attachments: %v", err)
			return false
		}
		if _, err := store.db.Exec(`DELETE FROM messages`); err != nil {
			t.Errorf("reset messages: %v", err)
			return false
		}

		heldIDs := map[string]bool{}
		heldKeys := map[naturalKey]bool{}
		insertsPerKey := map[naturalKey]int{}
		firstCallForKey := map[naturalKey]int{}
		var wantMessages []Message
		var wantAttachments []string
		for index, call := range sequence.Calls {
			nowMS := messageTestTimeMS + int64(index)
			clock.Set(nowMS)
			key := naturalKey{call.Message.AccountID, call.Message.ConversationID, call.Message.RemoteMessageID}
			if _, seen := firstCallForKey[key]; !seen {
				firstCallForKey[key] = index
			}
			wantInserted := !heldIDs[call.Message.MessageID] && !heldKeys[key]

			inserted, err := repository.InsertHistoricalMessage(context.Background(), MessageProjection{
				Message:     call.Message,
				Attachments: call.Attachments,
			})
			if err != nil {
				t.Errorf("call %d %+v: %v", index, call.Message, err)
				return false
			}
			if inserted != wantInserted {
				t.Errorf("call %d %+v: inserted = %v, model says %v (id held %v, key held %v)",
					index, call.Message, inserted, wantInserted, heldIDs[call.Message.MessageID], heldKeys[key])
				return false
			}
			// Independent of the model: the first call naming a key with its
			// derived ID always wins (no earlier call can hold that ID or key).
			if firstCallForKey[key] == index && !strings.HasPrefix(call.Message.MessageID, "m-stray-") && !inserted {
				t.Errorf("call %d: first derived-ID call for %+v was not inserted", index, key)
				return false
			}
			if !inserted {
				conflictsSeen++
				if !heldKeys[key] {
					strayCollisionsSeen++
				}
				continue
			}
			heldIDs[call.Message.MessageID] = true
			heldKeys[key] = true
			insertsPerKey[key]++
			row := call.Message
			row.CreatedAtMS = nowMS
			row.UpdatedAtMS = nowMS
			wantMessages = append(wantMessages, row)
			for _, attachment := range call.Attachments {
				mime := attachment.MIME
				if strings.TrimSpace(mime) == "" {
					mime = "application/octet-stream"
				}
				wantAttachments = append(wantAttachments, fmt.Sprintf(
					"%s|%d|%s|%s|pending|%d|%d",
					call.Message.MessageID, attachment.Ordinal, attachment.RemoteID, mime, nowMS, nowMS,
				))
			}
		}

		for key, count := range insertsPerKey {
			if count != 1 {
				t.Errorf("natural key %+v inserted %d times, want exactly once", key, count)
				return false
			}
		}
		for key := range firstCallForKey {
			stored, err := repository.GetMessageByRemote(context.Background(), key.account, key.conversation, key.remote)
			if heldKeys[key] != (err == nil) {
				t.Errorf("natural key %+v stored=%v but model held=%v (err %v, row %+v)", key, err == nil, heldKeys[key], err, stored)
				return false
			}
		}

		gotMessages := hinsAllMessages(t, store)
		sort.Slice(wantMessages, func(i, j int) bool { return wantMessages[i].MessageID < wantMessages[j].MessageID })
		if len(gotMessages) != len(wantMessages) || (len(wantMessages) > 0 && !reflect.DeepEqual(gotMessages, wantMessages)) {
			t.Errorf("final messages differ from first-writer model:\n got %+v\nwant %+v", gotMessages, wantMessages)
			return false
		}
		gotAttachments := hinsAttachmentSummary(t, store)
		sort.Strings(wantAttachments)
		if len(gotAttachments) != len(wantAttachments) || (len(wantAttachments) > 0 && !reflect.DeepEqual(gotAttachments, wantAttachments)) {
			t.Errorf("final attachments differ from first-writer model:\n got %q\nwant %q", gotAttachments, wantAttachments)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 120, Rand: rand.New(rand.NewSource(20261008))}); err != nil {
		t.Fatal(err)
	}
	if conflictsSeen == 0 || strayCollisionsSeen == 0 {
		t.Fatalf("generator produced %d conflicts and %d primary-key-only collisions; want both > 0", conflictsSeen, strayCollisionsSeen)
	}
	t.Logf("conflicting calls: %d (primary-key-only: %d)", conflictsSeen, strayCollisionsSeen)
}

// hinsDump returns every row of query as one formatted string per row, with
// column names, so before/after comparisons cover every column.
func hinsDump(t *testing.T, store *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := store.db.Query(query, args...)
	if err != nil {
		t.Fatalf("dump %q: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump columns: %v", err)
	}
	var dumped []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("dump scan: %v", err)
		}
		fields := make([]string, len(columns))
		for index, column := range columns {
			fields[index] = fmt.Sprintf("%s=%#v", column, values[index])
		}
		dumped = append(dumped, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump rows: %v", err)
	}
	return dumped
}

func hinsAllMessages(t *testing.T, store *Store) []Message {
	t.Helper()
	rows, err := store.db.Query(`SELECT ` + messageColumns + ` FROM messages ORDER BY message_id`)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	messages, err := collectRows(rows, scanMessage)
	if err != nil {
		t.Fatalf("scan messages: %v", err)
	}
	return messages
}

func hinsAttachmentSummary(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT message_id, ordinal, remote_id, mime, state, created_at_ms, updated_at_ms
		FROM message_attachments
		ORDER BY message_id, ordinal
	`)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	defer rows.Close()
	var summary []string
	for rows.Next() {
		var (
			messageID, remoteID, mime, state  string
			ordinal, createdAtMS, updatedAtMS int64
		)
		if err := rows.Scan(&messageID, &ordinal, &remoteID, &mime, &state, &createdAtMS, &updatedAtMS); err != nil {
			t.Fatalf("scan attachment: %v", err)
		}
		summary = append(summary, fmt.Sprintf("%s|%d|%s|%s|%s|%d|%d", messageID, ordinal, remoteID, mime, state, createdAtMS, updatedAtMS))
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("list attachments rows: %v", err)
	}
	sort.Strings(summary)
	return summary
}
