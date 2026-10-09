package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyReadsWithoutMutatingV2Store(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writable, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if err := writable.UpsertAccount(Account{
		AccountID:   "signal-primary",
		BridgeKey:   "signal_cli",
		DisplayName: "Signal",
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: 1_700_000_001_000,
		UpdatedAtMS: 1_700_000_001_000,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close(writable): %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly(): %v", err)
	}
	account, err := readOnly.GetAccount("signal-primary")
	if err != nil {
		t.Fatalf("GetAccount(): %v", err)
	}
	if account.BridgeKey != "signal_cli" {
		t.Fatalf("read-only account = %+v", account)
	}
	if err := readOnly.UpsertAccount(Account{
		AccountID:   "must-not-write",
		BridgeKey:   "signal_cli",
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: 1,
		UpdatedAtMS: 1,
	}); err == nil {
		t.Fatal("read-only store accepted a write")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatalf("Close(read-only): %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("OpenReadOnly changed v2 database bytes")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingV2Store(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly() succeeded for a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing path was created: %v", err)
	}
}
