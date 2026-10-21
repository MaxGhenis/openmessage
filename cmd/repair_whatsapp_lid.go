package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
)

type whatsAppLIDRepairOutput struct {
	OK         bool                            `json:"ok"`
	Applied    bool                            `json:"applied"`
	BackupDir  string                          `json:"backup_dir,omitempty"`
	ReportPath string                          `json:"report_path,omitempty"`
	Report     *ingest.WhatsAppLIDRepairReport `json:"report,omitempty"`
}

// runRepairWhatsAppLID handles "openmessage repair whatsapp-lid": it plans
// (and with --apply applies) the rewrite of e164 identities whose digits are
// a WhatsApp LID (ingest.PlanWhatsAppLIDRepair). The evidence comes from the
// v2 store and, read-only, the whatsmeow session store and WhatsApp
// Desktop's databases; a source that is missing or unreadable is reported and
// contributes nothing, which can only leave more identities as they are.
func runRepairWhatsAppLID(ctx context.Context, logger zerolog.Logger, args []string, stdout, stderr io.Writer) error {
	dataDir := app.DefaultDataDir()
	fs := flag.NewFlagSet("repair whatsapp-lid", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		accountID   string
		sessionPath string
		desktopPath string
		desktopLID  string
		apply       bool
		asJSON      bool
		reportPath  string
		skipProbe   bool
	)
	fs.StringVar(&accountID, "account", whatsappAccountID, "v2 WhatsApp account id")
	fs.StringVar(&sessionPath, "session", filepath.Join(dataDir, "whatsapp-session.db"), "whatsmeow session store to read LID evidence from ('' to skip)")
	fs.StringVar(&desktopPath, "desktop-db", whatsapplid.DefaultDesktopPath(), "WhatsApp Desktop ChatStorage.sqlite to read LID evidence from ('' to skip)")
	fs.StringVar(&desktopLID, "desktop-lid-db", "", "WhatsApp Desktop LID.sqlite (default: beside --desktop-db)")
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
	if desktopLID == "" && strings.TrimSpace(desktopPath) != "" {
		desktopLID = whatsapplid.DesktopLIDPath(desktopPath)
	}

	store, storePath, release, err := openRepairStore(dataDir, "openmessage repair whatsapp-lid", apply, skipProbe)
	if err != nil {
		return err
	}
	defer release()

	evidence := whatsapplid.NewEvidence()
	sources := []whatsapplid.SourceReport{
		whatsapplid.ReadSessionStore(ctx, sessionPath, evidence),
		whatsapplid.ReadDesktop(ctx, desktopPath, evidence),
		whatsapplid.ReadDesktopLIDPairs(ctx, desktopLID, evidence),
	}
	report, err := ingest.PlanWhatsAppLIDRepair(ctx, store, ingest.WhatsAppLIDRepairOptions{
		AccountID: accountID,
		Evidence:  evidence,
		Sources:   sources,
	})
	if err != nil {
		return err
	}
	output := whatsAppLIDRepairOutput{OK: true, Report: &report}

	if apply && len(report.Rewrites) > 0 {
		backupDir, err := backupRepairStore(dataDir, storePath)
		if err != nil {
			return err
		}
		output.BackupDir = backupDir
		if err := ingest.ApplyWhatsAppLIDRepair(ctx, store, report, time.Now()); err != nil {
			return err
		}
		output.Applied = true
		logger.Info().Str("backup_dir", backupDir).Int("rewrites", len(report.Rewrites)).Msg("applied whatsapp lid repair")
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
	writeWhatsAppLIDRepairSummary(stdout, output)
	return nil
}

func writeWhatsAppLIDRepairSummary(w io.Writer, output whatsAppLIDRepairOutput) {
	r := output.Report
	mode := "DRY RUN"
	if output.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintf(w, "%s: account %s, %d phone-number identities\n", mode, r.AccountID, r.PhoneIdentities)
	for _, source := range r.Sources {
		state := "read"
		switch {
		case source.Error != "":
			state = "error: " + source.Error
		case source.Missing:
			state = "missing"
		}
		fmt.Fprintf(w, "source %-11s %s (%s, %d rows)", source.Name, source.Path, state, source.Rows)
		if len(source.Skipped) > 0 {
			fmt.Fprintf(w, " skipped %v", source.Skipped)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "LIDs stored as numbers: %d  rewrites: %d (to a number %d, to a LID %d, into an existing identity %d)  refused: %d\n",
		len(r.Candidates), len(r.Rewrites), r.ToNumber, r.ToLID, r.MergedIntoTarget, r.Refused)
	for _, c := range r.Candidates {
		fmt.Fprintf(w, "- %-28s %s %q", c.Verdict, c.CanonicalValue, c.DisplayName)
		if c.TargetCanonicalValue != "" {
			into := "new"
			if c.TargetExists {
				into = "existing"
			}
			fmt.Fprintf(w, " -> %s %s (%s)", c.TargetKind, c.TargetCanonicalValue, into)
		}
		fmt.Fprintf(w, " messages=%d participants=%d reactions=%d lid=%v", c.Usage.Messages, c.Usage.Participants, c.Usage.Reactions, c.LIDSources)
		if len(c.PhoneSources) > 0 {
			fmt.Fprintf(w, " phone=%v", c.PhoneSources)
		}
		if c.Detail != "" {
			fmt.Fprintf(w, " [%s]", c.Detail)
		}
		fmt.Fprintln(w)
	}
	if output.BackupDir != "" {
		fmt.Fprintf(w, "backup: %s\n", output.BackupDir)
	}
}
