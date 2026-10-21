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

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	repairLIDDigits = "98765432109876"
	repairLIDPhone  = "15557654321"
)

// seedWhatsAppLIDRepair provisions a data dir whose v2 store holds a LID
// sender migrated as "+<lid digits>", and a session store whose LID map pairs
// that LID with a number. It returns the data dir and store path.
func seedWhatsAppLIDRepair(t *testing.T) (string, string) {
	t.Helper()
	storePath := seedRepairStore(t)
	dataDir := filepath.Dir(filepath.Dir(storePath))
	db, err := sql.Open("sqlite", "file:"+storePath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fake := v2keys.DeriveID("identity", whatsappAccountID, "e164\x1f+"+repairLIDDigits)
	for _, statement := range []string{
		`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('whatsapp-primary', 'whatsmeow', 1, 1)`,
		`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, created_at_ms, updated_at_ms)
			VALUES ('` + fake + `', 'whatsapp-primary', 'e164', '+` + repairLIDDigits + `', '+` + repairLIDDigits + `', '+` + repairLIDDigits + `', 1, 1)`,
		`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, created_at_ms, updated_at_ms)
			VALUES ('group', 'whatsapp-primary', 'whatsapp:120363019999999999@g.us', 'group', 1, 1)`,
		`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, occurred_at_ms, created_at_ms, updated_at_ms)
			VALUES ('m1', 'group', 'whatsapp-primary', 'm1', '` + fake + `', 'incoming', 1, 1, 1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	session, err := sql.Open("sqlite", filepath.Join(dataDir, "whatsapp-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL);
		INSERT INTO whatsmeow_lid_map VALUES ('` + repairLIDDigits + `', '` + repairLIDPhone + `')`); err != nil {
		t.Fatal(err)
	}
	return dataDir, storePath
}

func runWhatsAppLIDRepairJSON(t *testing.T, args ...string) whatsAppLIDRepairOutput {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runRepairWhatsAppLID(context.Background(), zerolog.Nop(), append([]string{"--json", "--desktop-db", ""}, args...), &stdout, &stderr); err != nil {
		t.Fatalf("repair whatsapp-lid %v: %v\n%s", args, err, stderr.String())
	}
	var output whatsAppLIDRepairOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output %q: %v", stdout.String(), err)
	}
	return output
}

func storeSenderOf(t *testing.T, storePath, messageID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT i.kind || ' ' || i.canonical_value FROM messages m JOIN identities i ON i.identity_id = m.sender_identity_id WHERE m.message_id = ?`, messageID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRepairWhatsAppLIDDryRunPlansAndWritesNothing(t *testing.T) {
	dataDir, storePath := seedWhatsAppLIDRepair(t)
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	output := runWhatsAppLIDRepairJSON(t)
	if output.Applied || output.BackupDir != "" || output.Report == nil {
		t.Fatalf("dry run output = %+v", output)
	}
	report := output.Report
	if len(report.Rewrites) != 1 || report.Rewrites[0].ToCanonicalValue != "+"+repairLIDPhone || report.ToNumber != 1 {
		t.Fatalf("plan = %+v", report)
	}
	if got := report.Candidates[0].LIDSources; len(got) != 1 || got[0] != "session_lid_map" {
		t.Fatalf("LID sources = %v", got)
	}
	var session, desktop bool
	for _, source := range report.Sources {
		session = session || (source.Name == "session" && source.Read)
		desktop = desktop || (source.Name == "desktop" && source.Missing)
	}
	if !session || !desktop {
		t.Fatalf("sources = %+v, want the session store read and Desktop skipped", report.Sources)
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

func TestRepairWhatsAppLIDDryRunNeverMigratesTheStore(t *testing.T) {
	_, storePath := seedWhatsAppLIDRepair(t)
	latest := rollBackLedgerOneVersion(t, storePath)
	var stdout, stderr bytes.Buffer
	err := runRepairWhatsAppLID(context.Background(), zerolog.Nop(), []string{"--desktop-db", ""}, &stdout, &stderr)
	if !errors.Is(err, sqlite.ErrMigrationPending) {
		t.Fatalf("dry run error = %v, want ErrMigrationPending", err)
	}
	if got := ledgerVersionCount(t, storePath); got != latest-1 {
		t.Fatalf("ledger rows = %d after the dry run, want %d", got, latest-1)
	}
}

func TestRepairWhatsAppLIDApplyBacksUpThenRewrites(t *testing.T) {
	dataDir, storePath := seedWhatsAppLIDRepair(t)
	if got := storeSenderOf(t, storePath, "m1"); got != "e164 +"+repairLIDDigits {
		t.Fatalf("seeded sender = %q", got)
	}
	output := runWhatsAppLIDRepairJSON(t, "--apply", "--skip-daemon-probe", "--report", filepath.Join(t.TempDir(), "report.json"))
	if !output.Applied || !strings.HasPrefix(output.BackupDir, filepath.Join(dataDir, "v2", "repair-backups")) || output.ReportPath == "" {
		t.Fatalf("apply output = %+v", output)
	}
	if _, err := os.Stat(filepath.Join(output.BackupDir, "store.sqlite3")); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := storeSenderOf(t, storePath, "m1"); got != "e164 +"+repairLIDPhone {
		t.Fatalf("sender after apply = %q, want the LID's number", got)
	}
	backupSender := storeSenderOf(t, filepath.Join(output.BackupDir, "store.sqlite3"), "m1")
	if backupSender != "e164 +"+repairLIDDigits {
		t.Fatalf("backup sender = %q, want the store as it was", backupSender)
	}
	if again := runWhatsAppLIDRepairJSON(t); len(again.Report.Rewrites) != 0 {
		t.Fatalf("second plan = %+v", again.Report)
	}
}

func TestRepairWhatsAppLIDSummaryNamesSourcesAndVerdicts(t *testing.T) {
	seedWhatsAppLIDRepair(t)
	var stdout, stderr bytes.Buffer
	if err := runRepairWhatsAppLID(context.Background(), zerolog.Nop(), []string{"--desktop-db", ""}, &stdout, &stderr); err != nil {
		t.Fatalf("dry run: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"DRY RUN: account whatsapp-primary", "source session", "rewrites: 1 (to a number 1", "+" + repairLIDDigits, "-> e164 +" + repairLIDPhone} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout.String())
		}
	}
}

// LID.sqlite is read from beside --desktop-db unless --desktop-lid-db names
// another file, and "" skips it even when Desktop's database is read.
func TestRepairWhatsAppLIDDesktopLIDFlag(t *testing.T) {
	seedWhatsAppLIDRepair(t)
	desktop := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	sourcePath := func(output whatsAppLIDRepairOutput, name string) (string, bool) {
		for _, source := range output.Report.Sources {
			if source.Name == name {
				return source.Path, source.Missing
			}
		}
		t.Fatalf("no %s source in %+v", name, output.Report.Sources)
		return "", false
	}
	var stdout, stderr bytes.Buffer
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--desktop-db", desktop}, filepath.Join(filepath.Dir(desktop), "LID.sqlite")},
		{[]string{"--desktop-db", desktop, "--desktop-lid-db", ""}, ""},
		{[]string{"--desktop-db", desktop, "--desktop-lid-db", "/elsewhere/LID.sqlite"}, "/elsewhere/LID.sqlite"},
	} {
		stdout.Reset()
		if err := runRepairWhatsAppLID(context.Background(), zerolog.Nop(), append([]string{"--json"}, tc.args...), &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, stderr.String())
		}
		var output whatsAppLIDRepairOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		if path, missing := sourcePath(output, "desktop_lid"); path != tc.want || !missing {
			t.Errorf("%v: LID.sqlite source = %q (missing %v), want %q", tc.args, path, missing, tc.want)
		}
	}
}

// --apply refuses while another process holds the instance lock, and writes
// nothing.
func TestRepairWhatsAppLIDApplyRefusesWhileTheInstanceLockIsHeld(t *testing.T) {
	dataDir, storePath := seedWhatsAppLIDRepair(t)
	lock, err := acquireInstanceLock(filepath.Join(dataDir, instanceLockName), instanceLockRecord{PID: 4242, Process: "test-daemon"})
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Close()
	var stdout, stderr bytes.Buffer
	err = runRepairWhatsAppLID(context.Background(), zerolog.Nop(), []string{"--apply", "--skip-daemon-probe", "--desktop-db", ""}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "holds the instance lock") {
		t.Fatalf("apply error = %v, want a refusal naming the instance lock", err)
	}
	if got := storeSenderOf(t, storePath, "m1"); got != "e164 +"+repairLIDDigits {
		t.Fatalf("sender = %q after a refused apply", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "v2", "repair-backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused apply made a backup: %v", err)
	}
}
