package cmd

import (
	"testing"

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
