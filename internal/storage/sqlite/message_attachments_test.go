package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const messageAttachmentTestTimeMS int64 = 1_950_000_000_000

func TestMessageAttachmentsMigrationAppliesToBlankAndExistingV7AndReopens(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		first, err := Open(path)
		if err != nil {
			t.Fatalf("first Open(): %v", err)
		}
		assertMessageAttachmentsMigration(t, first)
		firstLedger := readLedgerRows(t, first.db)
		if err := first.Close(); err != nil {
			t.Fatalf("first Close(): %v", err)
		}

		second, err := Open(path)
		if err != nil {
			t.Fatalf("second Open(): %v", err)
		}
		t.Cleanup(func() {
			if err := second.Close(); err != nil {
				t.Errorf("second Close(): %v", err)
			}
		})
		assertMessageAttachmentsMigration(t, second)
		if secondLedger := readLedgerRows(t, second.db); !slices.Equal(secondLedger, firstLedger) {
			t.Fatalf("migration ledger changed on reopen:\nfirst:  %+v\nsecond: %+v", firstLedger, secondLedger)
		}
	})

	t.Run("existing v7", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store.sqlite3")
		db, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatalf("sql.Open(): %v", err)
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			t.Fatalf("Ping(): %v", err)
		}
		if err := enableWAL(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("enableWAL(): %v", err)
		}
		if err := verifyConnectionPragmas(context.Background(), db); err != nil {
			_ = db.Close()
			t.Fatalf("verifyConnectionPragmas(): %v", err)
		}
		if err := runMigrations(context.Background(), db, embeddedMigrations[:7]); err != nil {
			_ = db.Close()
			t.Fatalf("runMigrations(v7): %v", err)
		}
		before := readLedgerRows(t, db)
		if len(before) != 7 {
			_ = db.Close()
			t.Fatalf("v7 ledger rows = %d, want 7", len(before))
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close v7 database: %v", err)
		}

		store, err := Open(path)
		if err != nil {
			t.Fatalf("Open(v7): %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close(): %v", err)
			}
		})
		after := readLedgerRows(t, store.db)
		if len(after) != 11 {
			t.Fatalf("migrated ledger rows = %d, want 11", len(after))
		}
		if !slices.Equal(after[:7], before) {
			t.Fatalf("migrations 0001-0007 changed:\nbefore: %+v\nafter:  %+v", before, after[:7])
		}
		assertMessageAttachmentsMigration(t, store)
	})
}

func TestMessageAttachmentsTableEnforcesStateAndHashChecks(t *testing.T) {
	store, _ := openMessageAttachmentTestRepository(t, func() time.Time {
		return time.UnixMilli(messageAttachmentTestTimeMS)
	})
	seedMessageAttachmentParent(t, store, "message-checks")
	validHash := strings.Repeat("ab", 32)

	tests := []struct {
		name     string
		ordinal  int64
		state    string
		blobHash any
	}{
		{name: "downloaded without hash", ordinal: 0, state: "downloaded", blobHash: nil},
		{name: "pending with hash", ordinal: 1, state: "pending", blobHash: validHash},
		{name: "bad hex", ordinal: 2, state: "downloaded", blobHash: strings.Repeat("xz", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expectExecError(t, store.db, test.name, `
				INSERT INTO message_attachments (
					message_id, ordinal, state, blob_hash, created_at_ms, updated_at_ms
				) VALUES (?, ?, ?, ?, ?, ?)
			`,
				"message-checks",
				test.ordinal,
				test.state,
				test.blobHash,
				messageAttachmentTestTimeMS,
				messageAttachmentTestTimeMS,
			)
		})
	}
	assertRowCount(t, store.db, "message_attachments", 0)
}

func TestMessageAttachmentRepositoryRecordsReadsAndMarksFailure(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	seedMessageAttachmentParent(t, store, "message-round-trip")
	size := int64(17)
	ignoredHash := strings.Repeat("ab", 32)
	want := MessageAttachment{
		MessageID: "message-round-trip",
		Ordinal:   2,
		RemoteID:  "remote-attachment",
		RemoteRef: []byte("opaque transport reference"),
		Filename:  "photo.png",
		MIME:      "image/png",
		SizeBytes: &size,
		State:     "downloaded",
		BlobHash:  &ignoredHash,
	}
	recordMessageAttachment(t, store, repository, want)

	got, err := repository.GetForDownload(context.Background(), want.MessageID, want.Ordinal)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if got.MessageID != want.MessageID || got.Ordinal != want.Ordinal || got.RemoteID != want.RemoteID ||
		!bytes.Equal(got.RemoteRef, want.RemoteRef) || got.Filename != want.Filename || got.MIME != want.MIME ||
		got.SizeBytes == nil || *got.SizeBytes != size || got.State != "pending" || got.BlobHash != nil ||
		got.AccountID != "account-a" {
		t.Fatalf("GetForDownload() = %+v, want pending account-scoped row", got)
	}

	clock.Set(messageAttachmentTestTimeMS + 100)
	if err := repository.MarkFailed(context.Background(), want.MessageID, want.Ordinal, "temporary transport failure"); err != nil {
		t.Fatalf("MarkFailed(): %v", err)
	}
	var (
		state       string
		lastError   sql.NullString
		updatedAtMS int64
	)
	if err := store.db.QueryRow(`
		SELECT state, last_error, updated_at_ms
		FROM message_attachments
		WHERE message_id = ? AND ordinal = ?
	`, want.MessageID, want.Ordinal).Scan(&state, &lastError, &updatedAtMS); err != nil {
		t.Fatalf("read failed attachment: %v", err)
	}
	if state != "pending" || !lastError.Valid || lastError.String != "temporary transport failure" ||
		updatedAtMS != messageAttachmentTestTimeMS+100 {
		t.Fatalf("failed attachment = state %q error %+v updated %d", state, lastError, updatedAtMS)
	}

	if _, err := repository.GetForDownload(context.Background(), "missing", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetForDownload(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestMessageAttachmentRepositoryMarkDownloadedIsIdempotent(t *testing.T) {
	clock := newMessageTestClock(messageAttachmentTestTimeMS)
	store, repository := openMessageAttachmentTestRepository(t, clock.Now)
	seedMessageAttachmentParent(t, store, "message-download")
	recordMessageAttachment(t, store, repository, MessageAttachment{
		MessageID: "message-download",
		Ordinal:   0,
		MIME:      "application/octet-stream",
	})

	firstHash := strings.Repeat("1a", 32)
	clock.Set(messageAttachmentTestTimeMS + 100)
	if err := repository.MarkDownloaded(
		context.Background(), "message-download", 0, firstHash, 42, "image/png",
	); err != nil {
		t.Fatalf("MarkDownloaded(first): %v", err)
	}
	secondHash := strings.Repeat("2b", 32)
	clock.Set(messageAttachmentTestTimeMS + 200)
	if err := repository.MarkDownloaded(
		context.Background(), "message-download", 0, secondHash, 99, "text/plain",
	); err != nil {
		t.Fatalf("MarkDownloaded(second): %v", err)
	}

	got, err := repository.GetForDownload(context.Background(), "message-download", 0)
	if err != nil {
		t.Fatalf("GetForDownload(): %v", err)
	}
	if got.State != "downloaded" || got.BlobHash == nil || *got.BlobHash != firstHash ||
		got.SizeBytes == nil || *got.SizeBytes != 42 || got.MIME != "image/png" {
		t.Fatalf("downloaded attachment = %+v, want first update preserved", got)
	}
	var updatedAtMS int64
	if err := store.db.QueryRow(`
		SELECT updated_at_ms FROM message_attachments WHERE message_id = ? AND ordinal = ?
	`, "message-download", 0).Scan(&updatedAtMS); err != nil {
		t.Fatalf("read downloaded timestamp: %v", err)
	}
	if updatedAtMS != messageAttachmentTestTimeMS+100 {
		t.Fatalf("updated_at_ms = %d, want first update %d", updatedAtMS, messageAttachmentTestTimeMS+100)
	}
	if err := repository.MarkDownloaded(
		context.Background(), "message-download", 0, strings.Repeat("AB", 32), 1, "image/png",
	); err == nil {
		t.Fatal("MarkDownloaded(invalid hash) succeeded")
	}
}

func TestMessageAttachmentRowsCascadeWithMessageDeletion(t *testing.T) {
	store, repository := openMessageAttachmentTestRepository(t, func() time.Time {
		return time.UnixMilli(messageAttachmentTestTimeMS)
	})
	seedMessageAttachmentParent(t, store, "message-cascade")
	recordMessageAttachment(t, store, repository, MessageAttachment{
		MessageID: "message-cascade",
		Ordinal:   0,
		MIME:      "image/png",
	})
	if _, err := store.db.Exec(`DELETE FROM messages WHERE message_id = ?`, "message-cascade"); err != nil {
		t.Fatalf("delete parent message: %v", err)
	}
	assertRowCount(t, store.db, "message_attachments", 0)
}

func assertMessageAttachmentsMigration(t *testing.T, store *Store) {
	t.Helper()
	if len(embeddedMigrations) != 11 {
		t.Fatalf("embedded migrations = %d, want 11", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", 11)
	ledger := readLedgerRow(t, store.db, 8)
	if ledger.name != "message_attachments" {
		t.Fatalf("migration 0008 name = %q, want message_attachments", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[7].checksumSHA256 {
		t.Fatalf(
			"migration 0008 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[7].checksumSHA256,
		)
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict
		FROM pragma_table_list
		WHERE schema = 'main' AND name = 'message_attachments'
	`).Scan(&strict); err != nil {
		t.Fatalf("read message_attachments STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("message_attachments strict = %d, want 1", strict)
	}
	var indexExists bool
	if err := store.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM sqlite_schema
			WHERE type = 'index' AND name = 'message_attachments_blob_hash_idx'
		)
	`).Scan(&indexExists); err != nil {
		t.Fatalf("inspect message_attachments blob index: %v", err)
	}
	if !indexExists {
		t.Fatal("message_attachments_blob_hash_idx is missing")
	}
}

func openMessageAttachmentTestRepository(
	t *testing.T,
	now func() time.Time,
) (*Store, *MessageAttachmentRepository) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	repository, err := NewMessageAttachmentRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	return store, repository
}

func seedMessageAttachmentParent(t *testing.T, store *Store, messageID string) {
	t.Helper()
	seedMessageAccount(t, store, "account-a", "test")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	mustExec(t, store.db, `
		INSERT INTO messages (
			message_id, conversation_id, account_id, remote_message_id,
			sender_identity_id, direction, body, reply_to_remote_id, state,
			occurred_at_ms, created_at_ms, updated_at_ms
		) VALUES (?, 'conversation-a', 'account-a', ?, NULL, 'incoming', '', NULL,
		          'active', ?, ?, ?)
	`,
		messageID,
		"remote-"+messageID,
		messageAttachmentTestTimeMS-1_000,
		messageAttachmentTestTimeMS,
		messageAttachmentTestTimeMS,
	)
}

func recordMessageAttachment(
	t *testing.T,
	store *Store,
	repository *MessageAttachmentRepository,
	attachment MessageAttachment,
) {
	t.Helper()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin attachment transaction: %v", err)
	}
	defer tx.Rollback()
	if err := repository.RecordInboundAttachment(context.Background(), tx, attachment); err != nil {
		t.Fatalf("RecordInboundAttachment(): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit attachment transaction: %v", err)
	}
}
