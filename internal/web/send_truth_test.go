package web

// Wire-level pieces of the truthful-send-states change: TTL parsing, the
// enriched delivery response (account/conversation/platform/expiry), and the
// /api/status per-platform send capability block.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	// Bounds are checked before multiplying into nanoseconds. Each of these
	// used to be accepted: 2^58 ms wrapped to 0 (never expire), 2^58+1 ms to
	// a 1ms window, and the larger ones to negative windows.
	for _, raw := range []string{
		"288230376151711744",  // 2^58
		"288230376151711745",  // 2^58 + 1
		"9223372036854775807", // MaxInt64
		"9223372036855",
		"86400001", // one past 24 hours
		"-1",
	} {
		ttl, err := parseOptionalTTL(raw)
		if err == nil {
			t.Errorf("parseOptionalTTL(%s) = %v, nil; want rejected", raw, ttl)
			continue
		}
		if !strings.Contains(err.Error(), "ttl_ms must be between 0 (no expiry) and 86400000") {
			t.Errorf("parseOptionalTTL(%s) error = %q, want the ttl_ms range", raw, err)
		}
	}
	accepted := map[string]time.Duration{
		"0":        0, // explicit 0 is no expiry, same as absent
		"1":        time.Millisecond,
		"86400000": 24 * time.Hour,
	}
	for raw, want := range accepted {
		if ttl, err := parseOptionalTTL(raw); err != nil || ttl != want {
			t.Errorf("parseOptionalTTL(%s) = %v, %v; want %v, nil", raw, ttl, err, want)
		}
	}
}

// TestSubmitRejectsWrappingTTLWith400 drives both submit routes: an
// out-of-range ttl_ms is a 400 naming the range, and it is refused before any
// v2 dependency is touched (the dependencies here are deliberately empty).
func TestSubmitRejectsWrappingTTLWith400(t *testing.T) {
	ts := newV1RecorderHarness(t, APIOptions{V2: &V2Options{}})
	for _, ttlMS := range []string{"288230376151711744", "86400001", "-5"} {
		body := `{"conversation_id":"conversation-ttl","body":"hi","idempotency_key":"ttl-wrap-key","ttl_ms":` + ttlMS + `}`
		textRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/outbox/messages", strings.NewReader(body))
		textRequest.Header.Set("Content-Type", "application/json")

		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		for field, value := range map[string]string{
			"conversation_id": "conversation-ttl",
			"idempotency_key": "ttl-wrap-media-key",
			"ttl_ms":          ttlMS,
		} {
			if err := writer.WriteField(field, value); err != nil {
				t.Fatalf("WriteField(%s): %v", field, err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("multipart close: %v", err)
		}
		mediaRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/outbox/media", &form)
		mediaRequest.Header.Set("Content-Type", writer.FormDataContentType())

		for route, request := range map[string]*http.Request{"text": textRequest, "media": mediaRequest} {
			resp := ts.do(t, request)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s ttl_ms=%s: status = %d, want 400; body=%s", route, ttlMS, resp.StatusCode, raw)
				continue
			}
			if !strings.Contains(string(raw), "ttl_ms must be between 0 (no expiry) and 86400000") {
				t.Errorf("%s ttl_ms=%s: body = %s, want the ttl_ms range", route, ttlMS, raw)
			}
		}
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
				sendcap.PlatformSignal:   {Available: false, Queueable: true, Reason: "signal-cli could not read the linked Signal account", Condition: sendcap.ConditionAccountRecheck},
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
	// The typed condition is published beside the tier (area G: it must
	// survive the daemon hop for clients to tell Signal's parks apart).
	if signal.Condition != sendcap.ConditionAccountRecheck {
		t.Fatalf("signal condition = %q, want %q", signal.Condition, sendcap.ConditionAccountRecheck)
	}
}
