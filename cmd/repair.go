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
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// RunRepair handles "openmessage repair google-idspace ...".
//
// The repair rewrites the v2 store, so it refuses to run while the daemon
// holds the instance lock or answers on its API, and it copies the store
// files aside before applying.
func RunRepair(logger zerolog.Logger, args ...string) error {
	if len(args) == 0 || args[0] != "google-idspace" {
		fmt.Fprintln(os.Stderr, "Usage: openmessage repair google-idspace --since <RFC3339|unix-ms> [--account google-primary] [--apply] [--json] [--report path]")
		return errors.New("repair: unknown target")
	}
	return runRepairGoogleIDSpace(context.Background(), logger, args[1:], os.Stdout, os.Stderr)
}

type repairOutput struct {
	OK         bool                        `json:"ok"`
	Applied    bool                        `json:"applied"`
	BackupDir  string                      `json:"backup_dir,omitempty"`
	ReportPath string                      `json:"report_path,omitempty"`
	Report     *ingest.IDSpaceRepairReport `json:"report,omitempty"`
	Error      string                      `json:"error,omitempty"`
}

func runRepairGoogleIDSpace(ctx context.Context, logger zerolog.Logger, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("repair google-idspace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sinceFlag  string
		accountID  string
		apply      bool
		asJSON     bool
		reportPath string
		skipProbe  bool
		reference  string
	)
	fs.StringVar(&sinceFlag, "since", "", "row creation time from which rows are suspect (RFC3339 or unix milliseconds)")
	fs.StringVar(&reference, "reference", "", "path to a copy of store.sqlite3 taken before --since; restores titles/rosters overwritten by re-keyed conversation events")
	fs.StringVar(&accountID, "account", "google-primary", "v2 account id")
	fs.BoolVar(&apply, "apply", false, "apply the plan (default: dry run)")
	fs.BoolVar(&asJSON, "json", false, "write the report as JSON to stdout")
	fs.StringVar(&reportPath, "report", "", "also write the JSON report to this path")
	fs.BoolVar(&skipProbe, "skip-daemon-probe", false, "do not probe the daemon API before applying (tests only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	sinceMS, err := parseSinceMS(sinceFlag)
	if err != nil {
		return err
	}

	dataDir := app.DefaultDataDir()
	storePath := filepath.Join(dataDir, "v2", "store.sqlite3")
	if _, err := os.Stat(storePath); err != nil {
		return fmt.Errorf("v2 store %q: %w", storePath, err)
	}

	var lock *instanceLock
	if apply {
		if !skipProbe && daemonAnswers(defaultBackendProbeURL()) {
			return errors.New("the OpenMessage daemon is answering on its API; quit the app (park the watchdog first) before applying")
		}
		commit, _ := buildRevision()
		lock, err = acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{
			PID:       os.Getpid(),
			Process:   "openmessage repair google-idspace",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			Commit:    commit,
		})
		if err != nil {
			if errors.Is(err, errInstanceLockHeld) {
				return fmt.Errorf("another OpenMessage process holds the instance lock; quit the app before applying: %w", err)
			}
			return err
		}
		defer lock.Close()
	}

	store, err := sqlite.Open(storePath)
	if err != nil {
		return fmt.Errorf("open v2 store: %w", err)
	}
	defer store.Close()
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		return err
	}

	var referenceStore *sqlite.Store
	if reference != "" {
		if _, err := os.Stat(reference); err != nil {
			return fmt.Errorf("reference store %q: %w", reference, err)
		}
		referenceStore, err = sqlite.Open(reference)
		if err != nil {
			return fmt.Errorf("open reference store: %w", err)
		}
		defer referenceStore.Close()
	}

	report, err := ingest.PlanGoogleIDSpaceRepair(ctx, store, messages, ingest.IDSpaceRepairOptions{
		AccountID: accountID,
		SinceMS:   sinceMS,
		Now:       time.Now,
		Reference: referenceStore,
	})
	if err != nil {
		return err
	}
	output := repairOutput{OK: true, Report: &report}

	if apply && len(report.Steps) > 0 {
		backupDir := filepath.Join(dataDir, "v2", "repair-backups", time.Now().UTC().Format("20060102-150405"))
		if err := copyStoreFiles(storePath, backupDir); err != nil {
			return fmt.Errorf("back up v2 store before repair: %w", err)
		}
		output.BackupDir = backupDir
		if err := ingest.ApplyGoogleIDSpaceRepair(ctx, store, report, time.Now()); err != nil {
			return err
		}
		output.Applied = true
		logger.Info().Str("backup_dir", backupDir).Int("steps", len(report.Steps)).Msg("applied google id-space repair")
	}

	if reportPath != "" {
		payload, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(reportPath, payload, 0o600); err != nil {
			return err
		}
		output.ReportPath = reportPath
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(output)
	}
	writeRepairSummary(stdout, output)
	return nil
}

func writeRepairSummary(w io.Writer, output repairOutput) {
	r := output.Report
	mode := "DRY RUN"
	if output.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintf(w, "%s: account %s, rows created since %s\n", mode, r.AccountID, time.UnixMilli(r.SinceMS).Local().Format(time.RFC3339))
	fmt.Fprintf(w, "candidate rows: %d in %d conversations\n", r.CandidateRows, len(r.Groups))
	fmt.Fprintf(w, "moves: %d  deletes: %d  rebinds: %d  mints: %d  drops: %d  restored: %d  ambiguous: %d\n",
		r.Moves, r.Deletes, r.Rebinds, r.Mints, r.Drops, r.Restored, r.Ambiguous)
	for _, restore := range r.Restores {
		fmt.Fprintf(w, "~ %-26s %s rid=%s title %q -> restored %q peers %v -> %v target=%s",
			restore.Verdict, restore.ConversationID, restore.RemoteConversationID, restore.LiveTitle, restore.ReferenceTitle,
			restore.LivePeers, restore.ReferencePeers, restore.TargetConversationID)
		if restore.Detail != "" {
			fmt.Fprintf(w, " [%s]", restore.Detail)
		}
		fmt.Fprintln(w)
	}
	for _, group := range r.Groups {
		fmt.Fprintf(w, "- %-26s %s rid=%s title=%q rows=%d senders=%v peers=%v",
			group.Verdict, group.ConversationID, group.RemoteConversationID, group.Title, group.Rows, group.Senders, group.Peers)
		if group.TargetConversationID != "" {
			fmt.Fprintf(w, " -> %s %q (moved %d, deleted %d)", group.TargetConversationID, group.TargetTitle, group.Moved, group.Deleted)
		} else if group.Deleted > 0 {
			fmt.Fprintf(w, " (deleted %d duplicates)", group.Deleted)
		}
		if group.Detail != "" {
			fmt.Fprintf(w, " [%s]", group.Detail)
		}
		fmt.Fprintln(w)
	}
	if output.BackupDir != "" {
		fmt.Fprintf(w, "backup: %s\n", output.BackupDir)
	}
}

func parseSinceMS(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("--since is required (RFC3339 or unix milliseconds)")
	}
	if ms, err := strconv.ParseInt(value, 10, 64); err == nil {
		if ms <= 0 {
			return 0, errors.New("--since must be positive")
		}
		return ms, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("--since %q is neither unix milliseconds nor RFC3339: %w", value, err)
	}
	return parsed.UnixMilli(), nil
}

func daemonAnswers(probeURL string) bool {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	response, err := client.Get(probeURL)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func copyStoreFiles(storePath, backupDir string) error {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		source := storePath + suffix
		payload, err := os.ReadFile(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(backupDir, filepath.Base(source)), payload, 0o600); err != nil {
			return err
		}
	}
	return nil
}
