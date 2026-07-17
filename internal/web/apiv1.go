package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

// V2Options contains the opt-in Wave-3 send stack. A nil value means the v2
// routes remain mounted but disabled, which preserves a stable local API shape
// while OPENMESSAGES_V2_SEND is off.
type V2Options struct {
	Service  *messaging.MessageService
	Media    *media.Service
	V2Store  *sqlite.Store
	Blobs    *blob.BlobStore
	Registry bridge.Registry
}

type v1API struct {
	legacy  *db.Store
	logger  zerolog.Logger
	v2      *V2Options
	primary bool
}

type v1SubmissionResponse struct {
	OutboxID       string                `json:"outbox_id"`
	LocalMessageID string                `json:"local_message_id"`
	State          messaging.OutboxState `json:"state"`
	ScheduledForMS int64                 `json:"scheduled_for_ms"`
	Deduplicated   bool                  `json:"deduplicated"`
}

type v1DeliveryResponse struct {
	OutboxID        string                `json:"outbox_id"`
	State           messaging.OutboxState `json:"state"`
	LocalMessageID  string                `json:"local_message_id,omitempty"`
	RemoteMessageID string                `json:"remote_message_id,omitempty"`
	ErrorClass      string                `json:"error_class,omitempty"`
	ErrorCode       string                `json:"error_code,omitempty"`
	Warning         string                `json:"warning,omitempty"`
}

type v1PendingResponse struct {
	OutboxID       string                `json:"outbox_id"`
	AccountID      string                `json:"account_id"`
	ConversationID string                `json:"conversation_id"`
	Kind           sqlite.OutboxKind     `json:"kind"`
	State          messaging.OutboxState `json:"state"`
	ScheduledForMS int64                 `json:"scheduled_for_ms"`
	NextAttemptMS  *int64                `json:"next_attempt_at_ms,omitempty"`
	AttemptCount   int64                 `json:"attempt_count"`
	CreatedAtMS    int64                 `json:"created_at_ms"`
	Summary        string                `json:"summary"`
	ErrorClass     string                `json:"error_class,omitempty"`
	ErrorCode      string                `json:"error_code,omitempty"`
}

func registerV1Routes(mux *http.ServeMux, legacy *db.Store, logger zerolog.Logger, v2 *V2Options, primary bool) {
	api := &v1API{legacy: legacy, logger: logger, v2: v2, primary: primary}

	mux.HandleFunc("POST /api/v1/outbox/messages", api.submitText)
	mux.HandleFunc("POST /api/v1/outbox/media", api.submitMedia)
	mux.HandleFunc("GET /api/v1/outbox", api.listPending)
	mux.HandleFunc("GET /api/v1/outbox/{id}", api.getDelivery)
	mux.HandleFunc("POST /api/v1/outbox/{id}/cancel", api.cancel)
	mux.HandleFunc("POST /api/v1/outbox/{id}/retry", api.retry)
	mux.HandleFunc("POST /api/v1/outbox/{id}/send-again", api.sendAgain)
	mux.HandleFunc("POST /api/v1/outbox/{id}/repair", api.repair)
	mux.HandleFunc("GET /api/v1/outbox/{id}/media", api.outboxMedia)
	mux.HandleFunc("GET /api/v1/messages/{id}/attachments/{ordinal}", api.messageAttachment)

	// Keep the whole versioned namespace deterministic while the flag is off,
	// including paths introduced by a newer client than this daemon knows.
	mux.HandleFunc("/api/v1", api.v1Fallback)
	mux.HandleFunc("/api/v1/", api.v1Fallback)
}

func (a *v1API) v1Fallback(w http.ResponseWriter, _ *http.Request) {
	if !a.enabled(w) {
		return
	}
	httpError(w, "not found", http.StatusNotFound)
}

func (a *v1API) enabled(w http.ResponseWriter) bool {
	if a.v2 != nil {
		return true
	}
	httpError(w, "v2_send_disabled", http.StatusServiceUnavailable)
	return false
}

func (a *v1API) submitText(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) {
		return
	}
	var request struct {
		ConversationID string `json:"conversation_id"`
		Body           string `json:"body"`
		ReplyToID      string `json:"reply_to_id,omitempty"`
		IdempotencyKey string `json:"idempotency_key"`
		NotBeforeMS    *int64 `json:"not_before_ms,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		httpError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	conversationID := strings.TrimSpace(request.ConversationID)
	if conversationID == "" {
		httpError(w, "conversation_id is required", http.StatusBadRequest)
		return
	}
	idempotencyKey, err := normalizeRequiredV2IdempotencyKey(request.IdempotencyKey)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	notBefore, err := validateOptionalSchedule(request.NotBeforeMS)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !a.submitDependenciesAvailable(w) {
		return
	}

	input := v2wire.TextInput{
		ConversationID: conversationID,
		Body:           request.Body,
		ReplyToID:      strings.TrimSpace(request.ReplyToID),
		IdempotencyKey: idempotencyKey,
		NotBefore:      notBefore,
	}
	var submission messaging.Submission
	if a.primary {
		submission, err = v2wire.SubmitTextV2(r.Context(), a.nativeDeps(), input)
	} else {
		submission, err = v2wire.SubmitText(r.Context(), a.v2Deps(), input)
	}
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeJSON(w, submissionResponse(submission))
}

func (a *v1API) submitMedia(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) {
		return
	}
	if !parseBoundedMultipartForm(w, r) {
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	conversationID := strings.TrimSpace(r.FormValue("conversation_id"))
	if conversationID == "" {
		httpError(w, "conversation_id is required", http.StatusBadRequest)
		return
	}
	idempotencyKey, err := normalizeRequiredV2IdempotencyKey(r.FormValue("idempotency_key"))
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	notBefore, err := parseOptionalSchedule(r.FormValue("not_before_ms"))
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !a.submitDependenciesAvailable(w) {
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpError(w, "file is required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	mimeType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	input := v2wire.MediaInput{
		ConversationID: conversationID,
		Content:        file,
		Filename:       header.Filename,
		MIME:           mimeType,
		Caption:        strings.TrimSpace(r.FormValue("caption")),
		ReplyToID:      strings.TrimSpace(r.FormValue("reply_to_id")),
		IdempotencyKey: idempotencyKey,
		NotBefore:      notBefore,
	}
	var submission messaging.Submission
	if a.primary {
		submission, err = v2wire.SubmitMediaV2(r.Context(), a.nativeDeps(), input)
	} else {
		submission, err = v2wire.SubmitMedia(r.Context(), a.v2Deps(), input)
	}
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeJSON(w, submissionResponse(submission))
}

func (a *v1API) listPending(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) || !a.serviceAvailable(w) {
		return
	}
	deliveries, err := a.v2.Service.ListPending(r.Context(), messaging.ListPendingQuery{
		AccountID:      strings.TrimSpace(r.URL.Query().Get("account_id")),
		ConversationID: strings.TrimSpace(r.URL.Query().Get("conversation_id")),
		Limit:          queryIntClamped(r, "limit", 100, 500),
	})
	if err != nil {
		a.writeError(w, err)
		return
	}
	response := make([]v1PendingResponse, 0, len(deliveries))
	for _, delivery := range deliveries {
		response = append(response, pendingResponse(delivery))
	}
	writeJSON(w, response)
}

func (a *v1API) getDelivery(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) || !a.serviceAvailable(w) {
		return
	}
	delivery, err := a.v2.Service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeJSON(w, deliveryResponse(delivery))
}

func (a *v1API) cancel(w http.ResponseWriter, r *http.Request) {
	a.deliveryAction(w, r, a.v2Cancel)
}

func (a *v1API) retry(w http.ResponseWriter, r *http.Request) {
	a.deliveryAction(w, r, a.v2Retry)
}

func (a *v1API) repair(w http.ResponseWriter, r *http.Request) {
	a.deliveryAction(w, r, a.v2Repair)
}

func (a *v1API) deliveryAction(
	w http.ResponseWriter,
	r *http.Request,
	action func(context.Context, string) (messaging.Delivery, error),
) {
	if !a.enabled(w) || !a.serviceAvailable(w) {
		return
	}
	delivery, err := action(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeJSON(w, deliveryResponse(delivery))
}

func (a *v1API) v2Cancel(ctx context.Context, id string) (messaging.Delivery, error) {
	return a.v2.Service.Cancel(ctx, id)
}

func (a *v1API) v2Retry(ctx context.Context, id string) (messaging.Delivery, error) {
	return a.v2.Service.RetryNotDispatched(ctx, id)
}

func (a *v1API) v2Repair(ctx context.Context, id string) (messaging.Delivery, error) {
	return a.v2.Service.RepairStoreFailed(ctx, id)
}

func (a *v1API) sendAgain(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) || !a.serviceAvailable(w) {
		return
	}
	var request struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		httpError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	idempotencyKey, err := normalizeRequiredV2IdempotencyKey(request.IdempotencyKey)
	if err != nil {
		httpError(w, err.Error(), http.StatusBadRequest)
		return
	}
	submission, err := a.v2.Service.SendAgain(r.Context(), r.PathValue("id"), idempotencyKey)
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeJSON(w, submissionResponse(submission))
}

func (a *v1API) outboxMedia(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) {
		return
	}
	if a.v2.V2Store == nil || a.v2.Blobs == nil {
		a.internalUnavailable(w, "outbox media dependencies are not configured")
		return
	}
	repository, err := sqlite.NewOutboxRepository(a.v2.V2Store, time.Now)
	if err != nil {
		a.writeError(w, err)
		return
	}
	attachment, err := repository.GetOutboxAttachment(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, err)
		return
	}
	reader, err := a.v2.Blobs.Open(blob.BlobRef{Hash: attachment.BlobHash})
	if err != nil {
		a.writeError(w, err)
		return
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		a.writeError(w, err)
		return
	}
	writeMediaResponse(w, data, attachment.Filename, attachment.MIME)
}

func (a *v1API) messageAttachment(w http.ResponseWriter, r *http.Request) {
	if !a.enabled(w) {
		return
	}
	if a.v2.V2Store == nil || a.v2.Media == nil {
		a.internalUnavailable(w, "message media dependencies are not configured")
		return
	}
	ordinal, err := strconv.ParseInt(r.PathValue("ordinal"), 10, 64)
	if err != nil || ordinal < 0 {
		httpError(w, "attachment ordinal must be a non-negative integer", http.StatusBadRequest)
		return
	}
	messages, err := sqlite.NewMessageRepository(a.v2.V2Store, time.Now)
	if err != nil {
		a.writeError(w, err)
		return
	}
	message, err := messages.GetMessage(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, err)
		return
	}
	descriptor, err := a.v2.Media.Resolve(r.Context(), media.DownloadCommand{
		AccountID: message.AccountID,
		MessageID: message.MessageID,
		Ordinal:   ordinal,
	})
	if err != nil {
		a.writeError(w, err)
		return
	}
	reader, err := a.v2.Media.Open(descriptor.Ref)
	if err != nil {
		a.writeError(w, err)
		return
	}
	defer reader.Close()
	writeDescriptorMediaResponse(w, reader, descriptor)
}

func (a *v1API) v2Deps() v2wire.Deps {
	return v2wire.Deps{
		Legacy:   a.legacy,
		V2:       a.v2.V2Store,
		Service:  a.v2.Service,
		Registry: a.v2.Registry,
	}
}

func (a *v1API) nativeDeps() v2wire.NativeDeps {
	return v2wire.NativeDeps{
		V2:       a.v2.V2Store,
		Service:  a.v2.Service,
		Registry: a.v2.Registry,
	}
}

func (a *v1API) submitDependenciesAvailable(w http.ResponseWriter) bool {
	legacyAvailable := a.primary || a.legacy != nil
	if legacyAvailable && a.v2.Service != nil && a.v2.V2Store != nil && a.v2.Registry != nil {
		return true
	}
	a.internalUnavailable(w, "v2 submission dependencies are not configured")
	return false
}

func (a *v1API) serviceAvailable(w http.ResponseWriter) bool {
	if a.v2.Service != nil {
		return true
	}
	a.internalUnavailable(w, "v2 message service is not configured")
	return false
}

func (a *v1API) internalUnavailable(w http.ResponseWriter, detail string) {
	a.logger.Error().Str("detail", detail).Msg("V2 API dependency unavailable")
	httpError(w, "v2_send_unavailable", http.StatusServiceUnavailable)
}

func (a *v1API) writeError(w http.ResponseWriter, err error) {
	code, message := v1ErrorResponse(err)
	if code == http.StatusInternalServerError {
		a.logger.Error().Err(err).Msg("V2 API request failed")
	}
	httpError(w, message, code)
}

func v1ErrorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, v2wire.ErrReplyTargetUnavailable):
		return http.StatusUnprocessableEntity, "reply_target_unavailable"
	case errors.Is(err, messaging.ErrIdempotencyConflict):
		return http.StatusConflict, err.Error()
	case errors.Is(err, messaging.ErrInvalidState):
		return http.StatusConflict, err.Error()
	case errors.Is(err, v2wire.ErrPlatformNotSendable),
		errors.Is(err, messaging.ErrUnsupported),
		errors.Is(err, media.ErrUnsupported):
		return http.StatusNotImplemented, err.Error()
	case errors.Is(err, media.ErrUnavailable):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, sqlite.ErrNotFound),
		errors.Is(err, sql.ErrNoRows),
		errors.Is(err, media.ErrNotFound),
		errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, "not found"
	case errors.Is(err, messaging.ErrInvalidCommand):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, messaging.ErrTooLarge), errors.Is(err, media.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, err.Error()
	}
	var bodyTooLarge *http.MaxBytesError
	if errors.As(err, &bodyTooLarge) {
		return http.StatusRequestEntityTooLarge, err.Error()
	}
	var blobTooLarge *blob.ErrTooLarge
	if errors.As(err, &blobTooLarge) {
		return http.StatusRequestEntityTooLarge, err.Error()
	}
	return http.StatusInternalServerError, "internal server error"
}

func validateOptionalSchedule(notBeforeMS *int64) (time.Time, error) {
	if notBeforeMS == nil {
		return time.Time{}, nil
	}
	if err := db.ValidateScheduleTime(*notBeforeMS, time.Now().UnixMilli()); err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(*notBeforeMS), nil
}

func parseOptionalSchedule(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("not_before_ms must be an integer")
	}
	return validateOptionalSchedule(&value)
}

func normalizeRequiredV2IdempotencyKey(raw string) (string, error) {
	key, err := normalizeSendIdempotencyKey(raw)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", errors.New("idempotency_key is required")
	}
	return key, nil
}

func submissionResponse(submission messaging.Submission) v1SubmissionResponse {
	return v1SubmissionResponse{
		OutboxID:       submission.OutboxID,
		LocalMessageID: submission.LocalMessageID,
		State:          submission.State,
		ScheduledForMS: submission.ScheduledFor.UnixMilli(),
		Deduplicated:   submission.Deduplicated,
	}
}

func deliveryResponse(delivery messaging.Delivery) v1DeliveryResponse {
	return v1DeliveryResponse{
		OutboxID:        delivery.OutboxID,
		State:           delivery.State,
		LocalMessageID:  delivery.LocalMessageID,
		RemoteMessageID: delivery.RemoteMessageID,
		ErrorClass:      delivery.ErrorClass,
		ErrorCode:       delivery.ErrorCode,
		Warning:         delivery.Warning,
	}
}

func pendingResponse(delivery messaging.PendingDelivery) v1PendingResponse {
	response := v1PendingResponse{
		OutboxID:       delivery.OutboxID,
		AccountID:      delivery.AccountID,
		ConversationID: delivery.ConversationID,
		Kind:           delivery.Kind,
		State:          delivery.State,
		ScheduledForMS: delivery.ScheduledFor.UnixMilli(),
		AttemptCount:   delivery.AttemptCount,
		CreatedAtMS:    delivery.CreatedAt.UnixMilli(),
		Summary:        delivery.Summary,
		ErrorClass:     delivery.ErrorClass,
		ErrorCode:      delivery.ErrorCode,
	}
	if !delivery.NextAttemptAt.IsZero() {
		nextAttemptMS := delivery.NextAttemptAt.UnixMilli()
		response.NextAttemptMS = &nextAttemptMS
	}
	return response
}

func writeV2BlobMediaResponse(w http.ResponseWriter, blobs *blob.BlobStore, message *db.Message) error {
	hash := strings.TrimPrefix(message.MediaID, "v2blob:")
	reader, err := blobs.Open(blob.BlobRef{Hash: hash})
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	writeMediaResponse(w, data, "", message.MimeType)
	return nil
}

func mirrorV2ReadCursor(
	ctx context.Context,
	legacy *db.Store,
	v2 *sqlite.Store,
	conversationID string,
	atMS int64,
) error {
	if v2 == nil {
		return fmt.Errorf("v2 store is not configured")
	}
	return v2wire.MirrorReadCursor(ctx, legacy, v2, conversationID, atMS)
}

func writeDescriptorMediaResponse(w http.ResponseWriter, reader io.Reader, descriptor media.Descriptor) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", descriptor.MIME)
	w.Header().Set(
		"Content-Disposition",
		string(descriptor.Disposition)+`; filename="`+sanitizeMediaFilename(descriptor.Filename)+`"`,
	)
	if descriptor.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(descriptor.Size, 10))
	}
	_, _ = io.Copy(w, reader)
}
