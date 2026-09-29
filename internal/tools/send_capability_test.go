package tools

// Send-capability contract tests for the typed Signal park tiers and the
// daemon hop (PR #166 area G): the condition survives /api/status, route
// discovery and send-time enforcement classify identically, an older daemon
// reads as unknown rather than invented availability, and get_status names
// the park.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/sendcap"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

var allSendConditions = []sendcap.Condition{
	sendcap.ConditionNoTransports,
	sendcap.ConditionAdapterMissing,
	sendcap.ConditionNotPaired,
	sendcap.ConditionAuthExpired,
	sendcap.ConditionNeedsRepair,
	sendcap.ConditionDisconnected,
	sendcap.ConditionPhoneNotResponding,
	sendcap.ConditionAccountRecheck,
	sendcap.ConditionRelinkRequired,
	sendcap.ConditionUpgradeRequired,
}

func jsonFieldNames(value any) []string {
	var names []string
	kind := reflect.TypeOf(value)
	for index := 0; index < kind.NumField(); index++ {
		tag := kind.Field(index).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TestSendCapabilityJSONMirror (I11, differential): the client-side mirror
// decodes every field the daemon publishes. The G3 defect was exactly this
// drift — a field added to sendcap.Capability silently vanished on the hop.
func TestSendCapabilityJSONMirror(t *testing.T) {
	published := jsonFieldNames(sendcap.Capability{})
	decoded := jsonFieldNames(localapi.PlatformSendCapability{})
	if !reflect.DeepEqual(published, decoded) {
		t.Fatalf("sendcap.Capability JSON fields %v != localapi.PlatformSendCapability fields %v", published, decoded)
	}
}

// capabilityMapsCoveringEveryCondition computes real capability maps whose
// union names every Condition.
func capabilityMapsCoveringEveryCondition(t *testing.T) []map[string]sendcap.Capability {
	t.Helper()
	healthy := func() sendcap.Inputs {
		return sendcap.Inputs{
			TransportsEnabled: true,
			Google:            app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true},
			WhatsApp:          whatsapplive.StatusSnapshot{Connected: true, Paired: true},
			Signal:            signallive.StatusSnapshot{Connected: true, Paired: true},
		}
	}
	var inputs []sendcap.Inputs
	inputs = append(inputs, healthy())

	off := healthy()
	off.TransportsEnabled = false
	inputs = append(inputs, off)

	degraded := healthy()
	degraded.Google.Connected = false
	degraded.WhatsApp.Connected = false
	degraded.Signal = signallive.StatusSnapshot{Paired: true, NeedsReauth: true, ParkFingerprint: signallive.SignalAccountUnreadableFingerprint}
	inputs = append(inputs, degraded)

	hard := healthy()
	hard.Google.AuthExpired = true
	hard.WhatsApp.Paired = false
	hard.Signal = signallive.StatusSnapshot{Paired: true, NeedsReauth: true, ParkFingerprint: signallive.SignalAccountInvalidFingerprint}
	inputs = append(inputs, hard)

	upgrade := healthy()
	upgrade.Google.NeedsRepair = true
	upgrade.Signal = signallive.StatusSnapshot{Paired: true, UpgradeRequired: true, ParkFingerprint: signallive.SignalCLIVersionFingerprint, LastError: "signal-cli 0.14.4 is below the required minimum 0.14.5"}
	upgrade.AdapterTextSend = func(platform string) bool { return platform != sendcap.PlatformWhatsApp }
	inputs = append(inputs, upgrade)

	phone := healthy()
	phone.Google.PhoneResponding = false
	phone.Signal.Connected = false
	inputs = append(inputs, phone)

	seen := map[sendcap.Condition]bool{}
	maps := make([]map[string]sendcap.Capability, 0, len(inputs))
	for _, in := range inputs {
		capabilities := sendcap.Compute(in)
		for _, capability := range capabilities {
			if capability.Condition != "" {
				seen[capability.Condition] = true
			}
		}
		maps = append(maps, capabilities)
	}
	for _, condition := range allSendConditions {
		if !seen[condition] {
			t.Fatalf("fixture maps never produce condition %q", condition)
		}
	}
	return maps
}

// TestStatusSendBlockRoundTrip (I11): sendcap.Capability → the real
// /api/status handler → localapi.Client.Status → routeSendCapabilities is
// lossless for every tier and condition, and send-time enforcement sees the
// same condition.
func TestStatusSendBlockRoundTrip(t *testing.T) {
	a := testApp(t)
	var mu sync.Mutex
	var current map[string]sendcap.Capability
	handler := web.APIHandlerWithOptions(a.Store, nil, zerolog.Nop(), nil, web.APIOptions{
		SendCapability: func() map[string]web.SendPlatformCapability {
			mu.Lock()
			defer mu.Unlock()
			return current
		},
	})
	daemon := daemonClientFor(t, handler)
	options := Options{Daemon: daemon}

	for index, capabilities := range capabilityMapsCoveringEveryCondition(t) {
		mu.Lock()
		current = capabilities
		mu.Unlock()

		routed, unknownReason := routeSendCapabilities(context.Background(), a, options)
		if unknownReason != "" {
			t.Fatalf("map %d: unexpected unknown reason %q from a daemon with a send block", index, unknownReason)
		}
		if !reflect.DeepEqual(routed, capabilities) {
			t.Fatalf("map %d round trip lost data:\n got  %+v\n want %+v", index, routed, capabilities)
		}

		status, reachable, err := daemon.Status(context.Background())
		if err != nil || !reachable {
			t.Fatalf("daemon.Status() = reachable %v, err %v", reachable, err)
		}
		for platform, want := range capabilities {
			published, known := status.SendCapabilityFor(platform)
			if !known {
				t.Fatalf("map %d: %s missing from the decoded send block", index, platform)
			}
			if got := capabilityFromDaemon(published); !reflect.DeepEqual(got, want) {
				t.Fatalf("map %d %s: enforcement sees %+v, want %+v", index, platform, got, want)
			}
		}
	}
}

// TestRouteDiscoveryOlderDaemonReportsUnknown (G4): a daemon without the send
// block leaves every send platform unknown — never an invented "sms is
// available" — matching send-time enforcement, which passes unknown through.
func TestRouteDiscoveryOlderDaemonReportsUnknown(t *testing.T) {
	a := testApp(t)
	seedLeighRoutes(t, a, true)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		// The pre-send-block shape: google unpaired, whatsapp/signal connected.
		json.NewEncoder(w).Encode(map[string]any{
			"connected": true,
			"google":    map[string]any{"connected": false, "paired": false},
			"whatsapp":  map[string]any{"connected": true, "paired": true},
			"signal":    map[string]any{"connected": true, "paired": true},
		})
	})
	options := Options{Daemon: daemonClientFor(t, mux)}

	result, err := resolveContactRoutesHandler(a, options)(context.Background(), toolRequest(map[string]any{"query": "Leigh"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	match := singleRouteMatch(t, result)
	if len(match.Routes) != 3 {
		t.Fatalf("routes = %+v, want sms, whatsapp and signal", match.Routes)
	}
	for _, route := range match.Routes {
		if route.SendCapability != string(sendcap.TierUnknown) || route.Sendable {
			t.Fatalf("%s route = %+v, want send_capability=unknown and sendable=false", route.Conversation.SourcePlatform, route)
		}
		if !strings.Contains(route.SendableReason, "predates per-platform send capability reporting") {
			t.Fatalf("%s reason = %q, want the older-app explanation", route.Conversation.SourcePlatform, route.SendableReason)
		}
	}
	if match.PreferredReplyConversationID != "sms-conv-1" {
		t.Fatalf("preferred = %q, want the unknown sms route as the fallback", match.PreferredReplyConversationID)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "sms (preferred, send capability unknown)") ||
		!strings.Contains(text, "whatsapp (send capability unknown)") {
		t.Fatalf("route text = %q, want unknown-capability labels", text)
	}

	status, _, err := options.Daemon.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"sms", "whatsapp", "signal"} {
		if failure := daemonCheckPlatformSendable(status, platform); failure != nil {
			t.Fatalf("send-time enforcement refused %s against an older daemon: %v", platform, failure.Content)
		}
	}
}

// TestResolveContactRoutesLabelsEachTier: queueable and hard-down routes are
// labeled distinctly (no longer both "history"), and the preferred route is
// never a queueable one.
func TestResolveContactRoutesLabelsEachTier(t *testing.T) {
	a := testApp(t)
	seedLeighRoutes(t, a, true)
	restore := stubPlatformStatuses(
		app.GoogleStatusSnapshot{Paired: true}, // disconnected → queueable
		whatsapplive.StatusSnapshot{Connected: true, Paired: true},
		signallive.StatusSnapshot{Paired: true, UpgradeRequired: true, ParkFingerprint: signallive.SignalCLIVersionFingerprint},
	)
	t.Cleanup(restore)

	result, err := resolveContactRoutesHandler(a)(context.Background(), toolRequest(map[string]any{"query": "Leigh"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	match := singleRouteMatch(t, result)
	want := map[string]string{
		"sms":      string(sendcap.TierQueueable),
		"whatsapp": string(sendcap.TierAvailable),
		"signal":   string(sendcap.TierUnavailable),
	}
	for _, route := range match.Routes {
		platform := route.Conversation.SourcePlatform
		if route.SendCapability != want[platform] {
			t.Fatalf("%s send_capability = %q, want %q", platform, route.SendCapability, want[platform])
		}
		if route.Sendable != (route.SendCapability == string(sendcap.TierAvailable)) {
			t.Fatalf("%s sendable = %v disagrees with send_capability %q", platform, route.Sendable, route.SendCapability)
		}
	}
	if match.PreferredReplyConversationID != "whatsapp:15551230000@s.whatsapp.net" {
		t.Fatalf("preferred = %q, want the only available route (never the queueable sms)", match.PreferredReplyConversationID)
	}
	text := resultText(t, result)
	for _, fragment := range []string{"sms (queues only)", "whatsapp (preferred)", "signal (unavailable)"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("route text %q missing %q", text, fragment)
		}
	}
}

// TestInProcessCapabilityHonorsTransportsEnabled: a process registered
// without transports (serve --no-transports) answers exactly like its own
// /api/status send block — nothing can send — instead of reading bridges
// nobody drives.
func TestInProcessCapabilityHonorsTransportsEnabled(t *testing.T) {
	a := testApp(t)
	seedLeighRoutes(t, a, false)
	restore := stubPlatformStatuses(
		app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true},
		whatsapplive.StatusSnapshot{Connected: true, Paired: true},
		signallive.StatusSnapshot{Connected: true, Paired: true},
	)
	t.Cleanup(restore)

	for _, transports := range []bool{false, true} {
		options := Options{Reads: a.Store, TransportsEnabled: transports}
		result, err := getStatusHandler(a, options)(context.Background(), toolRequest(nil))
		if err != nil {
			t.Fatal(err)
		}
		send, ok := structuredMap(t, result)["send"].(map[string]sendcap.Capability)
		if !ok {
			t.Fatalf("send payload = %T", structuredMap(t, result)["send"])
		}
		want := sendcap.Compute(sendcap.Inputs{
			TransportsEnabled: transports,
			Google:            googleStatus(a),
			WhatsApp:          whatsAppStatus(a),
			Signal:            signalStatus(a),
		})
		if !reflect.DeepEqual(send, want) {
			t.Fatalf("transports=%v get_status send = %+v, want the daemon computation %+v", transports, send, want)
		}

		routes, err := resolveContactRoutesHandler(a, options)(context.Background(), toolRequest(map[string]any{"query": "Leigh"}))
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range singleRouteMatch(t, routes).Routes {
			wantState := sendcap.TierAvailable
			if !transports {
				wantState = sendcap.TierUnavailable
			}
			if route.SendCapability != string(wantState) {
				t.Fatalf("transports=%v %s route = %+v, want %s", transports, route.Conversation.SourcePlatform, route, wantState)
			}
		}
	}
}

// TestDaemonSendSignalRecheckQueuesAndUpgradeRefuses: the account_recheck park
// queues (truthfully, not an error), while the upgrade park refuses without
// submitting and names its condition.
func TestDaemonSendSignalRecheckQueuesAndUpgradeRefuses(t *testing.T) {
	var submits atomic.Int64
	var signalCapability atomic.Value
	signalCapability.Store(map[string]any{
		"available": false, "queueable": true, "condition": "account_recheck",
		"reason": "signal-cli could not read the linked Signal account on several consecutive attempts",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"v2_send": true,
			"send": map[string]any{
				"sms":      map[string]any{"available": true},
				"whatsapp": map[string]any{"available": true},
				"signal":   signalCapability.Load(),
			},
		})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		submits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "outbox-recheck", "state": "queued"})
	})
	mux.HandleFunc("/api/v1/outbox/outbox-recheck", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"outbox_id": "outbox-recheck", "state": "queued"})
	})
	handler := daemonSendToConversationHandler(Options{Daemon: daemonClientFor(t, mux)})

	result, err := handler(context.Background(), toolRequest(map[string]any{
		"conversation_id": "signal:+15551230000",
		"message":         "queued behind the account recheck",
		"idempotency_key": "signal-recheck-key",
		"wait_seconds":    float64(1),
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError {
		t.Fatalf("account_recheck must queue, not error: %v", result.Content)
	}
	if got := submits.Load(); got != 1 {
		t.Fatalf("submits = %d, want 1", got)
	}
	if got, _ := structuredMap(t, result)["transport_state"].(string); got != "queued" {
		t.Fatalf("transport_state = %q, want queued", got)
	}

	signalCapability.Store(map[string]any{
		"available": false, "queueable": false, "condition": "upgrade_required",
		"reason": "signal is parked until signal-cli is upgraded; upgrade signal-cli (for a Homebrew install: brew upgrade signal-cli)",
	})
	result, err = handler(context.Background(), toolRequest(map[string]any{
		"conversation_id": "signal:+15551230000",
		"message":         "refused behind the upgrade park",
	}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatalf("upgrade_required must refuse, got %v", result.Content)
	}
	payload := structuredMap(t, result)
	if payload["error_kind"] != "platform_unsendable" || payload["condition"] != "upgrade_required" {
		t.Fatalf("refusal payload = %v, want platform_unsendable with condition upgrade_required", payload)
	}
	if !strings.Contains(resultText(t, result), "upgrade signal-cli") {
		t.Fatalf("refusal text = %q, want the upgrade instruction", resultText(t, result))
	}
	if got := submits.Load(); got != 1 {
		t.Fatalf("submits after the upgrade refusal = %d, want still 1", got)
	}
}

func TestGetStatusNamesSignalParkAndCondition(t *testing.T) {
	a := testApp(t)
	tests := []struct {
		name      string
		signal    signallive.StatusSnapshot
		fragments []string
		condition sendcap.Condition
	}{
		{
			name:   "unreadable park",
			signal: signallive.StatusSnapshot{Paired: true, NeedsReauth: true, ParkFingerprint: signallive.SignalAccountUnreadableFingerprint},
			fragments: []string{
				"Needs reauth: true",
				"Park fingerprint: signal_account_unreadable",
				"signal: DEGRADED (sends queue, not transmit) [account_recheck]",
			},
			condition: sendcap.ConditionAccountRecheck,
		},
		{
			name:   "upgrade park",
			signal: signallive.StatusSnapshot{Paired: true, UpgradeRequired: true, ParkFingerprint: signallive.SignalCLIVersionFingerprint},
			fragments: []string{
				"Upgrade required: true",
				"Park fingerprint: signal_cli_too_old",
				"signal: UNAVAILABLE [upgrade_required]",
			},
			condition: sendcap.ConditionUpgradeRequired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := stubPlatformStatuses(
				app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true},
				whatsapplive.StatusSnapshot{Connected: true, Paired: true},
				test.signal,
			)
			t.Cleanup(restore)
			result, err := getStatusHandler(a)(context.Background(), toolRequest(nil))
			if err != nil {
				t.Fatal(err)
			}
			text := resultText(t, result)
			for _, fragment := range test.fragments {
				if !strings.Contains(text, fragment) {
					t.Fatalf("get_status text missing %q:\n%s", fragment, text)
				}
			}
			send := structuredMap(t, result)["send"].(map[string]sendcap.Capability)
			if send[sendcap.PlatformSignal].Condition != test.condition {
				t.Fatalf("structured signal condition = %q, want %q", send[sendcap.PlatformSignal].Condition, test.condition)
			}
		})
	}
}

func TestDaemonGetStatusNamesSignalParkAndCondition(t *testing.T) {
	a := testApp(t)
	var withoutSendBlock atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"connected": true,
			"signal": map[string]any{
				"connected": false, "paired": true, "needs_reauth": true,
				"park_fingerprint": signallive.SignalAccountUnreadableFingerprint,
				"last_error":       "signal-cli cannot read the linked Signal account",
			},
		}
		if !withoutSendBlock.Load() {
			payload["send"] = map[string]any{
				"signal": map[string]any{
					"available": false, "queueable": true, "condition": "account_recheck",
					"reason": "re-checked about every 15 minutes",
				},
			}
		}
		json.NewEncoder(w).Encode(payload)
	})
	handler := daemonGetStatusHandler(a, Options{Reads: a.Store, Daemon: daemonClientFor(t, mux)})

	result, err := handler(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, result)
	for _, fragment := range []string{
		"Signal: connected=false paired=true needs_reauth=true park_fingerprint=signal_account_unreadable",
		"signal: DEGRADED (sends queue, not transmit) [account_recheck]",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("daemon get_status text missing %q:\n%s", fragment, text)
		}
	}

	withoutSendBlock.Store(true)
	result, err = handler(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, result); !strings.Contains(text, "Send capability: UNKNOWN") {
		t.Fatalf("older-daemon get_status text = %q, want an explicit unknown send capability", text)
	}
}

// quickDaemonStatus is a testing/quick generator for the daemon status a
// client decides against: no send block (older daemon), a partial block, or
// a full one, with arbitrary (including malformed) entries and conditions
// this build does not know.
type quickDaemonStatus struct {
	status localapi.DaemonStatus
}

func (quickDaemonStatus) Generate(r *rand.Rand, _ int) reflect.Value {
	if r.Intn(5) == 0 {
		return reflect.ValueOf(quickDaemonStatus{})
	}
	conditions := []string{"", "future_condition"}
	for _, condition := range allSendConditions {
		conditions = append(conditions, string(condition))
	}
	block := map[string]localapi.PlatformSendCapability{}
	for _, platform := range []string{sendcap.PlatformSMS, sendcap.PlatformWhatsApp, sendcap.PlatformSignal} {
		if r.Intn(4) == 0 {
			continue
		}
		block[platform] = localapi.PlatformSendCapability{
			Available: r.Intn(3) == 0,
			Queueable: r.Intn(2) == 0,
			Reason:    fmt.Sprintf("reason-%d", r.Intn(3)),
			Condition: conditions[r.Intn(len(conditions))],
		}
	}
	return reflect.ValueOf(quickDaemonStatus{status: localapi.DaemonStatus{Send: block}})
}

// TestQuickRouteSendabilityAgreesWithEnforcement (I10): for every daemon
// status, including an older daemon without a send block, a route is
// "unavailable" exactly when send-time enforcement refuses (daemon-routed
// and in-process alike), sendable implies a known available capability, and
// an unknown capability is never reported available (the G4 defect invented
// SMS availability here).
func TestQuickRouteSendabilityAgreesWithEnforcement(t *testing.T) {
	property := func(generated quickDaemonStatus) bool {
		status := generated.status
		capabilities, unknownReason := routeCapabilitiesFromDaemonStatus(status)
		for _, platform := range []string{sendcap.PlatformSMS, sendcap.PlatformWhatsApp, sendcap.PlatformSignal} {
			state, reason := routeSupportsOutbound(&db.Conversation{SourcePlatform: platform}, capabilities, unknownReason)
			daemonRefuses := daemonCheckPlatformSendable(status, platform) != nil
			inProcessRefuses := checkPlatformSendable(capabilities, platform) != nil
			routeUnavailable := state == string(sendcap.TierUnavailable)
			if routeUnavailable != daemonRefuses || daemonRefuses != inProcessRefuses {
				t.Logf("%s: route %q, daemon refuses %v, in-process refuses %v (status %+v)", platform, state, daemonRefuses, inProcessRefuses, status)
				return false
			}
			published, known := status.SendCapabilityFor(platform)
			if state == string(sendcap.TierAvailable) && (!known || !published.Available) {
				t.Logf("%s: route available without a published available capability (%+v, known %v)", platform, published, known)
				return false
			}
			if !known && state != string(sendcap.TierUnknown) {
				t.Logf("%s: unknown capability reported as %q", platform, state)
				return false
			}
			if state != string(sendcap.TierAvailable) && reason == "" {
				t.Logf("%s: %q route without a reason", platform, state)
				return false
			}
		}
		return true
	}
	config := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(0x6a0e7e5))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// seedLeighRoutes stores one person's sms and whatsapp threads, plus a signal
// thread when withSignal is set.
func seedLeighRoutes(t *testing.T, a *app.App, withSignal bool) {
	t.Helper()
	conversations := []*db.Conversation{
		{ConversationID: "sms-conv-1", Name: "Leigh Gibson", Participants: `[{"name":"Leigh Gibson","number":"+15551230000"}]`, LastMessageTS: 1000, SourcePlatform: "sms"},
		{ConversationID: "whatsapp:15551230000@s.whatsapp.net", Name: "Leigh Gibson", Participants: `[{"name":"Leigh Gibson","number":"+15551230000"}]`, LastMessageTS: 1001, SourcePlatform: "whatsapp"},
	}
	identifiers := `[{"platform":"sms","value":"+15551230000"},{"platform":"whatsapp","value":"+15551230000"}]`
	if withSignal {
		conversations = append(conversations, &db.Conversation{ConversationID: "signal:+15551230000", Name: "Leigh Gibson", Participants: `[{"name":"Leigh Gibson","number":"+15551230000"}]`, LastMessageTS: 1002, SourcePlatform: "signal"})
		identifiers = `[{"platform":"sms","value":"+15551230000"},{"platform":"whatsapp","value":"+15551230000"},{"platform":"signal","value":"+15551230000"}]`
	}
	for _, conversation := range conversations {
		if err := a.Store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %s: %v", conversation.ConversationID, err)
		}
	}
	if err := a.Store.UpsertUnifiedContact(&db.UnifiedContact{UnifiedID: "leigh", DisplayName: "Leigh Gibson", Identifiers: identifiers}); err != nil {
		t.Fatalf("seed unified contact: %v", err)
	}
}

func singleRouteMatch(t *testing.T, result *mcp.CallToolResult) resolvedRouteMatch {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	matches, ok := structuredMap(t, result)["matches"].([]resolvedRouteMatch)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %#v, want exactly one", structuredMap(t, result)["matches"])
	}
	return matches[0]
}

// stubPlatformStatuses swaps the in-process status seams and returns the
// restore function.
func stubPlatformStatuses(google app.GoogleStatusSnapshot, whatsApp whatsapplive.StatusSnapshot, signal signallive.StatusSnapshot) func() {
	originalGoogle, originalWhatsApp, originalSignal := googleStatus, whatsAppStatus, signalStatus
	googleStatus = func(*app.App) app.GoogleStatusSnapshot { return google }
	whatsAppStatus = func(*app.App) whatsapplive.StatusSnapshot { return whatsApp }
	signalStatus = func(*app.App) signallive.StatusSnapshot { return signal }
	return func() {
		googleStatus, whatsAppStatus, signalStatus = originalGoogle, originalWhatsApp, originalSignal
	}
}
