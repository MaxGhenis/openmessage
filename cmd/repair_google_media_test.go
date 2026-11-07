package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// repairThumbnailRef is the reference code before migration 0012 stored for
// a thumbnail: unmarked, and without the message part to ask for the original.
const repairThumbnailRef = `{"v":1,"media_id":"thumb-1","decryption_key":"0a"}`

// seedGoogleMediaRepair provisions a data dir whose v2 store holds a pending
// Google image whose reference is a thumbnail stored the old way, and the
// inbox frame that named it. It returns the data dir and store path.
func seedGoogleMediaRepair(t *testing.T) (string, string) {
	t.Helper()
	storePath := seedRepairStore(t)
	dataDir := filepath.Dir(filepath.Dir(storePath))
	occurredAtMS := time.Now().Add(-time.Hour).UnixMilli()
	db, err := sql.Open("sqlite", "file:"+storePath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms)
			VALUES ('conversation', 'google-primary', 'google-conversation', 'direct', 1, 1)`},
		{query: `INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, occurred_at_ms, created_at_ms, updated_at_ms)
			VALUES ('m1', 'conversation', 'google-primary', '89926', 'incoming', ?, 1, 1)`, args: []any{occurredAtMS}},
		{query: `INSERT INTO message_attachments (message_id, ordinal, remote_id, remote_ref, mime, state, created_at_ms, updated_at_ms)
			VALUES ('m1', 0, 'thumb-1', ?, 'image/jpeg', 'pending', 1, 1)`, args: []any{[]byte(repairThumbnailRef)}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}

	message := &gmproto.Message{
		MessageID:      "89926",
		ConversationID: "google-conversation",
		Timestamp:      occurredAtMS * 1000,
		MessageInfo: []*gmproto.MessageInfo{{
			ActionMessageID: proto.String("88846"),
			Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
				MimeType: "image/jpeg", ThumbnailMediaID: "thumb-1", ThumbnailDecryptionKey: []byte{0x0a},
			}},
		}},
	}
	record, err := ingest.GoogleMessageRecord("google-primary", 1, &libgm.WrappedMessage{Message: message}, time.UnixMilli(occurredAtMS))
	if err != nil {
		t.Fatalf("GoogleMessageRecord(): %v", err)
	}
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	if _, err := messages.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID: "inbox-1", AccountID: record.AccountID, Generation: int64(record.Generation),
		DedupeKey: record.DedupeKey, Codec: record.Codec, CodecVersion: int64(record.CodecVersion),
		Payload: record.Payload,
	}); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	return dataDir, storePath
}

func runGoogleMediaRepairJSON(t *testing.T, args ...string) googleMediaRepairOutput {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runRepairGoogleMedia(context.Background(), zerolog.Nop(), append([]string{"--json"}, args...), &stdout, &stderr); err != nil {
		t.Fatalf("repair google-media %v: %v\n%s", args, err, stderr.String())
	}
	var output googleMediaRepairOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output %q: %v", stdout.String(), err)
	}
	return output
}

// storeAttachmentRef returns the stored reference of m1's attachment and
// whether it is marked as a thumbnail.
func storeAttachmentRef(t *testing.T, storePath string) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ref []byte
	var preview bool
	if err := db.QueryRow(`SELECT remote_ref, remote_preview FROM message_attachments WHERE message_id = 'm1' AND ordinal = 0`).Scan(&ref, &preview); err != nil {
		t.Fatal(err)
	}
	return string(ref), preview
}

func TestRepairGoogleMediaDryRunPlansAndWritesNothing(t *testing.T) {
	dataDir, storePath := seedGoogleMediaRepair(t)
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	output := runGoogleMediaRepairJSON(t)
	if output.Applied || output.BackupDir != "" || output.Report == nil {
		t.Fatalf("dry run output = %+v", output)
	}
	report := output.Report
	if report.Frames != 1 || report.MediaMessages != 1 || report.Attachments != 1 ||
		len(report.Repairs) != 1 || report.Repairs[0].Outcome != "pending-thumbnail" || !report.Repairs[0].ToPreview {
		t.Fatalf("plan = %+v", report)
	}
	after, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the dry run wrote the store")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "v2", "repair-backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dry run made a backup: %v", err)
	}
}

func TestRepairGoogleMediaDryRunNeverMigratesTheStore(t *testing.T) {
	_, storePath := seedGoogleMediaRepair(t)
	latest := rollBackLedgerOneVersion(t, storePath)
	var stdout, stderr bytes.Buffer
	err := runRepairGoogleMedia(context.Background(), zerolog.Nop(), nil, &stdout, &stderr)
	if !errors.Is(err, sqlite.ErrMigrationPending) {
		t.Fatalf("dry run error = %v, want ErrMigrationPending", err)
	}
	if got := ledgerVersionCount(t, storePath); got != latest-1 {
		t.Fatalf("ledger rows = %d after the dry run, want %d", got, latest-1)
	}
}

func TestRepairGoogleMediaApplyBacksUpThenRewrites(t *testing.T) {
	dataDir, storePath := seedGoogleMediaRepair(t)
	output := runGoogleMediaRepairJSON(t, "--apply", "--skip-daemon-probe", "--report", filepath.Join(t.TempDir(), "report.json"))
	if !output.Applied || output.Report.AppliedRepairs != 1 || output.ReportPath == "" ||
		!strings.HasPrefix(output.BackupDir, filepath.Join(dataDir, "v2", "repair-backups")) {
		t.Fatalf("apply output = %+v", output)
	}
	ref, preview := storeAttachmentRef(t, storePath)
	if !preview || !strings.Contains(ref, `"message_id":"89926"`) || !strings.Contains(ref, `"part_id":"88846"`) {
		t.Fatalf("reference after apply = %s (preview %v), want the thumbnail marked, with its message and part", ref, preview)
	}
	backupRef, backupPreview := storeAttachmentRef(t, filepath.Join(output.BackupDir, "store.sqlite3"))
	if backupRef != repairThumbnailRef || backupPreview {
		t.Fatalf("backup reference = %s (preview %v), want the store as it was", backupRef, backupPreview)
	}
	if again := runGoogleMediaRepairJSON(t); len(again.Report.Repairs) != 0 {
		t.Fatalf("second plan = %+v", again.Report)
	}
}

func TestRepairGoogleMediaSummaryCountsOutcomes(t *testing.T) {
	seedGoogleMediaRepair(t)
	var stdout, stderr bytes.Buffer
	if err := runRepairGoogleMedia(context.Background(), zerolog.Nop(), []string{"--verbose"}, &stdout, &stderr); err != nil {
		t.Fatalf("dry run: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"DRY RUN: account google-primary, 1 frames", "pending-thumbnail", "repairs: 1", "thumb-1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout.String())
		}
	}
}

// --apply refuses while another process holds the instance lock, and writes
// nothing.
func TestRepairGoogleMediaApplyRefusesWhileTheInstanceLockIsHeld(t *testing.T) {
	dataDir, storePath := seedGoogleMediaRepair(t)
	lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{PID: 4242, Process: "test-daemon"})
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Close()
	var stdout, stderr bytes.Buffer
	err = runRepairGoogleMedia(context.Background(), zerolog.Nop(), []string{"--apply", "--skip-daemon-probe"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "holds the instance lock") {
		t.Fatalf("apply error = %v, want a refusal naming the instance lock", err)
	}
	if ref, preview := storeAttachmentRef(t, storePath); ref != repairThumbnailRef || preview {
		t.Fatalf("reference = %s (preview %v) after a refused apply", ref, preview)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "v2", "repair-backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused apply made a backup: %v", err)
	}
}
