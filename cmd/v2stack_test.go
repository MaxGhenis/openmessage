package cmd

import (
	"bytes"
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
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
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

func TestV2PrimaryEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty", value: "", want: false},
		{name: "zero", value: "0", want: false},
		{name: "unknown", value: "enabled", want: false},
		{name: "one", value: "1", want: true},
		{name: "true", value: "true", want: true},
		{name: "yes", value: "yes", want: true},
		{name: "on", value: "on", want: true},
		{name: "case and whitespace insensitive", value: "  TrUe\t", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENMESSAGES_V2_PRIMARY", tt.value)
			if got := v2PrimaryEnabled(); got != tt.want {
				t.Fatalf("v2PrimaryEnabled() = %t, want %t for %q", got, tt.want, tt.value)
			}
		})
	}
}

func TestResolveV2RuntimeMode(t *testing.T) {
	tests := []struct {
		name          string
		demo          bool
		primary       string
		send          string
		ingest        string
		createStore   bool
		want          v2RuntimeMode
		errorContains string
	}{
		{
			name: "legacy primary defaults unchanged",
			want: v2RuntimeMode{},
		},
		{
			name: "legacy primary preserves independent send",
			send: "1",
			want: v2RuntimeMode{Send: true},
		},
		{
			name:   "legacy primary preserves independent ingest",
			ingest: "1",
			want:   v2RuntimeMode{Ingest: true},
		},
		{
			name:          "primary rejects demo",
			demo:          true,
			primary:       "1",
			errorContains: "OPENMESSAGES_V2_PRIMARY is not available in demo mode",
		},
		{
			name:          "primary rejects explicit send off",
			primary:       "1",
			send:          "0",
			errorContains: "OPENMESSAGES_V2_PRIMARY requires v2 send and ingest; unset OPENMESSAGES_V2_SEND=0/OPENMESSAGES_V2_INGEST=0",
		},
		{
			name:          "primary rejects explicit ingest off",
			primary:       "1",
			ingest:        "0",
			errorContains: "OPENMESSAGES_V2_PRIMARY requires v2 send and ingest; unset OPENMESSAGES_V2_SEND=0/OPENMESSAGES_V2_INGEST=0",
		},
		{
			name:          "primary refuses an absent migrated store",
			primary:       "1",
			errorContains: "v2-primary selected but no migrated store at ",
		},
		{
			name:        "primary implies send and ingest",
			primary:     "1",
			createStore: true,
			want:        v2RuntimeMode{Primary: true, Send: true, Ingest: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("OPENMESSAGES_V2_PRIMARY", tt.primary)
			t.Setenv("OPENMESSAGES_V2_SEND", tt.send)
			t.Setenv("OPENMESSAGES_V2_INGEST", tt.ingest)
			if tt.createStore {
				v2Dir := filepath.Join(dataDir, "v2")
				if err := os.MkdirAll(v2Dir, 0o700); err != nil {
					t.Fatalf("create v2 dir: %v", err)
				}
				store, err := sqlite.Open(filepath.Join(v2Dir, "store.sqlite3"))
				if err != nil {
					t.Fatalf("create v2 store: %v", err)
				}
				if err := store.Close(); err != nil {
					t.Fatalf("close v2 store: %v", err)
				}
			}

			got, err := resolveV2RuntimeMode(tt.demo, dataDir)
			if tt.errorContains != "" {
				if err == nil {
					t.Fatalf("resolveV2RuntimeMode() succeeded, want error containing %q", tt.errorContains)
				}
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Fatalf("resolveV2RuntimeMode() error = %q, want %q", err, tt.errorContains)
				}
				if strings.Contains(tt.name, "absent migrated store") {
					wantPath := filepath.Join(dataDir, "v2", "store.sqlite3")
					if !strings.Contains(err.Error(), wantPath) || !strings.Contains(err.Error(), "run: openmessage migrate") {
						t.Fatalf("absent-store error = %q, want path %q and migrate command", err, wantPath)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveV2RuntimeMode(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveV2RuntimeMode() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveV2RuntimeModeRejectsNonRegularStoreWithoutReplacingIt(t *testing.T) {
	dataDir := t.TempDir()
	storePath := filepath.Join(dataDir, "v2", "store.sqlite3")
	if err := os.MkdirAll(storePath, 0o700); err != nil {
		t.Fatalf("create directory at store path: %v", err)
	}
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	_, err := resolveV2RuntimeMode(false, dataDir)
	if err == nil || !strings.Contains(err.Error(), "no migrated store") ||
		!strings.Contains(err.Error(), "run: openmessage migrate") {
		t.Fatalf("resolveV2RuntimeMode() error = %v", err)
	}
	info, statErr := os.Stat(storePath)
	if statErr != nil {
		t.Fatalf("stat sentinel after refusal: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("non-regular store sentinel was replaced: mode=%v", info.Mode())
	}
}

func TestLegacySchedulerGuard(t *testing.T) {
	if !shouldStartLegacyScheduler(false) {
		t.Fatal("legacy-primary must start the legacy scheduler")
	}
	if shouldStartLegacyScheduler(true) {
		t.Fatal("v2-primary must not start the legacy scheduler")
	}

	starts := 0
	start := func() { starts++ }
	startLegacyScheduler(false, start)
	if starts != 1 {
		t.Fatalf("legacy-primary scheduler starts = %d, want 1", starts)
	}
	startLegacyScheduler(true, start)
	if starts != 1 {
		t.Fatalf("v2-primary scheduler starts = %d, want unchanged at 1", starts)
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
	stop := stack.Start(context.Background(), legacy, web.NewEventBroker(), false)
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

	stop := stack.Start(context.Background(), legacy, web.NewEventBroker(), false)
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

func TestV2PrimaryStackDoesNotStartLegacyProjector(t *testing.T) {
	var logs bytes.Buffer
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.New(&logs),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}

	// A nil legacy store makes Projector.Run fail synchronously. If the
	// v2-primary branch accidentally starts it, the runner logs that failure
	// before Stop joins all goroutines.
	stop := stack.Start(context.Background(), nil, web.NewEventBroker(), true)
	stop()
	if strings.Contains(logs.String(), "V2 legacy projector stopped") {
		t.Fatalf("v2-primary started the legacy projector:\n%s", logs.String())
	}
}

func TestLegacyPrimaryStackStillStartsLegacyProjector(t *testing.T) {
	var logs bytes.Buffer
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.New(&logs),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}

	// The nil legacy store is a sentinel: the legacy-primary projector must
	// start, reject it, and log before Stop joins the runners.
	stop := stack.Start(context.Background(), nil, web.NewEventBroker(), false)
	stop()
	if !strings.Contains(logs.String(), "V2 legacy projector stopped") {
		t.Fatalf("legacy-primary did not start the legacy projector:\n%s", logs.String())
	}
}

func TestV2PrimaryStackRunsWorkerAndDispatcherWithoutProjector(t *testing.T) {
	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	adapter := &v2PrimaryTextAdapter{requests: make(chan bridge.TextRequest, 1)}
	if err := stack.RegisterAdapter(adapter); err != nil {
		stack.Store.Close()
		t.Fatalf("RegisterAdapter(): %v", err)
	}
	nowMS := time.Now().UnixMilli()
	if err := stack.Store.UpsertConversation(sqlite.Conversation{
		ConversationID:       "v2-primary-conversation",
		AccountID:            adapter.AccountID(),
		RemoteConversationID: "remote-v2-primary-conversation",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "V2 primary",
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		stack.Store.Close()
		t.Fatalf("UpsertConversation(): %v", err)
	}
	submission, err := v2wire.SubmitTextV2(context.Background(), v2wire.NativeDeps{
		V2: stack.Store, Service: stack.Service, Registry: stack.Registry,
	}, v2wire.TextInput{
		ConversationID: "v2-primary-conversation",
		Body:           "dispatch while primary",
		IdempotencyKey: "v2-primary-dispatch",
	})
	if err != nil {
		stack.Store.Close()
		t.Fatalf("SubmitTextV2(): %v", err)
	}

	stop := stack.Start(context.Background(), nil, web.NewEventBroker(), true)
	defer stop()
	if err := stack.Sink.AppendIngress(context.Background(), bridge.RawIngressRecord{
		AccountID:    adapter.AccountID(),
		Generation:   1,
		DedupeKey:    "v2-primary-malformed",
		Codec:        ingest.GoogleCodec,
		CodecVersion: ingest.GoogleCodecVersion,
		ReceivedAt:   time.Now(),
		Payload:      []byte("{"),
	}); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}

	var sent *bridge.TextRequest
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sent == nil {
			select {
			case request := <-adapter.requests:
				sent = &request
			default:
			}
		}
		delivery, deliveryErr := stack.Service.Get(context.Background(), submission.OutboxID)
		quarantined := stack.Counters.Snapshot(adapter.AccountID()).Quarantined == 1
		if sent != nil && deliveryErr == nil && delivery.State == messaging.OutboxConfirmed && quarantined {
			if sent.Body != "dispatch while primary" ||
				sent.Conversation.RemoteID != "remote-v2-primary-conversation" {
				t.Fatalf("dispatched request = %+v", *sent)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	delivery, deliveryErr := stack.Service.Get(context.Background(), submission.OutboxID)
	t.Fatalf(
		"v2-primary runners did not settle: sent=%v delivery=%+v err=%v counters=%+v",
		sent != nil,
		delivery,
		deliveryErr,
		stack.Counters.Snapshot(adapter.AccountID()),
	)
}

type v2PrimaryTextAdapter struct {
	requests chan bridge.TextRequest
}

func (*v2PrimaryTextAdapter) AccountID() string         { return googleAccountID }
func (*v2PrimaryTextAdapter) Platform() bridge.Platform { return bridge.PlatformGoogle }
func (*v2PrimaryTextAdapter) Start(
	context.Context,
	bridge.StartRequest,
	bridge.ConnectionSink,
) (bridge.Run, error) {
	return nil, nil
}
func (a *v2PrimaryTextAdapter) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	a.requests <- request
	return bridge.SendResult{
		RemoteMessageID: "remote-v2-primary-message",
		EchoExpected:    false,
	}, nil
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
