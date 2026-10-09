package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/web"
)

// TestSupervisedAppendFailureReachesStatusAppendErrors drives the daemon's ingest
// wiring end to end: the v2 stack newV2Stack builds, a bridge.Supervisor that
// holds stack.Sink exactly as serve.go wires it, and /api/status served from
// v2IngestCountersProvider. Adapters only ever see the supervisor's
// per-generation sink, so a failed durable append must be countable through
// that sink and land in v2_ingest.per_account.<account>.append_errors.
func TestSupervisedAppendFailureReachesStatusAppendErrors(t *testing.T) {
	tests := []struct {
		accountID string
		platform  bridge.Platform
		policy    bridge.Policy
	}{
		{accountID: googleAccountID, platform: bridge.PlatformGoogle, policy: googleSupervisorPolicy()},
		{accountID: whatsappAccountID, platform: bridge.PlatformWhatsApp, policy: whatsappSupervisorPolicy()},
		{accountID: signalAccountID, platform: bridge.PlatformSignal, policy: signalSupervisorPolicy()},
	}
	for _, test := range tests {
		t.Run(string(test.platform), func(t *testing.T) {
			dataDir := t.TempDir()
			stack, err := newV2Stack(v2StackDeps{Logger: zerolog.Nop(), DataDir: dataDir})
			if err != nil {
				t.Fatalf("newV2Stack(): %v", err)
			}
			t.Cleanup(func() {
				if err := stack.Store.Close(); err != nil {
					t.Errorf("close v2 store: %v", err)
				}
			})
			adapter := scripted.New(test.accountID, test.platform)
			if err := stack.RegisterAdapter(adapter); err != nil {
				t.Fatalf("RegisterAdapter(): %v", err)
			}

			supervisor, err := bridge.NewSupervisor(
				test.accountID,
				test.platform,
				adapter,
				test.policy,
				googleWallClock{},
				googleRandom{},
				bridge.WithConnectionSink(stack.Sink),
			)
			if err != nil {
				t.Fatalf("NewSupervisor(): %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := supervisor.Stop(ctx); err != nil {
					t.Errorf("Supervisor.Stop(): %v", err)
				}
			})
			startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelStart()
			if err := supervisor.Start(startCtx, bridge.StartRequest{}); err != nil {
				t.Fatalf("Supervisor.Start(): %v", err)
			}
			sink, generation := awaitScriptedGenerationSink(t, adapter)

			ctx := context.Background()
			if err := sink.AppendIngress(ctx, appendErrorsTestRecord(test.accountID, generation, "delivered")); err != nil {
				t.Fatalf("control AppendIngress(): %v", err)
			}

			// Make every later inbox insert fail inside SQLite, the way a full
			// disk or a corrupt page would, without touching any other table.
			inspection, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "v2", "store.sqlite3")+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatalf("open v2 store for fault injection: %v", err)
			}
			defer inspection.Close()
			if _, err := inspection.Exec(`
				CREATE TRIGGER reject_inbox_append BEFORE INSERT ON inbox
				BEGIN SELECT RAISE(ABORT, 'simulated inbox write fault'); END
			`); err != nil {
				t.Fatalf("install inbox fault trigger: %v", err)
			}

			appendErr := sink.AppendIngress(ctx, appendErrorsTestRecord(test.accountID, generation, "lost"))
			if appendErr == nil {
				t.Fatal("AppendIngress() succeeded with the inbox fault trigger installed")
			}
			if errors.Is(appendErr, bridge.ErrStaleGeneration) {
				t.Fatalf("AppendIngress() = %v, want a durable append fault, not a fence rejection", appendErr)
			}
			// This is what every adapter does with a non-stale append error.
			recorder, ok := sink.(bridge.IngressErrorRecorder)
			if !ok {
				t.Fatalf("supervisor sink %T cannot record ingest faults; append_errors would stay 0", sink)
			}
			recorder.RecordIngressError(test.accountID)

			got := statusV2IngestCounters(t, stack)[test.accountID]
			if got.Appended != 1 || got.AppendErrors != 1 {
				t.Fatalf("/api/status v2_ingest.per_account[%q] = %+v, want appended=1 append_errors=1", test.accountID, got)
			}
		})
	}
}

func awaitScriptedGenerationSink(t *testing.T, adapter *scripted.Adapter) (bridge.ConnectionSink, bridge.Generation) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink := adapter.Sink(); sink != nil {
			requests := adapter.StartRequests()
			return sink, requests[len(requests)-1].Generation
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("supervisor never started a generation on the scripted adapter")
	return nil, 0
}

func appendErrorsTestRecord(accountID string, generation bridge.Generation, dedupeKey string) bridge.RawIngressRecord {
	return bridge.RawIngressRecord{
		AccountID:    accountID,
		Generation:   generation,
		DedupeKey:    dedupeKey,
		Codec:        "append-errors-test",
		CodecVersion: 1,
		ReceivedAt:   time.Now(),
		Payload:      []byte(`{}`),
	}
}

func statusV2IngestCounters(t *testing.T, stack *v2Stack) map[string]ingest.CounterSnapshot {
	t.Helper()
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer legacy.Close()
	handler := web.APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, web.APIOptions{
		V2IngestCounters: v2IngestCountersProvider(stack),
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		V2Ingest struct {
			Enabled    bool                              `json:"enabled"`
			PerAccount map[string]ingest.CounterSnapshot `json:"per_account"`
		} `json:"v2_ingest"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	if !payload.V2Ingest.Enabled {
		t.Fatal("/api/status v2_ingest.enabled = false with a v2 stack")
	}
	return payload.V2Ingest.PerAccount
}
