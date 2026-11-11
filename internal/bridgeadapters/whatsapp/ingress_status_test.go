package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// TestIngressObserverReportsWhetherTheFrameWasCommitted pins what the adapter
// tells the bridge for each outcome, which the bridge passes to whatsmeow as
// the message's handled status: nil once committed, and the reason otherwise,
// so an uncommitted message is redelivered to a later generation.
func TestIngressObserverReportsWhetherTheFrameWasCommitted(t *testing.T) {
	frame := whatsapplive.IngressFrame{
		DedupeKey:  "msg:status:0123abcd",
		ReceivedAt: whatsappAdapterTestEpoch,
		Payload:    []byte(`{"kind":"message","message_id":"status"}`),
	}
	diskErr := errors.New("disk I/O error")
	for _, test := range []struct {
		name          string
		appendErr     error
		wantErr       error
		wantRecorded  int
		wantCommitted int
	}{
		{name: "committed", wantCommitted: 1},
		{name: "stale generation", appendErr: bridge.ErrStaleGeneration, wantErr: bridge.ErrStaleGeneration},
		{name: "append failed", appendErr: diskErr, wantErr: diskErr, wantRecorded: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newIngressObserverHarness()
			a := newTestAdapter(t, newFakeLifecycleClient(), func() time.Time { return whatsappAdapterTestEpoch })
			a.observeIngress = observer.Observe
			sink := &ingressRecordingSink{appendErr: test.appendErr}
			lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "whatsapp-primary", Generation: 5}, sink)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { stopRun(t, lifecycleRun) })

			if !observer.durable {
				t.Fatal("a generation with a durable sink registered a non-durable ingress observer")
			}
			if err := observer.Callback()(frame); !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
				t.Fatalf("observer result = %v, want %v", err, test.wantErr)
			}
			records, _, ingressErrors := sink.Snapshot()
			if len(records) != 1 {
				t.Fatalf("AppendIngress calls = %d, want 1", len(records))
			}
			if len(ingressErrors) != test.wantRecorded {
				t.Fatalf("RecordIngressError calls = %d, want %d", len(ingressErrors), test.wantRecorded)
			}
		})
	}

	t.Run("generation retired", func(t *testing.T) {
		observer := newIngressObserverHarness()
		a := newTestAdapter(t, newFakeLifecycleClient(), func() time.Time { return whatsappAdapterTestEpoch })
		a.observeIngress = observer.Observe
		sink := &ingressRecordingSink{}
		lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "whatsapp-primary", Generation: 6}, sink)
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		retained := observer.Callback()
		stopRun(t, lifecycleRun)

		if err := retained(frame); !errors.Is(err, errIngressAdmissionClosed) {
			t.Fatalf("observer result after Stop = %v, want errIngressAdmissionClosed", err)
		}
		if records, _, _ := sink.Snapshot(); len(records) != 0 {
			t.Fatalf("a retired generation appended %d records", len(records))
		}
	})

	t.Run("legacy-only sink", func(t *testing.T) {
		for _, appendErr := range []error{nil, bridge.ErrStaleGeneration, diskErr} {
			observer := newIngressObserverHarness()
			a := newTestAdapter(t, newFakeLifecycleClient(), func() time.Time { return whatsappAdapterTestEpoch })
			a.observeIngress = observer.Observe
			sink := &legacyOnlyIngressSink{ingressRecordingSink: ingressRecordingSink{appendErr: appendErr}}
			lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "whatsapp-primary", Generation: 8}, sink)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if observer.durable {
				t.Fatal("a legacy-only generation registered a durable ingress observer")
			}
			retained := observer.Callback()
			if err := retained(frame); err != nil {
				t.Fatalf("legacy-only observer result with append error %v = %v, want nil (nothing durable can commit it)", appendErr, err)
			}
			stopRun(t, lifecycleRun)
			if err := retained(frame); err != nil {
				t.Fatalf("legacy-only observer result after Stop = %v, want nil", err)
			}
		}
	})

	t.Run("no sink", func(t *testing.T) {
		observer := newIngressObserverHarness()
		a := newTestAdapter(t, newFakeLifecycleClient(), func() time.Time { return whatsappAdapterTestEpoch })
		a.observeIngress = observer.Observe
		lifecycleRun, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "whatsapp-primary", Generation: 7}, nil)
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		t.Cleanup(func() { stopRun(t, lifecycleRun) })
		if observer.durable {
			t.Fatal("a generation without a sink registered a durable ingress observer")
		}
		if err := observer.Callback()(frame); err != nil {
			t.Fatalf("observer result without a sink = %v, want nil (nothing can commit it)", err)
		}
	})
}

// legacyOnlyIngressSink is a ConnectionSink that reports, as the supervisor's
// generation sink does in legacy-only mode, that it commits nothing durably.
type legacyOnlyIngressSink struct {
	ingressRecordingSink
}

func (*legacyOnlyIngressSink) DurableIngress() bool { return false }
