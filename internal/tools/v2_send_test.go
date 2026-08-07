package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestV2SendToConversationReturnsConfirmedDelivery(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{result: bridge.SendResult{
		RemoteMessageID: "remote-confirmed",
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "settle this send",
		"idempotency_key": "mcp-confirmed-key",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("confirmed delivery returned tool error: %v", result.Content)
	}

	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolBool(t, payload, "settled", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
	assertV2ToolString(t, payload, "remote_message_id", "remote-confirmed")
	assertV2ToolString(t, payload, "idempotency_key", "mcp-confirmed-key")
	assertV2ToolBool(t, payload, "deduplicated", false)
	if got := v2ToolString(payload, "outbox_id"); got == "" {
		t.Fatal("outbox_id is empty")
	}
	if got := v2ToolString(payload, "local_message_id"); got == "" {
		t.Fatal("local_message_id is empty")
	}
	for _, key := range []string{"error_class", "error_code", "warning"} {
		if got := v2ToolString(payload, key); got != "" {
			t.Fatalf("%s = %q, want empty", key, got)
		}
	}
}

func TestV2SendToConversationReturnsHonestUncertainDelivery(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{err: bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: "send_text",
		Dispatch:  bridge.DispatchUncertain,
		Cause:     context.DeadlineExceeded,
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)

	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "the outcome may be ambiguous",
		"idempotency_key": "mcp-uncertain-key",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("uncertain delivery must be data, not a tool error: %v", result.Content)
	}

	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	// An unknown outcome is NOT settled: reporting it settled invited agents
	// to treat the send as finished. It is flagged uncertain instead.
	assertV2ToolBool(t, payload, "settled", false)
	assertV2ToolBool(t, payload, "transmitted", false)
	assertV2ToolBool(t, payload, "uncertain", true)
	assertV2ToolString(t, payload, "transport_state", "uncertain")
	assertV2ToolString(t, payload, "state", string(messaging.OutboxUncertain))
	assertV2ToolString(t, payload, "remote_message_id", "")
	assertV2ToolString(t, payload, "idempotency_key", "mcp-uncertain-key")
	assertV2ToolString(t, payload, "error_class", string(bridge.FailureTransient))
	assertV2ToolString(t, payload, "error_code", "send_text")
	assertV2ToolString(t, payload, "warning", "delivery outcome is unknown")
	if got := v2ToolString(payload, "outbox_id"); got == "" {
		t.Fatal("outbox_id is empty")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(strings.ToLower(text), "uncertain") {
		t.Fatalf("uncertain result text does not explain its state: %q", text)
	}
}

func TestV2SendToConversationDuplicateKeyReturnsSameOutboxAndDispatchesOnce(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{result: bridge.SendResult{
		RemoteMessageID: "remote-once",
	}})
	handler := sendToConversationHandler(harness.app, &harness.deps)
	request := v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "send this exactly once",
		"idempotency_key": "mcp-duplicate-key",
	})

	first, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("first handler call: %v", err)
	}
	if first.IsError {
		t.Fatalf("first handler result is error: %v", first.Content)
	}
	second, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("second handler call: %v", err)
	}
	if second.IsError {
		t.Fatalf("second handler result is error: %v", second.Content)
	}

	firstPayload := v2ToolPayload(t, first)
	secondPayload := v2ToolPayload(t, second)
	firstOutboxID := v2ToolString(firstPayload, "outbox_id")
	if firstOutboxID == "" || v2ToolString(secondPayload, "outbox_id") != firstOutboxID {
		t.Fatalf("duplicate outbox IDs = %q and %q, want same non-empty ID",
			firstOutboxID, v2ToolString(secondPayload, "outbox_id"))
	}
	assertV2ToolBool(t, firstPayload, "deduplicated", false)
	assertV2ToolBool(t, secondPayload, "deduplicated", true)
	assertV2ToolString(t, secondPayload, "state", string(messaging.OutboxConfirmed))
	assertV2ToolString(t, secondPayload, "remote_message_id", "remote-once")
	if got := harness.sender.requestCount(); got != 1 {
		t.Fatalf("transport dispatch count = %d, want 1", got)
	}
}

func TestV2SendMediaRejectsMaxPlusOneWithoutOutbox(t *testing.T) {
	harness := newV2ToolHarness(t)
	filePath := filepath.Join(t.TempDir(), "too-large.bin")
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("Create(%s): %v", filePath, err)
	}
	if err := file.Truncate(int64(maxMediaUploadBytes) + 1); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate(%s): %v", filePath, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(%s): %v", filePath, err)
	}

	handler := sendMediaToConversationHandler(harness.app, &harness.deps)
	result, err := handler(context.Background(), v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"file_path":       filePath,
		"idempotency_key": "mcp-media-too-large",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("max+1 media result IsError = false; content=%v", result.Content)
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", false)
	errorText := strings.ToLower(v2ToolString(payload, "error"))
	if !strings.Contains(errorText, "too large") && !strings.Contains(errorText, "size limit") {
		t.Fatalf("oversize error = %q, want a clean size-limit message", errorText)
	}
	if got := harness.ids.count(); got != 0 {
		t.Fatalf("message service allocated %d IDs, want 0 before outbox creation", got)
	}
	pending, err := harness.deps.Service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListPending(): %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending deliveries after oversize media = %+v, want none", pending)
	}
	outbox, err := sqlite.NewOutboxRepository(harness.deps.V2Store, harness.clock.Now)
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	confirmed, err := outbox.ListConfirmedSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ListConfirmedSince(): %v", err)
	}
	if len(confirmed) != 0 {
		t.Fatalf("confirmed deliveries after oversize media = %+v, want none", confirmed)
	}
	if files := v2ToolBlobFiles(t, harness.blobRoot); len(files) != 0 {
		t.Fatalf("blob files after oversize media = %v, want none", files)
	}
}

func TestV2SendMessageGoogleKeepsLegacyConversationLookupButUsesV2Transport(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{result: bridge.SendResult{
		RemoteMessageID: "remote-direct-google",
	}})
	lookupCalls := 0
	legacySendCalls := 0
	originalLookup := getOrCreateGoogleConversation
	originalLegacySend := sendGoogleTextPayload
	getOrCreateGoogleConversation = func(_ *app.App, phone string) (*gmproto.Conversation, error) {
		lookupCalls++
		if phone != "+15551230000" {
			t.Fatalf("legacy GetOrCreate phone = %q, want +15551230000", phone)
		}
		return &gmproto.Conversation{
			ConversationID:       v2ToolDirectConversationID,
			Name:                 "Taylor",
			LastMessageTimestamp: 1_700_000_000_000_000,
		}, nil
	}
	sendGoogleTextPayload = func(_ *app.App, _ *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		legacySendCalls++
		return nil, errors.New("legacy Google send must not run while v2 is enabled")
	}
	t.Cleanup(func() {
		getOrCreateGoogleConversation = originalLookup
		sendGoogleTextPayload = originalLegacySend
	})

	result, err := sendMessageHandler(harness.app, &harness.deps)(
		context.Background(),
		v2ToolCall(map[string]any{
			"phone_number":    "+15551230000",
			"message":         "direct through v2",
			"idempotency_key": "mcp-direct-google",
		}),
	)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("v2 direct send returned tool error: %v", result.Content)
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
	assertV2ToolString(t, payload, "remote_message_id", "remote-direct-google")
	if lookupCalls != 1 {
		t.Fatalf("legacy GetOrCreate calls = %d, want 1", lookupCalls)
	}
	if legacySendCalls != 0 {
		t.Fatalf("legacy Google send calls = %d, want 0", legacySendCalls)
	}
	conversation, err := harness.app.Store.GetConversation(v2ToolDirectConversationID)
	if err != nil {
		t.Fatalf("load persisted conversation: %v", err)
	}
	if conversation == nil || conversation.Name != "Taylor" || conversation.SourcePlatform != "sms" {
		t.Fatalf("persisted conversation = %#v, want legacy-upserted Google conversation", conversation)
	}
	legacyMessages, err := harness.app.Store.GetMessagesByConversation(v2ToolDirectConversationID, 10)
	if err != nil {
		t.Fatalf("load legacy messages: %v", err)
	}
	if len(legacyMessages) != 0 {
		t.Fatalf("legacy send persisted messages = %#v, want none", legacyMessages)
	}
	requests := harness.sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Conversation.RemoteID != v2ToolDirectConversationID ||
		requests[0].Body != "direct through v2" {
		t.Fatalf("v2 transport requests = %+v, want direct Google conversation/body", requests)
	}
}

func TestV2SendGroupMessageKeepsLegacyConversationLookupButUsesV2Transport(t *testing.T) {
	harness := newV2ToolHarness(t, v2ToolSendStep{result: bridge.SendResult{
		RemoteMessageID: "remote-group-google",
	}})
	lookupCalls := 0
	originalLookup := getOrCreateGoogleGroupConversationV2
	getOrCreateGoogleGroupConversationV2 = func(_ *app.App, phones []string) (*gmproto.Conversation, error) {
		lookupCalls++
		want := []string{"+15551230000", "+15551230001"}
		if len(phones) != len(want) || phones[0] != want[0] || phones[1] != want[1] {
			t.Fatalf("legacy group GetOrCreate phones = %v, want %v", phones, want)
		}
		return &gmproto.Conversation{
			ConversationID:       v2ToolGroupConversationID,
			Name:                 "Weekend plans",
			IsGroupChat:          true,
			LastMessageTimestamp: 1_700_000_000_000_000,
		}, nil
	}
	t.Cleanup(func() { getOrCreateGoogleGroupConversationV2 = originalLookup })

	result, err := sendGroupMessageHandler(harness.app, &harness.deps)(
		context.Background(),
		v2ToolCall(map[string]any{
			"phone_numbers":   `["+15551230000","+15551230001"]`,
			"message":         "group through v2",
			"idempotency_key": "mcp-group-google",
		}),
	)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("v2 group send returned tool error: %v", result.Content)
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
	assertV2ToolString(t, payload, "remote_message_id", "remote-group-google")
	if lookupCalls != 1 {
		t.Fatalf("legacy group GetOrCreate calls = %d, want 1", lookupCalls)
	}
	conversation, err := harness.app.Store.GetConversation(v2ToolGroupConversationID)
	if err != nil {
		t.Fatalf("load persisted group: %v", err)
	}
	if conversation == nil || conversation.Name != "Weekend plans" || !conversation.IsGroup ||
		conversation.SourcePlatform != "sms" {
		t.Fatalf("persisted group = %#v, want legacy-upserted Google group", conversation)
	}
	legacyMessages, err := harness.app.Store.GetMessagesByConversation(v2ToolGroupConversationID, 10)
	if err != nil {
		t.Fatalf("load legacy group messages: %v", err)
	}
	if len(legacyMessages) != 0 {
		t.Fatalf("legacy group send persisted messages = %#v, want none", legacyMessages)
	}
	requests := harness.sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Conversation.RemoteID != v2ToolGroupConversationID ||
		requests[0].Body != "group through v2" {
		t.Fatalf("v2 transport requests = %+v, want group Google conversation/body", requests)
	}
}

func TestV2SendMediaToConversationStreamsAndReturnsConfirmedDelivery(t *testing.T) {
	harness := newV2ToolHarness(t)
	content := bytes.Repeat([]byte("stream-through-v2-"), 256)
	filePath := filepath.Join(t.TempDir(), "streamed-media.bin")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", filePath, err)
	}

	originalUpload := uploadGoogleMedia
	originalGetConversation := getGoogleConversation
	originalLegacySend := sendGoogleMediaMessage
	uploadGoogleMedia = func(_ *app.App, _ []byte, _, _ string) (*gmproto.MediaContent, error) {
		return nil, errors.New("legacy Google upload must not run while v2 is enabled")
	}
	getGoogleConversation = func(_ *app.App, _ string) (*gmproto.Conversation, error) {
		return nil, errors.New("legacy Google media lookup must not run while v2 is enabled")
	}
	sendGoogleMediaMessage = func(_ *app.App, _ *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		return nil, errors.New("legacy Google media send must not run while v2 is enabled")
	}
	t.Cleanup(func() {
		uploadGoogleMedia = originalUpload
		getGoogleConversation = originalGetConversation
		sendGoogleMediaMessage = originalLegacySend
	})

	result, err := sendMediaToConversationHandler(harness.app, &harness.deps)(
		context.Background(),
		v2ToolCall(map[string]any{
			"conversation_id": v2ToolConversationID,
			"file_path":       filePath,
			"caption":         "streamed caption",
			"mime_type":       "application/x-openmessage-stream",
			"idempotency_key": "mcp-streamed-media",
		}),
	)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("v2 media send returned tool error: %v", result.Content)
	}
	payload := v2ToolPayload(t, result)
	assertV2ToolBool(t, payload, "ok", true)
	assertV2ToolString(t, payload, "state", string(messaging.OutboxConfirmed))
	if got := v2ToolString(payload, "remote_message_id"); got == "" {
		t.Fatal("remote_message_id is empty")
	}
	assertV2ToolBool(t, payload, "deduplicated", false)

	requests := harness.media.snapshotRequests()
	if len(requests) != 1 {
		t.Fatalf("media transport request count = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.AccountID != "google-primary" || request.ConversationID != v2ToolConversationID ||
		request.Filename != "streamed-media.bin" || request.MIME != "application/x-openmessage-stream" ||
		request.Caption != "streamed caption" || request.Size != int64(len(content)) ||
		!bytes.Equal(request.Content, content) {
		t.Fatalf("media transport request = %+v, want full streamed file and metadata", request)
	}
	assertV2ToolString(t, payload, "remote_message_id", "remote-"+request.RequestID)
}

const (
	v2ToolConversationID       = "v2-tool-conversation"
	v2ToolDirectConversationID = "v2-tool-direct-google"
	v2ToolGroupConversationID  = "v2-tool-group-google"
)

type v2ToolHarness struct {
	app      *app.App
	deps     V2Dependencies
	sender   *v2ToolTextSender
	media    *v2ToolMediaSender
	ids      *v2ToolIDs
	clock    messaging.SystemClock
	blobRoot string
}

// stubHealthyTransportStatus models all three platforms paired, connected,
// and healthy for the duration of one test, so send-capability prechecks
// exercise the durable pipeline rather than refusing at the door.
func stubHealthyTransportStatus(t *testing.T) {
	t.Helper()
	originalGoogle, originalWhatsApp, originalSignal := googleStatus, whatsAppStatus, signalStatus
	googleStatus = func(*app.App) app.GoogleStatusSnapshot {
		return app.GoogleStatusSnapshot{Connected: true, Paired: true, PhoneResponding: true}
	}
	whatsAppStatus = func(*app.App) whatsapplive.StatusSnapshot {
		return whatsapplive.StatusSnapshot{Connected: true, Paired: true}
	}
	signalStatus = func(*app.App) signallive.StatusSnapshot {
		return signallive.StatusSnapshot{Connected: true, Paired: true}
	}
	t.Cleanup(func() {
		googleStatus = originalGoogle
		whatsAppStatus = originalWhatsApp
		signalStatus = originalSignal
	})
}

func newV2ToolHarness(t *testing.T, steps ...v2ToolSendStep) *v2ToolHarness {
	t.Helper()
	stubHealthyTransportStatus(t)
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: v2ToolConversationID,
		Name:           "V2 tool conversation",
		Participants:   `[]`,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed legacy conversation: %v", err)
	}

	v2Store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	blobs, err := blob.New(blobRoot)
	if err != nil {
		_ = v2Store.Close()
		t.Fatalf("blob.New(): %v", err)
	}
	sender := &v2ToolTextSender{steps: append([]v2ToolSendStep(nil), steps...)}
	mediaSender := &v2ToolMediaSender{}
	registry := &v2ToolRegistry{
		accountID: "google-primary",
		text:      sender,
		media:     mediaSender,
	}
	clock := messaging.SystemClock{}
	ids := &v2ToolIDs{}
	service, err := messaging.NewMessageService(v2Store, registry, blobs, clock, ids)
	if err != nil {
		_ = v2Store.Close()
		t.Fatalf("NewMessageService(): %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("MessageService.Run(): %v", err)
		}
		if err := v2Store.Close(); err != nil {
			t.Errorf("close v2 store: %v", err)
		}
	})

	return &v2ToolHarness{
		app: a,
		deps: V2Dependencies{
			Enabled:  true,
			Service:  service,
			V2Store:  v2Store,
			Registry: registry,
		},
		sender:   sender,
		media:    mediaSender,
		ids:      ids,
		clock:    clock,
		blobRoot: blobRoot,
	}
}

func v2ToolCall(arguments map[string]any) mcp.CallToolRequest {
	request := mcp.CallToolRequest{}
	request.Params.Arguments = arguments
	return request
}

func v2ToolPayload(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
	if payload == nil {
		t.Fatalf("structured content = %s, want object", raw)
	}
	return payload
}

func assertV2ToolString(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	if got := v2ToolString(payload, key); got != want {
		t.Fatalf("%s = %q, want %q; payload=%v", key, got, want, payload)
	}
}

func v2ToolString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func assertV2ToolBool(t *testing.T, payload map[string]any, key string, want bool) {
	t.Helper()
	got, ok := payload[key].(bool)
	if !ok || got != want {
		t.Fatalf("%s = %#v, want %t; payload=%v", key, payload[key], want, payload)
	}
}

type v2ToolSendStep struct {
	result bridge.SendResult
	err    error
	// block, when non-nil, parks the transport call until the channel closes,
	// holding the outbox row in dispatching so tests can interrupt Wait.
	block <-chan struct{}
}

type v2ToolTextSender struct {
	mu       sync.Mutex
	steps    []v2ToolSendStep
	requests []bridge.TextRequest
}

func (s *v2ToolTextSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	if len(s.steps) == 0 {
		s.mu.Unlock()
		return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	s.mu.Unlock()
	if step.block != nil {
		<-step.block
	}
	return step.result, step.err
}

func (s *v2ToolTextSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *v2ToolTextSender) snapshotRequests() []bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.TextRequest(nil), s.requests...)
}

type v2ToolMediaRequest struct {
	AccountID      string
	ConversationID string
	Content        []byte
	Size           int64
	Filename       string
	MIME           string
	Caption        string
	RequestID      string
}

type v2ToolMediaSender struct {
	mu       sync.Mutex
	requests []v2ToolMediaRequest
}

func (s *v2ToolMediaSender) SendMedia(
	_ context.Context,
	request bridge.MediaRequest,
) (bridge.SendResult, error) {
	content, err := io.ReadAll(request.Reader)
	if err != nil {
		return bridge.SendResult{}, err
	}
	s.mu.Lock()
	s.requests = append(s.requests, v2ToolMediaRequest{
		AccountID:      request.AccountID,
		ConversationID: request.Conversation.RemoteID,
		Content:        append([]byte(nil), content...),
		Size:           request.Size,
		Filename:       request.Filename,
		MIME:           request.MIME,
		Caption:        request.Caption,
		RequestID:      request.RequestID,
	})
	s.mu.Unlock()
	return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
}

func (s *v2ToolMediaSender) snapshotRequests() []v2ToolMediaRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := make([]v2ToolMediaRequest, len(s.requests))
	for i, request := range s.requests {
		requests[i] = request
		requests[i].Content = append([]byte(nil), request.Content...)
	}
	return requests
}

type v2ToolRegistry struct {
	accountID string
	text      bridge.TextSender
	media     bridge.MediaSender
}

func (r *v2ToolRegistry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	if accountID != r.accountID {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{
		AccountID:  accountID,
		Platform:   bridge.PlatformGoogle,
		Generation: 1,
	}, true
}

func (r *v2ToolRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID != r.accountID {
		return nil, fmt.Errorf("%w: %s", bridge.ErrAccountNotRegistered, accountID)
	}
	lease := &bridge.DispatchLease{
		AccountID:  accountID,
		Platform:   bridge.PlatformGoogle,
		Generation: 1,
		Ctx:        ctx,
	}
	switch capability {
	case bridge.CapabilityTextSend:
		lease.Text = r.text
	case bridge.CapabilityMediaSend:
		lease.Media = r.media
	default:
		return nil, bridge.ErrCapabilityUnavailable
	}
	return lease, nil
}

func (r *v2ToolRegistry) Capabilities(accountID string) bridge.CapabilitySet {
	if accountID != r.accountID {
		return bridge.CapabilitySet{}
	}
	return bridge.CapabilitySet{
		TextSend:  r.text != nil,
		MediaSend: r.media != nil,
	}
}

type v2ToolIDs struct {
	mu   sync.Mutex
	next int
}

func (s *v2ToolIDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("v2-tool-id-%03d", s.next), nil
}

func (s *v2ToolIDs) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

func v2ToolBlobFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk blob root: %v", err)
	}
	return files
}
