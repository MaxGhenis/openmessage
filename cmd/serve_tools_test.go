package cmd

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestMCPV2Dependencies(t *testing.T) {
	if got := mcpV2Dependencies(nil); len(got) != 0 {
		t.Fatalf("mcpV2Dependencies(nil) returned %d dependencies, want zero", len(got))
	}

	service := &messaging.MessageService{}
	store := &sqlite.Store{}
	registry := bridge.NewRegistry()
	got := mcpV2Dependencies(&v2Stack{
		Service:  service,
		Store:    store,
		Registry: registry,
	})
	if len(got) != 1 {
		t.Fatalf("mcpV2Dependencies(stack) returned %d dependencies, want one", len(got))
	}
	deps := got[0]
	if deps == nil {
		t.Fatal("mcpV2Dependencies(stack) returned a nil dependency")
	}
	if !deps.Enabled {
		t.Fatal("mcpV2Dependencies(stack) returned disabled dependencies")
	}
	if deps.Service != service {
		t.Fatal("mcpV2Dependencies(stack) did not preserve the message service pointer")
	}
	if deps.V2Store != store {
		t.Fatal("mcpV2Dependencies(stack) did not preserve the v2 store pointer")
	}
	if deps.Registry != registry {
		t.Fatal("mcpV2Dependencies(stack) did not preserve the registry pointer")
	}
}

func TestV2SendWebOptionsRequiresSendFlag(t *testing.T) {
	if got := v2SendWebOptions(nil, true); got != nil {
		t.Fatal("v2SendWebOptions(nil, true) returned enabled options")
	}

	service := &messaging.MessageService{}
	store := &sqlite.Store{}
	registry := bridge.NewRegistry()
	stack := &v2Stack{
		Service:  service,
		Store:    store,
		Registry: registry,
	}
	if got := v2SendWebOptions(stack, false); got != nil {
		t.Fatal("v2SendWebOptions(stack, false) enabled v2 send routes")
	}

	got := v2SendWebOptions(stack, true)
	if got == nil {
		t.Fatal("v2SendWebOptions(stack, true) returned nil")
	}
	if got.Service != service || got.V2Store != store || got.Registry != registry {
		t.Fatal("v2SendWebOptions(stack, true) did not preserve stack dependencies")
	}
}

func TestV2IngestCountersProviderRequiresStack(t *testing.T) {
	if got := v2IngestCountersProvider(nil); got != nil {
		t.Fatal("v2IngestCountersProvider(nil) returned an enabled provider")
	}

	stack, err := newV2Stack(v2StackDeps{
		Logger:  zerolog.Nop(),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newV2Stack(): %v", err)
	}
	t.Cleanup(func() {
		if err := stack.Store.Close(); err != nil {
			t.Errorf("close v2 store: %v", err)
		}
	})

	provider := v2IngestCountersProvider(stack)
	if provider == nil {
		t.Fatal("v2IngestCountersProvider(stack) returned nil")
	}
	if got := provider(); got == nil || len(got) != 0 {
		t.Fatalf("provider() = %#v, want an empty per-account snapshot", got)
	}

	stack.Sink.RecordIngressError("signal:primary")
	got := provider()
	if snapshot, ok := got["signal:primary"]; !ok || snapshot.AppendErrors != 1 {
		t.Fatalf("provider() = %#v, want live signal:primary append_errors=1", got)
	}
}
