package web

// Wire-level pieces of the truthful-send-states change: TTL parsing, the
// enriched delivery response (account/conversation/platform/expiry), and the
// /api/status per-platform send capability block.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/sendcap"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestOptionalTTLParsing(t *testing.T) {
	if ttl, err := validateOptionalTTL(nil); err != nil || ttl != 0 {
		t.Fatalf("validateOptionalTTL(nil) = %v, %v; want 0, nil", ttl, err)
	}
	value := int64(600_000)
	if ttl, err := validateOptionalTTL(&value); err != nil || ttl != 10*time.Minute {
		t.Fatalf("validateOptionalTTL(600000) = %v, %v; want 10m, nil", ttl, err)
	}
	negative := int64(-1)
	if _, err := validateOptionalTTL(&negative); err == nil {
		t.Fatal("validateOptionalTTL(-1) accepted a negative window")
	}
	if ttl, err := parseOptionalTTL(""); err != nil || ttl != 0 {
		t.Fatalf("parseOptionalTTL(\"\") = %v, %v; want 0, nil", ttl, err)
	}
	if ttl, err := parseOptionalTTL("90000"); err != nil || ttl != 90*time.Second {
		t.Fatalf("parseOptionalTTL(90000) = %v, %v; want 90s, nil", ttl, err)
	}
	if _, err := parseOptionalTTL("not-a-number"); err == nil {
		t.Fatal("parseOptionalTTL accepted junk")
	}
}

func TestDeliveryResponseCarriesTransportAndExpiry(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "google-primary",
		BridgeKey:   "google",
		DisplayName: "Google",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}

	api := &v1API{v2: &V2Options{V2Store: store}}
	expiry := time.UnixMilli(nowMS).Add(10 * time.Minute)
	response := api.deliveryResponse(messaging.Delivery{
		OutboxID:       "outbox-wire",
		AccountID:      "google-primary",
		ConversationID: "conversation-wire",
		State:          messaging.OutboxConfirmed,
		ExpiresAt:      expiry,
	})
	if response.Platform != "sms" {
		t.Fatalf("platform = %q, want sms (google bridge key maps to the sms send platform)", response.Platform)
	}
	if response.AccountID != "google-primary" || response.ConversationID != "conversation-wire" {
		t.Fatalf("identity fields = %q/%q", response.AccountID, response.ConversationID)
	}
	if response.ExpiresAtMS != expiry.UnixMilli() {
		t.Fatalf("expires_at_ms = %d, want %d", response.ExpiresAtMS, expiry.UnixMilli())
	}
	if response.Expired {
		t.Fatal("confirmed delivery must not report expired")
	}

	ttlClass := sqlite.TTLErrorClass
	expired := api.deliveryResponse(messaging.Delivery{
		OutboxID:   "outbox-expired",
		State:      messaging.OutboxCanceled,
		ErrorClass: ttlClass,
	})
	if !expired.Expired {
		t.Fatal("ttl-canceled delivery must report expired")
	}
}

func TestStatusReportsSendCapabilityBlock(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{
		SendCapability: func() map[string]SendPlatformCapability {
			return map[string]SendPlatformCapability{
				sendcap.PlatformSMS:      {Available: true},
				sendcap.PlatformWhatsApp: {Available: false, Reason: "whatsapp is not paired"},
				sendcap.PlatformSignal:   {Available: false, Queueable: true, Reason: "signal is disconnected; a send submitted now would wait in the outbox until it reconnects"},
			}
		},
	})
	resp := ts.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	defer resp.Body.Close()

	var payload struct {
		Send map[string]SendPlatformCapability `json:"send"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Send == nil {
		t.Fatal("status payload missing send block")
	}
	if !payload.Send["sms"].Available {
		t.Fatalf("sms = %+v, want available", payload.Send["sms"])
	}
	whatsApp := payload.Send["whatsapp"]
	if whatsApp.Available || whatsApp.Queueable || whatsApp.Reason == "" {
		t.Fatalf("whatsapp = %+v, want unavailable+non-queueable with reason", whatsApp)
	}
	signal := payload.Send["signal"]
	if signal.Available || !signal.Queueable {
		t.Fatalf("signal = %+v, want unavailable but queueable", signal)
	}
}
