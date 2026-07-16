package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

var fixtureCounts = legacyCounts{
	Conversations:     2,
	Messages:          3,
	Contacts:          2,
	UnifiedContacts:   1,
	Drafts:            1,
	ScheduledMessages: 7,
	ScheduledMessagesByStatus: map[string]int64{
		db.ScheduleStatusPending:  2,
		db.ScheduleStatusSending:  1,
		db.ScheduleStatusSent:     1,
		db.ScheduleStatusFailed:   1,
		db.ScheduleStatusCanceled: 1,
		"future-status":           1,
	},
	ContactMeta: 2,
}

func TestBackupHappyPathWritesVerifiedManifestLast(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	writeBackupAuxiliaryFixture(t, dataDir)

	now := time.Date(2026, 7, 16, 12, 34, 56, 987000000, time.FixedZone("EDT", -4*60*60))
	deps := testBackupDependencies(t, now)
	result, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err != nil {
		t.Fatalf("executeBackup: %v", err)
	}

	canonicalDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(canonicalDataDir, "migration-backups", "20260716T163456Z")
	if result.BackupPath != wantDir {
		t.Fatalf("BackupPath = %q, want %q", result.BackupPath, wantDir)
	}
	if result.ManifestPath != filepath.Join(wantDir, "manifest.json") {
		t.Fatalf("ManifestPath = %q", result.ManifestPath)
	}

	manifest := readBackupManifest(t, result.ManifestPath)
	if manifest.Format != backupManifestFormat {
		t.Fatalf("manifest format = %d, want %d", manifest.Format, backupManifestFormat)
	}
	if manifest.CreatedAt != "2026-07-16T16:34:56Z" {
		t.Fatalf("created_at = %q", manifest.CreatedAt)
	}
	if manifest.AppVersion != "test-version" || manifest.Tool.Version != "test-version" || manifest.Tool.Commit != "test-commit" {
		t.Fatalf("tool metadata = %+v, app_version = %q", manifest.Tool, manifest.AppVersion)
	}
	if manifest.Source.Path != canonicalDataDir || manifest.Source.DatabasePath != filepath.Join(canonicalDataDir, "messages.db") {
		t.Fatalf("source = %+v", manifest.Source)
	}
	if manifest.Source.SchemaFingerprint == "" {
		t.Fatal("schema fingerprint is empty")
	}
	if manifest.Source.QuickCheck != "ok" {
		t.Fatalf("source.quick_check = %q, want ok", manifest.Source.QuickCheck)
	}
	assertLegacyCounts(t, manifest.Counts, fixtureCounts)

	wantFiles := map[string]bool{
		"messages.db":                 false,
		"session.json":                false,
		"signal-cli/accounts/account": false,
		"signal-cli/config.json":      false,
		"whatsapp-session.db":         false,
		"openmessage.db":              false,
	}
	for _, file := range manifest.Files {
		if _, ok := wantFiles[file.Path]; !ok {
			t.Errorf("unexpected manifest file %q", file.Path)
			continue
		}
		wantFiles[file.Path] = true
		backupPath := filepath.Join(result.BackupPath, filepath.FromSlash(file.Path))
		info, err := os.Stat(backupPath)
		if err != nil {
			t.Errorf("stat %q: %v", file.Path, err)
			continue
		}
		if file.Size != info.Size() {
			t.Errorf("%s size = %d, stat = %d", file.Path, file.Size, info.Size())
		}
		if file.SHA256 != fileSHA256(t, backupPath) {
			t.Errorf("%s sha256 does not match copied bytes", file.Path)
		}
		if !filepath.IsAbs(file.SourcePath) {
			t.Errorf("%s source_path = %q, want absolute", file.Path, file.SourcePath)
		}
	}
	for path, found := range wantFiles {
		if !found {
			t.Errorf("manifest missing %q", path)
		}
	}

	copyDB, err := sql.Open("sqlite", filepath.Join(result.BackupPath, "messages.db"))
	if err != nil {
		t.Fatalf("open copied database: %v", err)
	}
	defer copyDB.Close()
	var quickCheck string
	if err := copyDB.QueryRow("PRAGMA quick_check").Scan(&quickCheck); err != nil {
		t.Fatalf("quick_check copied database: %v", err)
	}
	if quickCheck != "ok" {
		t.Fatalf("copied database quick_check = %q", quickCheck)
	}
}

func TestBackupInterruptedBeforeManifestLeavesIncompleteDirectory(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	deps := testBackupDependencies(t, now)
	deps.beforeManifest = func() error { return errors.New("simulated interruption") }

	_, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err == nil {
		t.Fatal("executeBackup succeeded, want interruption")
	}
	if got := ExitCode(err); got != backupFailureExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, backupFailureExitCode, err)
	}

	backupDir := filepath.Join(dataDir, "migration-backups", "20260716T010203Z")
	if _, statErr := os.Stat(filepath.Join(backupDir, "messages.db")); statErr != nil {
		t.Fatalf("partial database copy should remain for diagnosis: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(backupDir, "manifest.json")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest exists after interrupted run: %v", statErr)
	}
}

func TestBackupRefusesWhenDaemonHealthEndpointResponds(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	requested := make(chan string, 1)

	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC))
	deps.probeURL = "http://127.0.0.1:7007/api/status"
	deps.httpClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requested <- request.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"connected":true}`)),
			Request:    request,
		}, nil
	})}
	_, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err == nil {
		t.Fatal("executeBackup succeeded while daemon responded")
	}
	if got := ExitCode(err); got != refusedRunningExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, refusedRunningExitCode, err)
	}
	if !strings.Contains(err.Error(), "running") || !strings.Contains(err.Error(), "127.0.0.1:7007") {
		t.Fatalf("refusal error is not actionable: %v", err)
	}
	select {
	case path := <-requested:
		if path != "/api/status" {
			t.Fatalf("probe path = %q", path)
		}
	default:
		t.Fatal("daemon endpoint was not probed")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "migration-backups")); !os.IsNotExist(statErr) {
		t.Fatalf("backup directory created despite refusal: %v", statErr)
	}
}

func TestBackupRefusesWhenInstanceLockIsHeld(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{
		PID: 4242, Process: "test-daemon",
	})
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer lock.Close()

	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC))
	_, err = executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err == nil {
		t.Fatal("executeBackup succeeded while instance lock held")
	}
	if got := ExitCode(err); got != refusedRunningExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, refusedRunningExitCode, err)
	}
}

func TestBackupRejectsInsufficientDestinationSpace(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	info, err := os.Stat(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	available := uint64(info.Size()*2 - 1)
	var checkedPath string
	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC))
	deps.availableBytes = func(path string) (uint64, error) {
		checkedPath = path
		return available, nil
	}

	_, err = executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err == nil {
		t.Fatal("executeBackup succeeded without required free space")
	}
	if got := ExitCode(err); got != preflightExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, preflightExitCode, err)
	}
	if checkedPath == "" || !strings.Contains(err.Error(), "free space") || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("space error is not actionable: checked=%q err=%v", checkedPath, err)
	}
	backupDB := filepath.Join(dataDir, "migration-backups", "20260716T000000Z", "messages.db")
	if _, statErr := os.Stat(backupDB); !os.IsNotExist(statErr) {
		t.Fatalf("database copied despite failed space preflight: %v", statErr)
	}
}

func TestBackupManifestRowCountsIncludeEveryScheduledStatus(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 5, 6, 7, 0, time.UTC))
	result, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err != nil {
		t.Fatal(err)
	}
	manifest := readBackupManifest(t, result.ManifestPath)
	assertLegacyCounts(t, manifest.Counts, fixtureCounts)

	var statusTotal int64
	for _, count := range manifest.Counts.ScheduledMessagesByStatus {
		statusTotal += count
	}
	if statusTotal != manifest.Counts.ScheduledMessages {
		t.Fatalf("scheduled status total = %d, scheduled_messages = %d", statusTotal, manifest.Counts.ScheduledMessages)
	}
}

func TestBackupJSONOutputIsMachineReadable(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	destination := filepath.Join(t.TempDir(), "operator-selected-backup")
	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 5, 6, 7, 0, time.UTC))
	var stdout, stderr bytes.Buffer

	err := runBackupCommand(context.Background(), []string{"--json", "--to", destination}, deps, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runBackupCommand: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var output backupCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	canonicalDestination := filepath.Join(canonicalParent, filepath.Base(destination))
	if !output.OK || output.ExitCode != 0 || output.BackupPath != canonicalDestination {
		t.Fatalf("JSON output = %+v", output)
	}
	if output.ManifestPath != filepath.Join(canonicalDestination, "manifest.json") {
		t.Fatalf("manifest_path = %q", output.ManifestPath)
	}
}

func TestBackupVerificationFailureUsesExitCodeFive(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	deps := testBackupDependencies(t, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC))
	deps.quickCheck = func(string) (string, error) { return "database disk image is malformed", nil }

	_, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err == nil {
		t.Fatal("executeBackup succeeded with failed quick_check")
	}
	if got := ExitCode(err); got != verificationFailureExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, verificationFailureExitCode, err)
	}
	manifestPath := filepath.Join(dataDir, "migration-backups", "20260716T000000Z", "manifest.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("manifest exists after verification failure: %v", statErr)
	}
}

func buildLegacyBackupFixture(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}

	fail := func(label string, err error) {
		t.Helper()
		if err != nil {
			_ = store.Close()
			t.Fatalf("%s: %v", label, err)
		}
	}
	fail("conversation 1", store.UpsertConversation(&db.Conversation{
		ConversationID: "conv-1", Name: "Alice", Participants: `[{"name":"Alice","number":"+15550000001"}]`, SourcePlatform: "sms",
	}))
	fail("conversation 2", store.UpsertConversation(&db.Conversation{
		ConversationID: "conv-2", Name: "Group", IsGroup: true, Participants: `[]`, SourcePlatform: "signal",
	}))
	for i, message := range []db.Message{
		{MessageID: "message-1", ConversationID: "conv-1", SenderName: "Alice", SenderNumber: "+15550000001", Body: "hello", TimestampMS: 1, Status: "delivered", SourcePlatform: "sms"},
		{MessageID: "message-2", ConversationID: "conv-1", SenderName: "Me", Body: "hi", TimestampMS: 2, Status: "sent", IsFromMe: true, SourcePlatform: "sms"},
		{MessageID: "signal:3", ConversationID: "conv-2", SenderName: "Bob", SenderNumber: "aci", Body: "signal", TimestampMS: 3, Status: "delivered", SourcePlatform: "signal"},
	} {
		m := message
		fail("message "+string(rune('1'+i)), store.UpsertMessage(&m))
	}
	fail("contact 1", store.UpsertContact(&db.Contact{ContactID: "contact-1", Name: "Alice", Number: "+15550000001"}))
	fail("contact 2", store.UpsertContact(&db.Contact{ContactID: "contact-2", Name: "Bob", Number: "+15550000002"}))
	fail("unified contact", store.UpsertUnifiedContact(&db.UnifiedContact{
		UnifiedID: "person-1", DisplayName: "Alice", Identifiers: `[{"platform":"sms","value":"+15550000001"}]`,
	}))
	fail("draft", store.UpsertDraft(&db.Draft{DraftID: "draft-1", ConversationID: "conv-1", Body: "later", CreatedAt: 4}))

	statuses := []string{
		db.ScheduleStatusPending,
		db.ScheduleStatusPending,
		db.ScheduleStatusSending,
		db.ScheduleStatusSent,
		db.ScheduleStatusFailed,
		db.ScheduleStatusCanceled,
		"future-status",
	}
	for i, status := range statuses {
		fail("scheduled message", store.CreateScheduledMessage(&db.ScheduledMessage{
			ID: "scheduled-" + string(rune('a'+i)), ConversationID: "conv-1", Body: status,
			SendAt: int64(100 + i), Status: status, CreatedAt: int64(10 + i),
		}))
	}
	fail("contact meta 1", store.SetContactTags("alice", "Alice", []string{"friend"}))
	fail("contact meta 2", store.SetContactTags("bob", "Bob", []string{"work"}))
	if err := store.Close(); err != nil {
		t.Fatalf("close fixture store: %v", err)
	}
	return dataDir
}

func writeBackupAuxiliaryFixture(t *testing.T, dataDir string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(dataDir, "session.json"), []byte(`{"token":"secret"}`), 0o600)
	writeFixtureSQLite(t, filepath.Join(dataDir, "whatsapp-session.db"), "whatsmeow_device")
	writeFixtureSQLite(t, filepath.Join(dataDir, "openmessage.db"), "openmessage_state")
	writeFixtureFile(t, filepath.Join(dataDir, "signal-cli", "config.json"), []byte(`{"account":"+15550000000"}`), 0o600)
	writeFixtureFile(t, filepath.Join(dataDir, "signal-cli", "accounts", "account"), []byte("signal account"), 0o600)
}

func writeFixtureFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

// writeFixtureSQLite creates a real, checkpointed SQLite database with one
// table so the aux-database snapshot path has valid input (production aux
// files -- whatsapp-session.db, openmessage.db -- are SQLite).
func writeFixtureSQLite(t *testing.T, path, table string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO " + table + " (v) VALUES ('seed')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func testBackupDependencies(t *testing.T, now time.Time) backupDependencies {
	t.Helper()
	deps := defaultBackupDependencies()
	deps.now = func() time.Time { return now }
	deps.version = "test-version"
	deps.commit = "test-commit"
	deps.probeURL = "http://127.0.0.1:7007/api/status"
	deps.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	})}
	deps.availableBytes = func(string) (uint64, error) { return ^uint64(0), nil }
	return deps
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func readBackupManifest(t *testing.T, path string) backupManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

func assertLegacyCounts(t *testing.T, got, want legacyCounts) {
	t.Helper()
	if got.Conversations != want.Conversations || got.Messages != want.Messages || got.Contacts != want.Contacts ||
		got.UnifiedContacts != want.UnifiedContacts || got.Drafts != want.Drafts ||
		got.ScheduledMessages != want.ScheduledMessages || got.ContactMeta != want.ContactMeta {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	if len(got.ScheduledMessagesByStatus) != len(want.ScheduledMessagesByStatus) {
		t.Fatalf("scheduled status counts = %#v, want %#v", got.ScheduledMessagesByStatus, want.ScheduledMessagesByStatus)
	}
	for status, count := range want.ScheduledMessagesByStatus {
		if got.ScheduledMessagesByStatus[status] != count {
			t.Errorf("scheduled status %q = %d, want %d", status, got.ScheduledMessagesByStatus[status], count)
		}
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// A crashed daemon can hold an auxiliary database's ENTIRE content in a hot
// WAL. A plain file copy silently produces a valid-looking empty database, so
// the backup must snapshot SQLite auxiliaries transactionally. This test fails
// under the plain-copy design.
func TestBackupCapturesAuxiliaryDatabaseHotWAL(t *testing.T) {
	dataDir := buildLegacyBackupFixture(t)
	writeBackupAuxiliaryFixture(t, dataDir)

	// Rebuild whatsapp-session.db so all of its content lives only in the WAL:
	// open in WAL mode, write, and DO NOT checkpoint or close cleanly enough to
	// merge (skip the close-checkpoint by leaving the -wal in place via a
	// second reader hold... simplest reliable approach: write, then copy the
	// -wal aside, checkpoint, restore the pre-checkpoint main file + wal).
	auxPath := filepath.Join(dataDir, "whatsapp-session.db")
	if err := os.Remove(auxPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(auxPath)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE hot (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	// Snapshot the main file while it is still empty (pre-checkpoint).
	preCheckpointMain, err := os.ReadFile(auxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO hot (v) VALUES ('only-in-wal-1'), ('only-in-wal-2')"); err != nil {
		t.Fatal(err)
	}
	walBytes, err := os.ReadFile(auxPath + "-wal")
	if err != nil {
		t.Fatalf("expected a hot WAL: %v", err)
	}
	if len(walBytes) == 0 {
		t.Fatal("fixture WAL is empty; test cannot discriminate")
	}
	if err := db.Close(); err != nil { // close checkpoints; restore the hot state below
		t.Fatal(err)
	}
	if err := os.WriteFile(auxPath, preCheckpointMain, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auxPath+"-wal", walBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	deps := testBackupDependencies(t, now)
	result, err := executeBackup(context.Background(), backupOptions{dataDir: dataDir}, deps)
	if err != nil {
		t.Fatalf("executeBackup: %v", err)
	}

	copyDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(result.BackupPath, "whatsapp-session.db"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM hot").Scan(&count); err != nil {
		t.Fatalf("hot table missing from backup copy (WAL content dropped): %v", err)
	}
	if count != 2 {
		t.Fatalf("backup copy hot rows = %d, want 2 (WAL content dropped)", count)
	}
}
