package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/migration"
)

const (
	migrateSourceExitCode    = 3
	migrateLockExitCode      = 4
	migrateTransformExitCode = 5

	v2StoreName        = "store.sqlite3"
	v2BlobName         = "blobs"
	v2TempStoreName    = "store.sqlite3.tmp"
	v2TempBlobName     = "blobs.tmp"
	v2TempSourceSuffix = ".source"
	migrateSpaceCopies = uint64(2)
)

// migrateError carries the stable exit statuses pre-registered for the
// one-shot migration command. It is deliberately separate from backupError:
// backup was released first and has different meanings for exit codes 2-4.
type migrateError struct {
	code int
	err  error
}

func (e *migrateError) Error() string { return e.err.Error() }
func (e *migrateError) Unwrap() error { return e.err }
func (e *migrateError) ExitCode() int { return e.code }

func newMigrateError(code int, format string, args ...any) error {
	return &migrateError{code: code, err: fmt.Errorf(format, args...)}
}

type migrateOptions struct {
	sourceDir string
	targetDir string
	check     bool
}

type migrateTransform func(context.Context, migration.Options) (migration.Report, error)

type migrateDependencies struct {
	now            func() time.Time
	version        string
	commit         string
	probeURL       string
	httpClient     *http.Client
	availableBytes func(string) (uint64, error)
	transform      migrateTransform
}

type migrateResult struct {
	report    migration.Report
	hasReport bool
}

type migrateFailureOutput struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

// RunMigrate handles
// "openmessage migrate [--check] [--from legacy-dir] [--to v2-dir] [--json]".
//
// stdout is always exactly one JSON value. The --json flag is accepted for
// compatibility with the other operator commands, but migration evidence is
// machine-readable even without it. Human instructions and warnings go only
// to stderr.
func RunMigrate(_ zerolog.Logger, args ...string) error {
	return runMigrateCommand(
		context.Background(), args, defaultMigrateDependencies(), os.Stdout, os.Stderr,
	)
}

func runMigrateCommand(
	ctx context.Context,
	args []string,
	deps migrateDependencies,
	stdout io.Writer,
	stderr io.Writer,
) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: openmessage migrate [--check] [--from <legacy-dir>] [--to <v2-dir>] [--json]")
		fmt.Fprintln(stderr, "  --from defaults to the OpenMessage data directory")
		fmt.Fprintln(stderr, "  --to defaults to <from>/v2")
		fmt.Fprintln(stderr, "  --check validates a complete staged migration without publishing it")
		fmt.Fprintln(stderr, "  the integrity report is JSON on stdout; human guidance is on stderr")
	}

	options := migrateOptions{sourceDir: app.DefaultDataDir()}
	var explicitJSON bool
	fs.BoolVar(&options.check, "check", false, "validate without publishing")
	fs.StringVar(&options.sourceDir, "from", options.sourceDir, "legacy OpenMessage data directory")
	fs.StringVar(&options.targetDir, "to", "", "v2 target directory")
	fs.BoolVar(&explicitJSON, "json", false, "emit the integrity report as JSON (always enabled)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		commandErr := newMigrateError(migrateSourceExitCode, "parse migrate options: %v", err)
		return writeMigrateFailure(stdout, stderr, commandErr)
	}
	_ = explicitJSON
	if fs.NArg() != 0 {
		commandErr := newMigrateError(
			migrateSourceExitCode,
			"unexpected migrate argument %q; use --from and --to for paths",
			fs.Arg(0),
		)
		return writeMigrateFailure(stdout, stderr, commandErr)
	}

	result, commandErr := executeMigrate(ctx, options, deps)
	if result.hasReport {
		if err := json.NewEncoder(stdout).Encode(result.report); err != nil {
			return newMigrateError(migrateTransformExitCode, "write migration integrity report: %v", err)
		}
		writeHumanMigrationReport(stderr, result.report)
	} else if commandErr != nil {
		return writeMigrateFailure(stdout, stderr, commandErr)
	}
	if commandErr != nil {
		fmt.Fprintf(stderr, "Migration failed: %v\n", commandErr)
	}
	return commandErr
}

func writeMigrateFailure(stdout, stderr io.Writer, commandErr error) error {
	output := migrateFailureOutput{
		OK: false, ExitCode: ExitCode(commandErr), Error: commandErr.Error(),
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return newMigrateError(migrateTransformExitCode, "write migration error report: %v", err)
	}
	fmt.Fprintf(stderr, "Migration failed: %v\n", commandErr)
	return commandErr
}

func defaultMigrateDependencies() migrateDependencies {
	commit, _ := buildRevision()
	return migrateDependencies{
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
		transform:      migration.Transform,
	}
}

func executeMigrate(
	ctx context.Context,
	options migrateOptions,
	deps migrateDependencies,
) (result migrateResult, resultErr error) {
	if ctx == nil {
		return result, newMigrateError(migrateTransformExitCode, "migration context is nil")
	}
	if deps.now == nil || deps.httpClient == nil || deps.availableBytes == nil || deps.transform == nil {
		return result, newMigrateError(migrateTransformExitCode, "migration dependencies are incomplete")
	}

	sourceDir, err := canonicalExistingDirectory(options.sourceDir)
	if err != nil {
		return result, newMigrateError(
			migrateSourceExitCode, "resolve legacy data directory %q: %v", options.sourceDir, err,
		)
	}
	sourceStorePath := filepath.Join(sourceDir, legacyDatabaseName)
	sourceInfo, err := os.Stat(sourceStorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return result, newMigrateError(
				migrateSourceExitCode, "legacy database not found at %s", sourceStorePath,
			)
		}
		return result, newMigrateError(
			migrateSourceExitCode, "inspect legacy database %s: %v", sourceStorePath, err,
		)
	}
	if !sourceInfo.Mode().IsRegular() {
		return result, newMigrateError(
			migrateSourceExitCode, "legacy database path is not a regular file: %s", sourceStorePath,
		)
	}

	targetDir, err := resolveMigrationTarget(sourceDir, options.targetDir)
	if err != nil {
		return result, newMigrateError(migrateSourceExitCode, "resolve v2 target directory: %v", err)
	}
	if pathWithin(targetDir, filepath.Join(sourceDir, "signal-cli")) {
		return result, newMigrateError(
			migrateSourceExitCode,
			"v2 target %s cannot be inside the legacy signal-cli source directory",
			targetDir,
		)
	}

	createdAt := deps.now().UTC().Truncate(time.Second)
	lockPath := filepath.Join(sourceDir, instanceLockName)
	lock, err := acquireInstanceLock(lockPath, instanceLockRecord{
		PID:           os.Getpid(),
		Process:       "openmessage migrate",
		StartedAt:     createdAt.Format(time.RFC3339),
		BuildID:       deps.version,
		Commit:        deps.commit,
		CanonicalPath: sourceDir,
		ProbeURL:      deps.probeURL,
	})
	if err != nil {
		if errors.Is(err, errInstanceLockHeld) {
			return result, newMigrateError(
				migrateLockExitCode,
				"OpenMessage state is in use: %s is locked; stop the backend and retry",
				lockPath,
			)
		}
		return result, newMigrateError(
			migrateLockExitCode, "acquire OpenMessage instance lock %s: %v", lockPath, err,
		)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil && resultErr == nil {
			resultErr = newMigrateError(
				migrateTransformExitCode, "release OpenMessage instance lock: %v", closeErr,
			)
		}
	}()

	running, probeErr := probeBackend(ctx, deps.httpClient, deps.probeURL)
	if probeErr != nil {
		return result, newMigrateError(
			migrateLockExitCode,
			"cannot safely rule out a running OpenMessage backend at %s: %v; stop the backend and retry",
			deps.probeURL,
			probeErr,
		)
	}
	if running {
		return result, newMigrateError(
			migrateLockExitCode,
			"OpenMessage backend is running at %s; stop it before migrating",
			deps.probeURL,
		)
	}

	if err := refuseCanonicalV2State(targetDir); err != nil {
		return result, newMigrateError(migrateSourceExitCode, "%v", err)
	}
	tempStorePath := filepath.Join(targetDir, v2TempStoreName)
	tempBlobPath := filepath.Join(targetDir, v2TempBlobName)
	// Discard an interrupted prior stage before measuring headroom. Otherwise
	// a large stale blobs.tmp could make an otherwise resumable retry report a
	// false insufficient-space failure.
	if err := removeStagedMigration(tempStorePath, tempBlobPath); err != nil {
		return result, newMigrateError(
			migrateTransformExitCode, "remove stale migration staging: %v", err,
		)
	}
	spacePath, err := nearestExistingDirectory(targetDir)
	if err != nil {
		return result, newMigrateError(
			migrateSourceExitCode, "resolve target filesystem for %s: %v", targetDir, err,
		)
	}
	available, err := deps.availableBytes(spacePath)
	if err != nil {
		return result, newMigrateError(
			migrateSourceExitCode, "check free space at %s: %v", spacePath, err,
		)
	}
	sourceBytes, err := migrationSourceFootprint(sourceStorePath, sourceInfo)
	if err != nil {
		return result, newMigrateError(
			migrateSourceExitCode, "inspect legacy database family size: %v", err,
		)
	}
	if sourceBytes > math.MaxUint64/migrateSpaceCopies {
		return result, newMigrateError(
			migrateSourceExitCode,
			"legacy database family size cannot be represented safely: %d",
			sourceBytes,
		)
	}
	required := sourceBytes * migrateSpaceCopies
	if available < required {
		return result, newMigrateError(
			migrateSourceExitCode,
			"insufficient free space at %s: migration staging requires approximately %s (2x the %s legacy database family), but only %s is available",
			spacePath,
			formatByteCount(required),
			formatByteCount(sourceBytes),
			formatByteCount(available),
		)
	}

	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return result, newMigrateError(
			migrateSourceExitCode, "create v2 target directory %s: %v", targetDir, err,
		)
	}
	// Re-check after creation. This closes the small preflight race where a
	// canonical store could appear between the first Lstat and MkdirAll.
	if err := refuseCanonicalV2State(targetDir); err != nil {
		return result, newMigrateError(migrateSourceExitCode, "%v", err)
	}

	// Re-run after MkdirAll to keep Transform's require-absent contract true
	// even if the target was created through an unusual filesystem alias.
	if err := removeStagedMigration(tempStorePath, tempBlobPath); err != nil {
		return result, newMigrateError(
			migrateTransformExitCode, "remove stale migration staging: %v", err,
		)
	}
	defer func() {
		if cleanupErr := removeStagedMigration(tempStorePath, tempBlobPath); cleanupErr != nil && resultErr == nil {
			resultErr = newMigrateError(
				migrateTransformExitCode, "clean migration staging: %v", cleanupErr,
			)
		}
	}()

	targetStorePath := filepath.Join(targetDir, v2StoreName)
	report, transformErr := deps.transform(ctx, migration.Options{
		SourcePath:      sourceStorePath,
		TempStorePath:   tempStorePath,
		TempBlobPath:    tempBlobPath,
		TargetPath:      targetDir,
		TargetStorePath: targetStorePath,
		Check:           options.check,
	})
	result = migrateResult{report: report, hasReport: true}
	result.report.Check = options.check
	result.report.Target.Path = targetDir
	result.report.Target.DatabasePath = targetStorePath
	result.report.Target.Published = false
	if transformErr != nil {
		result.report.OK = false
		code := migrateTransformExitCode
		if errors.Is(transformErr, migration.ErrSource) {
			code = migrateSourceExitCode
		}
		return result, newMigrateError(code, "transform legacy store: %v", transformErr)
	}
	if !report.OK || !report.Validation.Passed {
		result.report.OK = false
		return result, newMigrateError(
			migrateTransformExitCode,
			"transform returned without passing every validation gate",
		)
	}

	if options.check {
		return result, nil
	}
	if err := publishStagedMigration(targetDir, tempStorePath, tempBlobPath); err != nil {
		result.report.OK = false
		return result, newMigrateError(migrateTransformExitCode, "publish migrated v2 store: %v", err)
	}
	result.report.Target.Published = true
	return result, nil
}

func migrationSourceFootprint(databasePath string, databaseInfo os.FileInfo) (uint64, error) {
	if databaseInfo == nil || databaseInfo.Size() < 0 {
		return 0, fmt.Errorf("legacy database has an invalid size")
	}
	total := uint64(databaseInfo.Size())
	walPath := databasePath + "-wal"
	walInfo, err := os.Stat(walPath)
	if os.IsNotExist(err) {
		return total, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect WAL %s: %w", walPath, err)
	}
	if !walInfo.Mode().IsRegular() || walInfo.Size() < 0 {
		return 0, fmt.Errorf("legacy WAL is not a regular file with a valid size: %s", walPath)
	}
	walBytes := uint64(walInfo.Size())
	if total > math.MaxUint64-walBytes {
		return 0, fmt.Errorf("legacy database family size overflows")
	}
	return total + walBytes, nil
}

func resolveMigrationTarget(sourceDir, override string) (string, error) {
	target := override
	if target == "" {
		target = filepath.Join(sourceDir, "v2")
	} else if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("target path is empty")
	}

	// Existing directories need their final symlink component resolved too.
	// New directories use R1's destination resolver so their deepest existing
	// ancestor is canonical and an alias cannot silently select another store.
	info, err := os.Stat(target)
	switch {
	case err == nil:
		if !info.IsDir() {
			return "", fmt.Errorf("target exists and is not a directory: %s", target)
		}
		return canonicalExistingDirectory(target)
	case !os.IsNotExist(err):
		return "", err
	default:
		return resolveBackupDestination(sourceDir, target, time.Time{})
	}
}

func refuseCanonicalV2State(targetDir string) error {
	for _, name := range []string{
		v2StoreName, v2StoreName + "-wal", v2StoreName + "-shm", v2BlobName,
	} {
		path := filepath.Join(targetDir, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf(
				"v2 target already contains canonical state at %s; existing stores and blobs are never overwritten",
				path,
			)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect canonical v2 path %s: %w", path, err)
		}
	}
	return nil
}

func removeStagedMigration(tempStorePath, tempBlobPath string) error {
	var errs []error
	for _, path := range []string{
		tempStorePath,
		tempStorePath + "-wal",
		tempStorePath + "-shm",
		tempStorePath + v2TempSourceSuffix,
		tempBlobPath,
	} {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func publishStagedMigration(targetDir, tempStorePath, tempBlobPath string) (returnErr error) {
	storePath := filepath.Join(targetDir, v2StoreName)
	blobPath := filepath.Join(targetDir, v2BlobName)
	if err := requireStagedRegularFile(tempStorePath); err != nil {
		return err
	}
	if err := requireStagedDirectory(tempBlobPath); err != nil {
		return err
	}
	if err := refuseCanonicalV2State(targetDir); err != nil {
		return err
	}

	blobsPublished := false
	storePublished := false
	defer func() {
		if returnErr == nil {
			return
		}
		// A failed publication must not leave a target that looks cut over. Keep
		// the database as the commit marker and best-effort roll both paths back.
		var rollbackErrs []error
		if storePublished {
			if err := os.Remove(storePath); err != nil && !os.IsNotExist(err) {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove published store: %w", err))
			}
		}
		if blobsPublished {
			if err := os.RemoveAll(blobPath); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("remove published blobs: %w", err))
			}
		}
		if err := syncMigrationDirectory(targetDir); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("sync rollback: %w", err))
		}
		returnErr = errors.Join(returnErr, errors.Join(rollbackErrs...))
	}()

	// Blob references in the database cannot become visible before their
	// files. The store rename is the publication commit point.
	if err := os.Rename(tempBlobPath, blobPath); err != nil {
		return fmt.Errorf("publish blobs first: %w", err)
	}
	blobsPublished = true
	if err := syncMigrationDirectory(targetDir); err != nil {
		return fmt.Errorf("sync target after publishing blobs: %w", err)
	}
	if err := os.Rename(tempStorePath, storePath); err != nil {
		return fmt.Errorf("publish store database: %w", err)
	}
	storePublished = true
	if err := syncMigrationDirectory(targetDir); err != nil {
		return fmt.Errorf("sync target after publishing store: %w", err)
	}
	return nil
}

func requireStagedRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect staged store %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged store is not a regular file: %s", path)
	}
	return nil
}

func requireStagedDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect staged blob store %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("staged blob store is not a directory: %s", path)
	}
	return nil
}

func syncMigrationDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func writeHumanMigrationReport(writer io.Writer, report migration.Report) {
	mode := "publish"
	if report.Check {
		mode = "check"
	}
	status := "FAILED"
	if report.OK && report.Validation.Passed {
		status = "PASSED"
	}
	fmt.Fprintf(writer, "OpenMessage migration %s: %s\n", mode, status)
	fmt.Fprintf(writer, "Source: %s (unchanged=%t, quick_check=%s)\n",
		report.Source.DatabasePath, report.Source.Unchanged, report.Source.QuickCheck)
	fmt.Fprintf(writer, "Target: %s (schema=%d, published=%t)\n",
		report.Target.DatabasePath, report.Target.SchemaVersion, report.Target.Published)

	if messages, ok := report.TableCounts["messages"]; ok {
		fmt.Fprintf(writer, "Messages: legacy=%d v2=%d reconciled=%d ratio=%.6f collisions=%d\n",
			messages.Legacy, messages.V2, messages.Reconciled,
			messages.ReconciliationRatio, len(report.MessageCollisions))
	}
	platforms := make([]string, 0, len(report.PlatformCounts))
	for platform := range report.PlatformCounts {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		counts := report.PlatformCounts[platform]
		fmt.Fprintf(writer, "  %s (%s): legacy=%d v2=%d ratio=%.6f\n",
			platform, counts.AccountID, counts.Legacy, counts.V2, counts.ReconciliationRatio)
	}
	fmt.Fprintf(writer, "Identity graph: identities=%d people=%d un-unified=%d\n",
		report.Identities.Created, report.Identities.PeopleCreated,
		report.Identities.UnunifiedIdentities)
	fmt.Fprintf(writer, "Scheduled: pending->outbox=%d pending-media=%d sending-ambiguous=%d sent/canceled/failed-skipped=%d/%d/%d\n",
		report.Schedule.PendingToOutbox, report.Schedule.PendingMedia,
		report.Schedule.AmbiguousSending, report.Schedule.SkippedSent,
		report.Schedule.SkippedCanceled, report.Schedule.SkippedFailed)
	fmt.Fprintf(writer, "Reactions: messages=%d seeded_messages=%d seeded_rows=%d malformed=%d unmappable=%d\n",
		report.Reactions.MessagesWithReactions, report.Reactions.MessagesSeeded,
		report.Reactions.RowsSeeded, report.Dropped.MalformedReactions,
		report.Dropped.UnmappableReactions)
	fmt.Fprintf(writer, "Dropped (counted): transcripts=%d avatars=%d drafts=%d contact_meta(CRM)=%d outgoing_send_keys=%d tabs=%d\n",
		report.Dropped.TranscriptBearingMessages,
		report.Dropped.ContactAvatars,
		report.Dropped.Drafts,
		report.Dropped.ContactMetaCRM,
		report.Dropped.OutgoingSendKeys,
		report.Dropped.Tabs)

	matched := 0
	for _, sample := range report.SampledHashes {
		if sample.Matched {
			matched++
		}
	}
	fmt.Fprintf(writer, "Validation: quick_check=%s foreign_keys=%d orphans=%d sampled_hashes=%d/%d passed=%t\n",
		report.Validation.QuickCheck,
		len(report.Validation.ForeignKeyViolations),
		migrationOrphanTotal(report.Validation.Orphans),
		matched,
		len(report.SampledHashes),
		report.Validation.Passed)
	if report.ReadState.LossyWarning != "" {
		fmt.Fprintf(writer, "WARNING: %s (%d conversations had unread_count > 0)\n",
			report.ReadState.LossyWarning,
			report.ReadState.ConversationsWithUnreadCount)
	}
	for _, warning := range report.Warnings {
		if warning == report.ReadState.LossyWarning {
			continue
		}
		fmt.Fprintf(writer, "WARNING: %s\n", warning)
	}
	if report.Check && report.OK && report.Validation.Passed {
		fmt.Fprintln(writer, "Check complete; staged files were removed and no v2 store was published.")
	} else if report.Target.Published {
		fmt.Fprintln(writer, "Migration published. The legacy source remains available for rollback with the old release.")
	}
}

func migrationOrphanTotal(report migration.OrphanReport) int64 {
	return report.MessageConversations +
		report.MessageSenders +
		report.ConversationParticipants +
		report.MessageAttachments +
		report.OutboxAccounts +
		report.OutboxConversations +
		report.OutboxAttachments +
		report.ReadCursorMessages
}
