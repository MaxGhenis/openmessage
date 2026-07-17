package migration

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/blob"
)

func TestValidateScheduledBlobsUsesPersistedOutboxReferences(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	database, err := sql.Open("sqlite", filepath.Join(root, "references.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close reference database: %v", err)
		}
	})
	if _, err := database.Exec(`
		CREATE TABLE outbox_attachments (
			outbox_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			blob_hash TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			mime TEXT NOT NULL
		)
	`); err != nil {
		t.Fatal(err)
	}

	blobRoot := filepath.Join(root, "blobs")
	blobs, err := blob.New(blobRoot)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := blobs.Put(ctx, bytes.NewReader([]byte("persisted scheduled media")), "application/octet-stream", 1024)
	if err != nil {
		t.Fatal(err)
	}
	const outboxID = "fixture-outbox"
	if _, err := database.Exec(`
		INSERT INTO outbox_attachments (outbox_id, ordinal, blob_hash, size_bytes, mime)
		VALUES (?, 0, ?, ?, ?)
	`, outboxID, ref.Hash, ref.Size, ref.MIME); err != nil {
		t.Fatal(err)
	}
	expected := []scheduledBlob{{OutboxID: outboxID, Ref: ref}}
	if ok, verified := validateScheduledBlobs(ctx, database, blobRoot, expected); !ok || verified != 1 {
		t.Fatalf("valid persisted reference = ok %v, verified %d; want true, 1", ok, verified)
	}

	if _, err := database.Exec(`
		UPDATE outbox_attachments SET size_bytes = size_bytes + 1 WHERE outbox_id = ?
	`, outboxID); err != nil {
		t.Fatal(err)
	}
	if ok, verified := validateScheduledBlobs(ctx, database, blobRoot, expected); ok || verified != 0 {
		t.Fatalf("corrupt persisted reference = ok %v, verified %d; want false, 0", ok, verified)
	}
}
