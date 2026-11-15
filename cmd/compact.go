package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

// rewriteFailureExitCode reports that the rewrite itself failed after the
// backup was made and checked.
const rewriteFailureExitCode = 6

type compactOptions struct {
	dataDir   string
	backupDir string
	apply     bool
}

type compactDependencies struct {
	now            func() time.Time
	version        string
	commit         string
	probeURL       string
	httpClient     *http.Client
	availableBytes func(string) (uint64, error)
	quickCheck     func(string) (string, error)
	tempDir        func() string
	// afterBackup runs once the backup has passed quick_check, before the
	// store is compared with it. Tests write to the store here.
	afterBackup func() error
}

// compactFiles is the legacy store's files and pages at one moment. It holds
// sizes and counts only: compact never prints message content.
type compactFiles struct {
	FileBytes  int64  `json:"file_bytes"`
	WALBytes   int64  `json:"wal_bytes"`
	PageSize   int64  `json:"page_size"`
	Pages      int64  `json:"pages"`
	FreePages  int64  `json:"free_pages"`
	FreeBytes  int64  `json:"free_bytes"`
	AutoVacuum string `json:"auto_vacuum"`
}

// liveBytes is the space the pages in use take, which is about what a
// rewrite writes.
func (f compactFiles) liveBytes() uint64 {
	if f.Pages <= f.FreePages || f.PageSize <= 0 {
		return 0
	}
	return uint64(f.Pages-f.FreePages) * uint64(f.PageSize)
}

type compactOutput struct {
	OK           bool          `json:"ok"`
	ExitCode     int           `json:"exit_code"`
	Applied      bool          `json:"applied"`
	DatabasePath string        `json:"database_path,omitempty"`
	Before       *compactFiles `json:"before,omitempty"`
	After        *compactFiles `json:"after,omitempty"`
	// RequiredFreeBytes is the free disk --apply needs, on all filesystems
	// together, and SpaceProblem says where a dry run found too little.
	RequiredFreeBytes uint64 `json:"required_free_bytes,omitempty"`
	SpaceProblem      string `json:"space_problem,omitempty"`
	NothingToDo       bool   `json:"nothing_to_do,omitempty"`
	BackupPath        string `json:"backup_path,omitempty"`
	// Tables and Rows count what the digests compared: every ordinary
	// table, the search index's among them.
	Tables         int     `json:"tables,omitempty"`
	Rows           int64   `json:"rows,omitempty"`
	RewriteSeconds float64 `json:"rewrite_seconds,omitempty"`
	CheckpointBusy bool    `json:"checkpoint_busy,omitempty"`
	QuickCheck     string  `json:"quick_check,omitempty"`
	Error          string  `json:"error,omitempty"`
}

// RunCompact handles "openmessage compact [--apply] [--backup-dir <dir>]
// [--json]".
//
// Without --apply it only reads. With --apply it rewrites the legacy store
// with VACUUM, which returns its free pages to the filesystem and switches it
// to incremental auto-vacuum, so the daemon can return the pages that later
// deletes free (legacyFreePageReclaimer). It refuses while the app runs,
// takes and checks a backup first, and shows that every row survived.
func RunCompact(_ zerolog.Logger, args ...string) error {
	return runCompactCommand(context.Background(), args, defaultCompactDependencies(), os.Stdout, os.Stderr)
}

func runCompactCommand(ctx context.Context, args []string, deps compactDependencies, stdout, stderr io.Writer) error {
	asJSON := hasFlag(args, "--json")
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: openmessage compact [--apply] [--backup-dir <directory>] [--json]")
		fmt.Fprintln(stderr, "  without --apply, reports the legacy store's free pages and changes nothing")
		fmt.Fprintln(stderr, "  --backup-dir defaults to <data-dir>/compact-backups/<UTC-stamp>")
	}
	var opts compactOptions
	fs.BoolVar(&opts.apply, "apply", false, "rewrite the store (quit the app first)")
	fs.StringVar(&opts.backupDir, "backup-dir", "", "directory for the backup taken before the rewrite")
	fs.BoolVar(&asJSON, "json", asJSON, "write one machine-readable JSON object")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		err = newBackupError(preflightExitCode, "parse compact options: %w", err)
		return writeCompactOutput(stdout, asJSON, compactOutput{}, err)
	}
	if fs.NArg() != 0 {
		err := newBackupError(preflightExitCode, "unexpected compact argument %q", fs.Arg(0))
		return writeCompactOutput(stdout, asJSON, compactOutput{}, err)
	}
	opts.dataDir = app.DefaultDataDir()
	out, err := executeCompact(ctx, opts, deps)
	return writeCompactOutput(stdout, asJSON, out, err)
}

func defaultCompactDependencies() compactDependencies {
	commit, _ := buildRevision()
	return compactDependencies{
		now:      time.Now,
		version:  Version(),
		commit:   commit,
		probeURL: defaultBackendProbeURL(),
		httpClient: &http.Client{
			Timeout: 750 * time.Millisecond,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		availableBytes: filesystemAvailableBytes,
		quickCheck:     sqliteQuickCheck,
		tempDir:        sqliteTempDir,
	}
}

// sqliteTempDir is where SQLite puts VACUUM's temporary copy: the first
// writable directory among SQLITE_TMPDIR, TMPDIR, /var/tmp, /usr/tmp, /tmp
// and the working directory, the order SQLite's unix VFS tries.
func sqliteTempDir() string {
	for _, dir := range []string{os.Getenv("SQLITE_TMPDIR"), os.Getenv("TMPDIR"), "/var/tmp", "/usr/tmp", "/tmp"} {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() && syscall.Access(dir, 3) == nil {
			return dir
		}
	}
	return "."
}

func executeCompact(ctx context.Context, opts compactOptions, deps compactDependencies) (compactOutput, error) {
	var out compactOutput
	if deps.now == nil || deps.httpClient == nil || deps.availableBytes == nil || deps.quickCheck == nil || deps.tempDir == nil {
		return out, newBackupError(preflightExitCode, "compact dependencies are incomplete")
	}
	createdAt := deps.now().UTC().Truncate(time.Second)
	dataDir, err := canonicalExistingDirectory(opts.dataDir)
	if err != nil {
		return out, newBackupError(preflightExitCode, "resolve OpenMessage data directory %q: %w", opts.dataDir, err)
	}
	path := filepath.Join(dataDir, legacyDatabaseName)
	out.DatabasePath = path
	if info, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return out, newBackupError(preflightExitCode, "legacy database not found at %s", path)
		}
		return out, newBackupError(preflightExitCode, "inspect legacy database %s: %w", path, err)
	} else if !info.Mode().IsRegular() {
		return out, newBackupError(preflightExitCode, "legacy database path is not a regular file: %s", path)
	}

	if opts.apply {
		lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{
			PID:           os.Getpid(),
			Process:       "openmessage compact",
			StartedAt:     createdAt.Format(time.RFC3339),
			BuildID:       deps.version,
			Commit:        deps.commit,
			CanonicalPath: dataDir,
			ProbeURL:      deps.probeURL,
		})
		if err != nil {
			if errors.Is(err, errInstanceLockHeld) {
				return out, newBackupError(refusedRunningExitCode, "OpenMessage state is in use: %s is locked; quit the app and retry", filepath.Join(dataDir, instanceLockName))
			}
			return out, newBackupError(preflightExitCode, "acquire OpenMessage instance lock: %w", err)
		}
		defer lock.Close()
		running, probeErr := probeBackend(ctx, deps.httpClient, deps.probeURL)
		if probeErr != nil {
			return out, newBackupError(refusedRunningExitCode, "cannot rule out a running OpenMessage backend at %s: %w; quit the app and retry", deps.probeURL, probeErr)
		}
		if running {
			return out, newBackupError(refusedRunningExitCode, "OpenMessage backend is running at %s; quit the app before compacting", deps.probeURL)
		}
	}

	before, err := readCompactFiles(ctx, path)
	if err != nil {
		return out, newBackupError(preflightExitCode, "read the legacy store's pages: %w", err)
	}
	out.Before = &before
	if before.FreePages == 0 && before.AutoVacuum == db.AutoVacuumIncremental.String() {
		out.NothingToDo = true
		return out, nil
	}

	backupDir := opts.backupDir
	if backupDir == "" {
		backupDir = filepath.Join(dataDir, "compact-backups", createdAt.Format("20060102T150405Z"))
	}
	if backupDir, err = resolveBackupDestination(dataDir, backupDir, createdAt); err != nil {
		return out, newBackupError(preflightExitCode, "resolve backup directory: %w", err)
	}
	required, spaceErr := checkCompactSpace(compactSpaceNeeds(dataDir, backupDir, deps.tempDir(), before.liveBytes()), deps.availableBytes)
	out.RequiredFreeBytes = required
	if !opts.apply {
		if spaceErr != nil {
			out.SpaceProblem = spaceErr.Error()
		}
		return out, nil
	}
	if spaceErr != nil {
		return out, newBackupError(preflightExitCode, "%w", spaceErr)
	}
	if _, err := os.Lstat(backupDir); err == nil {
		return out, newBackupError(preflightExitCode, "backup directory already exists: %s (choose a new --backup-dir; compact never overwrites a backup)", backupDir)
	} else if !os.IsNotExist(err) {
		return out, newBackupError(preflightExitCode, "inspect backup directory %s: %w", backupDir, err)
	}

	// The backup comes first, read from the store without writing to it, so
	// it holds the store as the app left it.
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return out, newBackupError(backupFailureExitCode, "create backup directory %s: %w", backupDir, err)
	}
	if err := os.Chmod(backupDir, 0o700); err != nil {
		return out, newBackupError(backupFailureExitCode, "secure backup directory %s: %w", backupDir, err)
	}
	backupDB := filepath.Join(backupDir, legacyDatabaseName)
	if err := vacuumInto(ctx, path, backupDB); err != nil {
		return out, newBackupError(backupFailureExitCode, "back up the legacy store with VACUUM INTO: %w", err)
	}
	if err := os.Chmod(backupDB, 0o600); err != nil {
		return out, newBackupError(backupFailureExitCode, "secure the backup: %w", err)
	}
	out.BackupPath = backupDB
	if result, err := deps.quickCheck(backupDB); err != nil {
		return out, newBackupError(verificationFailureExitCode, "run quick_check on the backup: %w", err)
	} else if result != "ok" {
		return out, newBackupError(verificationFailureExitCode, "the backup failed quick_check: %s", result)
	}
	backupDigest, err := readOnlyContentDigest(ctx, backupDB)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "digest the backup: %w", err)
	}
	if deps.afterBackup != nil {
		if err := deps.afterBackup(); err != nil {
			return out, newBackupError(backupFailureExitCode, "compact interrupted after the backup: %w", err)
		}
	}
	sourceDigest, err := readOnlyContentDigest(ctx, path)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "digest the legacy store: %w", err)
	}
	if changed := db.ChangedTables(sourceDigest, backupDigest); len(changed) > 0 {
		return out, newBackupError(verificationFailureExitCode,
			"the backup does not match the store in %d tables (%s), so nothing was rewritten: another process wrote to the store; quit every OpenMessage process and retry",
			len(changed), strings.Join(changed, ", "))
	}

	// New runs the app's own open path, as the daemon does at every start,
	// so the rewrite starts from the store the app will open.
	store, err := db.New(path)
	if err != nil {
		return out, newBackupError(rewriteFailureExitCode, "open the legacy store: %w", err)
	}
	defer store.Close()
	digestBefore, err := store.ContentDigest(ctx)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "digest the legacy store before the rewrite: %w", err)
	}
	started := deps.now()
	report, err := store.Compact(ctx)
	if err != nil {
		return out, newBackupError(rewriteFailureExitCode, "rewrite the legacy store: %w (the backup is at %s)", err, backupDB)
	}
	out.Applied = true
	out.RewriteSeconds = deps.now().Sub(started).Seconds()
	out.CheckpointBusy = report.Checkpoint.Busy
	digestAfter, err := store.ContentDigest(ctx)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "digest the rewritten store: %w (the backup is at %s)", err, backupDB)
	}
	if changed := db.ChangedTables(digestBefore, digestAfter); len(changed) > 0 {
		return out, newBackupError(verificationFailureExitCode,
			"rows changed during the rewrite in %d tables (%s); the backup is at %s", len(changed), strings.Join(changed, ", "), backupDB)
	}
	out.Tables, out.Rows = digestTotals(digestAfter)
	if err := store.Close(); err != nil {
		return out, newBackupError(rewriteFailureExitCode, "close the rewritten store: %w", err)
	}
	result, err := deps.quickCheck(path)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "run quick_check on the rewritten store: %w (the backup is at %s)", err, backupDB)
	}
	out.QuickCheck = result
	if result != "ok" {
		return out, newBackupError(verificationFailureExitCode, "the rewritten store failed quick_check: %s (the backup is at %s)", result, backupDB)
	}
	after, err := readCompactFiles(ctx, path)
	if err != nil {
		return out, newBackupError(verificationFailureExitCode, "read the rewritten store's pages: %w", err)
	}
	out.After = &after
	return out, nil
}

// readCompactFiles reads the page counts through a read-only connection,
// which never runs New's migrations, and the file sizes from the filesystem.
func readCompactFiles(ctx context.Context, path string) (compactFiles, error) {
	database, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return compactFiles{}, err
	}
	defer database.Close()
	stats, err := db.StorageStatsOf(ctx, database)
	if err != nil {
		return compactFiles{}, err
	}
	files := compactFiles{
		PageSize:   stats.PageSize,
		Pages:      stats.PageCount,
		FreePages:  stats.FreePages,
		FreeBytes:  stats.FreeBytes(),
		AutoVacuum: stats.AutoVacuum.String(),
	}
	if files.FileBytes, err = fileSizeOrZero(path); err != nil {
		return compactFiles{}, err
	}
	if files.WALBytes, err = fileSizeOrZero(path + "-wal"); err != nil {
		return compactFiles{}, err
	}
	return files, nil
}

func readOnlyContentDigest(ctx context.Context, path string) (map[string]db.TableDigest, error) {
	database, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return db.ContentDigestOf(ctx, database)
}

func openReadOnlySQLite(ctx context.Context, path string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func fileSizeOrZero(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func digestTotals(digest map[string]db.TableDigest) (tables int, rows int64) {
	for _, table := range digest {
		tables++
		rows += table.Rows
	}
	return tables, rows
}

type spaceNeed struct {
	dir   string
	bytes uint64
	what  string
}

// compactSpaceNeeds says where --apply writes and about how much. VACUUM INTO
// writes a copy of the pages in use to the backup; VACUUM builds another copy
// in SQLite's temporary directory and then writes the rewritten pages
// through the WAL beside the store. Each gets a margin of a tenth plus 64 MiB.
func compactSpaceNeeds(dataDir, backupDir, tempDir string, liveBytes uint64) []spaceNeed {
	need := liveBytes + liveBytes/10 + 64<<20
	return []spaceNeed{
		{dir: backupDir, bytes: need, what: "the backup"},
		{dir: tempDir, bytes: need, what: "VACUUM's temporary copy"},
		{dir: dataDir, bytes: need, what: "the WAL during the rewrite"},
	}
}

// checkCompactSpace adds up the needs that share a filesystem and checks each
// filesystem's free space. It returns the total need.
func checkCompactSpace(needs []spaceNeed, availableBytes func(string) (uint64, error)) (uint64, error) {
	type filesystem struct {
		dir   string
		bytes uint64
		what  []string
	}
	var order []*filesystem
	byDevice := make(map[uint64]*filesystem)
	var total uint64
	for _, need := range needs {
		dir, err := nearestExistingDirectory(need.dir)
		if err != nil {
			return 0, fmt.Errorf("find the filesystem for %s: %w", need.what, err)
		}
		info, err := os.Stat(dir)
		if err != nil {
			return 0, fmt.Errorf("find the filesystem for %s: %w", need.what, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("find the filesystem for %s: no device for %s", need.what, dir)
		}
		fs := byDevice[uint64(stat.Dev)]
		if fs == nil {
			fs = &filesystem{dir: dir}
			byDevice[uint64(stat.Dev)] = fs
			order = append(order, fs)
		}
		fs.bytes += need.bytes
		fs.what = append(fs.what, need.what)
		total += need.bytes
	}
	for _, fs := range order {
		available, err := availableBytes(fs.dir)
		if err != nil {
			return total, fmt.Errorf("check free space at %s: %w", fs.dir, err)
		}
		if available < fs.bytes {
			verb := "needs"
			if len(fs.what) > 1 {
				verb = "need"
			}
			return total, fmt.Errorf("insufficient free space at %s: %s %s about %s, but only %s is available",
				fs.dir, strings.Join(fs.what, " and "), verb, formatByteCount(fs.bytes), formatByteCount(available))
		}
	}
	return total, nil
}

func writeCompactOutput(stdout io.Writer, asJSON bool, out compactOutput, commandErr error) error {
	out.OK = commandErr == nil
	out.ExitCode = ExitCode(commandErr)
	if commandErr != nil {
		out.Error = commandErr.Error()
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(out); err != nil && commandErr == nil {
			return newBackupError(backupFailureExitCode, "write JSON output: %w", err)
		}
		return commandErr
	}
	writeCompactSummary(stdout, out)
	return commandErr
}

func writeCompactSummary(w io.Writer, out compactOutput) {
	if out.Before == nil {
		return
	}
	before := out.Before
	fmt.Fprintf(w, "Legacy store: %s\n", out.DatabasePath)
	switch {
	case out.NothingToDo:
		fmt.Fprintf(w, "  main file    %s, no free pages, auto_vacuum %s\n", formatByteCount(uint64(before.FileBytes)), before.AutoVacuum)
		fmt.Fprintf(w, "  WAL          %s\n", formatByteCount(uint64(before.WALBytes)))
		fmt.Fprintln(w, "Nothing to compact: the daemon returns the pages later deletes free.")
	case out.After != nil:
		after := out.After
		fmt.Fprintf(w, "Backup: %s (quick_check ok; it holds the same rows as the store)\n", out.BackupPath)
		fmt.Fprintf(w, "Rewrote the store in %.1f s:\n", out.RewriteSeconds)
		fmt.Fprintf(w, "  main file    %s -> %s\n", formatByteCount(uint64(before.FileBytes)), formatByteCount(uint64(after.FileBytes)))
		fmt.Fprintf(w, "  free pages   %s -> %s\n", formatByteCount(uint64(before.FreeBytes)), formatByteCount(uint64(after.FreeBytes)))
		fmt.Fprintf(w, "  WAL          %s -> %s\n", formatByteCount(uint64(before.WALBytes)), formatByteCount(uint64(after.WALBytes)))
		fmt.Fprintf(w, "  auto_vacuum  %s -> %s\n", before.AutoVacuum, after.AutoVacuum)
		fmt.Fprintf(w, "All %d tables kept their %d rows, and quick_check reports %s.\n", out.Tables, out.Rows, out.QuickCheck)
		if out.CheckpointBusy {
			fmt.Fprintln(w, "Another process was reading, so part of the rewrite is still in the WAL; the next checkpoint shrinks the file.")
		}
	case out.Applied:
		fmt.Fprintf(w, "The rewrite ran, but its checks did not pass; the backup is at %s.\n", out.BackupPath)
	case out.BackupPath != "":
		fmt.Fprintf(w, "Nothing was rewritten; the backup is at %s.\n", out.BackupPath)
	case out.Error == "":
		fmt.Fprintf(w, "  main file    %s (%d pages of %d bytes)\n", formatByteCount(uint64(before.FileBytes)), before.Pages, before.PageSize)
		fmt.Fprintf(w, "  free pages   %s (%d pages)\n", formatByteCount(uint64(before.FreeBytes)), before.FreePages)
		fmt.Fprintf(w, "  WAL          %s\n", formatByteCount(uint64(before.WALBytes)))
		fmt.Fprintf(w, "  auto_vacuum  %s\n", before.AutoVacuum)
		fmt.Fprintf(w, "--apply would rewrite it to about %s, switch it to incremental auto-vacuum and empty the WAL.\n", formatByteCount(before.liveBytes()))
		fmt.Fprintf(w, "It needs about %s of free disk for the backup, VACUUM's temporary copy and the WAL. Quit the app first.\n", formatByteCount(out.RequiredFreeBytes))
		if out.SpaceProblem != "" {
			fmt.Fprintf(w, "--apply would refuse now: %s\n", out.SpaceProblem)
		}
	}
}
