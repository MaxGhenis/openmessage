//go:build livetransport

// Package livetransport is the manual live-verification harness for the v2
// send and ingest pipelines. It drives the REAL transports against the
// operator's own live pairing state. The ingest verification is passive and
// never sends; the send verification sends ONLY to self-recipient threads.
// Neither runs in CI (build tag), and both require the OpenMessage app to be
// quit so the harness has sole ownership of the live sessions. The send test
// additionally requires the operator's explicit go-ahead.
//
// Run:
//
//	GOWORK=off go test -tags livetransport -run TestLiveSelfSendVerification \
//	  -v -count=1 -timeout 10m ./internal/livetransport/
//
//	LIVE_PLATFORMS=google GOWORK=off go test -tags livetransport \
//	  -run TestLiveIngestVerification -v -count=1 -timeout 10m \
//	  ./internal/livetransport/
package livetransport

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	watypes "go.mau.fi/whatsmeow/types"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	liveDataDirEnv               = "OPENMESSAGES_DATA_DIR"
	googleAccountID              = "google-primary"
	whatsappAccountID            = "whatsapp-primary"
	signalAccountID              = "signal-primary"
	defaultGoogleSelfConvRemote  = "1770"  // live self-thread (canonical, per /api/new-conversation)
	defaultGoogleAnchorMessageID = "85269" // real anchor message sent via the legacy path
	signalSelfConvRemote         = "signal:+16506303657"
	onlineWait                   = 90 * time.Second
	settleWait                   = 60 * time.Second
	ingestWait                   = 2 * time.Minute
)

type liveIngestTransport struct {
	name                   string
	accountID              string
	bridgeKey              string
	displayName            string
	platform               bridge.Platform
	adapter                bridge.Adapter
	codec                  string
	selfRemoteConversation string
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }
func (wallClock) NewTimer(d time.Duration) bridge.Timer {
	return wallTimer{time.NewTimer(d)}
}

type wallTimer struct{ *time.Timer }

func (t wallTimer) C() <-chan time.Time { return t.Timer.C }
func (t wallTimer) Stop() bool          { return t.Timer.Stop() }

type wallRandom struct{}

func (wallRandom) Int63n(n int64) int64 { return rand.Int63n(n) }

func livePolicy() bridge.Policy {
	return bridge.Policy{
		ConnectTimeout:     60 * time.Second,
		ProbeEvery:         30 * time.Second,
		ProbeTimeout:       20 * time.Second,
		LivenessTimeout:    2 * time.Minute,
		MinBackoff:         2 * time.Second,
		MaxBackoff:         30 * time.Second,
		MaxSameFingerprint: 3,
	}
}

// TestLiveIngestVerification is deliberately receive-only. Selecting a
// platform means the operator expects its real receive path to produce a
// message-bearing frame during ingestWait. Google normally does this during
// startup sync; idle WhatsApp and Signal accounts need a genuinely inbound or
// history frame and may otherwise remain at zero.
func TestLiveIngestVerification(t *testing.T) {
	selected := liveIngestPlatforms(t)
	dataDir := os.Getenv(liveDataDirEnv)
	if dataDir == "" {
		dataDir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "OpenMessage")
		os.Setenv(liveDataDirEnv, dataDir)
	}
	if selected["google"] {
		if _, err := os.Stat(filepath.Join(dataDir, "session.json")); err != nil {
			t.Fatalf("live Google data dir not found or unpaired: %v", err)
		}
	}

	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	a, err := app.New(logger)
	if err != nil {
		t.Fatalf("app.New(): %v", err)
	}
	defer a.Close()

	transports := make([]liveIngestTransport, 0, len(selected))
	if selected["google"] {
		adapter := googleadapter.New(googleAccountID, a, func() bool { return false })
		a.SetGoogleLifecycleNotifier(adapter)
		transports = append(transports, liveIngestTransport{
			name:                   "google",
			accountID:              googleAccountID,
			bridgeKey:              "google_messages",
			displayName:            "Google Messages",
			platform:               bridge.PlatformGoogle,
			adapter:                adapter,
			codec:                  ingest.GoogleCodec,
			selfRemoteConversation: envOr("LIVE_GOOGLE_CONV", defaultGoogleSelfConvRemote),
		})
	}
	if selected["whatsapp"] {
		whatsappBridge, err := a.InitializeWhatsApp()
		if err != nil {
			t.Fatalf("InitializeWhatsApp(): %v", err)
		}
		paired := whatsappBridge.PairedAccount()
		if !paired.Paired {
			t.Fatal("WhatsApp is selected in LIVE_PLATFORMS but is not paired")
		}
		selfConversation := os.Getenv("LIVE_WHATSAPP_CONV")
		if selfConversation == "" {
			jid, err := watypes.ParseJID(paired.JID)
			if err != nil {
				t.Fatalf("parse paired WhatsApp JID %q: %v", paired.JID, err)
			}
			selfConversation = "whatsapp:" + jid.ToNonAD().String()
		}
		adapter := whatsappadapter.New(whatsappAccountID, whatsappBridge)
		a.SetWhatsAppLifecycleNotifier(adapter)
		transports = append(transports, liveIngestTransport{
			name:                   "whatsapp",
			accountID:              whatsappAccountID,
			bridgeKey:              "whatsmeow",
			displayName:            "WhatsApp",
			platform:               bridge.PlatformWhatsApp,
			adapter:                adapter,
			codec:                  ingest.WhatsAppCodec,
			selfRemoteConversation: selfConversation,
		})
	}
	if selected["signal"] {
		signalBridge, err := a.EnsureSignal()
		if err != nil {
			t.Fatalf("EnsureSignal(): %v", err)
		}
		signalStatus := signalBridge.Status()
		if !signalStatus.Paired {
			t.Fatal("Signal is selected in LIVE_PLATFORMS but is not paired")
		}
		defaultSelfConversation := signalSelfConvRemote
		if strings.TrimSpace(signalStatus.Account) != "" {
			defaultSelfConversation = "signal:" + strings.TrimSpace(signalStatus.Account)
		}
		adapter := signaladapter.New(signalAccountID, signalBridge)
		a.SetSignalLifecycleNotifier(adapter)
		transports = append(transports, liveIngestTransport{
			name:                   "signal",
			accountID:              signalAccountID,
			bridgeKey:              "signal_cli",
			displayName:            "Signal",
			platform:               bridge.PlatformSignal,
			adapter:                adapter,
			codec:                  ingest.SignalJSONRPCCodec,
			selfRemoteConversation: envOr("LIVE_SIGNAL_CONV", defaultSelfConversation),
		})
	}

	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "v2.sqlite")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	blobs, err := blob.New(filepath.Join(tempDir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	registry := bridge.NewRegistry()
	nowMS := time.Now().UnixMilli()
	for _, transport := range transports {
		seedLiveAccount(t, store, nowMS, transport)
		if err := registry.Register(transport.adapter); err != nil {
			t.Fatalf("register %s adapter: %v", transport.name, err)
		}
	}
	service, err := messaging.NewMessageService(
		store,
		registry,
		blobs,
		messaging.SystemClock{},
		messaging.CryptoIDSource{},
	)
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: service,
		Counters:     counters,
		Logger:       logger,
		Decoders: []ingest.DecoderRegistration{
			{
				Codec:    ingest.GoogleCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
			},
			ingest.NewWhatsAppDecoderRegistration(),
			{
				Codec:    ingest.SignalJSONRPCCodec,
				Platform: bridge.PlatformSignal,
				Decoder:  ingest.NewSignalDecoder(),
			},
		},
	})
	if err != nil {
		t.Fatalf("ingest.NewWorker(): %v", err)
	}
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
	})
	if err != nil {
		t.Fatalf("ingest.NewSink(): %v", err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	defer func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("ingest worker stopped: %v", err)
		}
	}()

	supervisors := make(map[string]*bridge.Supervisor, len(transports))
	for _, transport := range transports {
		supervisor, err := bridge.NewSupervisor(
			transport.accountID,
			transport.platform,
			transport.adapter,
			livePolicy(),
			wallClock{},
			wallRandom{},
			bridge.WithConnectionSink(sink),
		)
		if err != nil {
			t.Fatalf("%s NewSupervisor(): %v", transport.name, err)
		}
		supervisors[transport.name] = supervisor
		defer stopSupervisor(t, supervisor, transport.name)
	}
	for _, transport := range transports {
		startSupervisor(t, supervisors[transport.name], transport.name)
	}
	for _, transport := range transports {
		if !waitOnline(t, supervisors[transport.name], transport.name) {
			t.Fatalf("selected %s transport did not come online", transport.name)
		}
	}

	raw := openRawV2Store(t, storePath)
	defer raw.Close()
	deadline := time.Now().Add(ingestWait)
	pending := make(map[string]liveIngestTransport, len(transports))
	for _, transport := range transports {
		pending[transport.accountID] = transport
	}
	for len(pending) > 0 && time.Now().Before(deadline) {
		for accountID, transport := range pending {
			snapshot := counters.Snapshot(accountID)
			if snapshot.Quarantined != 0 {
				t.Fatalf("%s ingest quarantined frames while waiting: %+v", transport.name, snapshot)
			}
			if snapshot.Appended == 0 || snapshot.Projected == 0 {
				continue
			}
			projectedRows, err := countLiveIngestMessages(
				raw,
				transport.accountID,
				transport.selfRemoteConversation,
			)
			if err != nil {
				t.Fatalf("count %s self-thread messages while waiting: %v", transport.name, err)
			}
			if projectedRows > 0 {
				t.Logf(
					"INGEST [%s] counters ready with %d self-thread rows: %+v",
					transport.name,
					projectedRows,
					snapshot,
				)
				delete(pending, accountID)
			}
		}
		if len(pending) > 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if len(pending) > 0 {
		for _, transport := range pending {
			projectedRows, err := countLiveIngestMessages(raw, transport.accountID, transport.selfRemoteConversation)
			t.Logf(
				"INGEST [%s] timed out with counters=%+v self_thread_rows=%d query_error=%v",
				transport.name,
				counters.Snapshot(transport.accountID),
				projectedRows,
				err,
			)
		}
		t.Fatalf("selected live ingest platforms did not append and project a self-thread row within %s", ingestWait)
	}

	for _, transport := range transports {
		snapshot := counters.Snapshot(transport.accountID)
		if snapshot.Quarantined != 0 {
			t.Fatalf("%s ingest quarantined = %d, want 0", transport.name, snapshot.Quarantined)
		}
		var inboxRows int
		if err := raw.QueryRowContext(
			context.Background(),
			`SELECT COUNT(*) FROM inbox WHERE account_id = ? AND codec = ?`,
			transport.accountID,
			transport.codec,
		).Scan(&inboxRows); err != nil {
			t.Fatalf("count %s inbox rows: %v", transport.name, err)
		}
		if inboxRows == 0 {
			t.Fatalf("%s appended counters advanced without an inbox row using codec %q", transport.name, transport.codec)
		}

		projectedRows, err := countLiveIngestMessages(
			raw,
			transport.accountID,
			transport.selfRemoteConversation,
		)
		if err != nil {
			t.Fatalf("count %s self-thread messages: %v", transport.name, err)
		}
		if projectedRows == 0 {
			t.Fatalf(
				"%s projected no messages for live self-thread %q (counters=%+v)",
				transport.name,
				transport.selfRemoteConversation,
				snapshot,
			)
		}
		t.Logf(
			"INGEST [%s] verified inbox=%d self_thread_messages=%d quarantined=0",
			transport.name,
			inboxRows,
			projectedRows,
		)
	}
}

func liveIngestPlatforms(t *testing.T) map[string]bool {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("LIVE_PLATFORMS"))
	if raw == "" {
		t.Skip("set LIVE_PLATFORMS=google,whatsapp,signal to select passive ingest legs")
	}
	selected := make(map[string]bool)
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		name := strings.ToLower(strings.TrimSpace(field))
		switch name {
		case "all":
			selected["google"] = true
			selected["whatsapp"] = true
			selected["signal"] = true
		case "google", "whatsapp", "signal":
			selected[name] = true
		default:
			t.Fatalf("LIVE_PLATFORMS contains unsupported platform %q", field)
		}
	}
	if len(selected) == 0 {
		t.Fatal("LIVE_PLATFORMS selected no platforms")
	}
	return selected
}

func seedLiveAccount(t *testing.T, store *sqlite.Store, nowMS int64, transport liveIngestTransport) {
	t.Helper()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   transport.accountID,
		BridgeKey:   transport.bridgeKey,
		DisplayName: transport.displayName,
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(%s): %v", transport.accountID, err)
	}
}

func openRawV2Store(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	query := make(url.Values)
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	raw, err := sql.Open("sqlite", (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(dbPath),
		RawQuery: query.Encode(),
	}).String())
	if err != nil {
		t.Fatalf("open raw v2 store: %v", err)
	}
	if err := raw.PingContext(context.Background()); err != nil {
		raw.Close()
		t.Fatalf("ping raw v2 store: %v", err)
	}
	return raw
}

func countLiveIngestMessages(raw *sql.DB, accountID, remoteConversationID string) (int, error) {
	var count int
	err := raw.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*)
		 FROM messages AS m
		 JOIN conversations AS c
		   ON c.account_id = m.account_id
		  AND c.conversation_id = m.conversation_id
		 WHERE m.account_id = ? AND c.remote_conversation_id = ?`,
		accountID,
		remoteConversationID,
	).Scan(&count)
	return count, err
}

func TestLiveSelfSendVerification(t *testing.T) {
	dataDir := os.Getenv(liveDataDirEnv)
	if dataDir == "" {
		dataDir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "OpenMessage")
		os.Setenv(liveDataDirEnv, dataDir)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "session.json")); err != nil {
		t.Fatalf("live data dir not found or unpaired: %v", err)
	}
	stamp := time.Now().Format("15:04:05")
	platforms := os.Getenv("LIVE_PLATFORMS") // "" = all; e.g. "google" or "signal"
	googleSelfConvRemote := envOr("LIVE_GOOGLE_CONV", defaultGoogleSelfConvRemote)
	googleAnchorMessageID := envOr("LIVE_GOOGLE_ANCHOR", defaultGoogleAnchorMessageID)
	wantPlatform := func(name string) bool { return platforms == "" || platforms == name }

	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	a, err := app.New(logger)
	if err != nil {
		t.Fatalf("app.New(): %v", err)
	}
	defer a.Close()

	// ---- Signal supervisor (receive up, real poller) ----
	signalBridge, err := a.EnsureSignal()
	if err != nil {
		t.Fatalf("EnsureSignal(): %v", err)
	}
	signalAdapter := signaladapter.New(signalAccountID, signalBridge)
	a.SetSignalLifecycleNotifier(signalAdapter)
	signalSup, err := bridge.NewSupervisor(
		signalAccountID, bridge.PlatformSignal, signalAdapter,
		livePolicy(), wallClock{}, wallRandom{},
	)
	if err != nil {
		t.Fatalf("signal NewSupervisor(): %v", err)
	}
	startSupervisor(t, signalSup, "signal")
	defer stopSupervisor(t, signalSup, "signal")
	signalOK := wantPlatform("signal") && waitOnline(t, signalSup, "signal")

	// ---- Google supervisor (session-based connect; no repair in harness) ----
	googleAdapter := googleadapter.New(googleAccountID, a, func() bool { return false })
	googleSup, err := bridge.NewSupervisor(
		googleAccountID, bridge.PlatformGoogle, googleAdapter,
		livePolicy(), wallClock{}, wallRandom{},
	)
	if err != nil {
		t.Fatalf("google NewSupervisor(): %v", err)
	}
	startSupervisor(t, googleSup, "google")
	defer stopSupervisor(t, googleSup, "google")
	googleOK := wantPlatform("google") && waitOnline(t, googleSup, "google")

	if !signalOK && !googleOK {
		t.Fatal("neither Signal nor Google came online; nothing to verify")
	}

	// ---- v2 pipeline over a temp store, real registry, real adapters ----
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "v2.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	blobs, err := blob.New(filepath.Join(tempDir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	registry := bridge.NewRegistry()
	if err := registry.Register(signalAdapter); err != nil {
		t.Fatalf("register signal adapter: %v", err)
	}
	if err := registry.Register(googleAdapter); err != nil {
		t.Fatalf("register google adapter: %v", err)
	}
	service, err := messaging.NewMessageService(store, registry, blobs, messaging.SystemClock{}, messaging.CryptoIDSource{})
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}

	now := time.Now().UnixMilli()
	seedAccountAndConversation(t, store, now, signalAccountID, "conv-signal-self", signalSelfConvRemote, "Note to Self (Signal)")
	seedAccountAndConversation(t, store, now, googleAccountID, "conv-google-self", googleSelfConvRemote, "Self thread (Google)")
	_ = googleAnchorMessageID
	if err := store.UpsertDevice(sqlite.Device{
		DeviceID: "live-harness-device", AccountID: googleAccountID,
		Kind: sqlite.DeviceKindLocalInstallation, DisplayName: "Live harness",
		State: sqlite.DeviceStateActive, CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}
	anchorLocalID := seedGoogleAnchorMessage(t, filepath.Join(tempDir, "v2.sqlite"), now, googleAnchorMessageID)

	mustSubmit := func(submission messaging.Submission, err error) messaging.Submission {
		t.Helper()
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return submission
	}

	dispatch := func(label string, submission messaging.Submission) messaging.Delivery {
		t.Helper()
		deadline := time.Now().Add(settleWait)
		for {
			if _, err := service.DispatchDue(context.Background(), 8); err != nil {
				t.Fatalf("[%s] DispatchDue(): %v", label, err)
			}
			delivery, err := service.Get(context.Background(), submission.OutboxID)
			if err != nil {
				t.Fatalf("[%s] Get(): %v", label, err)
			}
			switch delivery.State {
			case messaging.OutboxConfirmed:
				t.Logf("RECEIPT [%s] confirmed remote=%q outbox=%s", label, delivery.RemoteMessageID, submission.OutboxID)
				return delivery
			case messaging.OutboxRejected, messaging.OutboxCanceled, messaging.OutboxStoreFailed, messaging.OutboxUncertain:
				t.Fatalf("[%s] settled %s (class=%s code=%s warning=%q)", label, delivery.State, delivery.ErrorClass, delivery.ErrorCode, delivery.Warning)
			}
			if time.Now().After(deadline) {
				t.Fatalf("[%s] did not settle in %s (state=%s)", label, settleWait, delivery.State)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// ---------------- SIGNAL ----------------
	var signalTextLocal string
	if signalOK {
		sub := mustSubmit(service.SendText(context.Background(), messaging.SendTextCommand{
			CommonCommand: common(signalAccountID, "conv-signal-self", "live-signal-text"),
			Body:          fmt.Sprintf("OpenMessage v2 live verification -- text (%s)", stamp),
		}))
		delivery := dispatch("signal/text", sub)
		if _, err := strconv.ParseInt(delivery.RemoteMessageID, 10, 64); err != nil {
			t.Fatalf("signal text remote id %q is not a timestamp", delivery.RemoteMessageID)
		}
		signalTextLocal = sub.LocalMessageID

		rsub := mustSubmit(service.SendReaction(context.Background(), messaging.SendReactionCommand{
			CommonCommand:   common(signalAccountID, "conv-signal-self", "live-signal-react"),
			TargetMessageID: signalTextLocal,
			Emoji:           "\U0001F44D",
			Action:          bridge.ReactionAdd,
		}))
		dispatch("signal/reaction", rsub)

		msub := mustSubmit(service.SendMedia(context.Background(), messaging.SendMediaCommand{
			CommonCommand: common(signalAccountID, "conv-signal-self", "live-signal-media"),
			Content:       bytes.NewReader(tinyPNG(t)),
			Filename:      "om-live-verify.png",
			MIME:          "image/png",
			Caption:       fmt.Sprintf("OpenMessage v2 live verification -- media (%s)", stamp),
		}))
		dispatch("signal/media", msub)

		if _, err := service.MarkRead(context.Background(), messaging.MarkReadCommand{
			CommonCommand:     common(signalAccountID, "conv-signal-self", "live-signal-read"),
			DeviceID:          "live-harness-device",
			LastReadMessageID: signalTextLocal,
		}); !errors.Is(err, messaging.ErrUnsupported) {
			t.Fatalf("signal MarkRead error = %v, want ErrUnsupported (capability honesty)", err)
		}
		t.Log("RECEIPT [signal/read] correctly ErrUnsupported (undeclared capability)")
	} else {
		t.Log("SKIP signal: supervisor did not come online")
	}

	// ---------------- GOOGLE ----------------
	if googleOK {
		sub := mustSubmit(service.SendText(context.Background(), messaging.SendTextCommand{
			CommonCommand: common(googleAccountID, "conv-google-self", "live-google-text"),
			Body:          fmt.Sprintf("OpenMessage v2 live verification -- text (%s)", stamp),
		}))
		delivery := dispatch("google/text", sub)
		if delivery.RemoteMessageID == "" {
			t.Fatal("google text confirmed without a TmpID remote id")
		}

		rsub := mustSubmit(service.SendReaction(context.Background(), messaging.SendReactionCommand{
			CommonCommand:   common(googleAccountID, "conv-google-self", "live-google-react"),
			TargetMessageID: anchorLocalID,
			Emoji:           "\U0001F44D",
			Action:          bridge.ReactionAdd,
		}))
		dispatch("google/reaction", rsub)

		ksub := mustSubmit(service.MarkRead(context.Background(), messaging.MarkReadCommand{
			CommonCommand:     common(googleAccountID, "conv-google-self", "live-google-read"),
			DeviceID:          "live-harness-device",
			LastReadMessageID: anchorLocalID,
		}))
		dispatch("google/read", ksub)

		msub := mustSubmit(service.SendMedia(context.Background(), messaging.SendMediaCommand{
			CommonCommand: common(googleAccountID, "conv-google-self", "live-google-media"),
			Content:       bytes.NewReader(tinyPNG(t)),
			Filename:      "om-live-verify.png",
			MIME:          "image/png",
			Caption:       fmt.Sprintf("OpenMessage v2 live verification -- media (%s)", stamp),
		}))
		dispatch("google/media", msub)
	} else {
		t.Log("SKIP google: supervisor did not come online (likely credentials need the app's repair flow)")
	}

	t.Logf("SUMMARY signalOK=%v googleOK=%v whatsapp=SKIPPED(unpaired)", signalOK, googleOK)
}

func common(account, conversation, key string) messaging.CommonCommand {
	return messaging.CommonCommand{
		AccountID:      account,
		ConversationID: conversation,
		IdempotencyKey: key + "-" + uuid.NewString(),
	}
}

func startSupervisor(t *testing.T, sup *bridge.Supervisor, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.Start(ctx, bridge.StartRequest{}); err != nil {
		t.Logf("%s supervisor Start: %v (state machine will report)", label, err)
	}
}

func stopSupervisor(t *testing.T, sup *bridge.Supervisor, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.Stop(ctx); err != nil {
		t.Logf("%s supervisor Stop: %v", label, err)
	}
}

func waitOnline(t *testing.T, sup *bridge.Supervisor, label string) bool {
	t.Helper()
	deadline := time.Now().Add(onlineWait)
	for time.Now().Before(deadline) {
		snapshot := sup.Snapshot()
		switch snapshot.State {
		case bridge.StateOnline:
			t.Logf("%s ONLINE (generation %d)", label, snapshot.Generation)
			return true
		case bridge.StateBlocked, bridge.StateUnpaired:
			t.Logf("%s parked %s (class=%s fp=%s) -- skipping", label, snapshot.State, snapshot.ErrorClass, snapshot.ErrorFingerprint)
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("%s did not come online within %s (last=%+v) -- skipping", label, onlineWait, sup.Snapshot())
	return false
}

func seedAccountAndConversation(t *testing.T, store *sqlite.Store, nowMS int64, accountID, conversationID, remote, title string) {
	t.Helper()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: accountID, BridgeKey: accountID, DisplayName: accountID,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: `{}`,
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(%s): %v", accountID, err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: conversationID, AccountID: accountID,
		RemoteConversationID: remote, Kind: sqlite.ConversationKindDirect,
		Title: title, NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: `{}`, CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(%s): %v", conversationID, err)
	}
}

// seedGoogleAnchorMessage inserts a v2 message row pointing at a REAL message
// in the operator's Google self-thread so reactions and read receipts target
// genuine remote state. Direction incoming + nil sender identity reads as
// self, matching the dispatch AuthorID contract.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func seedGoogleAnchorMessage(t *testing.T, dbPath string, nowMS int64, anchorRemoteID string) string {
	t.Helper()
	query := make(url.Values)
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath), RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatalf("open raw v2 store: %v", err)
	}
	defer raw.Close()
	localID := uuid.NewString()
	if _, err := raw.Exec(`
		INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id,
			direction, body, state, occurred_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, 'conv-google-self', ?, ?, 'incoming', 'live-verify anchor', 'active', ?, ?, ?)
	`, localID, googleAccountID, anchorRemoteID, nowMS, nowMS, nowMS); err != nil {
		t.Fatalf("seed google anchor message: %v", err)
	}
	return localID
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: uint8(32 * x), G: 200, B: uint8(32 * y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}
