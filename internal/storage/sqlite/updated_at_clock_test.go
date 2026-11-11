package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// clampFixture is a store seeded with the message projection graph whose
// message, attachment and reaction repositories share one settable clock.
type clampFixture struct {
	t           *testing.T
	ctx         context.Context
	clock       *messageTestClock
	store       *Store
	messages    *MessageRepository
	attachments *MessageAttachmentRepository
	reactions   *ReactionRepository
	// writeTime is when the write under test reads its clock.
	writeTime clockSkew
}

// repositoryWriteMS is the time a write under test stamps on an account,
// device, identity or conversation, which the fixture creates at
// repositoryTestTimeMS.
func (f *clampFixture) repositoryWriteMS() int64 {
	return repositoryTestTimeMS + f.writeTime.offset.Milliseconds()
}

func newClampFixture(t *testing.T) *clampFixture {
	t.Helper()
	clock := newMessageTestClock(messageTestTimeMS)
	store, messages := openMessageTestRepository(t, clock.Now)
	seedMessageProjectionGraph(t, store)
	attachments, err := NewMessageAttachmentRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	reactions, err := NewReactionRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	return &clampFixture{
		t:           t,
		ctx:         context.Background(),
		clock:       clock,
		store:       store,
		messages:    messages,
		attachments: attachments,
		reactions:   reactions,
	}
}

// project appends a frame and projects message from it at the current clock.
func (f *clampFixture) project(inboxID string, message Message, attachments ...MessageAttachment) {
	f.t.Helper()
	inbox := messageTestInbox(inboxID, message.AccountID, inboxID, []byte(inboxID))
	if _, err := f.messages.AppendInbox(f.ctx, inbox); err != nil {
		f.t.Fatalf("AppendInbox(%q): %v", inboxID, err)
	}
	if err := f.messages.ProjectMessage(f.ctx, MessageProjection{
		InboxID:     inbox.InboxID,
		Message:     message,
		Attachments: attachments,
	}); err != nil {
		f.t.Fatalf("ProjectMessage(%q): %v", inboxID, err)
	}
}

// appendInbox appends a frame at the current clock and returns its ID.
func (f *clampFixture) appendInbox(inboxID string) string {
	f.t.Helper()
	id, err := f.messages.AppendInbox(f.ctx, messageTestInbox(inboxID, "account-a", inboxID, []byte(inboxID)))
	if err != nil {
		f.t.Fatalf("AppendInbox(%q): %v", inboxID, err)
	}
	return id
}

func (f *clampFixture) projectMessageA(attachments ...MessageAttachment) Message {
	f.t.Helper()
	message := messageTestMessage("message-a", "conversation-a", "account-a", "remote-message-a", pointer("identity-a"))
	f.project("inbox-message-a", message, attachments...)
	return message
}

func (f *clampFixture) pendingAttachment() Message {
	f.t.Helper()
	return f.projectMessageA(MessageAttachment{Ordinal: 0, MIME: "image/png"})
}

func (f *clampFixture) applyReaction(emoji string, occurredAtMS int64) error {
	_, err := f.reactions.ApplyReaction(f.ctx, ReactionApply{
		AccountID:         "account-a",
		ConversationID:    "conversation-a",
		MessageID:         "message-a",
		ReactorKey:        "identity-a",
		ReactorIdentityID: pointer("identity-a"),
		ReactorLabel:      "identity-a",
		Emoji:             emoji,
		Action:            bridge.ReactionAdd,
		OccurredAtMS:      occurredAtMS,
	})
	return err
}

func (f *clampFixture) snapshot(sourceSeqMS int64, emojis ...string) error {
	entries := make([]ReactionSnapshotEntry, 0, len(emojis))
	for _, emoji := range emojis {
		entries = append(entries, ReactionSnapshotEntry{
			ReactorKey:        "identity-a",
			ReactorIdentityID: pointer("identity-a"),
			ReactorLabel:      "identity-a",
			Emoji:             emoji,
		})
	}
	_, err := f.reactions.ReplaceEmbeddedReactions(
		f.ctx, "message-a", "account-a", "conversation-a", entries, sourceSeqMS,
	)
	return err
}

// clampTarget names the one row a write under test changes.
type clampTarget struct {
	table string
	where string
	args  []any
}

func (f *clampFixture) updatedAt(target clampTarget) (int64, bool) {
	f.t.Helper()
	var count int
	var updatedAtMS int64
	if err := f.store.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(updated_at_ms), 0) FROM `+target.table+` WHERE `+target.where,
		target.args...,
	).Scan(&count, &updatedAtMS); err != nil {
		f.t.Fatalf("read %s updated_at_ms: %v", target.table, err)
	}
	if count > 1 {
		f.t.Fatalf("%s WHERE %s matched %d rows, want at most 1", target.table, target.where, count)
	}
	return updatedAtMS, count == 1
}

// rowExceptUpdatedAt formats every column of the target row but
// updated_at_ms, so a test can tell whether a write changed the row.
func (f *clampFixture) rowExceptUpdatedAt(target clampTarget) string {
	f.t.Helper()
	rows, err := f.store.db.Query(`SELECT * FROM `+target.table+` WHERE `+target.where, target.args...)
	if err != nil {
		f.t.Fatalf("read %s row: %v", target.table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		f.t.Fatalf("read %s columns: %v", target.table, err)
	}
	var formatted strings.Builder
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			f.t.Fatalf("scan %s row: %v", target.table, err)
		}
		for index, column := range columns {
			if column != "updated_at_ms" {
				fmt.Fprintf(&formatted, "%s=%v;", column, values[index])
			}
		}
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read %s row: %v", target.table, err)
	}
	return formatted.String()
}

// TestStoreWritesNeverStampUpdatedAtBelowCreatedAt covers every write outside
// the outbox that sets updated_at_ms on a table whose CHECK requires
// updated_at_ms >= created_at_ms (migrations 0002, 0004, 0008 and 0010). Each
// write runs with a time older than the row's current updated_at_ms, once for
// each of clockSkews: the clock read of a writer that raced the insert or a
// later update, or a wall clock stepped back. It must succeed, change its row,
// and move no row's updated_at_ms backward.
func TestStoreWritesNeverStampUpdatedAtBelowCreatedAt(t *testing.T) {
	// The mint case carries a creation time this far after its own clock.
	const skewMS = 1_000
	messageA := clampTarget{"messages", "message_id = ?", []any{"message-a"}}
	attachmentA := clampTarget{"message_attachments", "message_id = ? AND ordinal = 0", []any{"message-a"}}
	reactionA := clampTarget{"reactions", "message_id = ? AND reactor_key = ?", []any{"message-a", "identity-a"}}
	downloadedHash := strings.Repeat("ab", 32)

	for _, test := range []struct {
		name    string
		target  clampTarget
		arrange func(*clampFixture)
		act     func(*clampFixture) error
	}{
		{
			name:   "UpsertAccount",
			target: clampTarget{"accounts", "account_id = ?", []any{"account-a"}},
			act: func(f *clampFixture) error {
				account := repositoryTestAccount("account-a")
				account.DisplayName = "renamed by a writer that read its clock early"
				account.CreatedAtMS, account.UpdatedAtMS = f.repositoryWriteMS(), f.repositoryWriteMS()
				return f.store.UpsertAccount(account)
			},
		},
		{
			name:   "UpsertDevice",
			target: clampTarget{"devices", "device_id = ?", []any{"device-a"}},
			arrange: func(f *clampFixture) {
				mustRepositoryWrite(f.t, "UpsertDevice", f.store.UpsertDevice(Device{
					DeviceID:    "device-a",
					AccountID:   "account-a",
					Kind:        DeviceKindLocalInstallation,
					State:       DeviceStateActive,
					CreatedAtMS: repositoryTestTimeMS,
					UpdatedAtMS: repositoryTestTimeMS,
				}))
			},
			act: func(f *clampFixture) error {
				return f.store.UpsertDevice(Device{
					DeviceID:    "device-a",
					AccountID:   "account-a",
					Kind:        DeviceKindLocalInstallation,
					DisplayName: "renamed",
					State:       DeviceStateActive,
					IsCurrent:   true,
					CreatedAtMS: f.repositoryWriteMS(),
					UpdatedAtMS: f.repositoryWriteMS(),
				})
			},
		},
		{
			name:   "UpsertIdentity",
			target: clampTarget{"identities", "identity_id = ?", []any{"identity-a"}},
			act: func(f *clampFixture) error {
				identity := repositoryTestIdentity("identity-a", "account-a", "identity-a")
				identity.DisplayName = "renamed"
				identity.CreatedAtMS, identity.UpdatedAtMS = f.repositoryWriteMS(), f.repositoryWriteMS()
				return f.store.UpsertIdentity(identity)
			},
		},
		{
			name:   "UpsertConversation",
			target: clampTarget{"conversations", "conversation_id = ?", []any{"conversation-a"}},
			act: func(f *clampFixture) error {
				return f.store.UpsertConversation(Conversation{
					ConversationID:       "conversation-a",
					AccountID:            "account-a",
					RemoteConversationID: "remote-conversation-a",
					Kind:                 ConversationKindDirect,
					Title:                "renamed",
					NotificationMode:     NotificationModeAll,
					MetadataJSON:         `{}`,
					CreatedAtMS:          f.repositoryWriteMS(),
					UpdatedAtMS:          f.repositoryWriteMS(),
				})
			},
		},
		// The write-back cases read the row and keep its created_at_ms, as the
		// ingest worker does, so the row the upsert would insert has
		// updated_at_ms below created_at_ms (issue #169).
		{
			name:   "UpsertAccount writing back a stored row",
			target: clampTarget{"accounts", "account_id = ?", []any{"account-a"}},
			act: func(f *clampFixture) error {
				account, err := f.store.GetAccount("account-a")
				if err != nil {
					return err
				}
				account.DisplayName = "renamed"
				account.UpdatedAtMS = f.repositoryWriteMS()
				return f.store.UpsertAccount(account)
			},
		},
		{
			name:   "UpsertDevice writing back a stored row",
			target: clampTarget{"devices", "device_id = ?", []any{"device-a"}},
			arrange: func(f *clampFixture) {
				mustRepositoryWrite(f.t, "UpsertDevice", f.store.UpsertDevice(Device{
					DeviceID:    "device-a",
					AccountID:   "account-a",
					Kind:        DeviceKindLocalInstallation,
					State:       DeviceStateActive,
					CreatedAtMS: repositoryTestTimeMS,
					UpdatedAtMS: repositoryTestTimeMS,
				}))
			},
			act: func(f *clampFixture) error {
				device, err := f.store.GetDevice("device-a")
				if err != nil {
					return err
				}
				device.DisplayName = "renamed"
				device.UpdatedAtMS = f.repositoryWriteMS()
				return f.store.UpsertDevice(device)
			},
		},
		{
			name:   "UpsertIdentity writing back a stored row",
			target: clampTarget{"identities", "identity_id = ?", []any{"identity-a"}},
			act: func(f *clampFixture) error {
				identity, err := f.store.GetIdentity("identity-a")
				if err != nil {
					return err
				}
				identity.DisplayName = "renamed"
				identity.UpdatedAtMS = f.repositoryWriteMS()
				return f.store.UpsertIdentity(identity)
			},
		},
		{
			name:   "UpsertConversation writing back a stored row",
			target: clampTarget{"conversations", "conversation_id = ?", []any{"conversation-a"}},
			act: func(f *clampFixture) error {
				conversation, err := f.store.GetConversation("conversation-a")
				if err != nil {
					return err
				}
				conversation.Title = "renamed"
				conversation.UpdatedAtMS = f.repositoryWriteMS()
				return f.store.UpsertConversation(conversation)
			},
		},
		{
			name:    "ProjectMessage over a projected message",
			target:  messageA,
			arrange: func(f *clampFixture) { f.projectMessageA() },
			act: func(f *clampFixture) error {
				message := messageTestMessage("message-a", "conversation-a", "account-a", "remote-message-a", pointer("identity-a"))
				message.Body = "a later frame changed the body"
				inboxID := f.appendInbox("inbox-message-a-again")
				return f.messages.ProjectMessage(f.ctx, MessageProjection{InboxID: inboxID, Message: message})
			},
		},
		{
			name:    "ApplyMessageMutation edit",
			target:  messageA,
			arrange: func(f *clampFixture) { f.projectMessageA() },
			act: func(f *clampFixture) error {
				_, err := f.messages.ApplyMessageMutation(
					f.ctx, "account-a", "conversation-a", "remote-message-a",
					"edit", "edited body", f.appendInbox("inbox-edit"),
				)
				return err
			},
		},
		{
			name:    "ApplyMessageMutation delete",
			target:  messageA,
			arrange: func(f *clampFixture) { f.projectMessageA() },
			act: func(f *clampFixture) error {
				_, err := f.messages.ApplyMessageMutation(
					f.ctx, "account-a", "conversation-a", "remote-message-a",
					"delete", "", f.appendInbox("inbox-delete"),
				)
				return err
			},
		},
		{
			name:    "RecordInboundAttachment naming the media later",
			target:  attachmentA,
			arrange: func(f *clampFixture) { f.pendingAttachment() },
			act: func(f *clampFixture) error {
				message := messageTestMessage("message-a", "conversation-a", "account-a", "remote-message-a", pointer("identity-a"))
				inboxID := f.appendInbox("inbox-media-named")
				return f.messages.ProjectMessage(f.ctx, MessageProjection{
					InboxID: inboxID,
					Message: message,
					Attachments: []MessageAttachment{{
						Ordinal:  0,
						RemoteID: "remote-media-a",
						MIME:     "image/png",
					}},
				})
			},
		},
		{
			name:    "MarkDownloaded",
			target:  attachmentA,
			arrange: func(f *clampFixture) { f.pendingAttachment() },
			act: func(f *clampFixture) error {
				return f.attachments.MarkDownloaded(f.ctx, "message-a", 0, downloadedHash, 10, "image/png")
			},
		},
		{
			name:   "ReplaceDownloaded",
			target: attachmentA,
			arrange: func(f *clampFixture) {
				f.pendingAttachment()
				if err := f.attachments.MarkDownloaded(f.ctx, "message-a", 0, downloadedHash, 10, "image/png"); err != nil {
					f.t.Fatalf("MarkDownloaded(): %v", err)
				}
			},
			act: func(f *clampFixture) error {
				replaced, err := f.attachments.ReplaceDownloaded(
					f.ctx, "message-a", 0, downloadedHash, strings.Repeat("cd", 32), 12, "image/png",
				)
				if err == nil && !replaced {
					f.t.Fatal("ReplaceDownloaded() replaced = false, want true")
				}
				return err
			},
		},
		{
			name:    "MarkFailed",
			target:  attachmentA,
			arrange: func(f *clampFixture) { f.pendingAttachment() },
			act: func(f *clampFixture) error {
				return f.attachments.MarkFailed(f.ctx, "message-a", 0, "transport has no copy")
			},
		},
		{
			name:   "ApplyReaction over an older reaction",
			target: reactionA,
			arrange: func(f *clampFixture) {
				f.projectMessageA()
				if err := f.applyReaction("👍", messageTestTimeMS-500); err != nil {
					f.t.Fatalf("ApplyReaction(👍): %v", err)
				}
			},
			act: func(f *clampFixture) error { return f.applyReaction("🎉", messageTestTimeMS-400) },
		},
		{
			name:   "ReplaceEmbeddedReactions changing an emoji",
			target: reactionA,
			arrange: func(f *clampFixture) {
				f.projectMessageA()
				if err := f.snapshot(10, "👍"); err != nil {
					f.t.Fatalf("ReplaceEmbeddedReactions(👍): %v", err)
				}
			},
			act: func(f *clampFixture) error { return f.snapshot(11, "🎉") },
		},
		{
			name:   "ReplaceEmbeddedReactions dropping a reactor",
			target: reactionA,
			arrange: func(f *clampFixture) {
				f.projectMessageA()
				if err := f.snapshot(10, "👍"); err != nil {
					f.t.Fatalf("ReplaceEmbeddedReactions(👍): %v", err)
				}
			},
			act: func(f *clampFixture) error { return f.snapshot(11) },
		},
		{
			name:   "ApplyRepairPlan mint carrying a later creation time",
			target: clampTarget{"conversations", "conversation_id = ?", []any{"conversation-minted"}},
			act: func(f *clampFixture) error {
				nowMS := f.clock.Now().UnixMilli()
				return f.store.ApplyRepairPlan(f.ctx, "account-a", []RepairStep{{
					Op:                   "mint",
					ConversationID:       "conversation-minted",
					RemoteConversationID: "remote-conversation-minted",
					Kind:                 string(ConversationKindDirect),
					CreatedAtMS:          nowMS + skewMS,
				}}, nowMS)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, writeTime := range clockSkews {
				t.Run(writeTime.name, func(t *testing.T) {
					f := newClampFixture(t)
					f.writeTime = writeTime
					if test.arrange != nil {
						test.arrange(f)
					}
					rows := writeTime.prepare(t, f.store)
					before, existed := f.updatedAt(test.target)
					wasRow := f.rowExceptUpdatedAt(test.target)
					f.clock.Set(messageTestTimeMS + writeTime.offset.Milliseconds())

					if err := test.act(f); err != nil {
						t.Fatalf("write on a %s: %v", writeTime.name, err)
					}
					after, exists := f.updatedAt(test.target)
					if !exists {
						t.Fatalf("%s row is missing after the write", test.target.table)
					}
					if existed && after < before {
						t.Fatalf("%s updated_at_ms fell from %d to %d", test.target.table, before, after)
					}
					if existed && f.rowExceptUpdatedAt(test.target) == wasRow {
						t.Fatalf("the write left the %s row as it was, so this case checks nothing", test.target.table)
					}
					assertNoUpdatedAtFell(t, f.store, rows)
					assertNoRowUpdatedBeforeCreated(t, f.store)
				})
			}
		})
	}
}
