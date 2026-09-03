package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"

	_ "modernc.org/sqlite"
)

const (
	backupManifestFormat = 1
	instanceLockName     = "instance.lock"
	legacyDatabaseName   = "messages.db"
	manifestName         = "manifest.json"

	refusedRunningExitCode      = 2
	preflightExitCode           = 3
	backupFailureExitCode       = 4
	verificationFailureExitCode = 5
)

func defaultBackendProbeURL() string {
	host := strings.TrimSpace(os.Getenv("OPENMESSAGES_HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	port := strings.TrimSpace(os.Getenv("OPENMESSAGES_PORT"))
	if port == "" {
		port = "7007"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/status"
}

var errInstanceLockHeld = errors.New("instance lock is held")

// backupError carries the stable operator-facing exit status required by the
// migration runbook. Other commands retain the historical exit status 1.
type backupError struct {
	code int
	err  error
}

func (e *backupError) Error() string { return e.err.Error() }
func (e *backupError) Unwrap() error { return e.err }
func (e *backupError) ExitCode() int { return e.code }

func newBackupError(code int, format string, args ...any) error {
	return &backupError{code: code, err: fmt.Errorf(format, args...)}
}

// ExitCode returns the stable status carried by an error, or 1 for ordinary
// command failures. A nil error represents success.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}

type backupOptions struct {
	dataDir     string
	destination string
}

type backupDependencies struct {
	now            func() time.Time
	version        string
	commit         string
	modified       bool
	probeURL       string
	httpClient     *http.Client
	availableBytes func(string) (uint64, error)
	quickCheck     func(string) (string, error)
	beforeManifest func() error
}

type backupResult struct {
	BackupPath   string
	ManifestPath string
	Manifest     backupManifest
}

type backupManifest struct {
	Format         int                  `json:"format"`
	CreatedAt      string               `json:"created_at"`
	AppVersion     string               `json:"app_version"`
	Tool           backupTool           `json:"tool"`
	Source         backupSource         `json:"source"`
	Target         backupTarget         `json:"target"`
	Files          []backupFile         `json:"files"`
	Counts         legacyCounts         `json:"counts"`
	AuxiliaryState backupAuxiliaryState `json:"auxiliary_state"`
	Warnings       []string             `json:"warnings"`
}

type backupTool struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Modified bool   `json:"modified,omitempty"`
}

type backupSource struct {
	Path              string `json:"path"`
	DatabasePath      string `json:"database_path"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	SchemaUserVersion int64  `json:"schema_user_version"`
	QuickCheck        string `json:"quick_check"`
}

type backupTarget struct {
	Path         string `json:"path"`
	DatabasePath string `json:"database_path"`
}

type backupFile struct {
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Mode       string `json:"mode"`
}

type legacyCounts struct {
	Conversations             int64            `json:"conversations"`
	Messages                  int64            `json:"messages"`
	Contacts                  int64            `json:"contacts"`
	UnifiedContacts           int64            `json:"unified_contacts"`
	Drafts                    int64            `json:"drafts"`
	ScheduledMessages         int64            `json:"scheduled_messages"`
	ScheduledMessagesByStatus map[string]int64 `json:"scheduled_messages_by_status"`
	ContactMeta               int64            `json:"contact_meta"`
}

type backupAuxiliaryState struct {
	Included   []string `json:"included"`
	NotPresent []string `json:"not_present"`
}

type backupCommandOutput struct {
	OK           bool          `json:"ok"`
	ExitCode     int           `json:"exit_code"`
	BackupPath   string        `json:"backup_path,omitempty"`
	ManifestPath string        `json:"manifest_path,omitempty"`
	Counts       *legacyCounts `json:"counts,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type auxiliaryFile struct {
	sourcePath   string
	relativePath string
	isSQLite     bool
}

type auxiliaryPlan struct {
	files          []auxiliaryFile
	included       []string
	notPresent     []string
	signalDirFound bool
}

// RunBackup handles "openmessage backup [--to <directory>] [--json]".
//
// It deliberately does not construct app.App: app.New opens and repairs the
// live database, whereas a backup must leave its source untouched.
func RunBackup(_ zerolog.Logger, args ...string) error {
	return runBackupCommand(context.Background(), args, defaultBackupDependencies(), os.Stdout, os.Stderr)
}

func runBackupCommand(ctx context.Context, args []string, deps backupDependencies, stdout, stderr io.Writer) error {
	asJSON := hasFlag(args, "--json")
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: openmessage backup [--to <directory>] [--json]")
		fmt.Fprintln(stderr, "  --to defaults to <data-dir>/migration-backups/<UTC-stamp>")
	}
	var destination string
	fs.StringVar(&destination, "to", "", "backup destination directory")
	fs.BoolVar(&asJSON, "json", asJSON, "write one machine-readable JSON object")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		err = newBackupError(preflightExitCode, "parse backup options: %w", err)
		return writeBackupCommandError(stdout, asJSON, err)
	}
	if fs.NArg() != 0 {
		err := newBackupError(preflightExitCode, "unexpected backup argument %q; use --to <directory>", fs.Arg(0))
		return writeBackupCommandError(stdout, asJSON, err)
	}

	result, err := executeBackup(ctx, backupOptions{
		dataDir:     app.DefaultDataDir(),
		destination: destination,
	}, deps)
	if err != nil {
		return writeBackupCommandError(stdout, asJSON, err)
	}

	if asJSON {
		output := backupCommandOutput{
			OK:           true,
			ExitCode:     0,
			BackupPath:   result.BackupPath,
			ManifestPath: result.ManifestPath,
			Counts:       &result.Manifest.Counts,
		}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			return newBackupError(backupFailureExitCode, "write JSON output: %w", err)
		}
		return nil
	}

	fmt.Fprintf(stdout, "Backup complete: %s\n", result.BackupPath)
	fmt.Fprintf(stdout, "Manifest: %s\n", result.ManifestPath)
	fmt.Fprintln(stdout, "The legacy source was not modified. Keep this directory for rollback.")
	return nil
}

func writeBackupCommandError(stdout io.Writer, asJSON bool, commandErr error) error {
	if !asJSON {
		return commandErr
	}
	output := backupCommandOutput{
		OK:       false,
		ExitCode: ExitCode(commandErr),
		Error:    commandErr.Error(),
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return newBackupError(backupFailureExitCode, "write JSON error output: %w", err)
	}
	return commandErr
}

func defaultBackupDependencies() backupDependencies {
	commit, modified := buildRevision()
	return backupDependencies{
		now:      time.Now,
		version:  Version(),
		commit:   commit,
		modified: modified,
		probeURL: defaultBackendProbeURL(),
		httpClient: &http.Client{
			Timeout: 750 * time.Millisecond,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		availableBytes: filesystemAvailableBytes,
		quickCheck:     sqliteQuickCheck,
	}
}

func buildRevision() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false
	}
	commit := "unknown"
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if setting.Value != "" {
				commit = setting.Value
			}
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return commit, modified
}

func executeBackup(ctx context.Context, opts backupOptions, deps backupDependencies) (backupResult, error) {
	if deps.now == nil || deps.availableBytes == nil || deps.quickCheck == nil || deps.httpClient == nil {
		return backupResult{}, newBackupError(preflightExitCode, "backup dependencies are incomplete")
	}

	createdAt := deps.now().UTC().Truncate(time.Second)
	dataDir, err := canonicalExistingDirectory(opts.dataDir)
	if err != nil {
		return backupResult{}, newBackupError(preflightExitCode, "resolve OpenMessage data directory %q: %w", opts.dataDir, err)
	}
	sourceDB := filepath.Join(dataDir, legacyDatabaseName)
	sourceInfo, err := os.Stat(sourceDB)
	if err != nil {
		if os.IsNotExist(err) {
			return backupResult{}, newBackupError(preflightExitCode, "legacy database not found at %s", sourceDB)
		}
		return backupResult{}, newBackupError(preflightExitCode, "inspect legacy database %s: %w", sourceDB, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return backupResult{}, newBackupError(preflightExitCode, "legacy database path is not a regular file: %s", sourceDB)
	}

	lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{
		PID:           os.Getpid(),
		Process:       "openmessage backup",
		StartedAt:     createdAt.Format(time.RFC3339),
		BuildID:       deps.version,
		Commit:        deps.commit,
		CanonicalPath: dataDir,
		ProbeURL:      deps.probeURL,
	})
	if err != nil {
		if errors.Is(err, errInstanceLockHeld) {
			return backupResult{}, newBackupError(refusedRunningExitCode, "OpenMessage state is in use: %s is locked; stop the backend and retry", filepath.Join(dataDir, instanceLockName))
		}
		return backupResult{}, newBackupError(preflightExitCode, "acquire OpenMessage instance lock: %w", err)
	}
	defer lock.Close()

	running, probeErr := probeBackend(ctx, deps.httpClient, deps.probeURL)
	if probeErr != nil {
		return backupResult{}, newBackupError(refusedRunningExitCode, "cannot safely rule out a running OpenMessage backend at %s: %w; stop the backend and retry", deps.probeURL, probeErr)
	}
	if running {
		return backupResult{}, newBackupError(refusedRunningExitCode, "OpenMessage backend is running at %s; stop it before creating a migration backup", deps.probeURL)
	}

	destination, err := resolveBackupDestination(dataDir, opts.destination, createdAt)
	if err != nil {
		return backupResult{}, newBackupError(preflightExitCode, "resolve backup destination: %w", err)
	}
	if pathWithin(destination, filepath.Join(dataDir, "signal-cli")) {
		return backupResult{}, newBackupError(preflightExitCode, "backup destination %s cannot be inside the signal-cli source directory", destination)
	}
	if _, err := os.Lstat(destination); err == nil {
		return backupResult{}, newBackupError(preflightExitCode, "backup destination already exists: %s (choose a new --to directory; existing backups are never overwritten)", destination)
	} else if !os.IsNotExist(err) {
		return backupResult{}, newBackupError(preflightExitCode, "inspect backup destination %s: %w", destination, err)
	}

	auxiliary, err := discoverAuxiliaryState(dataDir)
	if err != nil {
		return backupResult{}, newBackupError(preflightExitCode, "inspect auxiliary state: %w", err)
	}

	spacePath, err := nearestExistingDirectory(filepath.Dir(destination))
	if err != nil {
		return backupResult{}, newBackupError(preflightExitCode, "resolve destination filesystem for %s: %w", destination, err)
	}
	available, err := deps.availableBytes(spacePath)
	if err != nil {
		return backupResult{}, newBackupError(preflightExitCode, "check free space at %s: %w", spacePath, err)
	}
	if sourceInfo.Size() < 0 || uint64(sourceInfo.Size()) > math.MaxUint64/2 {
		return backupResult{}, newBackupError(preflightExitCode, "legacy database size cannot be represented safely: %d", sourceInfo.Size())
	}
	required := uint64(sourceInfo.Size()) * 2
	if available < required {
		return backupResult{}, newBackupError(preflightExitCode,
			"insufficient free space at %s: backup requires approximately %s (2x the %s legacy database), but only %s is available",
			spacePath, formatByteCount(required), formatByteCount(uint64(sourceInfo.Size())), formatByteCount(available))
	}

	if err := os.MkdirAll(destination, 0o700); err != nil {
		return backupResult{}, newBackupError(backupFailureExitCode, "create backup directory %s: %w", destination, err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return backupResult{}, newBackupError(backupFailureExitCode, "secure backup directory %s: %w", destination, err)
	}

	backupDB := filepath.Join(destination, legacyDatabaseName)
	if err := vacuumInto(ctx, sourceDB, backupDB); err != nil {
		return backupResult{}, newBackupError(backupFailureExitCode, "back up legacy database with SQLite VACUUM INTO: %w", err)
	}
	if err := os.Chmod(backupDB, 0o600); err != nil {
		return backupResult{}, newBackupError(backupFailureExitCode, "secure copied legacy database: %w", err)
	}

	quickCheck, err := deps.quickCheck(backupDB)
	if err != nil {
		return backupResult{}, newBackupError(verificationFailureExitCode, "run PRAGMA quick_check on copied database: %w", err)
	}
	if quickCheck != "ok" {
		return backupResult{}, newBackupError(verificationFailureExitCode, "copied database failed PRAGMA quick_check: %s", quickCheck)
	}

	fingerprint, userVersion, counts, err := inspectLegacySnapshot(backupDB)
	if err != nil {
		return backupResult{}, newBackupError(verificationFailureExitCode, "inspect copied legacy database: %w", err)
	}

	files := make([]backupFile, 0, len(auxiliary.files)+1)
	dbFile, err := describeBackupFile(backupDB, sourceDB, legacyDatabaseName)
	if err != nil {
		return backupResult{}, newBackupError(verificationFailureExitCode, "hash copied legacy database: %w", err)
	}
	files = append(files, dbFile)

	if auxiliary.signalDirFound {
		if err := os.MkdirAll(filepath.Join(destination, "signal-cli"), 0o700); err != nil {
			return backupResult{}, newBackupError(backupFailureExitCode, "create Signal backup directory: %w", err)
		}
	}
	for _, source := range auxiliary.files {
		destinationPath := filepath.Join(destination, filepath.FromSlash(source.relativePath))
		if source.isSQLite {
			// SQLite databases must be snapshotted transactionally: a plain file
			// copy silently drops hot WAL/journal content (a crashed daemon can
			// hold the ENTIRE database in the WAL), producing a valid-looking but
			// empty or mid-transaction backup that quick_check cannot flag.
			if err := vacuumInto(ctx, source.sourcePath, destinationPath); err != nil {
				return backupResult{}, newBackupError(backupFailureExitCode, "snapshot auxiliary database %s: %w", source.sourcePath, err)
			}
			if result, err := sqliteQuickCheck(destinationPath); err != nil {
				return backupResult{}, newBackupError(verificationFailureExitCode, "verify auxiliary database %s: %w", source.relativePath, err)
			} else if result != "ok" {
				return backupResult{}, newBackupError(verificationFailureExitCode, "auxiliary database %s quick_check: %s", source.relativePath, result)
			}
			if err := os.Chmod(destinationPath, 0o600); err != nil {
				return backupResult{}, newBackupError(backupFailureExitCode, "harden auxiliary database %s: %w", source.relativePath, err)
			}
		} else if err := copyPlainFile(source.sourcePath, destinationPath); err != nil {
			return backupResult{}, newBackupError(backupFailureExitCode, "copy auxiliary state %s: %w", source.sourcePath, err)
		}
		file, err := describeBackupFile(destinationPath, source.sourcePath, source.relativePath)
		if err != nil {
			return backupResult{}, newBackupError(verificationFailureExitCode, "hash copied auxiliary state %s: %w", source.relativePath, err)
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	manifest := backupManifest{
		Format:     backupManifestFormat,
		CreatedAt:  createdAt.Format(time.RFC3339),
		AppVersion: deps.version,
		Tool: backupTool{
			Version: deps.version, Commit: deps.commit, Modified: deps.modified,
		},
		Source: backupSource{
			Path: dataDir, DatabasePath: sourceDB, SchemaFingerprint: fingerprint,
			SchemaUserVersion: userVersion, QuickCheck: quickCheck,
		},
		Target: backupTarget{
			Path: destination, DatabasePath: backupDB,
		},
		Files:  files,
		Counts: counts,
		AuxiliaryState: backupAuxiliaryState{
			Included: auxiliary.included, NotPresent: auxiliary.notPresent,
		},
		Warnings: []string{},
	}

	if deps.beforeManifest != nil {
		if err := deps.beforeManifest(); err != nil {
			return backupResult{}, newBackupError(backupFailureExitCode, "backup interrupted before manifest completion: %w", err)
		}
	}
	manifestPath := filepath.Join(destination, manifestName)
	if err := writeManifestAtomically(manifestPath, manifest); err != nil {
		return backupResult{}, newBackupError(backupFailureExitCode, "write final backup manifest: %w", err)
	}

	return backupResult{
		BackupPath: destination, ManifestPath: manifestPath, Manifest: manifest,
	}, nil
}

func canonicalExistingDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return filepath.Clean(realPath), nil
}

func resolveBackupDestination(dataDir, override string, createdAt time.Time) (string, error) {
	path := override
	if path == "" {
		path = filepath.Join(dataDir, "migration-backups", createdAt.Format("20060102T150405Z"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	existing, err := nearestExistingDirectory(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	remainder, err := filepath.Rel(existing, abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(realExisting, remainder)), nil
}

func nearestExistingDirectory(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", current)
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent directory for %s", path)
		}
		current = parent
	}
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func probeBackend(ctx context.Context, client *http.Client, probeURL string) (bool, error) {
	if strings.TrimSpace(probeURL) == "" {
		return false, fmt.Errorf("backend probe URL is empty")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return false, nil
		}
		return false, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	// Any HTTP response is conservative evidence that something owns the
	// configured OpenMessage endpoint, even if it is unhealthy or unauthorized.
	return true, nil
}

// instanceLockRecord is diagnostic only. The advisory OS lock held on the file
// descriptor is authoritative; stale JSON after a crash never blocks a run.
type instanceLockRecord struct {
	PID           int    `json:"pid"`
	Process       string `json:"process"`
	StartedAt     string `json:"started_at,omitempty"`
	BuildID       string `json:"build_id,omitempty"`
	Commit        string `json:"commit,omitempty"`
	CanonicalPath string `json:"canonical_path,omitempty"`
	ProbeURL      string `json:"probe_url,omitempty"`
}

type instanceLock struct {
	file *os.File
}

func acquireInstanceLock(path string, record instanceLockRecord) (*instanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeWithError := func(lockErr error) (*instanceLock, error) {
		_ = file.Close()
		return nil, lockErr
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return closeWithError(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeWithError(fmt.Errorf("%w: %s", errInstanceLockHeld, path))
		}
		return closeWithError(err)
	}

	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWithError(err)
	}
	payload = append(payload, '\n')
	if err := file.Truncate(0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWithError(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWithError(err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWithError(err)
	}
	if err := file.Sync(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return closeWithError(err)
	}
	return &instanceLock{file: file}, nil
}

func (l *instanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}

func filesystemAvailableBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	blockSize := uint64(stat.Bsize)
	availableBlocks := uint64(stat.Bavail)
	if blockSize == 0 || availableBlocks > math.MaxUint64/blockSize {
		return 0, fmt.Errorf("filesystem free-space value overflows uint64")
	}
	return availableBlocks * blockSize, nil
}

func vacuumInto(ctx context.Context, sourcePath, destinationPath string) error {
	database, err := sql.Open("sqlite", sqliteReadOnlyDSN(sourcePath))
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, "VACUUM INTO ?", destinationPath); err != nil {
		return err
	}
	return database.Close()
}

func sqliteReadOnlyDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}).String()
}

func sqliteQuickCheck(path string) (string, error) {
	database, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return "", err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	rows, err := database.Query("PRAGMA quick_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return "", err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", fmt.Errorf("PRAGMA quick_check returned no rows")
	}
	return strings.Join(results, "; "), nil
}

func inspectLegacySnapshot(path string) (string, int64, legacyCounts, error) {
	database, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return "", 0, legacyCounts{}, err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	fingerprint, err := schemaFingerprint(database)
	if err != nil {
		return "", 0, legacyCounts{}, err
	}
	var userVersion int64
	if err := database.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return "", 0, legacyCounts{}, err
	}

	counts := legacyCounts{ScheduledMessagesByStatus: map[string]int64{
		"pending": 0, "sending": 0, "sent": 0, "failed": 0, "canceled": 0,
	}}
	queries := []struct {
		table string
		value *int64
	}{
		{"conversations", &counts.Conversations},
		{"messages", &counts.Messages},
		{"contacts", &counts.Contacts},
		{"unified_contacts", &counts.UnifiedContacts},
		{"drafts", &counts.Drafts},
		{"scheduled_messages", &counts.ScheduledMessages},
		{"contact_meta", &counts.ContactMeta},
	}
	for _, query := range queries {
		if err := database.QueryRow("SELECT COUNT(*) FROM " + query.table).Scan(query.value); err != nil {
			return "", 0, legacyCounts{}, fmt.Errorf("count %s: %w", query.table, err)
		}
	}
	rows, err := database.Query("SELECT status, COUNT(*) FROM scheduled_messages GROUP BY status ORDER BY status")
	if err != nil {
		return "", 0, legacyCounts{}, fmt.Errorf("count scheduled messages by status: %w", err)
	}
	var statusTotal int64
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return "", 0, legacyCounts{}, err
		}
		counts.ScheduledMessagesByStatus[status] = count
		statusTotal += count
	}
	if err := rows.Close(); err != nil {
		return "", 0, legacyCounts{}, err
	}
	if err := rows.Err(); err != nil {
		return "", 0, legacyCounts{}, err
	}
	if statusTotal != counts.ScheduledMessages {
		return "", 0, legacyCounts{}, fmt.Errorf("scheduled status counts sum to %d, want %d", statusTotal, counts.ScheduledMessages)
	}
	return fingerprint, userVersion, counts, nil
}

func schemaFingerprint(database *sql.DB) (string, error) {
	rows, err := database.Query(`
		SELECT type, name, tbl_name, IFNULL(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name, tbl_name
	`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return "", err
		}
		for _, field := range []string{kind, name, table, statement} {
			_, _ = io.WriteString(hash, field)
			_, _ = hash.Write([]byte{0x1f})
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func discoverAuxiliaryState(dataDir string) (auxiliaryPlan, error) {
	plan := auxiliaryPlan{included: []string{}, notPresent: []string{}}
	fileNames := []string{"session.json", "whatsapp-session.db", "openmessage.db"}
	for _, name := range fileNames {
		path := filepath.Join(dataDir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			plan.notPresent = append(plan.notPresent, name)
			continue
		}
		if err != nil {
			return auxiliaryPlan{}, err
		}
		if !info.Mode().IsRegular() {
			return auxiliaryPlan{}, fmt.Errorf("%s is not a regular file", path)
		}
		plan.included = append(plan.included, name)
		plan.files = append(plan.files, auxiliaryFile{
			sourcePath:   path,
			relativePath: name,
			isSQLite:     strings.HasSuffix(name, ".db"),
		})
	}

	signalPath := filepath.Join(dataDir, "signal-cli")
	info, err := os.Lstat(signalPath)
	if os.IsNotExist(err) {
		plan.notPresent = append(plan.notPresent, "signal-cli/")
	} else if err != nil {
		return auxiliaryPlan{}, err
	} else {
		if !info.IsDir() {
			return auxiliaryPlan{}, fmt.Errorf("%s is not a directory", signalPath)
		}
		plan.signalDirFound = true
		plan.included = append(plan.included, "signal-cli/")
		if err := filepath.WalkDir(signalPath, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == signalPath {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("signal-cli state contains unsupported symbolic link: %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("signal-cli state contains unsupported non-regular file: %s", path)
			}
			relative, err := filepath.Rel(dataDir, path)
			if err != nil {
				return err
			}
			plan.files = append(plan.files, auxiliaryFile{
				sourcePath: path, relativePath: filepath.ToSlash(relative),
			})
			return nil
		}); err != nil {
			return auxiliaryPlan{}, err
		}
	}

	sort.Slice(plan.files, func(i, j int) bool { return plan.files[i].relativePath < plan.files[j].relativePath })
	sort.Strings(plan.included)
	sort.Strings(plan.notPresent)
	return plan, nil
}

func copyPlainFile(sourcePath, destinationPath string) (returnErr error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := destination.Close(); returnErr == nil {
			returnErr = closeErr
		}
	}()
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return err
	}
	return nil
}

func describeBackupFile(path, sourcePath, relativePath string) (backupFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return backupFile{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return backupFile{}, copyErr
	}
	if closeErr != nil {
		return backupFile{}, closeErr
	}
	info, err := os.Stat(path)
	if err != nil {
		return backupFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return backupFile{}, fmt.Errorf("file changed while hashing")
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return backupFile{}, err
	}
	return backupFile{
		Path:       filepath.ToSlash(relativePath),
		SourcePath: filepath.Clean(sourceAbs),
		Size:       size,
		SHA256:     hex.EncodeToString(hash.Sum(nil)),
		Mode:       fmt.Sprintf("%04o", info.Mode().Perm()),
	}, nil
}

func writeManifestAtomically(path string, manifest backupManifest) (returnErr error) {
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary := filepath.Join(filepath.Dir(path), ".manifest.json.tmp")
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if closeErr := file.Close(); returnErr == nil && !renamed {
			returnErr = closeErr
		}
		if !renamed {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	renamed = true
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func formatByteCount(bytes uint64) string {
	const (
		kiB = uint64(1024)
		miB = 1024 * kiB
		giB = 1024 * miB
	)
	switch {
	case bytes >= giB:
		return fmt.Sprintf("%.2f GiB", float64(bytes)/float64(giB))
	case bytes >= miB:
		return fmt.Sprintf("%.2f MiB", float64(bytes)/float64(miB))
	case bytes >= kiB:
		return fmt.Sprintf("%.2f KiB", float64(bytes)/float64(kiB))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}
