package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/ingest"
)

type googleMediaRepairOutput struct {
	OK         bool                            `json:"ok"`
	Applied    bool                            `json:"applied"`
	BackupDir  string                          `json:"backup_dir,omitempty"`
	ReportPath string                          `json:"report_path,omitempty"`
	Report     *ingest.GoogleMediaRepairReport `json:"report,omitempty"`
}

// runRepairGoogleMedia handles "openmessage repair google-media": it plans
// (and with --apply applies) ingest.PlanGoogleMediaRepair, which gives each
// Google attachment the reference today's decoder and projection would have
// stored from the inbox's raw frames, and marks downloaded thumbnails so the
// media service replaces them with the original.
func runRepairGoogleMedia(ctx context.Context, logger zerolog.Logger, args []string, stdout, stderr io.Writer) error {
	dataDir := app.DefaultDataDir()
	fs := flag.NewFlagSet("repair google-media", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		accountID  string
		apply      bool
		asJSON     bool
		verbose    bool
		reportPath string
		skipProbe  bool
	)
	fs.StringVar(&accountID, "account", googleAccountID, "v2 Google account id")
	fs.BoolVar(&apply, "apply", false, "apply the plan (default: dry run)")
	fs.BoolVar(&asJSON, "json", false, "write the report as JSON to stdout")
	fs.BoolVar(&verbose, "verbose", false, "list every repair")
	fs.StringVar(&reportPath, "report", "", "also write the JSON report to this path")
	fs.BoolVar(&skipProbe, "skip-daemon-probe", false, "do not probe the daemon API before applying (tests only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	store, storePath, release, err := openRepairStore(dataDir, "openmessage repair google-media", apply, skipProbe)
	if err != nil {
		return err
	}
	defer release()

	report, err := ingest.PlanGoogleMediaRepair(ctx, store, ingest.GoogleMediaRepairOptions{AccountID: accountID})
	if err != nil {
		return err
	}
	output := googleMediaRepairOutput{OK: true, Report: &report}

	if apply && len(report.Repairs) > 0 {
		backupDir, err := backupRepairStore(dataDir, storePath)
		if err != nil {
			return err
		}
		output.BackupDir = backupDir
		if err := ingest.ApplyGoogleMediaRepair(ctx, store, &report); err != nil {
			return err
		}
		output.Applied = true
		logger.Info().Str("backup_dir", backupDir).Int("planned", len(report.Repairs)).
			Int("applied", report.AppliedRepairs).Msg("applied google media repair")
	}

	if reportPath != "" {
		if err := writeRepairReport(reportPath, output); err != nil {
			return err
		}
		output.ReportPath = reportPath
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(output)
	}
	writeGoogleMediaRepairSummary(stdout, output, verbose)
	return nil
}

func writeGoogleMediaRepairSummary(w io.Writer, output googleMediaRepairOutput, verbose bool) {
	r := output.Report
	mode := "DRY RUN"
	if output.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintf(w, "%s: account %s, %d frames (%d malformed), %d messages with media frames, %d attachments\n",
		mode, r.AccountID, r.Frames, r.Malformed, r.MediaMessages, r.Attachments)
	outcomes := make([]string, 0, len(r.Outcomes))
	for outcome := range r.Outcomes {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	for _, outcome := range outcomes {
		fmt.Fprintf(w, "  %-30s %d\n", outcome, r.Outcomes[outcome])
	}
	fmt.Fprintf(w, "repairs: %d", len(r.Repairs))
	if output.Applied {
		fmt.Fprintf(w, ", applied %d", r.AppliedRepairs)
		if skipped := len(r.Repairs) - r.AppliedRepairs; skipped > 0 {
			fmt.Fprintf(w, " (%d attachments changed since the plan)", skipped)
		}
	}
	fmt.Fprintln(w)
	if verbose {
		for _, repair := range r.Repairs {
			fmt.Fprintf(w, "- %s\n", repair)
		}
	}
	if output.BackupDir != "" {
		fmt.Fprintf(w, "backup: %s\n", output.BackupDir)
	}
}
