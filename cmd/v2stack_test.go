package cmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/web"
)

func TestV2SendEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		unset bool
		want  bool
	}{
		{name: "unset", unset: true, want: false},
		{name: "zero", value: "0", want: false},
		{name: "false", value: "false", want: false},
		{name: "no", value: "no", want: false},
		{name: "off", value: "off", want: false},
		{name: "unknown", value: "enabled", want: false},
		{name: "one", value: "1", want: true},
		{name: "true", value: "true", want: true},
		{name: "yes", value: "yes", want: true},
		{name: "on", value: "on", want: true},
		{name: "case and whitespace insensitive", value: "  TrUe\t", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENMESSAGES_V2_SEND", tt.value)
			if tt.unset {
				if err := os.Unsetenv("OPENMESSAGES_V2_SEND"); err != nil {
					t.Fatalf("unset OPENMESSAGES_V2_SEND: %v", err)
				}
			}
			if got := v2SendEnabled(); got != tt.want {
				t.Fatalf("v2SendEnabled() = %t, want %t for %q", got, tt.want, tt.value)
			}
		})
	}
}

func TestV2IngestEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		unset bool
		want  bool
	}{
		{name: "unset", unset: true, want: false},
		{name: "zero", value: "0", want: false},
		{name: "false", value: "false", want: false},
		{name: "no", value: "no", want: false},
		{name: "off", value: "off", want: false},
		{name: "unknown", value: "enabled", want: false},
		{name: "one", value: "1", want: true},
		{name: "true", value: "true", want: true},
		{name: "yes", value: "yes", want: true},
		{name: "on", value: "on", want: true},
		{name: "case and whitespace insensitive", value: "  TrUe\t", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENMESSAGES_V2_INGEST", tt.value)
			if tt.unset {
				if err := os.Unsetenv("OPENMESSAGES_V2_INGEST"); err != nil {
					t.Fatalf("unset OPENMESSAGES_V2_INGEST: %v", err)
				}
			}
			if got := v2IngestEnabled(); got != tt.want {
				t.Fatalf("v2IngestEnabled() = %t, want %t for %q", got, tt.want, tt.value)
			}
		})
	}
}

func TestNewV2StackFailsFast(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(*testing.T, string)
		errorContains string
	}{
		{
			name: "unwritable data directory",
			setup: func(t *testing.T, dataDir string) {
				if err := os.Chmod(dataDir, 0o500); err != nil {
					t.Fatalf("make data directory read-only: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })

				probe := filepath.Join(dataDir, "write-probe")
				if err := os.Mkdir(probe, 0o700); err == nil {
					_ = os.Remove(probe)
					t.Skip("filesystem does not enforce read-only directory permissions")
				}
			},
			errorContains: "v2",
		},
		{
			name: "v2 directory path is obstructed",
			setup: func(t *testing.T, dataDir string) {
				if err := os.WriteFile(filepath.Join(dataDir, "v2"), []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("create v2 path obstruction: %v", err)
				}
			},
			errorContains: "v2",
		},
		{
			name: "blob directory is unusable",
			setup: func(t *testing.T, dataDir string) {
				v2Dir := filepath.Join(dataDir, "v2")
				if err := os.Mkdir(v2Dir, 0o700); err != nil {
					t.Fatalf("create v2 directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(v2Dir, "blobs"), []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("create invalid blob path: %v", err)
				}
			},
			errorContains: "blob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			tt.setup(t, dataDir)

			stack, err := newV2Stack(v2StackDeps{
				Logger:  zerolog.Nop(),
				DataDir: dataDir,
			})
			if err == nil {
				if stack != nil && stack.Store != nil {
					_ = stack.Store.Close()
				}
				t.Fatal("newV2Stack() succeeded for invalid configuration")
			}
			if stack != nil {
				t.Fatalf("newV2Stack() returned stack %+v with error %v", stack, err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tt.errorContains) {
				t.Fatalf("newV2Stack() error = %q, want %q context", err, tt.errorContains)
			}
		})
	}
}

func TestNewV2StackAllowsEmptyAdapterRegistry(t *testing.T) {
	dataDir := t.TempDir()
	stack, err := newV2Stack(v2StackDeps{
		Logger:   zerolog.Nop(),
		DataDir:  dataDir,
		Google:   nil,
		WhatsApp: nil,
		Signal:   nil,
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	defer stack.Store.Close()

	if stack.Store == nil || stack.Blobs == nil || stack.Registry == nil || stack.Service == nil ||
		stack.Media == nil || stack.Sink == nil || stack.Counters == nil || stack.worker == nil {
		t.Fatalf("newV2Stack() returned incomplete stack: %+v", stack)
	}
	if stack.worker.Counters() != stack.Counters {
		t.Fatal("newV2Stack() did not expose the worker's shared ingest counters")
	}
	for _, accountID := range []string{googleAccountID, whatsappAccountID, signalAccountID} {
		if _, ok := stack.Registry.Snapshot(accountID); ok {
			t.Fatalf("empty registry unexpectedly contains account %q", accountID)
		}
	}
	accounts, err := stack.Store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("new stack contains %d accounts, want zero", len(accounts))
	}

	v2Dir := filepath.Join(dataDir, "v2")
	assertPrivateDirectory(t, v2Dir)
	assertPrivateDirectory(t, filepath.Join(v2Dir, "blobs"))
	if _, err := os.Stat(filepath.Join(v2Dir, "store.sqlite3")); err != nil {
		t.Fatalf("stat v2 store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "store.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("root store path exists or returned unexpected error: %v", err)
	}
}

func TestNewV2StackRegistersConcreteLifecycleAdapters(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:   zerolog.Nop(),
		DataDir:  t.TempDir(),
		Google:   googleadapter.New(googleAccountID, nil, func() bool { return false }),
		WhatsApp: whatsappadapter.New(whatsappAccountID, nil),
		Signal:   signaladapter.New(signalAccountID, nil),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	defer stack.Store.Close()

	for _, want := range []struct {
		accountID   string
		platform    bridge.Platform
		bridgeKey   string
		displayName string
	}{
		{accountID: googleAccountID, platform: bridge.PlatformGoogle, bridgeKey: "google_messages", displayName: "Google Messages"},
		{accountID: whatsappAccountID, platform: bridge.PlatformWhatsApp, bridgeKey: "whatsmeow", displayName: "WhatsApp"},
		{accountID: signalAccountID, platform: bridge.PlatformSignal, bridgeKey: "signal_cli", displayName: "Signal"},
	} {
		snapshot, ok := stack.Registry.Snapshot(want.accountID)
		if !ok {
			t.Errorf("registry is missing account %q", want.accountID)
			continue
		}
		if snapshot.Platform != want.platform {
			t.Errorf("registry platform for %q = %q, want %q", want.accountID, snapshot.Platform, want.platform)
		}
		account, err := stack.Store.GetAccount(want.accountID)
		if err != nil {
			t.Errorf("GetAccount(%q): %v", want.accountID, err)
			continue
		}
		if account.BridgeKey != want.bridgeKey || account.DisplayName != want.displayName ||
			account.Mode != sqlite.AccountModeLive || !account.Enabled || account.ConfigJSON != "{}" {
			t.Errorf("bootstrapped account %q = %+v", want.accountID, account)
		}
	}
}

func TestV2StackRegisterAdapterPreservesExistingAccount(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	defer stack.Store.Close()

	remoteID := "remote-google-account"
	existing := sqlite.Account{
		AccountID:       googleAccountID,
		BridgeKey:       "preserve-this-bridge",
		RemoteAccountID: &remoteID,
		DisplayName:     "Preserve this name",
		Mode:            sqlite.AccountModeArchive,
		Enabled:         false,
		ConfigJSON:      `{"preserve":true}`,
		CreatedAtMS:     111,
		UpdatedAtMS:     222,
	}
	if err := stack.Store.UpsertAccount(existing); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := stack.RegisterAdapter(googleadapter.New(googleAccountID, nil, func() bool { return false })); err != nil {
		t.Fatalf("RegisterAdapter(): %v", err)
	}
	got, err := stack.Store.GetAccount(googleAccountID)
	if err != nil {
		t.Fatalf("GetAccount(): %v", err)
	}
	if !reflect.DeepEqual(got, existing) {
		t.Fatalf("RegisterAdapter() changed existing account\n got: %+v\nwant: %+v", got, existing)
	}
}

func TestV2StackFreshGoogleIngressReachesWorker(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
		Google:  googleadapter.New(googleAccountID, nil, func() bool { return false }),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}

	legacy, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer legacy.Close()
	stop := stack.Start(context.Background(), legacy, web.NewEventBroker())
	defer stop()

	if err := stack.Sink.AppendIngress(context.Background(), bridge.RawIngressRecord{
		AccountID:    googleAccountID,
		Generation:   1,
		DedupeKey:    "fresh-google-malformed",
		Codec:        ingest.GoogleCodec,
		CodecVersion: ingest.GoogleCodecVersion,
		ReceivedAt:   time.Now(),
		Payload:      []byte("{"),
	}); err != nil {
		t.Fatalf("AppendIngress() on fresh stack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := stack.Counters.Snapshot(googleAccountID)
		if snapshot.Quarantined == 1 {
			if snapshot.Appended != 1 {
				t.Fatalf("ingest counters after quarantine = %+v", snapshot)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("malformed fresh-stack frame was not quarantined: %+v", stack.Counters.Snapshot(googleAccountID))
}

func TestV2StackStopJoinsRunAndClosesStore(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}

	legacy, err := db.New(filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer legacy.Close()

	stop := stack.Start(context.Background(), legacy, web.NewEventBroker())
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("v2 stack stop did not join the dispatcher")
	}

	if _, err := stack.Store.ListAccounts(); err == nil {
		t.Fatal("v2 store remains usable after stack stop")
	}
}

func assertPrivateDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode for %q = %#o, want 0700", path, got)
	}
}
