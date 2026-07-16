package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestV1OutboxA3ContractMatrix(t *testing.T) {
	t.Run("duplicate intent is one row and one dispatch", func(t *testing.T) {
		h := newA3Harness(t, true, a3SendStep{result: bridge.SendResult{
			RemoteMessageID: "remote-deduplicated",
		}})

		first := h.postText(t, "a3-duplicate-key", "send exactly once")
		assertA3Status(t, first, http.StatusOK)
		firstSubmission := decodeA3JSON[v1SubmissionResponse](t, first.body)
		if firstSubmission.State != messaging.OutboxQueued || firstSubmission.Deduplicated {
			t.Fatalf("first submission = %+v, want new queued intent", firstSubmission)
		}

		replay := h.postText(t, "a3-duplicate-key", "send exactly once")
		assertA3Status(t, replay, http.StatusOK)
		replaySubmission := decodeA3JSON[v1SubmissionResponse](t, replay.body)
		if replaySubmission.OutboxID != firstSubmission.OutboxID ||
			replaySubmission.LocalMessageID != firstSubmission.LocalMessageID ||
			!replaySubmission.Deduplicated {
			t.Fatalf("replay = %+v, want same durable identities with deduplicated=true", replaySubmission)
		}

		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
			t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
		}
		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 0 {
			t.Fatalf("DispatchDue(after confirmation) = %d, %v; want 0, nil", processed, err)
		}
		requests := h.sender.snapshotRequests()
		if len(requests) != 1 || requests[0].Body != "send exactly once" || requests[0].RequestID == "" {
			t.Fatalf("transport requests = %+v, want exactly one matching request", requests)
		}

		got := h.getDelivery(t, firstSubmission.OutboxID)
		if got.State != messaging.OutboxConfirmed || got.RemoteMessageID != "remote-deduplicated" {
			t.Fatalf("GET delivery = %+v, want confirmed remote delivery", got)
		}
	})

	t.Run("changed payload conflicts and list exposes original", func(t *testing.T) {
		h := newA3Harness(t, false)
		first := h.postText(t, "a3-conflict-key", "original body")
		assertA3Status(t, first, http.StatusOK)
		original := decodeA3JSON[v1SubmissionResponse](t, first.body)

		changed := h.postText(t, "a3-conflict-key", "changed body")
		assertA3Status(t, changed, http.StatusConflict)

		listed := h.request(t, http.MethodGet,
			"/api/v1/outbox?account_id=google-primary&conversation_id="+a3GoogleConversationID+"&limit=10",
			"", nil)
		assertA3Status(t, listed, http.StatusOK)
		pending := decodeA3JSON[[]v1PendingResponse](t, listed.body)
		if len(pending) != 1 || pending[0].OutboxID != original.OutboxID ||
			pending[0].State != messaging.OutboxQueued || pending[0].Summary != "original body" {
			t.Fatalf("GET pending = %+v, want only original queued intent", pending)
		}
		if got := h.sender.requestCount(); got != 0 {
			t.Fatalf("transport dispatch count = %d, want 0", got)
		}
	})

	t.Run("unavailable bridge remains queued then dispatches", func(t *testing.T) {
		h := newA3Harness(t, false, a3SendStep{result: bridge.SendResult{
			RemoteMessageID: "remote-after-connect",
		}})
		response := h.postText(t, "a3-offline-key", "wait for connection")
		assertA3Status(t, response, http.StatusOK)
		submission := decodeA3JSON[v1SubmissionResponse](t, response.body)

		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(unavailable) = %d, %v; want 1, nil", processed, err)
		}
		queued := h.getDelivery(t, submission.OutboxID)
		if queued.State != messaging.OutboxQueued || h.sender.requestCount() != 0 {
			t.Fatalf("offline delivery = %+v, sends=%d; want queued with no send", queued, h.sender.requestCount())
		}
		row, err := h.outbox.FindByID(context.Background(), submission.OutboxID)
		if err != nil {
			t.Fatal(err)
		}
		if row.AttemptCount != 0 || row.NextAttemptAtMS != nil {
			t.Fatalf("offline outbox row = %+v, want zero attempts and no retry time", row)
		}

		h.registry.setOnline(true)
		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(available) = %d, %v; want 1, nil", processed, err)
		}
		confirmed := h.getDelivery(t, submission.OutboxID)
		if confirmed.State != messaging.OutboxConfirmed ||
			confirmed.RemoteMessageID != "remote-after-connect" || h.sender.requestCount() != 1 {
			t.Fatalf("connected delivery = %+v, sends=%d; want one confirmed send", confirmed, h.sender.requestCount())
		}

		listed := h.request(t, http.MethodGet, "/api/v1/outbox", "", nil)
		assertA3Status(t, listed, http.StatusOK)
		if pending := decodeA3JSON[[]v1PendingResponse](t, listed.body); len(pending) != 0 {
			t.Fatalf("GET pending after confirmation = %+v, want empty", pending)
		}
	})

	t.Run("safe retry reuses transport request ID", func(t *testing.T) {
		h := newA3Harness(t, true,
			a3SendStep{err: bridge.OpError{
				Class:     bridge.FailureTransient,
				Operation: "prepare_send",
				Dispatch:  bridge.DispatchNotCalled,
				Cause:     errors.New("failed before remote call"),
			}},
			a3SendStep{result: bridge.SendResult{RemoteMessageID: "remote-after-retry"}},
		)
		response := h.postText(t, "a3-retry-key", "safe to retry")
		assertA3Status(t, response, http.StatusOK)
		submission := decodeA3JSON[v1SubmissionResponse](t, response.body)

		if processed, err := h.service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(first) = %d, %v; want 1, nil", processed, err)
		}
		failed := h.getDelivery(t, submission.OutboxID)
		if failed.State != messaging.OutboxNotDispatched ||
			failed.ErrorClass != string(bridge.FailureTransient) || failed.ErrorCode != "prepare_send" {
			t.Fatalf("pre-call failure = %+v, want transient not_dispatched", failed)
		}
		before, err := h.outbox.FindByID(context.Background(), submission.OutboxID)
		if err != nil {
			t.Fatal(err)
		}

		retried := h.request(t, http.MethodPost, "/api/v1/outbox/"+submission.OutboxID+"/retry", "", nil)
		assertA3Status(t, retried, http.StatusOK)
		if delivery := decodeA3JSON[v1DeliveryResponse](t, retried.body); delivery.State != messaging.OutboxNotDispatched {
			t.Fatalf("retry action response = %+v, want immediately due not_dispatched", delivery)
		}
		after, err := h.outbox.FindByID(context.Background(), submission.OutboxID)
		if err != nil {
			t.Fatal(err)
		}
		if before.TransportRequestID == "" || after.TransportRequestID != before.TransportRequestID {
			t.Fatalf("transport request ID changed across Retry: before=%q after=%q", before.TransportRequestID, after.TransportRequestID)
		}

		if processed, err := h.service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(retry) = %d, %v; want 1, nil", processed, err)
		}
		requests := h.sender.snapshotRequests()
		if len(requests) != 2 || requests[0].RequestID != requests[1].RequestID ||
			requests[0].RequestID != before.TransportRequestID {
			t.Fatalf("retry requests = %+v, want same stable request ID", requests)
		}
		if got := h.getDelivery(t, submission.OutboxID); got.State != messaging.OutboxConfirmed ||
			got.RemoteMessageID != "remote-after-retry" {
			t.Fatalf("delivery after Retry = %+v, want confirmed", got)
		}
	})

	t.Run("uncertain replay never redispatches and send-again is linked", func(t *testing.T) {
		h := newA3Harness(t, true,
			a3SendStep{err: bridge.OpError{
				Class:     bridge.FailureTransient,
				Operation: "send_text",
				Dispatch:  bridge.DispatchUncertain,
				Cause:     context.DeadlineExceeded,
			}},
			a3SendStep{result: bridge.SendResult{RemoteMessageID: "remote-deliberate-resend"}},
		)
		response := h.postText(t, "a3-uncertain-key", "maybe delivered")
		assertA3Status(t, response, http.StatusOK)
		original := decodeA3JSON[v1SubmissionResponse](t, response.body)

		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(timeout) = %d, %v; want 1, nil", processed, err)
		}
		ambiguous := h.getDelivery(t, original.OutboxID)
		if ambiguous.State != messaging.OutboxUncertain || ambiguous.Warning == "" {
			t.Fatalf("post-call timeout delivery = %+v, want uncertain warning", ambiguous)
		}

		replay := h.postText(t, "a3-uncertain-key", "maybe delivered")
		assertA3Status(t, replay, http.StatusOK)
		replayed := decodeA3JSON[v1SubmissionResponse](t, replay.body)
		if replayed.OutboxID != original.OutboxID || !replayed.Deduplicated ||
			replayed.State != messaging.OutboxUncertain {
			t.Fatalf("repeated HTTP submission = %+v, want same uncertain row", replayed)
		}
		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 0 {
			t.Fatalf("DispatchDue(after replay) = %d, %v; want 0, nil", processed, err)
		}
		if got := h.sender.requestCount(); got != 1 {
			t.Fatalf("send count after repeated HTTP = %d, want 1", got)
		}

		sendAgain := h.request(t, http.MethodPost, "/api/v1/outbox/"+original.OutboxID+"/send-again",
			"application/json", []byte(`{"idempotency_key":"a3-send-again-key"}`))
		assertA3Status(t, sendAgain, http.StatusOK)
		resent := decodeA3JSON[v1SubmissionResponse](t, sendAgain.body)
		if resent.OutboxID == original.OutboxID || resent.State != messaging.OutboxQueued || resent.Deduplicated {
			t.Fatalf("send-again submission = %+v, want new queued intent", resent)
		}
		resentRow, err := h.outbox.FindByID(context.Background(), resent.OutboxID)
		if err != nil {
			t.Fatal(err)
		}
		if resentRow.SendAgainOfOutboxID == nil || *resentRow.SendAgainOfOutboxID != original.OutboxID {
			t.Fatalf("send-again row = %+v, want predecessor %q", resentRow, original.OutboxID)
		}

		sendAgainReplay := h.request(t, http.MethodPost, "/api/v1/outbox/"+original.OutboxID+"/send-again",
			"application/json", []byte(`{"idempotency_key":"a3-send-again-key"}`))
		assertA3Status(t, sendAgainReplay, http.StatusOK)
		replayedResend := decodeA3JSON[v1SubmissionResponse](t, sendAgainReplay.body)
		if replayedResend.OutboxID != resent.OutboxID || !replayedResend.Deduplicated {
			t.Fatalf("repeated send-again = %+v, want same linked row", replayedResend)
		}

		if processed, err := h.service.DispatchDue(context.Background(), 10); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(send-again) = %d, %v; want 1, nil", processed, err)
		}
		requests := h.sender.snapshotRequests()
		if len(requests) != 2 || requests[0].RequestID == requests[1].RequestID {
			t.Fatalf("original/resend requests = %+v, want exactly two distinct intents", requests)
		}
		if got := h.getDelivery(t, original.OutboxID); got.State != messaging.OutboxUncertain {
			t.Fatalf("send-again mutated predecessor = %+v", got)
		}
		if got := h.getDelivery(t, resent.OutboxID); got.State != messaging.OutboxConfirmed ||
			got.RemoteMessageID != "remote-deliberate-resend" {
			t.Fatalf("send-again delivery = %+v, want confirmed", got)
		}
	})

	t.Run("multipart max plus one is rejected before outbox and blob allocation", func(t *testing.T) {
		h := newA3Harness(t, true)
		originalLimit := maxUploadBytes
		maxUploadBytes = 1024
		t.Cleanup(func() { maxUploadBytes = originalLimit })

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("conversation_id", a3GoogleConversationID); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("idempotency_key", "a3-oversized-media"); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("file", "too-large.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte{'x'}, int(maxUploadBytes+1))); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		response := h.request(t, http.MethodPost, "/api/v1/outbox/media", writer.FormDataContentType(), body.Bytes())
		assertA3Status(t, response, http.StatusRequestEntityTooLarge)
		if got := h.ids.count(); got != 0 {
			t.Fatalf("ID allocations = %d, want 0 before outbox allocation", got)
		}
		pending, err := h.service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("pending outbox after oversized upload = %+v, want empty", pending)
		}
		confirmed, err := h.outbox.ListConfirmedSince(context.Background(), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(confirmed) != 0 {
			t.Fatalf("confirmed outbox after oversized upload = %+v, want empty", confirmed)
		}
		if files := a3BlobFiles(t, h.blobRoot); len(files) != 0 {
			t.Fatalf("blob files after oversized upload = %v, want none", files)
		}
	})

	t.Run("archive platform is not sendable", func(t *testing.T) {
		h := newA3Harness(t, true)
		const conversationID = "archived-imessage-thread"
		if err := h.legacy.UpsertConversation(&db.Conversation{
			ConversationID: conversationID,
			Name:           "Old iMessage thread",
			Participants:   `[]`,
			SourcePlatform: "imessage",
		}); err != nil {
			t.Fatal(err)
		}

		response := h.postTextToConversation(t, conversationID, "a3-imessage-key", "must not enqueue")
		assertA3Status(t, response, http.StatusNotImplemented)
		if got := h.sender.requestCount(); got != 0 {
			t.Fatalf("archive-platform send count = %d, want 0", got)
		}
		pending, err := h.service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("archive-platform pending rows = %+v, want none", pending)
		}
	})
}

const a3GoogleConversationID = "google-thread-a3"

type a3Harness struct {
	legacy   *db.Store
	v2       *sqlite.Store
	blobRoot string
	service  *messaging.MessageService
	outbox   *sqlite.OutboxRepository
	registry *a3Registry
	sender   *a3TextSender
	ids      *a3IDs
	handler  http.Handler
}

func newA3Harness(t *testing.T, online bool, steps ...a3SendStep) *a3Harness {
	t.Helper()
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: a3GoogleConversationID,
		Name:           "A3 Google conversation",
		Participants:   `[]`,
		SourcePlatform: "sms",
	}); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}

	v2, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	blobs, err := blob.New(blobRoot)
	if err != nil {
		_ = v2.Close()
		_ = legacy.Close()
		t.Fatal(err)
	}

	sender := &a3TextSender{steps: append([]a3SendStep(nil), steps...)}
	registry := &a3Registry{
		accountID: "google-primary",
		platform:  bridge.PlatformGoogle,
		online:    online,
		text:      sender,
		media:     a3MediaSender{},
	}
	clock := messaging.SystemClock{}
	ids := &a3IDs{}
	service, err := messaging.NewMessageService(v2, registry, blobs, clock, ids)
	if err != nil {
		_ = v2.Close()
		_ = legacy.Close()
		t.Fatal(err)
	}
	mediaService, err := media.NewService(v2, registry, blobs, clock)
	if err != nil {
		_ = v2.Close()
		_ = legacy.Close()
		t.Fatal(err)
	}
	outbox, err := sqlite.NewOutboxRepository(v2, clock.Now)
	if err != nil {
		_ = v2.Close()
		_ = legacy.Close()
		t.Fatal(err)
	}

	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{V2: &V2Options{
		Service:  service,
		Media:    mediaService,
		V2Store:  v2,
		Blobs:    blobs,
		Registry: registry,
	}})
	t.Cleanup(func() {
		_ = v2.Close()
		_ = legacy.Close()
	})

	return &a3Harness{
		legacy:   legacy,
		v2:       v2,
		blobRoot: blobRoot,
		service:  service,
		outbox:   outbox,
		registry: registry,
		sender:   sender,
		ids:      ids,
		handler:  handler,
	}
}

func (h *a3Harness) postText(t *testing.T, key, body string) a3HTTPResponse {
	t.Helper()
	return h.postTextToConversation(t, a3GoogleConversationID, key, body)
}

func (h *a3Harness) postTextToConversation(
	t *testing.T,
	conversationID, key, body string,
) a3HTTPResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"conversation_id": conversationID,
		"body":            body,
		"idempotency_key": key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h.request(t, http.MethodPost, "/api/v1/outbox/messages", "application/json", payload)
}

func (h *a3Harness) getDelivery(t *testing.T, outboxID string) v1DeliveryResponse {
	t.Helper()
	response := h.request(t, http.MethodGet, "/api/v1/outbox/"+outboxID, "", nil)
	assertA3Status(t, response, http.StatusOK)
	return decodeA3JSON[v1DeliveryResponse](t, response.body)
}

func (h *a3Harness) request(
	t *testing.T,
	method, path, contentType string,
	body []byte,
) a3HTTPResponse {
	t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return a3HTTPResponse{status: response.StatusCode, body: raw}
}

type a3HTTPResponse struct {
	status int
	body   []byte
}

func assertA3Status(t *testing.T, response a3HTTPResponse, want int) {
	t.Helper()
	if response.status != want {
		t.Fatalf("HTTP status = %d, want %d; body=%s", response.status, want, response.body)
	}
}

func decodeA3JSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode JSON %q: %v", body, err)
	}
	return value
}

type a3SendStep struct {
	result bridge.SendResult
	err    error
}

type a3TextSender struct {
	mu       sync.Mutex
	steps    []a3SendStep
	requests []bridge.TextRequest
}

func (s *a3TextSender) SendText(
	_ context.Context,
	request bridge.TextRequest,
) (bridge.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if len(s.steps) == 0 {
		return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step.result, step.err
}

func (s *a3TextSender) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *a3TextSender) snapshotRequests() []bridge.TextRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bridge.TextRequest(nil), s.requests...)
}

type a3MediaSender struct{}

func (a3MediaSender) SendMedia(_ context.Context, request bridge.MediaRequest) (bridge.SendResult, error) {
	if _, err := io.Copy(io.Discard, request.Reader); err != nil {
		return bridge.SendResult{}, err
	}
	return bridge.SendResult{RemoteMessageID: "remote-" + request.RequestID}, nil
}

type a3Registry struct {
	mu        sync.Mutex
	accountID string
	platform  bridge.Platform
	online    bool
	text      bridge.TextSender
	media     bridge.MediaSender
}

func (r *a3Registry) setOnline(online bool) {
	r.mu.Lock()
	r.online = online
	r.mu.Unlock()
}

func (r *a3Registry) Snapshot(accountID string) (bridge.Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != r.accountID {
		return bridge.Snapshot{}, false
	}
	return bridge.Snapshot{
		AccountID:  r.accountID,
		Platform:   r.platform,
		Generation: 1,
	}, true
}

func (r *a3Registry) Acquire(
	ctx context.Context,
	accountID string,
	capability bridge.Capability,
) (*bridge.DispatchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID != r.accountID || !r.online {
		return nil, fmt.Errorf("%w: %s", bridge.ErrAccountNotRegistered, accountID)
	}
	lease := &bridge.DispatchLease{
		AccountID:  r.accountID,
		Platform:   r.platform,
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

func (r *a3Registry) Capabilities(accountID string) bridge.CapabilitySet {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountID != r.accountID {
		return bridge.CapabilitySet{}
	}
	return bridge.CapabilitySet{
		TextSend:  r.text != nil,
		MediaSend: r.media != nil,
	}
}

type a3IDs struct {
	mu   sync.Mutex
	next int
}

func (s *a3IDs) NewID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("a3-id-%03d", s.next), nil
}

func (s *a3IDs) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

func a3BlobFiles(t *testing.T, root string) []string {
	t.Helper()
	files := make([]string, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
