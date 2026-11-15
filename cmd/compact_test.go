package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// compactTestBodyMarker appears in every message body the tests save, so a
// test can show that no output carries message content.
const compactTestBodyMarker = "zqxj-private-body"

// buildBloatedLegacyStore creates <dataDir>/messages.db the way stores were
// created before New chose incremental auto-vacuum, saves messages through
// the store's own write path, and deletes most of them, so the file holds
// many free pages. It returns the data directory and the kept message IDs.
func buildBloatedLegacyStore(t *testing.T) (string, []string) {
	t.Helper()
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, legacyDatabaseName)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	// The switch to WAL writes the file's first page, which fixes
	// auto_vacuum at none.
	if _, err := raw.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		raw.Close()
		t.Fatalf("create raw store: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	kept := saveAndThinMessages(t, store, thinnedStoreMessages(), 1500, 10)
	return dataDir, kept
}

// thinnedStoreMessages is how many messages a test store saves before it
// deletes nine in ten. The race job saves fewer: each save costs it far more
// time, and these tests check data, not concurrency.
func thinnedStoreMessages() int {
	if raceDetectorEnabled {
		return 80
	}
	return 400
}

// saveAndThinMessages saves count messages of about bodyBytes each and
// deletes all but every keepEvery-th, returning the IDs it kept. Each body
// starts with its message's own token, note<NNNNN>.
func saveAndThinMessages(t *testing.T, store *db.Store, count, bodyBytes, keepEvery int) []string {
	t.Helper()
	if err := store.UpsertConversation(&db.Conversation{ConversationID: "c1", Name: "Compact Test Contact"}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	words := []string{"harbor", "lantern", "meadow", "quartz", "saffron", "tundra", "velvet", "zephyr"}
	r := rand.New(rand.NewSource(int64(count)))
	var kept []string
	for i := 0; i < count; i++ {
		var body strings.Builder
		fmt.Fprintf(&body, "note%05d %s", i, compactTestBodyMarker)
		for body.Len() < bodyBytes {
			body.WriteByte(' ')
			body.WriteString(words[r.Intn(len(words))])
		}
		id := fmt.Sprintf("msg-%05d", i)
		if err := store.UpsertMessage(&db.Message{
			MessageID:      id,
			ConversationID: "c1",
			Body:           body.String(),
			TimestampMS:    int64(1_700_000_000_000 + i),
			SourcePlatform: "sms",
		}); err != nil {
			t.Fatalf("UpsertMessage(%s): %v", id, err)
		}
		if i%keepEvery == 0 {
			kept = append(kept, id)
		}
	}
	for i := 0; i < count; i++ {
		if i%keepEvery != 0 {
			if err := store.DeleteMessageByID(fmt.Sprintf("msg-%05d", i)); err != nil {
				t.Fatalf("DeleteMessageByID(): %v", err)
			}
		}
	}
	return kept
}

func testCompactDependencies(t *testing.T) compactDependencies {
	t.Helper()
	deps := defaultCompactDependencies()
	deps.now = func() time.Time { return time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC) }
	deps.version = "test-version"
	deps.commit = "test-commit"
	deps.probeURL = "http://127.0.0.1:7007/api/status"
	deps.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNREFUSED
	})}
	deps.availableBytes = func(string) (uint64, error) { return ^uint64(0), nil }
	tempDir := t.TempDir()
	deps.tempDir = func() string { return tempDir }
	return deps
}

func answeringBackend() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"connected":true}`)),
			Request:    request,
		}, nil
	})}
}

func legacyStoreStats(t *testing.T, path string) compactFiles {
	t.Helper()
	files, err := readCompactFiles(context.Background(), path)
	if err != nil {
		t.Fatalf("readCompactFiles(): %v", err)
	}
	return files
}

// A dry run reads the store, even beside a running app, and writes nothing:
// no backup, and the main file keeps every byte.
func TestCompactDryRunReportsAndChangesNothing(t *testing.T) {
	dataDir, _ := buildBloatedLegacyStore(t)
	path := filepath.Join(dataDir, legacyDatabaseName)
	before := fileSHA256(t, path)
	deps := testCompactDependencies(t)
	deps.httpClient = answeringBackend()

	out, err := executeCompact(context.Background(), compactOptions{dataDir: dataDir}, deps)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out.Applied || out.After != nil || out.BackupPath != "" {
		t.Fatalf("dry run applied something: %+v", out)
	}
	if out.Before == nil || out.Before.FreePages == 0 || out.Before.AutoVacuum != "none" {
		t.Fatalf("dry run Before = %+v, want free pages under auto_vacuum none", out.Before)
	}
	if out.RequiredFreeBytes == 0 {
		t.Error("dry run reported no space need")
	}
	if fileSHA256(t, path) != before {
		t.Error("dry run changed the main file")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "compact-backups")); !os.IsNotExist(err) {
		t.Errorf("dry run created a backup directory: %v", err)
	}
}

func TestCompactRefusesWhileTheBackendAnswers(t *testing.T) {
	dataDir, _ := buildBloatedLegacyStore(t)
	path := filepath.Join(dataDir, legacyDatabaseName)
	before := fileSHA256(t, path)
	deps := testCompactDependencies(t)
	deps.httpClient = answeringBackend()

	_, err := executeCompact(context.Background(), compactOptions{dataDir: dataDir, apply: true}, deps)
	if got := ExitCode(err); got != refusedRunningExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, refusedRunningExitCode, err)
	}
	if !strings.Contains(err.Error(), "quit the app") {
		t.Errorf("refusal does not say what to do: %v", err)
	}
	if fileSHA256(t, path) != before {
		t.Error("refused run changed the main file")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "compact-backups")); !os.IsNotExist(err) {
		t.Errorf("refused run created a backup directory: %v", err)
	}
}

func TestCompactRefusesWhileTheInstanceLockIsHeld(t *testing.T) {
	dataDir, _ := buildBloatedLegacyStore(t)
	lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{PID: 4242, Process: "test-daemon"})
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Close()

	_, err = executeCompact(context.Background(), compactOptions{dataDir: dataDir, apply: true}, testCompactDependencies(t))
	if got := ExitCode(err); got != refusedRunningExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, refusedRunningExitCode, err)
	}
}

func TestCompactRefusesWithoutSpaceBeforeWritingAnything(t *testing.T) {
	dataDir, _ := buildBloatedLegacyStore(t)
	path := filepath.Join(dataDir, legacyDatabaseName)
	before := fileSHA256(t, path)
	deps := testCompactDependencies(t)
	deps.availableBytes = func(string) (uint64, error) { return 1 << 20, nil }

	_, err := executeCompact(context.Background(), compactOptions{dataDir: dataDir, apply: true}, deps)
	if got := ExitCode(err); got != preflightExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, preflightExitCode, err)
	}
	if !strings.Contains(err.Error(), "insufficient free space") {
		t.Errorf("error does not name the shortfall: %v", err)
	}
	if fileSHA256(t, path) != before {
		t.Error("refused run changed the main file")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "compact-backups")); !os.IsNotExist(err) {
		t.Errorf("refused run created a backup directory: %v", err)
	}
}

// --apply backs the store up, rewrites it, and shows every row survived: the
// backup and the rewritten store each hold exactly the rows the store held.
func TestCompactApplyBacksUpThenRewritesKeepingEveryRow(t *testing.T) {
	ctx := context.Background()
	dataDir, kept := buildBloatedLegacyStore(t)
	path := filepath.Join(dataDir, legacyDatabaseName)
	original, err := readOnlyContentDigest(ctx, path)
	if err != nil {
		t.Fatalf("digest the store: %v", err)
	}
	deps := testCompactDependencies(t)

	out, err := executeCompact(ctx, compactOptions{dataDir: dataDir, apply: true}, deps)
	if err != nil {
		t.Fatalf("compact --apply: %v", err)
	}
	if !out.Applied || out.After == nil {
		t.Fatalf("compact --apply did not finish: %+v", out)
	}
	before, after := out.Before, out.After
	if after.FreePages != 0 || after.AutoVacuum != "incremental" {
		t.Errorf("after: %d free pages under auto_vacuum %s, want none under incremental", after.FreePages, after.AutoVacuum)
	}
	if after.FileBytes >= before.FileBytes || after.FileBytes != after.Pages*after.PageSize {
		t.Errorf("main file %d -> %d bytes for %d pages of %d, want a smaller file of exactly its pages", before.FileBytes, after.FileBytes, after.Pages, after.PageSize)
	}
	if after.WALBytes != 0 {
		t.Errorf("WAL after compact = %d bytes, want 0", after.WALBytes)
	}
	if out.QuickCheck != "ok" || out.Tables == 0 || out.Rows == 0 {
		t.Errorf("checks = quick_check %q over %d tables and %d rows", out.QuickCheck, out.Tables, out.Rows)
	}

	canonicalDataDir, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	wantBackup := filepath.Join(canonicalDataDir, "compact-backups", "20261103T230000Z", legacyDatabaseName)
	if out.BackupPath != wantBackup {
		t.Fatalf("backup = %s, want %s", out.BackupPath, wantBackup)
	}
	for path, mode := range map[string]os.FileMode{wantBackup: 0o600, filepath.Dir(wantBackup): 0o700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %v, want %v", path, info.Mode().Perm(), mode)
		}
	}
	for name, file := range map[string]string{"backup": wantBackup, "rewritten store": path} {
		digest, err := readOnlyContentDigest(ctx, file)
		if err != nil {
			t.Fatalf("digest the %s: %v", name, err)
		}
		if changed := db.ChangedTables(original, digest); len(changed) != 0 {
			t.Errorf("the %s differs from the original store in %v", name, changed)
		}
	}

	store, err := db.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	results, err := store.SearchMessages("note00010", "", 10)
	store.Close()
	if err != nil || len(results) != 1 || results[0].MessageID != kept[1] {
		t.Fatalf("search after compact = %d results (err %v), want kept message %s", len(results), err, kept[1])
	}

	// A second run finds nothing to do and takes no second backup.
	again, err := executeCompact(ctx, compactOptions{dataDir: dataDir, apply: true}, deps)
	if err != nil || !again.NothingToDo || again.BackupPath != "" {
		t.Fatalf("second run = %+v (err %v), want nothing to do", again, err)
	}
}

// A write that lands between the backup and the rewrite stops the run before
// it rewrites anything, so the backup always matches what was rewritten.
func TestCompactStopsWhenTheStoreChangesAfterTheBackup(t *testing.T) {
	dataDir, _ := buildBloatedLegacyStore(t)
	path := filepath.Join(dataDir, legacyDatabaseName)
	before := legacyStoreStats(t, path)
	deps := testCompactDependencies(t)
	deps.afterBackup = func() error {
		store, err := db.New(path)
		if err != nil {
			return err
		}
		defer store.Close()
		return store.UpsertMessage(&db.Message{MessageID: "late", ConversationID: "c1", Body: "late " + compactTestBodyMarker, TimestampMS: 1_800_000_000_000, SourcePlatform: "sms"})
	}

	out, err := executeCompact(context.Background(), compactOptions{dataDir: dataDir, apply: true}, deps)
	if got := ExitCode(err); got != verificationFailureExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, verificationFailureExitCode, err)
	}
	if out.Applied {
		t.Fatal("the run rewrote the store after the check failed")
	}
	if !strings.Contains(err.Error(), "messages") || strings.Contains(err.Error(), compactTestBodyMarker) {
		t.Errorf("error should name the changed tables and nothing more: %v", err)
	}
	after := legacyStoreStats(t, path)
	if after.AutoVacuum != "none" || after.FreePages == 0 || after.FreePages > before.FreePages {
		t.Errorf("store after the refused rewrite: %+v, before: %+v", after, before)
	}
	if _, err := os.Stat(out.BackupPath); err != nil {
		t.Errorf("the backup should stay for diagnosis: %v", err)
	}
}

// Neither the text nor the JSON output names a message, a body or a contact:
// only paths, sizes and counts.
func TestCompactOutputCarriesNoMessageContent(t *testing.T) {
	for _, args := range [][]string{{"--apply"}, {"--apply", "--json"}, {}, {"--json"}} {
		dataDir, kept := buildBloatedLegacyStore(t)
		t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
		var stdout, stderr bytes.Buffer
		if err := runCompactCommand(context.Background(), args, testCompactDependencies(t), &stdout, &stderr); err != nil {
			t.Fatalf("compact %v: %v", args, err)
		}
		output := stdout.String() + stderr.String()
		for _, private := range []string{compactTestBodyMarker, "note000", kept[0], "Compact Test Contact", "harbor", "lantern"} {
			if strings.Contains(output, private) {
				t.Errorf("compact %v output contains %q:\n%s", args, private, output)
			}
		}
		if len(args) > 0 && args[len(args)-1] == "--json" {
			var decoded compactOutput
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || !decoded.OK || decoded.Before == nil {
				t.Errorf("compact %v JSON = %+v (err %v)", args, decoded, err)
			}
		}
	}
}

func TestCheckCompactSpaceAddsNeedsThatShareAFilesystem(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	needs := []spaceNeed{{dir: a, bytes: 100, what: "one"}, {dir: filepath.Join(b, "not", "yet"), bytes: 100, what: "two"}}
	if total, err := checkCompactSpace(needs, func(string) (uint64, error) { return 150, nil }); err == nil || total != 200 {
		t.Fatalf("150 bytes free for two 100-byte needs on one filesystem: total %d, err %v; want an error", total, err)
	}
	if total, err := checkCompactSpace(needs, func(string) (uint64, error) { return 200, nil }); err != nil || total != 200 {
		t.Fatalf("200 bytes free: total %d, err %v; want no error", total, err)
	}
}

func TestSQLiteTempDirFollowsSQLitesOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	t.Setenv("SQLITE_TMPDIR", first)
	t.Setenv("TMPDIR", second)
	if got := sqliteTempDir(); got != first {
		t.Errorf("with SQLITE_TMPDIR set: %s, want %s", got, first)
	}
	t.Setenv("SQLITE_TMPDIR", "")
	if got := sqliteTempDir(); got != second {
		t.Errorf("with only TMPDIR set: %s, want %s", got, second)
	}
	t.Setenv("TMPDIR", filepath.Join(second, "missing"))
	if got := sqliteTempDir(); got != "/var/tmp" && got != "/usr/tmp" && got != "/tmp" && got != "." {
		t.Errorf("with TMPDIR missing: %s, want a system fallback", got)
	}
}
