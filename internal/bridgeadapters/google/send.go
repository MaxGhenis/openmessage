package google

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

// conversationResolver is the part of the libgm client a send uses to find the
// conversation it sends into.
type conversationResolver interface {
	GetConversation(conversationID string) (*gmproto.Conversation, error)
	GetOrCreateConversation(
		req *gmproto.GetOrCreateConversationRequest,
	) (*gmproto.GetOrCreateConversationResponse, error)
}

type textSendClient interface {
	conversationResolver
	SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
}

var textSendClientFor = func(cli *client.Client) textSendClient {
	return cli.GM
}

type reactionSendClient interface {
	conversationResolver
	SendReaction(payload *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error)
}

var reactionSendClientFor = func(cli *client.Client) reactionSendClient {
	return cli.GM
}

type readSendClient interface {
	MarkRead(conversationID, messageID string) error
}

var readSendClientFor = func(cli *client.Client) readSendClient {
	return cli.GM
}

type downloadClient interface {
	DownloadMedia(mediaID string, key []byte) ([]byte, string, error)
}

type downloadClientFunc func(mediaID string, key []byte) ([]byte, string, error)

func (f downloadClientFunc) DownloadMedia(mediaID string, key []byte) ([]byte, string, error) {
	return f(mediaID, key)
}

var downloadClientFor = func(cli *client.Client) downloadClient {
	if cli == nil || cli.GM == nil {
		return nil
	}
	// The pinned libgm fork exposes DownloadMedia(string, []byte)
	// ([]byte, error), with no MIME result. Adapt that actual return shape to
	// the lifecycle seam; the shim consequently falls back to MediaRef.MIME.
	return downloadClientFunc(func(mediaID string, key []byte) ([]byte, string, error) {
		data, err := cli.GM.DownloadMedia(mediaID, key)
		return data, "", err
	})
}

// googleDownloadOpaqueV1 is Google's versioned Wave-4-to-M4b wire contract.
// The future Google decoder must pack these transport inputs into
// message_attachments.remote_ref. No decoder does so yet, so this shim is
// unit-testable but remains production-inert until Wave-4 ingest lands.
type googleDownloadOpaqueV1 struct {
	V             int    `json:"v"`
	MediaID       string `json:"media_id"`
	DecryptionKey string `json:"decryption_key"`
}

type googleDownloadRef struct {
	mediaID string
	key     []byte
}

// ValidateDownloadOpaque reports whether raw is a well-formed v1 download Opaque this
// adapter can decode. It is the exported round-trip check the Wave-4
// migration uses to prove the refs it packs will unpack here.
func ValidateDownloadOpaque(raw []byte) error {
	_, err := decodeGoogleDownloadOpaque(raw)
	return err
}

func decodeGoogleDownloadOpaque(opaque []byte) (googleDownloadRef, error) {
	if !utf8.Valid(opaque) {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"google_opaque_malformed",
			errors.New("Google media opaque payload is not valid UTF-8"),
		)
	}
	var payload googleDownloadOpaqueV1
	if err := json.Unmarshal(opaque, &payload); err != nil {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"google_opaque_malformed",
			fmt.Errorf("decode Google media opaque payload: %w", err),
		)
	}
	if payload.V != 1 {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"opaque_version_unsupported",
			fmt.Errorf("Google media opaque version %d is unsupported", payload.V),
		)
	}
	if strings.TrimSpace(payload.MediaID) == "" {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"google_opaque_media_id_missing",
			errors.New("Google media opaque payload has no media_id"),
		)
	}
	if payload.DecryptionKey == "" {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"google_opaque_decryption_key_missing",
			errors.New("Google media opaque payload has no decryption_key"),
		)
	}
	key, err := hex.DecodeString(payload.DecryptionKey)
	if err != nil {
		return googleDownloadRef{}, unsupportedGoogleOpaqueError(
			"google_opaque_decryption_key_invalid",
			fmt.Errorf("decode Google media decryption key: %w", err),
		)
	}
	return googleDownloadRef{mediaID: payload.MediaID, key: key}, nil
}

// Structural opaque failures are terminal unsupported errors: remote_ref is
// immutable input, so retrying the same malformed or incomplete payload can
// never make the download succeed.
func unsupportedGoogleOpaqueError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureUnsupported,
		Operation:   "download_media",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

type mediaSendClient interface {
	conversationResolver
	UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error)
	SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
}

var mediaSendClientFor = func(cli *client.Client) mediaSendClient {
	return cli.GM
}

// SendText adapts the durable text outbox request to the connected libgm
// client retained by the lifecycle-owned App generation.
func (a *Adapter) SendText(
	ctx context.Context,
	req bridge.TextRequest,
) (bridge.SendResult, error) {
	if a == nil || a.host == nil || !a.host.Connected.Load() {
		return bridge.SendResult{}, notConnectedTextError()
	}
	cli := a.host.GetClient()
	if cli == nil || cli.GM == nil {
		return bridge.SendResult{}, notConnectedTextError()
	}
	transport := textSendClientFor(cli)
	if transport == nil {
		return bridge.SendResult{}, notConnectedTextError()
	}
	if ctx == nil {
		return bridge.SendResult{}, preDispatchTextError(
			"google_text_context_invalid",
			errors.New("Google text send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, preDispatchTextError("google_text_context_done", err)
	}

	budget := newCallBudget(ctx)
	conversation, err := a.resolveSendConversation(budget, transport, req.Conversation, textConversationLookup)
	if err != nil {
		return bridge.SendResult{}, err
	}
	participantID, sim := app.ExtractSIMAndParticipant(conversation)
	replyToID := ""
	if req.ReplyTo != nil {
		replyToID = req.ReplyTo.RemoteID
	}
	payload := app.BuildSendPayloadWithTmpID(
		req.Conversation.RemoteID,
		req.Body,
		replyToID,
		participantID,
		sim,
		req.RequestID,
	)
	response, err := boundedCall(ctx, budget.sendLimit(), func() (*gmproto.SendMessageResponse, error) {
		return transport.SendMessage(payload)
	})
	if failure, unanswered := unansweredCallFailure(err, "send_text", "send Google text", "google_text_send_timeout",
		"google_text_context_done", bridge.DispatchUncertain); unanswered {
		// The request may have reached the phone, which may have sent it.
		return bridge.SendResult{}, failure
	}
	if err != nil {
		failure := a.classifyTextTransportError(
			fmt.Errorf("send Google text: %w", err),
			"google_text_send_failed",
			"",
		)
		return bridge.SendResult{}, failure
	}
	if response == nil {
		failure := a.classifyTextTransportError(
			errors.New("send Google text: transport returned no response"),
			"google_text_send_failed",
			"",
		)
		return bridge.SendResult{}, failure
	}
	if failure, failed := a.classifySendMessageResponse(cli, "send_text", "text", response); failed {
		return bridge.SendResult{}, failure
	}

	return bridge.SendResult{
		RemoteMessageID: payload.GetTmpID(),
		EchoExpected:    true,
	}, nil
}

// SendReaction adapts a durable reaction request to the connected libgm
// client retained by the lifecycle-owned App generation.
func (a *Adapter) SendReaction(
	ctx context.Context,
	req bridge.ReactionRequest,
) (bridge.SendResult, error) {
	if a == nil || a.host == nil || !a.host.Connected.Load() {
		return bridge.SendResult{}, notConnectedReactionError()
	}
	cli := a.host.GetClient()
	if cli == nil || cli.GM == nil {
		return bridge.SendResult{}, notConnectedReactionError()
	}
	transport := reactionSendClientFor(cli)
	if transport == nil {
		return bridge.SendResult{}, notConnectedReactionError()
	}
	if ctx == nil {
		return bridge.SendResult{}, preDispatchReactionError(
			"google_reaction_context_invalid",
			errors.New("Google reaction send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, preDispatchReactionError("google_reaction_context_done", err)
	}

	budget := newCallBudget(ctx)
	conversation, err := a.resolveSendConversation(budget, transport, req.Conversation, reactionConversationLookup)
	if err != nil {
		return bridge.SendResult{}, err
	}
	_, sim := app.ExtractSIMAndParticipant(conversation)
	payload := app.BuildReactionPayload(
		req.Target.RemoteID,
		req.Emoji,
		string(req.Action),
		sim,
	)
	response, err := boundedCall(ctx, budget.sendLimit(), func() (*gmproto.SendReactionResponse, error) {
		return transport.SendReaction(payload)
	})
	if failure, unanswered := unansweredCallFailure(err, "send_reaction", "send Google reaction", "google_reaction_send_timeout",
		"google_reaction_context_done", bridge.DispatchUncertain); unanswered {
		// The request may have reached the phone, which may have applied it.
		return bridge.SendResult{}, failure
	}
	if err != nil {
		failure := a.classifyReactionTransportError(
			fmt.Errorf("send Google reaction: %w", err),
			"google_reaction_send_failed",
			"",
		)
		return bridge.SendResult{}, failure
	}
	if !response.GetSuccess() {
		// A rejected response proves the connection is healthy enough to
		// respond; this must not touch the receive lifecycle. A nil response
		// also has GetSuccess false and is equally safe to retry.
		cause := errors.New("Google reaction send was rejected")
		if response == nil {
			cause = errors.New("send Google reaction: transport returned no response")
		}
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_reaction",
			Fingerprint: "google_reaction_rejected",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       cause,
		}
	}

	// Reaction dispatch confirms an empty result via ConfirmWithoutResult;
	// there is no reaction-message ID or echo-reconciliation consumer.
	return bridge.SendResult{}, nil
}

// MarkRead advances the remote Google Messages read cursor through the
// connected libgm client retained by the lifecycle-owned App generation.
func (a *Adapter) MarkRead(ctx context.Context, req bridge.ReadReceiptRequest) error {
	if a == nil || a.host == nil || !a.host.Connected.Load() {
		return notConnectedReadError()
	}
	cli := a.host.GetClient()
	if cli == nil || cli.GM == nil {
		return notConnectedReadError()
	}
	transport := readSendClientFor(cli)
	if transport == nil {
		return notConnectedReadError()
	}
	if ctx == nil {
		return preDispatchReadError(
			"google_mark_read_context_invalid",
			errors.New("Google mark-read context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return preDispatchReadError("google_mark_read_context_done", err)
	}
	if len(req.Messages) == 0 {
		return preDispatchReadError(
			"google_mark_read_no_messages",
			errors.New("Google mark-read request has no messages"),
		)
	}

	messageID := req.Messages[len(req.Messages)-1].RemoteID
	_, err := boundedCall(ctx, newCallBudget(ctx).sendLimit(), func() (struct{}, error) {
		return struct{}{}, transport.MarkRead(req.Conversation.RemoteID, messageID)
	})
	// A read receipt is idempotent, so an unanswered one stays retryable as
	// not dispatched, like every other mark-read failure below.
	if failure, unanswered := unansweredCallFailure(err, "mark_read", "mark Google conversation read", "google_mark_read_timeout",
		"google_mark_read_context_done", bridge.DispatchNotCalled); unanswered {
		return failure
	}
	if err != nil {
		return a.classifyReadTransportError(
			fmt.Errorf("mark Google conversation read: %w", err),
			"google_mark_read_failed",
		)
	}
	return nil
}

// DownloadMedia adapts Google's versioned remote media reference to the
// connected libgm client retained by the lifecycle-owned App generation.
func (a *Adapter) DownloadMedia(
	ctx context.Context,
	accountID string,
	ref bridge.MediaRef,
) (bridge.MediaStream, error) {
	if a == nil || a.host == nil || !a.host.Connected.Load() {
		return bridge.MediaStream{}, notConnectedDownloadError()
	}
	cli := a.host.GetClient()
	if cli == nil || cli.GM == nil {
		return bridge.MediaStream{}, notConnectedDownloadError()
	}
	transport := downloadClientFor(cli)
	if transport == nil {
		return bridge.MediaStream{}, notConnectedDownloadError()
	}
	if ctx == nil {
		return bridge.MediaStream{}, preDownloadError(
			"google_download_context_invalid",
			errors.New("Google media download context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.MediaStream{}, preDownloadError("google_download_context_done", err)
	}

	decoded, err := decodeGoogleDownloadOpaque(ref.Opaque)
	if err != nil {
		return bridge.MediaStream{}, err
	}
	data, transportMIME, err := transport.DownloadMedia(decoded.mediaID, decoded.key)
	if err != nil {
		return bridge.MediaStream{}, a.classifyDownloadTransportError(
			fmt.Errorf("download Google media: %w", err),
			"google_media_download_failed",
		)
	}

	// libgm's retained downloader is fully buffered. This NopCloser adaptation
	// intentionally forfeits true streaming until the transport exposes a
	// reader, while still guaranteeing every nil-error result has a ReadCloser.
	return bridge.MediaStream{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		Size:       int64(len(data)),
		Filename:   ref.Filename,
		MIME:       firstNonEmpty(transportMIME, ref.MIME),
	}, nil
}

// SendMedia adapts the durable media outbox request to the connected libgm
// client retained by the lifecycle-owned App generation.
func (a *Adapter) SendMedia(
	ctx context.Context,
	req bridge.MediaRequest,
) (bridge.SendResult, error) {
	if a == nil || a.host == nil || !a.host.Connected.Load() {
		return bridge.SendResult{}, notConnectedMediaError()
	}
	cli := a.host.GetClient()
	if cli == nil || cli.GM == nil {
		return bridge.SendResult{}, notConnectedMediaError()
	}
	transport := mediaSendClientFor(cli)
	if transport == nil {
		return bridge.SendResult{}, notConnectedMediaError()
	}
	if ctx == nil {
		return bridge.SendResult{}, preDispatchMediaError(
			"google_media_context_invalid",
			errors.New("Google media send context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.SendResult{}, preDispatchMediaError("google_media_context_done", err)
	}
	if req.Reader == nil {
		return bridge.SendResult{}, preDispatchMediaError(
			"google_media_read_failed",
			errors.New("Google media reader is nil"),
		)
	}
	if req.Size < 0 || req.Size == math.MaxInt64 {
		return bridge.SendResult{}, preDispatchMediaError(
			"google_media_size_mismatch",
			fmt.Errorf("invalid Google media size %d", req.Size),
		)
	}

	data, err := io.ReadAll(io.LimitReader(req.Reader, req.Size+1))
	if err != nil {
		return bridge.SendResult{}, preDispatchMediaError(
			"google_media_read_failed",
			fmt.Errorf("read Google media: %w", err),
		)
	}
	if int64(len(data)) != req.Size {
		return bridge.SendResult{}, preDispatchMediaError(
			"google_media_size_mismatch",
			fmt.Errorf("Google media size is %d bytes, want %d", len(data), req.Size),
		)
	}

	// Resolve the conversation before uploading: a send that cannot find its
	// conversation must not re-upload the whole file on every retry.
	budget := newCallBudget(ctx)
	conversation, err := a.resolveSendConversation(budget, transport, req.Conversation, mediaConversationLookup)
	if err != nil {
		return bridge.SendResult{}, err
	}
	media, err := boundedCall(ctx, budget.preSendLimit(0), func() (*gmproto.MediaContent, error) {
		return transport.UploadMedia(data, req.Filename, req.MIME)
	})
	// Nothing has been sent while the upload is outstanding.
	if failure, unanswered := unansweredCallFailure(err, "send_media", "upload Google media", "google_media_upload_timeout",
		"google_media_context_done", bridge.DispatchNotCalled); unanswered {
		return bridge.SendResult{}, failure
	}
	if err != nil {
		failure := a.classifyMediaTransportError(
			fmt.Errorf("upload Google media: %w", err),
			"google_media_upload_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	if media == nil {
		failure := a.classifyMediaTransportError(
			errors.New("upload Google media: transport returned no media"),
			"google_media_upload_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	participantID, sim := app.ExtractSIMAndParticipant(conversation)
	payload := app.BuildSendMediaPayloadWithTmpID(
		req.Conversation.RemoteID,
		media,
		participantID,
		sim,
		req.RequestID,
	)
	response, err := boundedCall(ctx, budget.sendLimit(), func() (*gmproto.SendMessageResponse, error) {
		return transport.SendMessage(payload)
	})
	if failure, unanswered := unansweredCallFailure(err, "send_media", "send Google media", "google_media_send_timeout",
		"google_media_context_done", bridge.DispatchUncertain); unanswered {
		// The request may have reached the phone, which may have sent it.
		return bridge.SendResult{}, failure
	}
	if err != nil {
		failure := a.classifyMediaTransportError(
			fmt.Errorf("send Google media: %w", err),
			"google_media_send_failed",
			"",
		)
		return bridge.SendResult{}, failure
	}
	if response == nil {
		failure := a.classifyMediaTransportError(
			errors.New("send Google media: transport returned no response"),
			"google_media_send_failed",
			"",
		)
		return bridge.SendResult{}, failure
	}
	if failure, failed := a.classifySendMessageResponse(cli, "send_media", "media", response); failed {
		return bridge.SendResult{}, failure
	}

	if caption := strings.TrimSpace(req.Caption); caption != "" {
		replyToID := ""
		if req.ReplyTo != nil {
			replyToID = req.ReplyTo.RemoteID
		}
		captionPayload := app.BuildSendPayloadWithTmpID(
			req.Conversation.RemoteID,
			caption,
			replyToID,
			participantID,
			sim,
			req.RequestID+":caption",
		)
		captionResponse, err := boundedCall(ctx, budget.sendLimit(), func() (*gmproto.SendMessageResponse, error) {
			return transport.SendMessage(captionPayload)
		})
		// The media part already went out, so an unanswered caption (even one
		// never started) leaves the overall outcome ambiguous (Dispatch ""),
		// like every caption failure: not dispatched would resend the media.
		if failure, unanswered := unansweredCallFailure(err, "send_media", "send Google media caption", "google_caption_send_timeout",
			"google_media_context_done", ""); unanswered {
			failure.Dispatch = ""
			return bridge.SendResult{}, failure
		}
		if err != nil {
			failure := a.classifyMediaTransportError(
				fmt.Errorf("send Google media caption: %w", err),
				"google_caption_send_failed",
				"",
			)
			return bridge.SendResult{}, afterMediaSent(failure)
		}
		if captionResponse == nil {
			failure := a.classifyMediaTransportError(
				errors.New("send Google media caption: transport returned no response"),
				"google_caption_send_failed",
				"",
			)
			return bridge.SendResult{}, afterMediaSent(failure)
		}
		if captionResponse.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			// The connection just delivered the media; a rejected caption is not a
			// lifecycle event. Whatever the status, the media part already went
			// out, so the overall outcome stays ambiguous (Dispatch ""): a
			// terminal class here would let the outbox reject, and so offer to
			// resend, media that was delivered. An account switch reported on
			// the caption is still recorded for status.
			if account := captionResponse.GetGoogleAccountSwitch().GetAccount(); strings.ContainsRune(account, '@') {
				a.host.NoteGoogleAccountSwitch(cli, account)
			}
			return bridge.SendResult{}, bridge.OpError{
				Class:       bridge.FailureTransient,
				Operation:   "send_media",
				Fingerprint: "google_caption_send_rejected",
				Cause:       errors.New(sendStatusDetail("media caption", captionResponse)),
			}
		}
	}

	return bridge.SendResult{
		RemoteMessageID: payload.GetTmpID(),
		EchoExpected:    true,
	}, nil
}

// afterMediaSent keeps a caption failure ambiguous for the outbox. The media
// part has already gone out, so the request as a whole can be neither "not
// dispatched" (a retry would resend the media) nor terminal: the dispatcher
// rejects terminal classes, and a rejected row reads "Nothing was sent" and
// offers to send the media again. A failure that indicts the session has
// already been reported to the lifecycle by classifyMediaTransportError.
func afterMediaSent(failure bridge.OpError) bridge.OpError {
	failure.Dispatch = ""
	switch failure.Class {
	case bridge.FailureUnpaired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired,
		bridge.FailureMisconfigured,
		bridge.FailureUnsupported:
		failure.Class = bridge.FailureTransient
	}
	return failure
}

// Fingerprints for conversation resolution and send refusals. The outbox
// keeps them in error detail; status surfaces and the agent runbook key on
// the exact strings.
const (
	fingerprintAccountPairingSwitched      = "google_account_pairing_switched"
	fingerprintConversationNotFound        = "google_conversation_not_found"
	fingerprintConversationResolveMismatch = "google_conversation_resolve_mismatch"
	fingerprintConversationGetTimeout      = "google_conversation_get_timeout"
)

// Bounds on libgm requests. The pinned libgm fork waits for the phone's reply
// with no deadline (session_handler.go: after 5 s it nudges the pinger, then
// keeps waiting), and the outbox dispatcher works one lease at a time, so on
// 2026-10-08 a single unanswered GetConversation held every platform's
// outbox. Each request therefore runs on its own goroutine and the adapter
// stops waiting at a deadline. One send attempt's calls share
// googleSendAttemptTimeout; the send itself always keeps at least
// googleSendMinimumTimeout, so an attempt waits at most their sum, which
// stays below the dispatcher's 30 s lease (messaging defaultLeaseTime).
// Tests shorten them.
var (
	googleSendAttemptTimeout = 22 * time.Second
	googleSendMinimumTimeout = 5 * time.Second
	// googleLookupTimeout caps one conversation lookup. A lookup sends
	// nothing, so giving up early is always safe, and a phone that slow would
	// likely leave the send itself unanswered (an uncertain outcome).
	googleLookupTimeout = 8 * time.Second
)

// callBudget is the deadline the libgm calls of one send attempt share.
type callBudget struct {
	ctx      context.Context
	deadline time.Time
	now      func() time.Time
}

// newCallBudget starts an attempt's budget now, ending at
// googleSendAttemptTimeout or at ctx's deadline, whichever is sooner.
func newCallBudget(ctx context.Context) callBudget {
	return newCallBudgetAt(ctx, time.Now)
}

func newCallBudgetAt(ctx context.Context, now func() time.Time) callBudget {
	deadline := now().Add(googleSendAttemptTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	return callBudget{ctx: ctx, deadline: deadline, now: now}
}

// preSendLimit is how long a call that precedes the send (a lookup or the
// upload) may wait: at most capacity when capacity is positive, and never
// into the googleSendMinimumTimeout kept for the send. A limit of zero or
// less means the call must not start.
func (b callBudget) preSendLimit(capacity time.Duration) time.Duration {
	limit := b.deadline.Sub(b.now()) - googleSendMinimumTimeout
	if capacity > 0 && capacity < limit {
		limit = capacity
	}
	return limit
}

// sendLimit is how long a send (or another call that may act on the phone)
// may wait: what is left of the budget, but never less than
// googleSendMinimumTimeout, so a request is never started only to be
// abandoned at once and left uncertain.
func (b callBudget) sendLimit() time.Duration {
	limit := b.deadline.Sub(b.now())
	if limit < googleSendMinimumTimeout {
		limit = googleSendMinimumTimeout
	}
	return limit
}

var (
	// errCallNotStarted marks a call that boundedCall skipped because its
	// budget was spent or ctx had already ended. Unlike a timeout it proves
	// the request never left.
	errCallNotStarted = errors.New("Google Messages request not started")
	// errCallInterrupted marks a call boundedCall stopped waiting for because
	// ctx ended. It keeps the caller's context errors apart from a libgm
	// error that merely wraps a deadline of its own (an HTTP timeout).
	errCallInterrupted = errors.New("stopped waiting for Google Messages")
)

// transportCallTimeoutError reports that the adapter stopped waiting for a
// libgm request after waited. The request may still be answered later; that
// late result is discarded.
type transportCallTimeoutError struct {
	waited time.Duration
}

func (e *transportCallTimeoutError) Error() string {
	return fmt.Sprintf("Google Messages did not answer within %s", e.waited)
}

type callResult[T any] struct {
	value T
	err   error
}

// boundedCall runs call on its own goroutine and returns its result, a
// *transportCallTimeoutError once limit has passed, or an error wrapping
// errCallInterrupted and ctx's error once ctx ends. When limit is zero or
// less, or ctx has already ended, it returns an error wrapping
// errCallNotStarted without calling. libgm cannot cancel a
// request, so an abandoned call keeps its goroutine until libgm returns; each
// outbox attempt can leave at most one behind.
func boundedCall[T any](ctx context.Context, limit time.Duration, call func() (T, error)) (T, error) {
	var zero T
	if limit <= 0 {
		return zero, fmt.Errorf("%w: the send attempt's time budget was spent", errCallNotStarted)
	}
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("%w: %w", errCallNotStarted, err)
	}
	results := make(chan callResult[T], 1)
	go func() {
		var result callResult[T]
		defer func() {
			// Nothing above this goroutine recovers, so a libgm panic would
			// end the daemon; report it as the call's error instead.
			if recovered := recover(); recovered != nil {
				result = callResult[T]{err: fmt.Errorf("panic in Google Messages client: %v", recovered)}
			}
			results <- result
		}()
		result.value, result.err = call()
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.value, result.err
	case <-timer.C:
		return zero, &transportCallTimeoutError{waited: limit}
	case <-ctx.Done():
		return zero, fmt.Errorf("%w: %w", errCallInterrupted, ctx.Err())
	}
}

// unansweredCallFailure classifies a boundedCall error that means the
// adapter got no answer: the deadline passed (timeoutFingerprint) or ctx
// ended (contextFingerprint). call, when set, prefixes the cause. dispatch is
// the certainty to report when the request was started, i.e. whether the
// phone may have acted on it; a call that never started is always
// DispatchNotCalled. ok is false for any other error, which the caller
// classifies as a transport failure.
func unansweredCallFailure(
	err error,
	operation string,
	call string,
	timeoutFingerprint string,
	contextFingerprint string,
	dispatch bridge.DispatchCertainty,
) (bridge.OpError, bool) {
	if err == nil {
		return bridge.OpError{}, false
	}
	cause := err
	if call != "" {
		cause = fmt.Errorf("%s: %w", call, err)
	}
	failure := bridge.OpError{
		Class:     bridge.FailureTransient,
		Operation: operation,
		Dispatch:  dispatch,
		Cause:     cause,
	}
	var timeout *transportCallTimeoutError
	notStarted := errors.Is(err, errCallNotStarted)
	contextEnded := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	switch {
	case errors.Is(err, errCallInterrupted) || (notStarted && contextEnded):
		failure.Fingerprint = contextFingerprint
	case notStarted || errors.As(err, &timeout):
		failure.Fingerprint = timeoutFingerprint
	default:
		return bridge.OpError{}, false
	}
	if notStarted {
		failure.Dispatch = bridge.DispatchNotCalled
	}
	return failure, true
}

// conversationLookup describes how one send operation resolves the
// conversation it sends into.
type conversationLookup struct {
	operation          string
	contextFingerprint string
	// byNumber allows re-resolving a direct thread by its stored peer number
	// when the phone returns nothing for the stored remote ID. Only text and
	// media sends use it; a reaction targets a message in the stored thread.
	byNumber bool
}

var (
	textConversationLookup = conversationLookup{
		operation:          "send_text",
		contextFingerprint: "google_text_context_done",
		byNumber:           true,
	}
	mediaConversationLookup = conversationLookup{
		operation:          "send_media",
		contextFingerprint: "google_media_context_done",
		byNumber:           true,
	}
	reactionConversationLookup = conversationLookup{
		operation:          "send_reaction",
		contextFingerprint: "google_reaction_context_done",
	}
)

// resolveSendConversation finds the conversation a send goes into, or returns
// a DispatchNotCalled failure; nothing has been sent when it fails. A non-nil
// result is either the phone's answer for ref.RemoteID or a direct thread
// re-resolved by number and validated to be the same remote ID, so no caller
// ever sends after a nil lookup without one of those.
func (a *Adapter) resolveSendConversation(
	budget callBudget,
	transport conversationResolver,
	ref bridge.ConversationRef,
	lookup conversationLookup,
) (*gmproto.Conversation, error) {
	conversation, err := boundedCall(budget.ctx, budget.preSendLimit(googleLookupTimeout), func() (*gmproto.Conversation, error) {
		return transport.GetConversation(ref.RemoteID)
	})
	if err != nil {
		return nil, a.conversationLookupFailure(
			fmt.Errorf("get Google conversation: %w", err),
			lookup,
		)
	}
	if conversation != nil {
		// The phone answered this session with data, so it is not refusing it.
		a.host.ClearGoogleAccountSwitch()
		return conversation, nil
	}
	// libgm decrypts a response frame, firing AccountChange for an account
	// container, before it hands the (then empty) response to this caller,
	// so the flag must be read after the call.
	if refusal, switched := a.accountSwitchRefusal(lookup.operation, ""); switched {
		return nil, refusal
	}
	peer := canonicalPhoneNumber(ref.DirectPeerNumber)
	if !lookup.byNumber || ref.Kind != "direct" || peer == "" {
		return nil, conversationNotFoundError(lookup.operation, fmt.Errorf(
			"get Google conversation %q: transport returned no conversation",
			ref.RemoteID,
		))
	}
	if err := budget.ctx.Err(); err != nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   lookup.operation,
			Fingerprint: lookup.contextFingerprint,
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       err,
		}
	}
	return a.resolveDirectConversationByNumber(budget, transport, ref, peer, lookup)
}

// resolveDirectConversationByNumber asks the phone for the 1:1 thread with
// peer after a lookup by remote ID came back empty. It sends nothing itself
// and returns a conversation only when it is the same thread the outbox
// targets; a thread filed under another ID is reported as moved so the
// dispatcher can rebind the local conversation before anything is sent.
func (a *Adapter) resolveDirectConversationByNumber(
	budget callBudget,
	transport conversationResolver,
	ref bridge.ConversationRef,
	peer string,
	lookup conversationLookup,
) (*gmproto.Conversation, error) {
	request := &gmproto.GetOrCreateConversationRequest{
		Numbers: app.NewContactNumbers([]string{peer}),
	}
	response, err := boundedCall(budget.ctx, budget.preSendLimit(googleLookupTimeout), func() (*gmproto.GetOrCreateConversationResponse, error) {
		return transport.GetOrCreateConversation(request)
	})
	if err != nil {
		return nil, a.conversationLookupFailure(
			fmt.Errorf("resolve Google conversation %q by peer number: %w", ref.RemoteID, err),
			lookup,
		)
	}
	resolved := response.GetConversation()
	if resolved == nil {
		if refusal, switched := a.accountSwitchRefusal(lookup.operation, ""); switched {
			return nil, refusal
		}
		return nil, conversationNotFoundError(lookup.operation, fmt.Errorf(
			"get Google conversation %q: transport returned no conversation, and resolving the direct thread by peer number returned none (status %s)",
			ref.RemoteID,
			response.GetStatus().String(),
		))
	}
	a.host.ClearGoogleAccountSwitch()
	if err := validateDirectConversation(resolved, peer); err != nil {
		return nil, bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   lookup.operation,
			Fingerprint: fingerprintConversationResolveMismatch,
			Dispatch:    bridge.DispatchNotCalled,
			Cause: fmt.Errorf(
				"resolve Google conversation %q by peer number: %w",
				ref.RemoteID,
				err,
			),
		}
	}
	if resolvedID := resolved.GetConversationID(); resolvedID != ref.RemoteID {
		// Sending under resolvedID now would route the echo to a different
		// local conversation; the dispatcher rebinds first and retries.
		return nil, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   lookup.operation,
			Fingerprint: bridge.FingerprintConversationMoved,
			Dispatch:    bridge.DispatchNotCalled,
			Cause: &bridge.ConversationMovedError{
				FromRemoteID: ref.RemoteID,
				ToRemoteID:   resolvedID,
			},
		}
	}
	return resolved, nil
}

// validateDirectConversation accepts only a 1:1 thread whose sole non-self
// participant has the stored peer number.
func validateDirectConversation(conversation *gmproto.Conversation, peer string) error {
	if strings.TrimSpace(conversation.GetConversationID()) == "" {
		return errors.New("the phone returned a conversation without an ID")
	}
	if conversation.GetIsGroupChat() {
		return fmt.Errorf("the phone returned group conversation %q", conversation.GetConversationID())
	}
	var others []string
	for _, participant := range conversation.GetParticipants() {
		if participant.GetIsMe() {
			continue
		}
		number := participant.GetID().GetNumber()
		if number == "" {
			number = participant.GetFormattedNumber()
		}
		others = append(others, number)
	}
	if len(others) != 1 {
		return fmt.Errorf(
			"the phone returned conversation %q with %d other participants, want 1",
			conversation.GetConversationID(),
			len(others),
		)
	}
	if canonicalPhoneNumber(others[0]) != peer {
		return fmt.Errorf(
			"the phone returned conversation %q with a different participant than the stored peer",
			conversation.GetConversationID(),
		)
	}
	return nil
}

// canonicalPhoneNumber keeps a leading '+' and the digits of number, so
// formatting differences such as "+1 (555) 123-4567" compare equal. It
// returns "" when number has no digits.
func canonicalPhoneNumber(number string) string {
	number = strings.TrimSpace(number)
	var canonical strings.Builder
	if strings.HasPrefix(number, "+") {
		canonical.WriteByte('+')
	}
	digits := 0
	for _, r := range number {
		if r >= '0' && r <= '9' {
			canonical.WriteRune(r)
			digits++
		}
	}
	if digits == 0 {
		return ""
	}
	return canonical.String()
}

// conversationLookupFailure classifies a GetConversation or by-number lookup
// error exactly as before, except that a failure which does not indict the
// session's credentials becomes the account-switch refusal when the phone has
// reported Google-account pairing: a phone refusing this session may surface
// as a typed no-payload error instead of an empty response, and retrying it
// cannot succeed.
func (a *Adapter) conversationLookupFailure(err error, lookup conversationLookup) bridge.OpError {
	failure, unanswered := unansweredCallFailure(err, lookup.operation, "", fingerprintConversationGetTimeout,
		lookup.contextFingerprint, bridge.DispatchNotCalled)
	if !unanswered {
		failure = a.classifyTransportError(err, lookup.operation, "google_conversation_get_failed")
		failure.Dispatch = bridge.DispatchNotCalled
	}
	if !authIndicting(failure.Class) {
		if refusal, switched := a.accountSwitchRefusal(lookup.operation, err.Error()); switched {
			return refusal
		}
	}
	a.reportIfAuthIndicting(failure)
	return failure
}

func conversationNotFoundError(operation string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   operation,
		Fingerprint: fingerprintConversationNotFound,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

// accountSwitchRefusal returns the terminal refusal for a send while the
// phone reports Google-account pairing for this QR-paired session.
func (a *Adapter) accountSwitchRefusal(operation, detail string) (bridge.OpError, bool) {
	switched, account := a.host.GoogleAccountSwitch()
	if !switched {
		return bridge.OpError{}, false
	}
	return accountSwitchError(operation, account, detail), true
}

// accountSwitchError is built directly, never through classifyTransportError
// or reportIfAuthIndicting: ReauthRequired routed to the lifecycle would
// retire the receive generation, and the phone keeps delivering inbound
// messages to this session. The outbox rejects it as terminal instead.
func accountSwitchError(operation, account, detail string) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureReauthRequired,
		Operation:   operation,
		Fingerprint: fingerprintAccountPairingSwitched,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       &accountPairingSwitchedError{account: account, detail: detail},
	}
}

// accountPairingSwitchedError explains a send refused because the phone
// switched Google Messages to Google-account pairing. Its text starts with
// the fingerprint because a terminal outbox row keeps only the error text.
type accountPairingSwitchedError struct {
	account string
	detail  string
}

func (e *accountPairingSwitchedError) Error() string {
	var text strings.Builder
	text.WriteString("[" + fingerprintAccountPairingSwitched + "] ")
	text.WriteString("Your phone switched Google Messages to Google-account pairing")
	if e.account != "" {
		text.WriteString(" (" + e.account + ")")
	}
	text.WriteString(" and refuses requests from this QR-paired session")
	if e.detail != "" {
		text.WriteString(" (" + e.detail + ")")
	}
	text.WriteString(", so nothing was sent. Re-link OpenMessage with Google-account pairing, " +
		"or turn Google-account pairing off on the phone and pair again by QR.")
	return text.String()
}

// classifySendMessageResponse maps a non-nil SendMessageResponse for the
// text or media part of a send. kind names the part in error text and
// fingerprints. failed is false only for SUCCESS.
func (a *Adapter) classifySendMessageResponse(
	cli *client.Client,
	operation string,
	kind string,
	response *gmproto.SendMessageResponse,
) (bridge.OpError, bool) {
	status := response.GetStatus()
	if status == gmproto.SendMessageResponse_SUCCESS {
		a.host.ClearGoogleAccountSwitch()
		return bridge.OpError{}, false
	}
	// A refused status proves the connection is healthy enough to respond;
	// none of these failures touches the receive lifecycle.
	detail := sendStatusDetail(kind, response)
	if account := response.GetGoogleAccountSwitch().GetAccount(); strings.ContainsRune(account, '@') {
		// The mautrix-gmessages connector reports every non-SUCCESS as a
		// certain failure and words this one as "switch back to QR pairing
		// or log in with Google account to send messages".
		a.host.NoteGoogleAccountSwitch(cli, account)
		return accountSwitchError(operation, account, detail), true
	}
	switch status {
	case gmproto.SendMessageResponse_UNKNOWN:
		// The phone answered without saying whether it sent the message.
		// Retrying could send it twice, so the outcome stays uncertain and a
		// late echo can still confirm it.
		return bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   operation,
			Fingerprint: "google_" + kind + "_send_unknown_status",
			Dispatch:    bridge.DispatchUncertain,
			Cause:       errors.New(detail),
		}, true
	case gmproto.SendMessageResponse_FAILURE_2, gmproto.SendMessageResponse_FAILURE_3:
		return bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   operation,
			Fingerprint: "google_" + kind + "_send_rejected",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New(detail),
		}, true
	default:
		// FAILURE_4 (the mautrix-gmessages connector words it "Google
		// Messages is not your default SMS app") or a status this client does
		// not know: retrying the same request cannot help.
		return bridge.OpError{
			Class:       bridge.FailureMisconfigured,
			Operation:   operation,
			Fingerprint: "google_" + kind + "_send_refused",
			Dispatch:    bridge.DispatchNotCalled,
			Cause:       errors.New(detail),
		}, true
	}
}

// sendStatusDetail names a SendMessage status and any account switch the
// phone attached, so the outbox's error detail keeps both.
func sendStatusDetail(kind string, response *gmproto.SendMessageResponse) string {
	detail := fmt.Sprintf("Google %s send returned %s", kind, response.GetStatus().String())
	if accountSwitch := response.GetGoogleAccountSwitch(); accountSwitch != nil {
		detail += fmt.Sprintf(
			" with account switch %q (enabled %t)",
			accountSwitch.GetAccount(),
			accountSwitch.GetEnabled(),
		)
	}
	return detail
}

func authIndicting(class bridge.FailureClass) bool {
	switch class {
	case bridge.FailureCredentialsExpired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired:
		return true
	default:
		return false
	}
}

func notConnectedTextError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: "google_not_connected",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("Google Messages is not connected"),
	}
}

func preDispatchTextError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_text",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

func (a *Adapter) classifyTextTransportError(
	err error,
	fingerprint string,
	dispatch bridge.DispatchCertainty,
) bridge.OpError {
	failure := a.classifyTransportError(err, "send_text", fingerprint)
	failure.Dispatch = dispatch
	a.reportIfAuthIndicting(failure)
	return failure
}

func notConnectedReactionError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_reaction",
		Fingerprint: "google_not_connected",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("Google Messages is not connected"),
	}
}

func preDispatchReactionError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_reaction",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

func (a *Adapter) classifyReactionTransportError(
	err error,
	fingerprint string,
	dispatch bridge.DispatchCertainty,
) bridge.OpError {
	failure := a.classifyTransportError(err, "send_reaction", fingerprint)
	failure.Dispatch = dispatch
	a.reportIfAuthIndicting(failure)
	return failure
}

func notConnectedReadError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "mark_read",
		Fingerprint: "google_not_connected",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("Google Messages is not connected"),
	}
}

func preDispatchReadError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "mark_read",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

func (a *Adapter) classifyReadTransportError(err error, fingerprint string) bridge.OpError {
	failure := a.classifyTransportError(err, "mark_read", fingerprint)
	// Deliberate idempotent-read divergence: even after libgm's transport call,
	// repeating a read receipt is harmless, so failures stay retryable as
	// DispatchNotCalled instead of becoming uncertain like text/media sends.
	failure.Dispatch = bridge.DispatchNotCalled
	a.reportIfAuthIndicting(failure)
	return failure
}

func notConnectedDownloadError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "download_media",
		Fingerprint: "google_not_connected",
		Cause:       errors.New("Google Messages is not connected"),
	}
}

func preDownloadError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "download_media",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

func (a *Adapter) classifyDownloadTransportError(err error, fingerprint string) bridge.OpError {
	failure := a.classifyTransportError(err, "download_media", fingerprint)
	// Downloads are idempotent reads. The media service simply retries on a
	// later request, so the send-path Dispatch certainty does not apply here.
	failure.Dispatch = ""
	a.reportIfAuthIndicting(failure)
	return failure
}

func notConnectedMediaError() bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_media",
		Fingerprint: "google_not_connected",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       errors.New("Google Messages is not connected"),
	}
}

func preDispatchMediaError(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "send_media",
		Fingerprint: fingerprint,
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	}
}

func (a *Adapter) classifyMediaTransportError(
	err error,
	fingerprint string,
	dispatch bridge.DispatchCertainty,
) bridge.OpError {
	failure := a.classifyTransportError(err, "send_media", fingerprint)
	failure.Dispatch = dispatch
	a.reportIfAuthIndicting(failure)
	return failure
}

// reportIfAuthIndicting forwards only failures that indict the session itself
// (credential expiry / reauth / upgrade-required) to the lifecycle owner, where
// the supervisor routes them to repair or park — the C4 notify-on-auth-expiry
// contract. Plain transient send failures must never retire a healthy receive
// generation (the C4/C5/C6 lesson): a malformed conversation or media-server
// hiccup retrying every ~5s would otherwise bounce the Google connection
// indefinitely — the over-reconnect throttle vector the runbook warns about.
func (a *Adapter) reportIfAuthIndicting(failure bridge.OpError) {
	if authIndicting(failure.Class) {
		a.ReportError(failure)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var _ bridge.TextSender = (*Adapter)(nil)
var _ bridge.ReactionSender = (*Adapter)(nil)
var _ bridge.ReadReceiptSender = (*Adapter)(nil)
var _ bridge.MediaSender = (*Adapter)(nil)
var _ bridge.MediaDownloader = (*Adapter)(nil)
