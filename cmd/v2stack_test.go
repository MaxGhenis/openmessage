package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
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

	if stack.Store == nil || stack.Blobs == nil || stack.Registry == nil || stack.Service == nil || stack.Media == nil {
		t.Fatalf("newV2Stack() returned incomplete stack: %+v", stack)
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
		accountID string
		platform  bridge.Platform
	}{
		{accountID: googleAccountID, platform: bridge.PlatformGoogle},
		{accountID: whatsappAccountID, platform: bridge.PlatformWhatsApp},
		{accountID: signalAccountID, platform: bridge.PlatformSignal},
	} {
		snapshot, ok := stack.Registry.Snapshot(want.accountID)
		if !ok {
			t.Errorf("registry is missing account %q", want.accountID)
			continue
		}
		if snapshot.Platform != want.platform {
			t.Errorf("registry platform for %q = %q, want %q", want.accountID, snapshot.Platform, want.platform)
		}
	}
}

func TestV2StackStopJoinsRunAndClosesStore(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}

	stop := stack.Start(context.Background())
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

func TestEnsureLocalDevice(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "test-account",
		BridgeKey:   "test",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: now,
		UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}

	if err := ensureLocalDevice(store, "test-account"); err != nil {
		t.Fatalf("ensureLocalDevice(): %v", err)
	}
	if err := ensureLocalDevice(store, "test-account"); err != nil {
		t.Fatalf("ensureLocalDevice() second call: %v", err)
	}

	device, err := store.GetDevice(localDeviceID("test-account"))
	if err != nil {
		t.Fatalf("GetDevice(): %v", err)
	}
	if device.DeviceID != localDeviceID("test-account") {
		t.Errorf("DeviceID = %q, want %q", device.DeviceID, localDeviceID("test-account"))
	}
	if device.AccountID != "test-account" {
		t.Errorf("AccountID = %q, want test-account", device.AccountID)
	}
	if device.Kind != sqlite.DeviceKindLocalInstallation {
		t.Errorf("Kind = %q, want %q", device.Kind, sqlite.DeviceKindLocalInstallation)
	}
	if device.State != sqlite.DeviceStateActive {
		t.Errorf("State = %q, want %q", device.State, sqlite.DeviceStateActive)
	}
	if !device.IsCurrent {
		t.Error("IsCurrent = false, want true")
	}
	if device.CreatedAtMS <= 0 || device.UpdatedAtMS < device.CreatedAtMS {
		t.Errorf("invalid device timestamps: created=%d updated=%d", device.CreatedAtMS, device.UpdatedAtMS)
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

// Two accounts must each get their own local device row: devices.device_id is
// a GLOBAL primary key and UpsertDevice conflicts on it alone, so a constant
// device id would let the second account silently steal the first account's
// row and break its read_cursors composite FK. This test fails under the
// constant-id design.
func TestEnsureLocalDevicePerAccountRowsCoexist(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()

	now := time.Now().UnixMilli()
	for _, accountID := range []string{"google-primary", "signal-primary"} {
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: accountID, BridgeKey: accountID, Mode: sqlite.AccountModeLive,
			Enabled: true, ConfigJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", accountID, err)
		}
		if err := ensureLocalDevice(store, accountID); err != nil {
			t.Fatalf("ensureLocalDevice(%s): %v", accountID, err)
		}
	}

	for _, accountID := range []string{"google-primary", "signal-primary"} {
		device, err := store.GetDevice(localDeviceID(accountID))
		if err != nil {
			t.Fatalf("GetDevice(%s): %v", accountID, err)
		}
		if device.AccountID != accountID {
			t.Fatalf("device for %s has AccountID %q (row stolen)", accountID, device.AccountID)
		}
		// The composite-FK write that the constant-id design breaks: a read
		// cursor for each account's own device must succeed.
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: "conv-" + accountID, AccountID: accountID,
			RemoteConversationID: "remote-" + accountID, Kind: sqlite.ConversationKindDirect,
			Title: accountID, NotificationMode: sqlite.NotificationModeAll,
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", accountID, err)
		}
		if err := store.UpsertReadCursor(sqlite.ReadCursor{
			AccountID: accountID, DeviceID: localDeviceID(accountID),
			ConversationID: "conv-" + accountID, LastReadAtMS: 0, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertReadCursor(%s): %v", accountID, err)
		}
	}
}
