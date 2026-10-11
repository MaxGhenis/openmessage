package whatsapplive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
)

func TestLastConnectedAtSurvivesOfflineStatusObservations(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bridge := &Bridge{store: store, logger: zerolog.Nop()}
	var observations []StatusSnapshot
	bridge.callbacks.OnStatusChange = func() {
		observations = append(observations, bridge.Status())
	}
	before := time.Now()
	// A nil transport client keeps this unit test network-free and makes each
	// Status() observation offline, just as a brief connection whose callback
	// is delayed until after disconnect. handleConnected still records its
	// usable-state transition before performing any presence work.
	bridge.handleConnected()
	bridge.handleDisconnected("")
	status := bridge.Status()
	if status.Connected || status.LastConnectedAt.Before(before) || status.LastConnectedAt.After(time.Now()) {
		t.Fatalf("connection timestamp was not retained after disconnect: %+v", status)
	}
	if len(observations) != 2 {
		t.Fatalf("status callback count = %d, want 2", len(observations))
	}
	for _, observation := range observations {
		if observation.Connected || !observation.LastConnectedAt.Equal(status.LastConnectedAt) {
			t.Fatalf("offline observer lost usable connection timestamp: %+v", observation)
		}
	}
	raw, err := json.Marshal(status)
	if err != nil || strings.Contains(string(raw), "LastConnectedAt") || strings.Contains(string(raw), "last_connected_at") {
		t.Fatalf("internal timestamp changed live status JSON: %s, %v", raw, err)
	}
}

func TestUnpairedTransportDoesNotRecordUsableConnection(t *testing.T) {
	bridge := &Bridge{client: &whatsmeow.Client{Store: &wastore.Device{}}, logger: zerolog.Nop()}
	bridge.handleConnected()
	if !bridge.Status().LastConnectedAt.IsZero() {
		t.Fatal("pairing-only transport was recorded as a usable connection")
	}
}
