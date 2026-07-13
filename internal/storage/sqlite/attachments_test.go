package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const attachmentTestTimeMS int64 = 1_700_000_123_456

func TestAttachmentsMigrationAndRepositoryRoundTrip(t *testing.T) {
	store, repository := openAttachmentTestRepository(t)

	var tableExists bool
	if err := store.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM sqlite_schema
			WHERE type = 'table' AND name = 'attachments'
		)
	`).Scan(&tableExists); err != nil {
		t.Fatalf("inspect attachments table: %v", err)
	}
	if !tableExists {
		t.Fatal("attachments table does not exist")
	}
	var strict int
	if err := store.db.QueryRow(`
		SELECT strict
		FROM pragma_table_list
		WHERE schema = 'main' AND name = 'attachments'
	`).Scan(&strict); err != nil {
		t.Fatalf("read attachments STRICT flag: %v", err)
	}
	if strict != 1 {
		t.Fatalf("attachments strict = %d, want 1", strict)
	}

	want := Attachment{
		AttachmentID: "attachment-1",
		BlobHash:     strings.Repeat("ab", 32),
		Size:         42,
		MIME:         "image/png",
		Filename:     "photo.png",
		CreatedAtMS:  attachmentTestTimeMS,
	}
	if err := repository.RecordAttachment(context.Background(), want); err != nil {
		t.Fatalf("RecordAttachment(): %v", err)
	}
	got, err := repository.GetAttachment(context.Background(), want.AttachmentID)
	if err != nil {
		t.Fatalf("GetAttachment(): %v", err)
	}
	if got != want {
		t.Fatalf("GetAttachment() = %+v, want %+v", got, want)
	}
}

func TestAttachmentRepositoryListsUnreferencedCandidates(t *testing.T) {
	_, repository := openAttachmentTestRepository(t)
	referenced := strings.Repeat("1a", 32)
	firstUnreferenced := strings.Repeat("2b", 32)
	secondUnreferenced := strings.Repeat("3c", 32)
	if err := repository.RecordAttachment(context.Background(), Attachment{
		AttachmentID: "attachment-1",
		BlobHash:     referenced,
		Size:         1,
		MIME:         "application/octet-stream",
	}); err != nil {
		t.Fatalf("RecordAttachment(): %v", err)
	}

	got, err := repository.ListUnreferencedBlobHashes(context.Background(), []string{
		firstUnreferenced,
		referenced,
		secondUnreferenced,
		firstUnreferenced,
	})
	if err != nil {
		t.Fatalf("ListUnreferencedBlobHashes(): %v", err)
	}
	want := []string{firstUnreferenced, secondUnreferenced}
	if !slices.Equal(got, want) {
		t.Fatalf("ListUnreferencedBlobHashes() = %v, want %v", got, want)
	}
}

func TestAttachmentRepositoryTreatsQueuedOutboxBlobAsReferenced(t *testing.T) {
	store, repository := openAttachmentTestRepository(t)
	seedMessageAccount(t, store, "account-a", "test")
	seedMessageIdentity(t, store, "identity-a", "account-a")
	seedMessageConversation(t, store, "conversation-a", "account-a")
	outbox, err := NewOutboxRepository(
		store,
		func() time.Time { return time.UnixMilli(attachmentTestTimeMS) },
	)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}

	queuedHash := strings.Repeat("4d", 32)
	unreferencedHash := strings.Repeat("5e", 32)
	item := outboxTestMediaItem("gc-reference")
	attachment := outboxTestAttachment()
	attachment.BlobHash = queuedHash
	if _, _, err := outbox.EnqueueOutgoingMediaMessage(
		context.Background(),
		item,
		outboxTestOutgoingMessage(item, "caption"),
		attachment,
	); err != nil {
		t.Fatalf("EnqueueOutgoingMediaMessage(): %v", err)
	}

	got, err := repository.ListUnreferencedBlobHashes(
		context.Background(),
		[]string{queuedHash, unreferencedHash},
	)
	if err != nil {
		t.Fatalf("ListUnreferencedBlobHashes(): %v", err)
	}
	if want := []string{unreferencedHash}; !slices.Equal(got, want) {
		t.Fatalf("ListUnreferencedBlobHashes() = %v, want %v", got, want)
	}
}

func TestAttachmentRepositoryMissingRowPreservesSentinel(t *testing.T) {
	_, repository := openAttachmentTestRepository(t)

	_, err := repository.GetAttachment(context.Background(), "missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetAttachment() error = %v, want sql.ErrNoRows", err)
	}
}

func TestAttachmentsTableEnforcesBlobMetadataConstraints(t *testing.T) {
	store, _ := openAttachmentTestRepository(t)
	validHash := strings.Repeat("ab", 32)

	tests := []struct {
		name         string
		attachmentID string
		blobHash     string
		size         int64
		mime         string
		createdAtMS  int64
	}{
		{
			name:         "blank attachment ID",
			attachmentID: " ",
			blobHash:     validHash,
			mime:         "image/png",
			createdAtMS:  attachmentTestTimeMS,
		},
		{
			name:         "uppercase blob hash",
			attachmentID: "attachment-uppercase",
			blobHash:     strings.Repeat("AB", 32),
			mime:         "image/png",
			createdAtMS:  attachmentTestTimeMS,
		},
		{
			name:         "short blob hash",
			attachmentID: "attachment-short",
			blobHash:     "ab",
			mime:         "image/png",
			createdAtMS:  attachmentTestTimeMS,
		},
		{
			name:         "negative size",
			attachmentID: "attachment-size",
			blobHash:     validHash,
			size:         -1,
			mime:         "image/png",
			createdAtMS:  attachmentTestTimeMS,
		},
		{
			name:         "blank MIME",
			attachmentID: "attachment-mime",
			blobHash:     validHash,
			mime:         " ",
			createdAtMS:  attachmentTestTimeMS,
		},
		{
			name:         "nonpositive timestamp",
			attachmentID: "attachment-time",
			blobHash:     validHash,
			mime:         "image/png",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expectExecError(t, store.db, test.name, `
				INSERT INTO attachments (
					attachment_id, blob_hash, size, mime, created_at_ms
				) VALUES (?, ?, ?, ?, ?)
			`,
				test.attachmentID,
				test.blobHash,
				test.size,
				test.mime,
				test.createdAtMS,
			)
		})
	}
	assertRowCount(t, store.db, "attachments", 0)
}

func TestAttachmentRepositoryRejectsInvalidInputBeforeWrite(t *testing.T) {
	store, repository := openAttachmentTestRepository(t)

	tests := []struct {
		name       string
		attachment Attachment
	}{
		{
			name: "empty attachment ID",
			attachment: Attachment{
				BlobHash: strings.Repeat("ab", 32),
				MIME:     "image/png",
			},
		},
		{
			name: "invalid hash",
			attachment: Attachment{
				AttachmentID: "attachment-1",
				BlobHash:     strings.Repeat("AB", 32),
				MIME:         "image/png",
			},
		},
		{
			name: "negative size",
			attachment: Attachment{
				AttachmentID: "attachment-1",
				BlobHash:     strings.Repeat("ab", 32),
				Size:         -1,
				MIME:         "image/png",
			},
		},
		{
			name: "empty MIME",
			attachment: Attachment{
				AttachmentID: "attachment-1",
				BlobHash:     strings.Repeat("ab", 32),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := repository.RecordAttachment(context.Background(), test.attachment); err == nil {
				t.Fatal("RecordAttachment() succeeded, want validation error")
			}
		})
	}
	assertRowCount(t, store.db, "attachments", 0)
}

func openAttachmentTestRepository(t *testing.T) (*Store, *AttachmentRepository) {
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

	if len(embeddedMigrations) != 6 {
		t.Fatalf("embedded migrations = %d, want 6", len(embeddedMigrations))
	}
	assertPragmaInt(t, store.db, "user_version", len(embeddedMigrations))
	ledger := readLedgerRow(t, store.db, 3)
	if ledger.name != "attachments" {
		t.Fatalf("migration 0003 name = %q, want attachments", ledger.name)
	}
	if ledger.checksum != embeddedMigrations[2].checksumSHA256 {
		t.Fatalf(
			"migration 0003 checksum = %q, want %q",
			ledger.checksum,
			embeddedMigrations[2].checksumSHA256,
		)
	}

	repository, err := NewAttachmentRepository(
		store,
		func() time.Time { return time.UnixMilli(attachmentTestTimeMS) },
	)
	if err != nil {
		t.Fatalf("NewAttachmentRepository(): %v", err)
	}
	return store, repository
}
