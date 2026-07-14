package sqlite

import (
	"errors"
	"testing"
	"time"
)

func TestReadCursorUpsertIsMonotone(t *testing.T) {
	store, _ := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	for _, messageID := range []string{"message-first", "message-equal", "message-forward"} {
		seedOutboxTestMessage(t, store, messageID, "account-a", "conversation-a")
	}

	firstID := "message-first"
	first := ReadCursor{
		AccountID:         "account-a",
		DeviceID:          "device-a",
		ConversationID:    "conversation-a",
		LastReadMessageID: &firstID,
		LastReadAtMS:      100,
		UpdatedAtMS:       1_000,
	}
	if err := store.UpsertReadCursor(first); err != nil {
		t.Fatalf("UpsertReadCursor(first): %v", err)
	}
	assertReadCursor(t, store, first)

	backwardID := "message-equal"
	backward := first
	backward.LastReadMessageID = &backwardID
	backward.LastReadAtMS = 99
	backward.UpdatedAtMS = 2_000
	if err := store.UpsertReadCursor(backward); err != nil {
		t.Fatalf("UpsertReadCursor(backward): %v", err)
	}
	assertReadCursor(t, store, first)

	equal := backward
	equal.LastReadAtMS = 100
	equal.UpdatedAtMS = 3_000
	if err := store.UpsertReadCursor(equal); err != nil {
		t.Fatalf("UpsertReadCursor(equal): %v", err)
	}
	assertReadCursor(t, store, equal)

	forwardID := "message-forward"
	forward := equal
	forward.LastReadMessageID = &forwardID
	forward.LastReadAtMS = 101
	forward.UpdatedAtMS = 4_000
	if err := store.UpsertReadCursor(forward); err != nil {
		t.Fatalf("UpsertReadCursor(forward): %v", err)
	}
	assertReadCursor(t, store, forward)
}

func TestReadCursorAllowsNullMessageAtZero(t *testing.T) {
	store, _ := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	seedMessageConversation(t, store, "conversation-a", "account-a")
	seedOutboxTestDevice(t, store, "device-a", "account-a")
	sourceUpdatedAtMS := int64(500)
	cursor := ReadCursor{
		AccountID:         "account-a",
		DeviceID:          "device-a",
		ConversationID:    "conversation-a",
		LastReadAtMS:      0,
		SourceUpdatedAtMS: &sourceUpdatedAtMS,
		UpdatedAtMS:       1_000,
	}
	if err := store.UpsertReadCursor(cursor); err != nil {
		t.Fatalf("UpsertReadCursor(): %v", err)
	}
	got, err := store.GetReadCursor("device-a", "conversation-a")
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if got.LastReadMessageID != nil || got.LastReadAtMS != 0 || got.SourceUpdatedAtMS != nil {
		t.Fatalf("GetReadCursor() = %+v, want null message/source and zero time", got)
	}
}

func TestReadCursorForeignKeysAndChecksMapConstraintErrors(t *testing.T) {
	tests := []struct {
		name   string
		cursor func(*testing.T, *Store) ReadCursor
	}{
		{
			name: "unknown device",
			cursor: func(_ *testing.T, _ *Store) ReadCursor {
				messageID := "message-a"
				return ReadCursor{
					AccountID: "account-a", DeviceID: "missing", ConversationID: "conversation-a",
					LastReadMessageID: &messageID, LastReadAtMS: 100, UpdatedAtMS: 1_000,
				}
			},
		},
		{
			name: "cross-account device",
			cursor: func(t *testing.T, store *Store) ReadCursor {
				seedMessageAccount(t, store, "account-b", "test-b")
				seedOutboxTestDevice(t, store, "device-b", "account-b")
				messageID := "message-a"
				return ReadCursor{
					AccountID: "account-a", DeviceID: "device-b", ConversationID: "conversation-a",
					LastReadMessageID: &messageID, LastReadAtMS: 100, UpdatedAtMS: 1_000,
				}
			},
		},
		{
			name: "cross-account conversation",
			cursor: func(t *testing.T, store *Store) ReadCursor {
				seedMessageAccount(t, store, "account-b", "test-b")
				seedMessageConversation(t, store, "conversation-b", "account-b")
				seedOutboxTestMessage(t, store, "message-b", "account-b", "conversation-b")
				messageID := "message-b"
				return ReadCursor{
					AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-b",
					LastReadMessageID: &messageID, LastReadAtMS: 100, UpdatedAtMS: 1_000,
				}
			},
		},
		{
			name: "cross-conversation message",
			cursor: func(t *testing.T, store *Store) ReadCursor {
				seedMessageConversation(t, store, "conversation-b", "account-a")
				seedOutboxTestMessage(t, store, "message-b", "account-a", "conversation-b")
				messageID := "message-b"
				return ReadCursor{
					AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
					LastReadMessageID: &messageID, LastReadAtMS: 100, UpdatedAtMS: 1_000,
				}
			},
		},
		{
			name: "message requires positive read time",
			cursor: func(_ *testing.T, _ *Store) ReadCursor {
				messageID := "message-a"
				return ReadCursor{
					AccountID: "account-a", DeviceID: "device-a", ConversationID: "conversation-a",
					LastReadMessageID: &messageID, LastReadAtMS: 0, UpdatedAtMS: 1_000,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := openOutboxTestRepository(t, func() time.Time {
				return time.UnixMilli(outboxTestTimeMS)
			})
			seedMessageConversation(t, store, "conversation-a", "account-a")
			seedOutboxTestDevice(t, store, "device-a", "account-a")
			seedOutboxTestMessage(t, store, "message-a", "account-a", "conversation-a")
			if err := store.UpsertReadCursor(test.cursor(t, store)); !errors.Is(err, ErrConstraintViolation) {
				t.Fatalf("UpsertReadCursor() error = %v, want ErrConstraintViolation", err)
			}
			assertRowCount(t, store.db, "read_cursors", 0)
		})
	}
}

func TestGetReadCursorMissingReturnsNotFound(t *testing.T) {
	store, _ := openOutboxTestRepository(t, func() time.Time {
		return time.UnixMilli(outboxTestTimeMS)
	})
	if _, err := store.GetReadCursor("missing-device", "missing-conversation"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetReadCursor(missing) error = %v, want ErrNotFound", err)
	}
}

func assertReadCursor(t *testing.T, store *Store, want ReadCursor) {
	t.Helper()
	got, err := store.GetReadCursor(want.DeviceID, want.ConversationID)
	if err != nil {
		t.Fatalf("GetReadCursor(): %v", err)
	}
	if got.AccountID != want.AccountID || got.DeviceID != want.DeviceID ||
		got.ConversationID != want.ConversationID || got.LastReadAtMS != want.LastReadAtMS ||
		got.UpdatedAtMS != want.UpdatedAtMS || got.SourceUpdatedAtMS != nil ||
		(got.LastReadMessageID == nil) != (want.LastReadMessageID == nil) ||
		(got.LastReadMessageID != nil && *got.LastReadMessageID != *want.LastReadMessageID) {
		t.Fatalf("GetReadCursor() = %+v, want %+v", got, want)
	}
}
