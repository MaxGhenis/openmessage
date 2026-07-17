package whatsapp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestIngressObserverStampsFrameAndRegistersBeforeConnect(t *testing.T) {
	client := newFakeLifecycleClient()
	observer := newIngressObserverHarness()
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
	a.observeIngress = observer.Observe
	connectSawObserver := make(chan bool, 1)
	a.connect = func(context.Context) error {
		connectSawObserver <- observer.Registered()
		return nil
	}
	sink := &ingressRecordingSink{}

	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 17,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, lifecycleRun) })

	if registered := receiveValue(t, connectSawObserver, "Connect observer state"); !registered {
		t.Fatal("Connect began before the ingress observer was registered")
	}
	frame := whatsapplive.IngressFrame{
		DedupeKey: "msg:stanza-1:0123abcd",
		ReceivedAt: whatsappAdapterTestEpoch.Add(
			3 * time.Second,
		),
		Payload: []byte(`{"kind":"message","message_id":"stanza-1"}`),
	}
	if !observer.Emit(frame) {
		t.Fatal("ingress observer was not installed")
	}

	records, contexts, ingressErrors := sink.Snapshot()
	want := bridge.RawIngressRecord{
		AccountID:    "whatsapp-primary",
		Generation:   17,
		DedupeKey:    frame.DedupeKey,
		Codec:        whatsapplive.IngressCodec,
		CodecVersion: whatsapplive.IngressCodecVersion,
		ReceivedAt:   frame.ReceivedAt,
		Payload:      frame.Payload,
	}
	if len(records) != 1 || !reflect.DeepEqual(records[0], want) {
		t.Fatalf("AppendIngress records = %#v, want [%#v]", records, want)
	}
	if len(contexts) != 1 || contexts[0] != context.Background() {
		t.Fatalf("AppendIngress context = %#v, want context.Background()", contexts)
	}
	if len(ingressErrors) != 0 {
		t.Fatalf("RecordIngressError calls = %v, want none", ingressErrors)
	}
	registrations, removals := observer.Counts()
	if registrations != 1 || removals != 0 {
		t.Fatalf("observer counts before Stop = (%d registrations, %d removals), want (1, 0)", registrations, removals)
	}
}

func TestIngressObserverUnregisteredOnStop(t *testing.T) {
	client := newFakeLifecycleClient()
	observer := newIngressObserverHarness()
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
	a.observeIngress = observer.Observe
	sink := &ingressRecordingSink{}

	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 2,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	retained := observer.Callback()
	if retained == nil {
		t.Fatal("Start() did not register an ingress observer")
	}
	stopRun(t, lifecycleRun)
	stopRun(t, lifecycleRun)

	registrations, removals := observer.Counts()
	if registrations != 1 || removals != 1 {
		t.Fatalf("observer counts after repeated Stop = (%d registrations, %d removals), want (1, 1)", registrations, removals)
	}
	if observer.Registered() {
		t.Fatal("ingress observer remains registered after Stop")
	}
	retained(whatsapplive.IngressFrame{
		DedupeKey:  "late",
		ReceivedAt: whatsappAdapterTestEpoch,
		Payload:    []byte(`{"kind":"receipt"}`),
	})
	records, _, _ := sink.Snapshot()
	if len(records) != 0 {
		t.Fatalf("late retained observer appended %d records after Stop, want zero", len(records))
	}
}

func TestIngressObserverUnregisteredOnTerminalEvent(t *testing.T) {
	client := newFakeLifecycleClient()
	observer := newIngressObserverHarness()
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
	a.observeIngress = observer.Observe

	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 3,
	}, &ingressRecordingSink{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, lifecycleRun) })

	a.handleLifecycleEvent(whatsapplive.LifecycleEvent{
		Raw: &waevents.Disconnected{},
		At:  whatsappAdapterTestEpoch,
	})
	select {
	case terminal := <-lifecycleRun.Done():
		if terminal == nil {
			t.Fatal("Done terminal error = nil after Disconnected event")
		}
	case <-time.After(whatsappAdapterTestTimeout):
		t.Fatal("timed out waiting for terminal generation completion")
	}

	registrations, removals := observer.Counts()
	if registrations != 1 || removals != 1 {
		t.Fatalf("observer counts after terminal event = (%d registrations, %d removals), want (1, 1)", registrations, removals)
	}
	if observer.Registered() {
		t.Fatal("ingress observer remains registered after terminal completion")
	}
}

func TestIngressAppendErrorsDoNotRetireGeneration(t *testing.T) {
	nonStale := errors.New("durable ingress unavailable")
	tests := []struct {
		name             string
		appendErr        error
		wantErrorRecords int
		wantLogs         int
	}{
		{
			name:      "stale generation is benign",
			appendErr: bridge.ErrStaleGeneration,
		},
		{
			name:             "other append error is counted and logged",
			appendErr:        nonStale,
			wantErrorRecords: 1,
			wantLogs:         1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeLifecycleClient()
			observer := newIngressObserverHarness()
			a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
			a.observeIngress = observer.Observe
			logs := &ingressLogCapture{}
			a.logIngressError = logs.Record
			sink := &ingressRecordingSink{appendErr: test.appendErr}

			lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
				AccountID:  "whatsapp-primary",
				Generation: 23,
			}, sink)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { stopRun(t, lifecycleRun) })
			if !observer.Emit(whatsapplive.IngressFrame{
				DedupeKey:  "frame",
				ReceivedAt: whatsappAdapterTestEpoch,
				Payload:    []byte(`{"kind":"message"}`),
			}) {
				t.Fatal("ingress observer was not installed")
			}

			select {
			case terminal := <-lifecycleRun.Done():
				t.Fatalf("generation completed after AppendIngress error: %v", terminal)
			default:
			}
			records, _, errorRecords := sink.Snapshot()
			if len(records) != 1 {
				t.Fatalf("AppendIngress call count = %d, want 1", len(records))
			}
			if len(errorRecords) != test.wantErrorRecords {
				t.Fatalf("RecordIngressError calls = %v, want %d", errorRecords, test.wantErrorRecords)
			}
			gotLogs := logs.Snapshot()
			if len(gotLogs) != test.wantLogs {
				t.Fatalf("LogIngressError calls = %+v, want %d", gotLogs, test.wantLogs)
			}
			if test.wantLogs == 1 {
				got := gotLogs[0]
				if !errors.Is(got.err, nonStale) || got.accountID != "whatsapp-primary" || got.generation != 23 {
					t.Fatalf("LogIngressError call = %+v, want error/account/generation %v/%q/%d", got, nonStale, "whatsapp-primary", 23)
				}
			}
		})
	}
}

func TestStopJoinsInFlightIngressAppend(t *testing.T) {
	client := newFakeLifecycleClient()
	observer := newIngressObserverHarness()
	a := newTestAdapter(t, client, func() time.Time { return whatsappAdapterTestEpoch })
	a.observeIngress = observer.Observe
	appendEntered := make(chan struct{})
	releaseAppend := make(chan struct{})
	sink := &ingressRecordingSink{
		append: func(context.Context, bridge.RawIngressRecord) error {
			close(appendEntered)
			<-releaseAppend
			return nil
		},
	}

	lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "whatsapp-primary",
		Generation: 29,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	callback := observer.Callback()
	if callback == nil {
		t.Fatal("Start() did not register an ingress observer")
	}
	appendDone := make(chan struct{})
	go func() {
		defer close(appendDone)
		callback(whatsapplive.IngressFrame{
			DedupeKey:  "blocking-frame",
			ReceivedAt: whatsappAdapterTestEpoch,
			Payload:    []byte(`{"kind":"history_sync"}`),
		})
	}()
	receiveValue(t, appendEntered, "in-flight AppendIngress")

	stopCtx, cancel := context.WithTimeout(context.Background(), whatsappAdapterTestTimeout)
	defer cancel()
	stopResult := make(chan error, 1)
	go func() { stopResult <- lifecycleRun.Stop(stopCtx) }()
	observer.AwaitRemoval(t)
	select {
	case err := <-stopResult:
		t.Fatalf("Stop() returned before in-flight AppendIngress completed: %v", err)
	default:
	}

	close(releaseAppend)
	receiveValue(t, appendDone, "AppendIngress completion")
	if err := receiveValue(t, stopResult, "Run.Stop completion"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

type ingressObserverHarness struct {
	mu            sync.Mutex
	nextID        uint64
	currentID     uint64
	callback      func(whatsapplive.IngressFrame)
	registrations int
	removals      int
	removed       chan struct{}
}

func newIngressObserverHarness() *ingressObserverHarness {
	return &ingressObserverHarness{removed: make(chan struct{})}
}

func (h *ingressObserverHarness) Observe(callback func(whatsapplive.IngressFrame)) func() {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.currentID = id
	h.callback = callback
	h.registrations++
	h.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			if h.currentID == id {
				h.currentID = 0
				h.callback = nil
				h.removals++
				close(h.removed)
			}
			h.mu.Unlock()
		})
	}
}

func (h *ingressObserverHarness) Callback() func(whatsapplive.IngressFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callback
}

func (h *ingressObserverHarness) Emit(frame whatsapplive.IngressFrame) bool {
	callback := h.Callback()
	if callback == nil {
		return false
	}
	callback(frame)
	return true
}

func (h *ingressObserverHarness) Registered() bool {
	return h.Callback() != nil
}

func (h *ingressObserverHarness) Counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registrations, h.removals
}

func (h *ingressObserverHarness) AwaitRemoval(t *testing.T) {
	t.Helper()
	select {
	case <-h.removed:
	case <-time.After(whatsappAdapterTestTimeout):
		t.Fatal("timed out waiting for ingress observer removal")
	}
}

type ingressRecordingSink struct {
	mu            sync.Mutex
	records       []bridge.RawIngressRecord
	contexts      []context.Context
	ingressErrors []string
	appendErr     error
	append        func(context.Context, bridge.RawIngressRecord) error
}

func (s *ingressRecordingSink) AppendIngress(ctx context.Context, record bridge.RawIngressRecord) error {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.contexts = append(s.contexts, ctx)
	appendFn := s.append
	appendErr := s.appendErr
	s.mu.Unlock()
	if appendFn != nil {
		return appendFn(ctx, record)
	}
	return appendErr
}

func (*ingressRecordingSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error {
	return nil
}

func (*ingressRecordingSink) Beat(bridge.Generation, time.Time, string) {}

func (s *ingressRecordingSink) RecordIngressError(accountID string) {
	s.mu.Lock()
	s.ingressErrors = append(s.ingressErrors, accountID)
	s.mu.Unlock()
}

func (s *ingressRecordingSink) Snapshot() (
	[]bridge.RawIngressRecord,
	[]context.Context,
	[]string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.RawIngressRecord(nil), s.records...),
		append([]context.Context(nil), s.contexts...),
		append([]string(nil), s.ingressErrors...)
}

type ingressLogCall struct {
	err        error
	accountID  string
	generation uint64
}

type ingressLogCapture struct {
	mu    sync.Mutex
	calls []ingressLogCall
}

func (l *ingressLogCapture) Record(err error, accountID string, generation uint64) {
	l.mu.Lock()
	l.calls = append(l.calls, ingressLogCall{
		err:        err,
		accountID:  accountID,
		generation: generation,
	})
	l.mu.Unlock()
}

func (l *ingressLogCapture) Snapshot() []ingressLogCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ingressLogCall(nil), l.calls...)
}

var _ bridge.ConnectionSink = (*ingressRecordingSink)(nil)
