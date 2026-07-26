package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/reconcile"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	reconcileSourceExitCode = 3
	reconcileLockExitCode   = 4
	reconcileRunExitCode    = 5
)

type reconcileSignalError struct {
	code int
	err  error
}

func (e *reconcileSignalError) Error() string { return e.err.Error() }
func (e *reconcileSignalError) Unwrap() error { return e.err }
func (e *reconcileSignalError) ExitCode() int { return e.code }

func newReconcileSignalError(code int, format string, args ...any) error {
	return &reconcileSignalError{code: code, err: fmt.Errorf(format, args...)}
}

type reconcileSignalCommandOptions struct {
	sourceDir string
	sinceMS   int64
	dryRun    bool
}

type reconcileSignalFunc func(
	context.Context,
	reconcile.Options,
) (reconcile.Report, error)

type reconcileSignalDependencies struct {
	now        func() time.Time
	version    string
	commit     string
	probeURL   string
	httpClient *http.Client
	openLegacy func(string) (*db.Store, error)
	openV2     func(string) (*sqlite.Store, error)
	reconcile  reconcileSignalFunc
}

type reconcileSignalResult struct {
	report    reconcile.Report
	hasReport bool
}

type reconcileSignalFailureOutput struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

// RunReconcileSignal handles
// "openmessage v2 reconcile-signal [--from dir] [--since YYYY-MM-DD] [--dry-run] [--json]".
//
// stdout is always exactly one JSON value. Human summaries and safety guidance
// go only to stderr.
func RunReconcileSignal(logger zerolog.Logger, args ...string) error {
	return runReconcileSignalCommand(
		context.Background(),
		logger,
		args,
		defaultReconcileSignalDependencies(),
		os.Stdout,
		os.Stderr,
	)
}

func runReconcileSignalCommand(
	ctx context.Context,
	logger zerolog.Logger,
	args []string,
	deps reconcileSignalDependencies,
	stdout io.Writer,
	stderr io.Writer,
) error {
	fs := flag.NewFlagSet("v2 reconcile-signal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: openmessage v2 reconcile-signal [--from <dir>] [--since YYYY-MM-DD] [--dry-run] [--json]")
		fmt.Fprintln(stderr, "  --from defaults to the OpenMessage data directory")
		fmt.Fprintln(stderr, "  --since includes legacy messages at or after local midnight on that date")
		fmt.Fprintln(stderr, "  --dry-run performs all reads and derivations without mutating either message store")
		fmt.Fprintln(stderr, "  the reconciliation report is JSON on stdout; human guidance is on stderr")
	}

	options := reconcileSignalCommandOptions{sourceDir: app.DefaultDataDir()}
	var since string
	var explicitJSON bool
	fs.StringVar(&options.sourceDir, "from", options.sourceDir, "OpenMessage data directory")
	fs.StringVar(&since, "since", "", "earliest legacy message date (YYYY-MM-DD)")
	fs.BoolVar(&options.dryRun, "dry-run", false, "report prospective changes without writing")
	fs.BoolVar(&explicitJSON, "json", false, "emit the reconciliation report as JSON (always enabled)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		commandErr := newReconcileSignalError(
			reconcileSourceExitCode,
			"parse reconcile-signal options: %v",
			err,
		)
		return writeReconcileSignalFailure(stdout, stderr, commandErr)
	}
	_ = explicitJSON
	if fs.NArg() != 0 {
		commandErr := newReconcileSignalError(
			reconcileSourceExitCode,
			"unexpected reconcile-signal argument %q; use --from for the data directory",
			fs.Arg(0),
		)
		return writeReconcileSignalFailure(stdout, stderr, commandErr)
	}
	sinceMS, err := parseDayBound(since, false)
	if err != nil {
		commandErr := newReconcileSignalError(
			reconcileSourceExitCode,
			"parse --since: %v",
			err,
		)
		return writeReconcileSignalFailure(stdout, stderr, commandErr)
	}
	options.sinceMS = sinceMS

	result, commandErr := executeReconcileSignal(ctx, logger, options, deps)
	if result.hasReport {
		if err := json.NewEncoder(stdout).Encode(result.report); err != nil {
			return newReconcileSignalError(
				reconcileRunExitCode,
				"write Signal reconciliation report: %v",
				err,
			)
		}
		writeHumanReconcileSignalReport(stderr, result.report)
	} else if commandErr != nil {
		return writeReconcileSignalFailure(stdout, stderr, commandErr)
	}
	if commandErr != nil {
		fmt.Fprintf(stderr, "Signal reconciliation failed: %v\n", commandErr)
	}
	return commandErr
}

func defaultReconcileSignalDependencies() reconcileSignalDependencies {
	commit, _ := buildRevision()
	return reconcileSignalDependencies{
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
		openLegacy: db.New,
		openV2:     sqlite.Open,
		reconcile:  reconcile.Signal,
	}
}

func executeReconcileSignal(
	ctx context.Context,
	logger zerolog.Logger,
	options reconcileSignalCommandOptions,
	deps reconcileSignalDependencies,
) (result reconcileSignalResult, resultErr error) {
	if ctx == nil {
		return result, newReconcileSignalError(
			reconcileRunExitCode,
			"Signal reconciliation context is nil",
		)
	}
	if deps.now == nil || deps.httpClient == nil || deps.openLegacy == nil ||
		deps.openV2 == nil || deps.reconcile == nil {
		return result, newReconcileSignalError(
			reconcileRunExitCode,
			"Signal reconciliation dependencies are incomplete",
		)
	}

	sourceDir, err := canonicalExistingDirectory(options.sourceDir)
	if err != nil {
		return result, newReconcileSignalError(
			reconcileSourceExitCode,
			"resolve OpenMessage data directory %q: %v",
			options.sourceDir,
			err,
		)
	}
	legacyPath := filepath.Join(sourceDir, legacyDatabaseName)
	if err := requireRegularReconcileStore("legacy database", legacyPath); err != nil {
		return result, err
	}
	v2Path := filepath.Join(sourceDir, "v2", v2StoreName)
	if err := requireRegularReconcileStore("v2 database", v2Path); err != nil {
		return result, err
	}

	createdAt := deps.now().UTC().Truncate(time.Second)
	lockPath := filepath.Join(sourceDir, instanceLockName)
	lock, err := acquireInstanceLock(lockPath, instanceLockRecord{
		PID:           os.Getpid(),
		Process:       "openmessage v2 reconcile-signal",
		StartedAt:     createdAt.Format(time.RFC3339),
		BuildID:       deps.version,
		Commit:        deps.commit,
		CanonicalPath: sourceDir,
		ProbeURL:      deps.probeURL,
	})
	if err != nil {
		if errors.Is(err, errInstanceLockHeld) {
			return result, newReconcileSignalError(
				reconcileLockExitCode,
				"OpenMessage state is in use: %s is locked; stop the backend and retry",
				lockPath,
			)
		}
		return result, newReconcileSignalError(
			reconcileLockExitCode,
			"acquire OpenMessage instance lock %s: %v",
			lockPath,
			err,
		)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil && resultErr == nil {
			resultErr = newReconcileSignalError(
				reconcileRunExitCode,
				"release OpenMessage instance lock: %v",
				closeErr,
			)
		}
	}()

	running, probeErr := probeBackend(ctx, deps.httpClient, deps.probeURL)
	if probeErr != nil {
		return result, newReconcileSignalError(
			reconcileLockExitCode,
			"cannot safely rule out a running OpenMessage backend at %s: %v; stop the backend and retry",
			deps.probeURL,
			probeErr,
		)
	}
	if running {
		return result, newReconcileSignalError(
			reconcileLockExitCode,
			"OpenMessage backend is running at %s; stop the backend and retry",
			deps.probeURL,
		)
	}

	legacy, err := deps.openLegacy(legacyPath)
	if err != nil {
		return result, newReconcileSignalError(
			reconcileSourceExitCode,
			"open legacy database %s: %v",
			legacyPath,
			err,
		)
	}
	defer func() {
		if closeErr := legacy.Close(); closeErr != nil && resultErr == nil {
			resultErr = newReconcileSignalError(
				reconcileRunExitCode,
				"close legacy database: %v",
				closeErr,
			)
		}
	}()
	v2, err := deps.openV2(v2Path)
	if err != nil {
		return result, newReconcileSignalError(
			reconcileSourceExitCode,
			"open v2 database %s: %v",
			v2Path,
			err,
		)
	}
	defer func() {
		if closeErr := v2.Close(); closeErr != nil && resultErr == nil {
			resultErr = newReconcileSignalError(
				reconcileRunExitCode,
				"close v2 database: %v",
				closeErr,
			)
		}
	}()

	report, reconcileErr := deps.reconcile(ctx, reconcile.Options{
		Legacy:  legacy,
		V2:      v2,
		SinceMS: options.sinceMS,
		DryRun:  options.dryRun,
		Logger:  logger,
	})
	result = reconcileSignalResult{report: report, hasReport: true}
	if reconcileErr != nil {
		return result, newReconcileSignalError(
			reconcileRunExitCode,
			"reconcile Signal history: %v",
			reconcileErr,
		)
	}
	return result, nil
}

func requireRegularReconcileStore(description string, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newReconcileSignalError(
				reconcileSourceExitCode,
				"%s not found at %s",
				description,
				path,
			)
		}
		return newReconcileSignalError(
			reconcileSourceExitCode,
			"inspect %s %s: %v",
			description,
			path,
			err,
		)
	}
	if !info.Mode().IsRegular() {
		return newReconcileSignalError(
			reconcileSourceExitCode,
			"%s path is not a regular file: %s",
			description,
			path,
		)
	}
	return nil
}

func writeReconcileSignalFailure(
	stdout io.Writer,
	stderr io.Writer,
	commandErr error,
) error {
	output := reconcileSignalFailureOutput{
		OK:       false,
		ExitCode: ExitCode(commandErr),
		Error:    commandErr.Error(),
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return newReconcileSignalError(
			reconcileRunExitCode,
			"write Signal reconciliation error report: %v",
			err,
		)
	}
	fmt.Fprintf(stderr, "Signal reconciliation failed: %v\n", commandErr)
	return commandErr
}

func writeHumanReconcileSignalReport(writer io.Writer, report reconcile.Report) {
	action := "Signal reconciliation complete"
	if report.DryRun {
		action = "Signal reconciliation dry run complete"
	}
	fmt.Fprintf(
		writer,
		"%s: conversations scanned=%d, created=%d; messages scanned=%d, imported=%d, already present=%d, media deferred=%d, skipped=%d\n",
		action,
		report.ConversationsScanned,
		report.ConversationsCreated,
		report.MessagesScanned,
		report.MessagesImported,
		report.MessagesAlreadyPresent,
		report.MediaDeferred,
		report.Skipped,
	)
}
