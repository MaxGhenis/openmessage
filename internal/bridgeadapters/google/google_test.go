package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestEventFailureClassification(t *testing.T) {
	a := New("google-primary", nil, func() bool { return true })
	r := &run{adapter: a}

	tests := []struct {
		name      string
		event     any
		terminal  bool
		wantClass bridge.FailureClass
	}{
		{
			name:      "listen fatal transient",
			event:     &events.ListenFatalError{Error: errors.New("temporary token refresh failure")},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:      "listen fatal without cause is transient",
			event:     &events.ListenFatalError{},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:      "listen fatal auth expiry requires credential repair",
			event:     &events.ListenFatalError{Error: errors.New("HTTP 401: invalid authentication credentials")},
			terminal:  true,
			wantClass: bridge.FailureCredentialsExpired,
		},
		{
			name:      "Gaia logout requires reauth",
			event:     &events.GaiaLoggedOut{},
			terminal:  true,
			wantClass: bridge.FailureReauthRequired,
		},
		{
			name:     "second ping is tolerated",
			event:    &events.PingFailed{ErrorCount: 2},
			terminal: false,
		},
		{
			name:      "third ping ends generation",
			event:     &events.PingFailed{ErrorCount: 3, Error: errors.New("phone ping failed")},
			terminal:  true,
			wantClass: bridge.FailureTransient,
		},
		{
			name:     "phone offline is not credential failure",
			event:    &events.PhoneNotResponding{},
			terminal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure, terminal := r.classifyEvent(test.event)
			if terminal != test.terminal {
				t.Fatalf("terminal = %v, want %v", terminal, test.terminal)
			}
			if failure.Class != test.wantClass {
				t.Fatalf("class = %q, want %q", failure.Class, test.wantClass)
			}
		})
	}
}

func TestAuthExpiredWithoutRepairIsBlocked(t *testing.T) {
	a := New("google-primary", nil, func() bool { return false })
	failure := a.classifyTransportError(
		errors.New("HTTP 401: invalid authentication credentials"),
		"connect",
		"connect_failed",
	)
	if failure.Class != bridge.FailureUpgradeRequired {
		t.Fatalf("class = %q, want %q", failure.Class, bridge.FailureUpgradeRequired)
	}
}

func TestReadyUsesForkRealPingerSignal(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{probeResponse: make(chan *libgm.IncomingRPCMessage, 1)}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}

	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	assertNotClosed(t, run.Ready(), "Connect return")
	fake.emit(&events.AuthTokenRefreshed{})
	assertNotClosed(t, run.Ready(), "AuthTokenRefreshed")

	fake.emit(&events.PhoneNotResponding{})
	select {
	case <-run.Ready():
	case <-time.After(time.Second):
		t.Fatal("Ready did not close after the fork's PhoneNotResponding pinger signal")
	}
	if !host.Connected.Load() {
		t.Fatal("compatibility Connected status was not published on readiness")
	}
	if host.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneNotResponding readiness evidence was mistaken for phone reachability")
	}
	if got := sink.beatCount(); got != 1 {
		t.Fatalf("liveness beats = %d, want 1", got)
	}
}

func TestReadyUsesForkRealLongPollDataEvidence(t *testing.T) {
	tests := []struct {
		name  string
		event any
	}{
		{name: "no data pinger signal", event: &events.NoDataReceived{}},
		{name: "phone responding pinger signal", event: &events.PhoneRespondingAgain{}},
		{name: "conversation", event: &gmproto.Conversation{ConversationID: "ready-conversation"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestApp(t)
			fake := &fakeTransport{}
			a := newTestAdapter(t, host, fake)
			run, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "google-primary",
				Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { stopRun(t, run) })

			fake.emit(test.event)
			select {
			case <-run.Ready():
			case <-time.After(time.Second):
				t.Fatalf("Ready did not close after %T", test.event)
			}
		})
	}
}

func TestLateCallbackCannotMutateNewGeneration(t *testing.T) {
	host := newTestApp(t)
	firstTransport := &fakeTransport{}
	secondTransport := &fakeTransport{}
	transports := []*fakeTransport{firstTransport, secondTransport}
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		transport := transports[0]
		transports = transports[1:]
		return newLegacyClient(t), transport, nil
	}

	first, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	firstHandler := firstTransport.currentHandler()
	firstTransport.emit(&events.PhoneNotResponding{})
	<-first.Ready()
	a.ReportError(errors.New("temporary disconnect"))
	terminalError(t, first.Done(), bridge.FailureTransient)

	second, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 2}, nil)
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, second) })
	secondTransport.emit(&gmproto.Conversation{ConversationID: "generation-2-ready"})
	<-second.Ready()
	current := host.GetClient()

	firstHandler(&events.GaiaLoggedOut{})
	if host.GetClient() != current {
		t.Fatal("late generation-1 callback replaced or cleared generation-2 client")
	}
	if !host.Connected.Load() {
		t.Fatal("late generation-1 callback marked generation 2 disconnected")
	}
	if !host.GooglePaired() {
		t.Fatal("late generation-1 callback deleted current stored session")
	}
}

func TestRecoveredHandlerPanicEndsGenerationTransient(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	host.EventHandler.OnSessionInvalid = func() { panic("malformed Google event") }
	fake.emit(&events.GaiaLoggedOut{})
	terminalError(t, run.Done(), bridge.FailureTransient)
	if host.Connected.Load() {
		t.Fatal("recovered callback panic left Google connected")
	}
}

func TestParkCurrentClearsConnectedAndSurfacesNeedsRepair(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	fake.emit(&gmproto.Conversation{ConversationID: "park-ready"})
	<-run.Ready()
	if !host.Connected.Load() {
		t.Fatal("precondition: generation did not publish connected status")
	}

	if !a.ParkCurrent(errors.New("linked device requires repair")) {
		t.Fatal("ParkCurrent() did not terminate the active generation")
	}
	terminalError(t, run.Done(), bridge.FailureUpgradeRequired)
	if host.Connected.Load() {
		t.Fatal("parked generation left compatibility Connected=true")
	}
	if !host.GoogleStatus().NeedsRepair {
		t.Fatal("parked generation did not surface needs_repair")
	}
}

func TestStopWaitsForAdmittedCallback(t *testing.T) {
	host := newTestApp(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	host.OnStatusChange = func(connected bool) {
		if connected {
			close(entered)
			<-release
		}
	}
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	go fake.emit(&gmproto.Conversation{ConversationID: "stop-ready"})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := run.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline while callback is admitted", err)
	}
	close(release)
	stopRun(t, run)
	if fake.disconnectCount() != 1 {
		t.Fatalf("Disconnect calls = %d, want 1", fake.disconnectCount())
	}
}

func TestProbeCompletionCannotConsumeRunTerminalError(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{probeResponse: make(chan *libgm.IncomingRPCMessage)}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	probeDone := make(chan error, 1)
	go func() {
		_, probeErr := run.Probe(context.Background())
		probeDone <- probeErr
	}()
	a.ReportError(errors.New("temporary disconnect"))
	select {
	case err := <-probeDone:
		if err == nil {
			t.Fatal("Probe unexpectedly succeeded after generation ended")
		}
	case <-time.After(time.Second):
		t.Fatal("Probe did not stop with its generation")
	}
	terminalError(t, run.Done(), bridge.FailureTransient)
}

func TestProbeWithoutProtocolActivityStillTimesOut(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, probeErr := run.Probe(ctx); !errors.Is(probeErr, context.DeadlineExceeded) {
		t.Fatalf("Probe error = %v, want deadline with no pinger or long-poll activity", probeErr)
	}
}

func TestPhoneUnreachableSurvivesRepeatedLivenessWindowsAndRecovers(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	// The pinned fork emits this after its first ditto ping times out. It is
	// readiness evidence for the linked-device session, but not evidence that
	// the Android phone itself is reachable.
	fake.emit(&events.PhoneNotResponding{})
	select {
	case <-run.Ready():
	case <-time.After(time.Second):
		t.Fatal("PhoneNotResponding did not make the generation ready")
	}
	if host.GoogleStatus().PhoneResponding {
		t.Fatal("precondition: phone should be recorded as unreachable")
	}

	// Model three consecutive supervisor liveness windows. NotifyDittoActivity
	// has no phone response in any window, but the known phone-off state keeps
	// the healthy generation alive instead of triggering reconnect churn.
	for window := 1; window <= 3; window++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		liveness, probeErr := run.Probe(ctx)
		cancel()
		if probeErr != nil {
			t.Fatalf("Probe window %d error = %v", window, probeErr)
		}
		if liveness.Detail != "phone_not_responding" {
			t.Fatalf("Probe window %d detail = %q, want phone_not_responding", window, liveness.Detail)
		}
		select {
		case err := <-run.Done():
			t.Fatalf("generation ended during phone-off window %d: %v", window, err)
		default:
		}
	}
	if got := fake.probeCount(); got != 3 {
		t.Fatalf("NotifyDittoActivity calls = %d, want 3", got)
	}

	// Because the generation and its callback remain installed, the fork's
	// recovery signal is still handled and renews liveness/reconciliation.
	fake.emit(&events.PhoneRespondingAgain{})
	if !host.GoogleStatus().PhoneResponding {
		t.Fatal("PhoneRespondingAgain was ignored after repeated phone-off probes")
	}
	if got := sink.beatCount(); got != 2 {
		t.Fatalf("liveness beats = %d, want 2 (offline and recovery)", got)
	}
	select {
	case err := <-run.Done():
		t.Fatalf("generation ended instead of accepting PhoneRespondingAgain: %v", err)
	default:
	}
}

func TestIngressTeeMessageFramesUseContentHash(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 7,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	message := &gmproto.Message{
		MessageID:      "message-1",
		ConversationID: "conversation-1",
		Timestamp:      1_234_000,
	}
	frame := &libgm.WrappedMessage{Message: message, IsOld: false}
	fake.emit(frame)
	fake.emit(frame)

	changed := proto.Clone(message).(*gmproto.Message)
	changed.MessageStatus = &gmproto.MessageStatus{SubCode: 1}
	fake.emit(&libgm.WrappedMessage{Message: changed, IsOld: false})

	records := sink.ingressRecords()
	if len(records) != 3 {
		t.Fatalf("AppendIngress calls = %d, want 3", len(records))
	}
	first := records[0]
	if first.AccountID != "google-primary" || first.Generation != 7 {
		t.Fatalf("frame ownership = (%q, %d), want (google-primary, 7)", first.AccountID, first.Generation)
	}
	if first.Codec != "google.protobuf" || first.CodecVersion != 1 {
		t.Fatalf("codec = %q v%d, want google.protobuf v1", first.Codec, first.CodecVersion)
	}
	if first.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt was not stamped at the tee")
	}

	var envelope struct {
		Kind  string `json:"kind"`
		Proto []byte `json:"proto_b64"`
		IsOld *bool  `json:"is_old"`
	}
	if err := json.Unmarshal(first.Payload, &envelope); err != nil {
		t.Fatalf("decode message envelope: %v", err)
	}
	if envelope.Kind != "message" || envelope.IsOld == nil || *envelope.IsOld {
		t.Fatalf("message envelope metadata = kind %q is_old %v", envelope.Kind, envelope.IsOld)
	}
	var roundTrip gmproto.Message
	if err := proto.Unmarshal(envelope.Proto, &roundTrip); err != nil {
		t.Fatalf("unmarshal teed message proto: %v", err)
	}
	if !proto.Equal(&roundTrip, message) {
		t.Fatalf("teed proto = %v, want %v", &roundTrip, message)
	}
	digest := sha256.Sum256(envelope.Proto)
	wantKey := "msg:message-1:" + hex.EncodeToString(digest[:4])
	if first.DedupeKey != wantKey {
		t.Fatalf("dedupe key = %q, want %q", first.DedupeKey, wantKey)
	}
	if records[1].DedupeKey != first.DedupeKey || !bytes.Equal(records[1].Payload, first.Payload) {
		t.Fatal("an exact frame replay did not produce the same dedupe key and payload")
	}
	if records[2].DedupeKey == first.DedupeKey {
		t.Fatal("same MessageID with changed status content reused the replay dedupe key")
	}
}

func TestIngressTeeThroughRealSinkDedupesExactFrames(t *testing.T) {
	host := newTestApp(t)
	storePath := filepath.Join(t.TempDir(), "v2.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now
	nowMS := now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google_messages",
		DisplayName: "Google Messages",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:     store,
		Messages:  messages,
		Reactions: reactions,
		Counters:  counters,
		Logger:    zerolog.Nop(),
		Decoders: []ingest.DecoderRegistration{{
			Codec:    ingest.GoogleCodec,
			Platform: bridge.PlatformGoogle,
			Decoder:  ingest.NewGoogleDecoder(counters),
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		select {
		case runErr := <-workerDone:
			if runErr != nil {
				t.Errorf("Worker.Run(): %v", runErr)
			}
		case <-time.After(time.Second):
			t.Error("Worker.Run() did not stop")
		}
	})

	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 21,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	message := &gmproto.Message{
		MessageID:      "real-sink-message",
		ConversationID: "real-sink-conversation",
		Timestamp:      1_750_000_000_000_000,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		SenderParticipant: &gmproto.Participant{
			FullName: "Ada",
			ID:       &gmproto.SmallInfo{Number: "+15551234567"},
		},
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "same body"},
			},
		}},
	}
	frame := &libgm.WrappedMessage{Message: message}
	fake.emit(frame)
	fake.emit(frame)
	changed := proto.Clone(message).(*gmproto.Message)
	changed.MessageStatus.SubCode = 1
	fake.emit(&libgm.WrappedMessage{Message: changed})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := counters.Snapshot("google-primary")
		if snapshot.Appended == 2 && snapshot.Deduped == 1 && snapshot.Projected >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshot := counters.Snapshot("google-primary")
	if snapshot.Appended != 2 || snapshot.Deduped != 1 || snapshot.Projected < 2 {
		t.Fatalf("real sink counters = %+v", snapshot)
	}

	inspection, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	defer inspection.Close()
	for table, want := range map[string]int{"inbox": 2, "messages": 1} {
		var got int
		if err := inspection.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}

func TestIngressTeeConversationEnvelope(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 4,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	conversation := &gmproto.Conversation{
		ConversationID: "conversation-1",
		Name:           "Family",
		IsGroupChat:    true,
	}
	fake.emit(conversation)

	records := sink.ingressRecords()
	if len(records) != 1 {
		t.Fatalf("AppendIngress calls = %d, want 1", len(records))
	}
	var envelope struct {
		Kind  string          `json:"kind"`
		Proto []byte          `json:"proto_b64"`
		IsOld json.RawMessage `json:"is_old"`
	}
	if err := json.Unmarshal(records[0].Payload, &envelope); err != nil {
		t.Fatalf("decode conversation envelope: %v", err)
	}
	if envelope.Kind != "conversation" {
		t.Fatalf("envelope kind = %q, want conversation", envelope.Kind)
	}
	if envelope.IsOld != nil {
		t.Fatalf("conversation envelope unexpectedly included is_old: %s", envelope.IsOld)
	}
	var roundTrip gmproto.Conversation
	if err := proto.Unmarshal(envelope.Proto, &roundTrip); err != nil {
		t.Fatalf("unmarshal teed conversation proto: %v", err)
	}
	if !proto.Equal(&roundTrip, conversation) {
		t.Fatalf("teed proto = %v, want %v", &roundTrip, conversation)
	}
	digest := sha256.Sum256(envelope.Proto)
	wantKey := "conv:conversation-1:" + hex.EncodeToString(digest[:4])
	if records[0].DedupeKey != wantKey {
		t.Fatalf("dedupe key = %q, want %q", records[0].DedupeKey, wantKey)
	}
}

func TestIngressTeeRoutesTypingButNotLifecycleEvents(t *testing.T) {
	host := newTestApp(t)
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 9,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	fake.emit(&gmproto.TypingData{
		ConversationID: "conversation-typing",
		User:           &gmproto.User{Number: "+15551234567"},
		Type:           gmproto.TypingTypes_STARTED_TYPING,
	})
	fake.emit(&events.PhoneRespondingAgain{})

	if records := sink.ingressRecords(); len(records) != 0 {
		t.Fatalf("durable ingress records = %d, want 0 for typing/lifecycle events", len(records))
	}
	ephemeral := sink.ephemeralEvents()
	if len(ephemeral) != 1 {
		t.Fatalf("EmitEphemeral calls = %d, want 1", len(ephemeral))
	}
	got := ephemeral[0]
	if got.AccountID != "google-primary" || got.Generation != 9 || got.Typing == nil {
		t.Fatalf("ephemeral ownership/payload = %#v", got)
	}
	if got.Typing.RemoteConversationID != "conversation-typing" ||
		got.Typing.Actor.Raw != "+15551234567" || !got.Typing.Typing {
		t.Fatalf("typing payload = %#v", got.Typing)
	}
}

func TestIngressTeeAppendErrorDoesNotInterruptLegacyHandler(t *testing.T) {
	host := newTestApp(t)
	legacyHandled := 0
	host.OnConversationsChange = func() { legacyHandled++ }
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{appendErr: errors.New("inbox unavailable")}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 12,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	fake.emit(&gmproto.Conversation{ConversationID: "legacy-still-runs", Name: "Legacy"})

	if legacyHandled != 1 {
		t.Fatalf("legacy conversation callbacks = %d, want 1", legacyHandled)
	}
	if got := a.IngressErrorCount(); got != 1 {
		t.Fatalf("recorded ingress errors = %d, want 1", got)
	}
	if records := sink.ingressRecords(); len(records) != 1 {
		t.Fatalf("AppendIngress calls = %d, want 1", len(records))
	}
	select {
	case terminal := <-run.Done():
		t.Fatalf("tee error ended the generation: %v", terminal)
	default:
	}
}

func TestIngressTeeAppendPanicDoesNotInterruptLegacyHandler(t *testing.T) {
	host := newTestApp(t)
	legacyHandled := 0
	host.OnConversationsChange = func() { legacyHandled++ }
	fake := &fakeTransport{}
	a := newTestAdapter(t, host, fake)
	sink := &recordingSink{appendPanic: "inbox panic"}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 13,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	fake.emit(&gmproto.Conversation{ConversationID: "legacy-survives-panic", Name: "Legacy"})

	if legacyHandled != 1 {
		t.Fatalf("legacy conversation callbacks = %d, want 1", legacyHandled)
	}
	if got := a.IngressErrorCount(); got != 1 {
		t.Fatalf("recorded ingress errors = %d, want 1", got)
	}
	select {
	case terminal := <-run.Done():
		t.Fatalf("tee panic ended the generation: %v", terminal)
	default:
	}
}

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("OPENMESSAGES_DATA_DIR", t.TempDir())
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	host, err := app.New(zerolog.Nop())
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}
	if err := client.SaveSession(host.SessionPath, &client.SessionData{AuthDataJSON: []byte(`{}`)}); err != nil {
		host.Close()
		t.Fatalf("SaveSession() error = %v", err)
	}
	t.Cleanup(host.Close)
	return host
}

func newLegacyClient(t *testing.T) *client.Client {
	t.Helper()
	legacy, err := client.NewFromSession(
		&client.SessionData{AuthDataJSON: []byte(`{}`)},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewFromSession() error = %v", err)
	}
	return legacy
}

func newTestAdapter(t *testing.T, host *app.App, fake *fakeTransport) *Adapter {
	t.Helper()
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		return newLegacyClient(t), fake, nil
	}
	return a
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("Ready closed after %s without a real long-poll event", after)
	default:
	}
}

func terminalError(t *testing.T, done <-chan error, want bridge.FailureClass) {
	t.Helper()
	select {
	case err := <-done:
		failure, ok := asOpError(err)
		if !ok || failure.Class != want {
			t.Fatalf("Done error = %v, want class %q", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Run.Done")
	}
}

func stopRun(t *testing.T, run bridge.Run) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := run.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

type fakeTransport struct {
	mu            sync.Mutex
	handler       libgm.EventHandler
	connectErr    error
	disconnects   int
	probeResponse chan *libgm.IncomingRPCMessage
	probeErr      error
	probes        int
}

func (f *fakeTransport) SetEventHandler(handler libgm.EventHandler) {
	f.mu.Lock()
	f.handler = handler
	f.mu.Unlock()
}

func (f *fakeTransport) Connect() error { return f.connectErr }

func (f *fakeTransport) Disconnect() {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
}

func (f *fakeTransport) NotifyDittoActivity() (<-chan *libgm.IncomingRPCMessage, error) {
	f.mu.Lock()
	f.probes++
	response, err := f.probeResponse, f.probeErr
	f.mu.Unlock()
	return response, err
}

func (f *fakeTransport) emit(event any) {
	if handler := f.currentHandler(); handler != nil {
		handler(event)
	}
}

func (f *fakeTransport) currentHandler() libgm.EventHandler {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	return handler
}

func (f *fakeTransport) disconnectCount() int {
	f.mu.Lock()
	count := f.disconnects
	f.mu.Unlock()
	return count
}

func (f *fakeTransport) probeCount() int {
	f.mu.Lock()
	count := f.probes
	f.mu.Unlock()
	return count
}

type recordingSink struct {
	mu           sync.Mutex
	beats        int
	records      []bridge.RawIngressRecord
	ephemeral    []bridge.EphemeralEvent
	appendErr    error
	appendPanic  any
	ephemeralErr error
}

func (s *recordingSink) AppendIngress(_ context.Context, record bridge.RawIngressRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.Payload = bytes.Clone(record.Payload)
	s.records = append(s.records, record)
	if s.appendPanic != nil {
		panic(s.appendPanic)
	}
	return s.appendErr
}

func (s *recordingSink) EmitEphemeral(_ context.Context, event bridge.EphemeralEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ephemeral = append(s.ephemeral, event)
	return s.ephemeralErr
}

func (s *recordingSink) Beat(bridge.Generation, time.Time, string) {
	s.mu.Lock()
	s.beats++
	s.mu.Unlock()
}

func (s *recordingSink) beatCount() int {
	s.mu.Lock()
	count := s.beats
	s.mu.Unlock()
	return count
}

func (s *recordingSink) ingressRecords() []bridge.RawIngressRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]bridge.RawIngressRecord, len(s.records))
	for i, record := range s.records {
		record.Payload = bytes.Clone(record.Payload)
		records[i] = record
	}
	return records
}

func (s *recordingSink) ephemeralEvents() []bridge.EphemeralEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.EphemeralEvent(nil), s.ephemeral...)
}
