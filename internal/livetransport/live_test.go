//go:build livetransport

// Package livetransport is the manual live-verification harness for the v2
// send pipeline (T1-T3 text, T5a reactions, T5b read receipts, M2b media).
// It drives the REAL transports against the operator's own live pairing state
// and sends ONLY to self-recipient threads. It never runs in CI (build tag)
// and must only run with the OpenMessage app quit (session safety) and with
// the operator's explicit go-ahead.
//
// Run:
//
//	GOWORK=off go test -tags livetransport -run TestLiveSelfSendVerification \
//	  -v -count=1 -timeout 10m ./internal/livetransport/
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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	liveDataDirEnv        = "OPENMESSAGES_DATA_DIR"
	googleAccountID       = "google-primary"
	signalAccountID       = "signal-primary"
	defaultGoogleSelfConvRemote  = "1770"  // live self-thread (canonical, per /api/new-conversation)
	defaultGoogleAnchorMessageID = "85269" // real anchor message sent via the legacy path
	signalSelfConvRemote  = "signal:+16506303657"
	onlineWait            = 90 * time.Second
	settleWait            = 60 * time.Second
)

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
