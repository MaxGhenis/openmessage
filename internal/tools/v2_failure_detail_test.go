package tools

// Failure-detail reporting for durable sends (stuck-send fix, 2026-10-07). A
// Google send sat in not_dispatched for hours with only "error class
// transient" visible, and a send the dispatcher gives up on must read as NOT
// SENT rather than an ambiguous rejection. These tests pin what both MCP serve
// modes say and carry for those outcomes, and the get_status line for a phone
// that switched to Google-account pairing.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
)

const (
	exhaustedDetail = "retry budget exhausted after 6 attempts; last failure transient [google_conversation_not_found]: send_text: transient: get Google conversation: transport returned no conversation"

	// switchedAdapterCause is the Google adapter's account-switch refusal
	// text (accountPairingSwitchedError), which starts with the fingerprint.
	switchedAdapterCause = "[google_account_pairing_switched] Your phone switched Google Messages to Google-account pairing (x@gmail.com) and refuses requests from this QR-paired session, so nothing was sent. Re-link OpenMessage with Google-account pairing, or turn Google-account pairing off on the phone and pair again by QR."
	// switchedDetail is the error_detail the outbox stores for that refusal:
	// the dispatcher's "[fingerprint] " prefix (OpError.Error() omits the
	// fingerprint), then "operation: class: cause".
	switchedDetail = "[google_account_pairing_switched] send_text: reauth_required: " + switchedAdapterCause
	// switchedPrefixOnlyDetail and switchedCauseOnlyDetail each carry the
	// fingerprint in one place only; either is enough to name the switch.
	switchedPrefixOnlyDetail = "[google_account_pairing_switched] send_text: reauth_required: the phone refused this session"
	switchedCauseOnlyDetail  = "send_text: reauth_required: " + switchedAdapterCause
)

var allOutboxStates = []messaging.OutboxState{
	messaging.OutboxQueued,
	messaging.OutboxDispatching,
	messaging.OutboxNotDispatched,
	messaging.OutboxUncertain,
	messaging.OutboxConfirmed,
	messaging.OutboxStoreFailed,
	messaging.OutboxRejected,
	messaging.OutboxCanceled,
}

func TestV2DeliveryTextRetryExhaustedSaysNotSent(t *testing.T) {
	text := v2DeliveryText(messaging.Delivery{
		OutboxID:       "outbox-exhausted",
		State:          messaging.OutboxRejected,
		ErrorClass:     "retry_exhausted",
		ErrorDetail:    exhaustedDetail,
		AttemptCount:   6,
		RetryExhausted: true,
	})
	want := "NOT SENT: gave up after 6 attempts (" + exhaustedDetail + "). Nothing was sent and it will not be retried. Send it again only if it is still wanted."
	if !strings.HasPrefix(text, want) {
		t.Fatalf("retry-exhausted text = %q\nwant prefix %q", text, want)
	}
	if !strings.Contains(text, "outbox-exhausted") {
		t.Fatalf("retry-exhausted text must name the outbox: %q", text)
	}

	// A count the daemon did not report still reads as a sentence.
	unknownCount := v2DeliveryText(messaging.Delivery{
		OutboxID:       "outbox-exhausted",
		State:          messaging.OutboxRejected,
		ErrorClass:     "retry_exhausted",
		RetryExhausted: true,
	})
	if !strings.HasPrefix(unknownCount, "NOT SENT: gave up after exhausting its retry budget. Nothing was sent") {
		t.Fatalf("retry-exhausted text without count/detail = %q", unknownCount)
	}
}

func TestV2DeliveryTextAccountPairingSwitchedNamesTheRelink(t *testing.T) {
	for _, detail := range []string{switchedDetail, switchedPrefixOnlyDetail, switchedCauseOnlyDetail} {
		text := v2DeliveryText(messaging.Delivery{
			OutboxID:    "outbox-switched",
			State:       messaging.OutboxRejected,
			ErrorClass:  "reauth_required",
			ErrorDetail: detail,
		})
		for _, fragment := range []string{
			"NOT SENT: the phone switched Google Messages to Google-account pairing",
			"Nothing was sent and it will not be retried",
			"re-link OpenMessage with Google-account pairing",
			"switch the phone back to QR pairing",
			"do not re-pair",
			detail,
			"outbox-switched",
		} {
			if !strings.Contains(text, fragment) {
				t.Fatalf("detail %q: account-switch text missing %q: %q", detail, fragment, text)
			}
		}
	}
}

func TestV2DeliveryTextShowsTheLastFailure(t *testing.T) {
	retrying := v2DeliveryText(messaging.Delivery{
		OutboxID:    "outbox-retrying",
		State:       messaging.OutboxNotDispatched,
		ErrorClass:  "transient",
		ErrorDetail: "[google_conversation_not_found] send_text: transient: no conversation",
	})
	for _, fragment := range []string{"retrying it automatically", "do NOT send this message again", "Last failure: [google_conversation_not_found]"} {
		if !strings.Contains(retrying, fragment) {
			t.Fatalf("not_dispatched text missing %q: %q", fragment, retrying)
		}
	}

	plain := v2DeliveryText(messaging.Delivery{
		OutboxID:    "outbox-plain",
		State:       messaging.OutboxRejected,
		ErrorClass:  "misconfigured",
		ErrorDetail: "send_text: misconfigured: bad conversation",
	})
	for _, fragment := range []string{"will not retry", "Reason: send_text: misconfigured: bad conversation"} {
		if !strings.Contains(plain, fragment) {
			t.Fatalf("plain rejected text missing %q: %q", fragment, plain)
		}
	}
	if strings.Contains(plain, "NOT SENT") {
		t.Fatalf("a plain rejection must keep the generic wording: %q", plain)
	}
}

// TestV2DeliveryTextNotSentOnlyForRejected is exhaustive over states x error
// class x RetryExhausted x detail kinds x attempt counts: "NOT SENT" appears
// iff the delivery is rejected and it gave up on its budget, was refused for
// the account switch, or is any other reauth_required refusal (the rejections
// the web tray lists as "Not sent"); "gave up" iff rejected, exhausted and
// not the account switch; the re-link wording iff rejected and the account
// switch or reauth_required. In particular an uncertain or retrying send is
// never described as not sent, whatever its class or detail.
func TestV2DeliveryTextNotSentOnlyForRejected(t *testing.T) {
	details := []string{
		"", exhaustedDetail, switchedDetail, switchedPrefixOnlyDetail, switchedCauseOnlyDetail,
		"send_text: transient: timeout",
	}
	classes := []string{"", "retry_exhausted", "reauth_required", "transient", "misconfigured", "upgrade_required"}
	for _, state := range allOutboxStates {
		for _, class := range classes {
			for _, exhausted := range []bool{false, true} {
				for _, detail := range details {
					for _, attempts := range []int64{0, 1, 6, 100} {
						delivery := messaging.Delivery{
							OutboxID:       "outbox-table",
							State:          state,
							ErrorClass:     class,
							ErrorDetail:    detail,
							AttemptCount:   attempts,
							RetryExhausted: exhausted,
						}
						text := v2DeliveryText(delivery)
						rejected := state == messaging.OutboxRejected
						switched := strings.Contains(detail, googleAccountPairingSwitchedFingerprint)
						reauth := class == "reauth_required"
						wantNotSent := rejected && (exhausted || switched || reauth)
						if got := strings.HasPrefix(text, "NOT SENT"); got != wantNotSent {
							t.Fatalf("%+v: NOT SENT = %v, want %v; text %q", delivery, got, wantNotSent, text)
						}
						wantGaveUp := rejected && exhausted && !switched
						if got := strings.Contains(text, "gave up after"); got != wantGaveUp {
							t.Fatalf("%+v: gave up = %v, want %v; text %q", delivery, got, wantGaveUp, text)
						}
						wantRelink := rejected && (switched || (reauth && !exhausted))
						if got := strings.Contains(text, "do not re-pair or reconnect on your own"); got != wantRelink {
							t.Fatalf("%+v: re-link guidance = %v, want %v; text %q", delivery, got, wantRelink, text)
						}
						if state == messaging.OutboxUncertain && !strings.Contains(text, "may have accepted it") {
							t.Fatalf("uncertain text lost its may-have-sent warning: %q", text)
						}
					}
				}
			}
		}
	}
}

// TestV2DeliveryTextReauthWithoutFingerprintStillNamesTheRelink covers a
// reauth_required refusal whose detail does not name the account switch (for
// example a WhatsApp session that was logged out): the agent still reads NOT
// SENT, the re-link, and that re-pairing is the user's call.
func TestV2DeliveryTextReauthWithoutFingerprintStillNamesTheRelink(t *testing.T) {
	const detail = "[whatsapp_session_invalid] send_text: reauth_required: the store doesn't contain a device JID"
	text := v2DeliveryText(messaging.Delivery{
		OutboxID:    "outbox-reauth",
		State:       messaging.OutboxRejected,
		ErrorClass:  "reauth_required",
		ErrorCode:   "send_text",
		ErrorDetail: detail,
	})
	want := "NOT SENT: the account must be re-linked before it can send (" + detail + "). Nothing was sent and it will not be retried. Re-linking is the user's call; do not re-pair or reconnect on your own. Outbox outbox-reauth."
	if text != want {
		t.Fatalf("reauth text = %q\nwant %q", text, want)
	}
}

// TestAddV2DeliveryFailureDetailMirrorsTheDelivery checks, for random
// deliveries, that the MCP payload's retry_exhausted always equals the
// delivery's flag and the detail/count keys appear exactly when set.
func TestAddV2DeliveryFailureDetailMirrorsTheDelivery(t *testing.T) {
	property := func(detail string, attempts int64, exhausted bool) bool {
		payload := map[string]any{}
		addV2DeliveryFailureDetail(payload, messaging.Delivery{
			ErrorDetail:    detail,
			AttemptCount:   attempts,
			RetryExhausted: exhausted,
		})
		if payload["retry_exhausted"] != exhausted {
			return false
		}
		gotDetail, hasDetail := payload["error_detail"]
		if hasDetail != (detail != "") || (hasDetail && gotDetail != detail) {
			return false
		}
		gotAttempts, hasAttempts := payload["attempt_count"]
		return hasAttempts == (attempts > 0) && (!hasAttempts || gotAttempts == attempts)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// TestDeliveryFromLocalAPIPreservesFailureDetail is the daemon-mode half of
// the round trip (the web package checks API JSON -> localapi.Delivery): every
// field the daemon reports survives into the delivery the tool reasons about.
func TestDeliveryFromLocalAPIPreservesFailureDetail(t *testing.T) {
	property := func(in localapi.Delivery) bool {
		out := deliveryFromLocalAPI(in)
		if out.OutboxID != in.OutboxID || string(out.State) != in.State ||
			out.LocalMessageID != in.LocalMessageID || out.RemoteMessageID != in.RemoteMessageID ||
			out.ErrorClass != in.ErrorClass || out.ErrorCode != in.ErrorCode ||
			out.ErrorDetail != in.ErrorDetail || out.AttemptCount != in.AttemptCount ||
			out.RetryExhausted != in.RetryExhausted || out.Warning != in.Warning {
			return false
		}
		if in.NextAttemptAtMS > 0 {
			return out.NextAttemptAt.UnixMilli() == in.NextAttemptAtMS
		}
		return out.NextAttemptAt.IsZero()
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func failureDaemonHandler(t *testing.T, delivery map[string]any) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true, "connected": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": delivery["outbox_id"], "state": "queued"})
	})
	mux.HandleFunc("/api/v1/outbox/"+delivery["outbox_id"].(string), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(delivery)
	})
	return mux
}

func TestDaemonSendReportsFailureDetail(t *testing.T) {
	cases := []struct {
		name          string
		delivery      map[string]any
		wantSettled   bool
		wantExhausted bool
		wantAttempts  float64
		wantText      []string
	}{
		{
			name: "retry budget exhausted",
			delivery: map[string]any{
				"outbox_id":       "out-exhausted",
				"state":           "rejected",
				"error_class":     "retry_exhausted",
				"error_code":      "send_text",
				"error_detail":    exhaustedDetail,
				"attempt_count":   6,
				"retry_exhausted": true,
			},
			wantSettled:   true,
			wantExhausted: true,
			wantAttempts:  6,
			wantText:      []string{"NOT SENT: gave up after 6 attempts", "Nothing was sent and it will not be retried"},
		},
		{
			name: "account pairing switched",
			delivery: map[string]any{
				"outbox_id":     "out-switched",
				"state":         "rejected",
				"error_class":   "reauth_required",
				"error_code":    "send_text",
				"error_detail":  switchedDetail,
				"attempt_count": 1,
			},
			wantSettled:  true,
			wantAttempts: 1,
			wantText:     []string{"NOT SENT", "Google-account pairing", "do not re-pair"},
		},
		{
			name: "reauth required without the fingerprint",
			delivery: map[string]any{
				"outbox_id":     "out-reauth",
				"state":         "rejected",
				"error_class":   "reauth_required",
				"error_code":    "send_text",
				"error_detail":  "send_text: reauth_required: session refused",
				"attempt_count": 1,
			},
			wantSettled:  true,
			wantAttempts: 1,
			wantText:     []string{"NOT SENT: the account must be re-linked", "do not re-pair"},
		},
		{
			name: "retrying with a reason",
			delivery: map[string]any{
				"outbox_id":          "out-retrying",
				"state":              "not_dispatched",
				"error_class":        "transient",
				"error_code":         "send_text",
				"error_detail":       "[google_conversation_not_found] send_text: transient: no conversation",
				"attempt_count":      2,
				"next_attempt_at_ms": time.Now().Add(10 * time.Second).UnixMilli(),
			},
			wantSettled:  false,
			wantAttempts: 2,
			wantText:     []string{"retrying it automatically", "Last failure: [google_conversation_not_found]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := daemonSendToConversationHandler(Options{Daemon: daemonClientFor(t, failureDaemonHandler(t, tc.delivery))})
			req := mcp.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"conversation_id": "conv-1",
				"message":         "hello",
				"idempotency_key": "key-failure-detail",
			}
			result, err := handler(context.Background(), req)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if result.IsError {
				t.Fatalf("an enqueued send must report data, not a tool error: %+v", result)
			}
			payload := structuredMap(t, result)
			if payload["settled"] != tc.wantSettled {
				t.Fatalf("settled = %v, want %v", payload["settled"], tc.wantSettled)
			}
			if payload["retry_exhausted"] != tc.wantExhausted {
				t.Fatalf("retry_exhausted = %v, want %v", payload["retry_exhausted"], tc.wantExhausted)
			}
			if payload["error_detail"] != tc.delivery["error_detail"] {
				t.Fatalf("error_detail = %v, want %v", payload["error_detail"], tc.delivery["error_detail"])
			}
			if got, _ := payload["attempt_count"].(int64); float64(got) != tc.wantAttempts {
				t.Fatalf("attempt_count = %#v, want %v", payload["attempt_count"], tc.wantAttempts)
			}
			text := resultText(t, result)
			for _, fragment := range tc.wantText {
				if !strings.Contains(text, fragment) {
					t.Fatalf("text missing %q: %q", fragment, text)
				}
			}
		})
	}
}

func TestGetStatusReportsGoogleAccountSwitch(t *testing.T) {
	a := testApp(t)
	a.DataDir = t.TempDir()
	switchedAt := time.Date(2026, 10, 7, 15, 56, 10, 0, time.Local).UnixMilli()

	originalGoogleStatus := googleStatus
	t.Cleanup(func() { googleStatus = originalGoogleStatus })
	for _, switched := range []bool{false, true} {
		snapshot := app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true}
		if switched {
			snapshot.AccountSwitched = true
			snapshot.SwitchedAccount = "x@gmail.com"
			snapshot.AccountSwitchedAtMS = switchedAt
		}
		googleStatus = func(*app.App) app.GoogleStatusSnapshot { return snapshot }

		result, err := getStatusHandler(a)(context.Background(), mcp.CallToolRequest{})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		text := result.Content[0].(mcp.TextContent).Text
		line := "  Account pairing switched: the phone switched Google Messages to Google-account pairing (x@gmail.com, since " +
			time.UnixMilli(switchedAt).Format(time.RFC3339) + ")."
		if got := strings.Contains(text, line); got != switched {
			t.Fatalf("switched=%v: account-switch line present=%v in %q", switched, got, text)
		}
		if got, want := strings.Count(text, "switched Google Messages to Google-account pairing"), map[bool]int{false: 0, true: 1}[switched]; got != want {
			t.Fatalf("switched=%v: %d account-switch mentions, want %d: %q", switched, got, want, text)
		}
		encoded, err := json.Marshal(structuredMap(t, result)["google"])
		if err != nil {
			t.Fatalf("marshal google payload: %v", err)
		}
		if got := strings.Contains(string(encoded), `"account_pairing_switched":true`); got != switched {
			t.Fatalf("switched=%v: structured google payload = %s", switched, encoded)
		}
	}
}

func TestDaemonGetStatusReportsGoogleAccountSwitch(t *testing.T) {
	switchedAt := time.Date(2026, 10, 7, 15, 56, 10, 0, time.Local).UnixMilli()
	for _, switched := range []bool{false, true} {
		google := map[string]any{"connected": true, "paired": true}
		if switched {
			google["account_pairing_switched"] = true
			google["switched_account"] = "x@gmail.com"
			google["account_pairing_switched_at_ms"] = switchedAt
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"connected": true, "v2_primary": true, "google": google})
		})
		a := testApp(t)
		handler := daemonGetStatusHandler(a, Options{Reads: a.Store, Daemon: daemonClientFor(t, mux)})
		result, err := handler(context.Background(), mcp.CallToolRequest{})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		text := resultText(t, result)
		line := "Google Messages account pairing switched: the phone switched Google Messages to Google-account pairing (x@gmail.com, since " +
			time.UnixMilli(switchedAt).Format(time.RFC3339) + ")."
		if got := strings.Contains(text, line); got != switched {
			t.Fatalf("switched=%v: account-switch line present=%v in %q", switched, got, text)
		}
		if got, want := strings.Count(text, "switched Google Messages to Google-account pairing"), map[bool]int{false: 0, true: 1}[switched]; got != want {
			t.Fatalf("switched=%v: %d account-switch mentions, want %d: %q", switched, got, want, text)
		}
		if !strings.Contains(text, "Google Messages: connected=true paired=true") {
			t.Fatalf("the platform line must stay: %q", text)
		}
	}
}

func TestGoogleAccountSwitchSummaryOmitsUnknowns(t *testing.T) {
	got := googleAccountSwitchSummary("", 0)
	if !strings.HasPrefix(got, "the phone switched Google Messages to Google-account pairing. This QR-paired session") {
		t.Fatalf("summary without account/time = %q", got)
	}
	if !strings.Contains(googleAccountSwitchSummary("x@gmail.com", 0), "pairing (x@gmail.com).") {
		t.Fatalf("account-only summary = %q", googleAccountSwitchSummary("x@gmail.com", 0))
	}
}

// TestDaemonDeliveryDecodesFromAPIShape guards the JSON names the daemon-mode
// tools depend on against drift in localapi.Delivery.
func TestDaemonDeliveryDecodesFromAPIShape(t *testing.T) {
	raw := `{"outbox_id":"o","state":"rejected","error_class":"retry_exhausted","error_detail":"d","attempt_count":6,"next_attempt_at_ms":42,"retry_exhausted":true}`
	var decoded localapi.Delivery
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	want := localapi.Delivery{
		OutboxID:        "o",
		State:           "rejected",
		ErrorClass:      "retry_exhausted",
		ErrorDetail:     "d",
		AttemptCount:    6,
		NextAttemptAtMS: 42,
		RetryExhausted:  true,
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded = %+v, want %+v", decoded, want)
	}
}
