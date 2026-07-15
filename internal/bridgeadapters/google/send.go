package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

type textSendClient interface {
	GetConversation(conversationID string) (*gmproto.Conversation, error)
	SendMessage(payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
}

var textSendClientFor = func(cli *client.Client) textSendClient {
	return cli.GM
}

type reactionSendClient interface {
	GetConversation(conversationID string) (*gmproto.Conversation, error)
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

type mediaSendClient interface {
	UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error)
	GetConversation(conversationID string) (*gmproto.Conversation, error)
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

	conversation, err := transport.GetConversation(req.Conversation.RemoteID)
	if err != nil {
		failure := a.classifyTextTransportError(
			fmt.Errorf("get Google conversation: %w", err),
			"google_conversation_get_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	if conversation == nil {
		failure := a.classifyTextTransportError(
			errors.New("get Google conversation: transport returned no conversation"),
			"google_conversation_get_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
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
	response, err := transport.SendMessage(payload)
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
	if response.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		// A rejected status proves the connection is healthy enough to respond;
		// this must not touch the receive lifecycle.
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_text",
			Fingerprint: "google_text_send_rejected",
			Dispatch:    bridge.DispatchNotCalled,
			Cause: fmt.Errorf(
				"Google text send returned %s",
				response.GetStatus().String(),
			),
		}
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

	conversation, err := transport.GetConversation(req.Conversation.RemoteID)
	if err != nil {
		failure := a.classifyReactionTransportError(
			fmt.Errorf("get Google conversation: %w", err),
			"google_conversation_get_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	if conversation == nil {
		failure := a.classifyReactionTransportError(
			errors.New("get Google conversation: transport returned no conversation"),
			"google_conversation_get_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	_, sim := app.ExtractSIMAndParticipant(conversation)
	payload := app.BuildReactionPayload(
		req.Target.RemoteID,
		req.Emoji,
		string(req.Action),
		sim,
	)
	response, err := transport.SendReaction(payload)
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
	if err := transport.MarkRead(req.Conversation.RemoteID, messageID); err != nil {
		return a.classifyReadTransportError(
			fmt.Errorf("mark Google conversation read: %w", err),
			"google_mark_read_failed",
		)
	}
	return nil
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

	media, err := transport.UploadMedia(data, req.Filename, req.MIME)
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
	conversation, err := transport.GetConversation(req.Conversation.RemoteID)
	if err != nil {
		failure := a.classifyMediaTransportError(
			fmt.Errorf("get Google conversation: %w", err),
			"google_conversation_get_failed",
			bridge.DispatchNotCalled,
		)
		return bridge.SendResult{}, failure
	}
	if conversation == nil {
		failure := a.classifyMediaTransportError(
			errors.New("get Google conversation: transport returned no conversation"),
			"google_conversation_get_failed",
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
	response, err := transport.SendMessage(payload)
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
	if response.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		// A rejected status proves the connection is healthy enough to respond;
		// this must not touch the receive lifecycle.
		return bridge.SendResult{}, bridge.OpError{
			Class:       bridge.FailureTransient,
			Operation:   "send_media",
			Fingerprint: "google_media_send_rejected",
			Dispatch:    bridge.DispatchNotCalled,
			Cause: fmt.Errorf(
				"Google media send returned %s",
				response.GetStatus().String(),
			),
		}
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
		captionResponse, err := transport.SendMessage(captionPayload)
		if err != nil {
			failure := a.classifyMediaTransportError(
				fmt.Errorf("send Google media caption: %w", err),
				"google_caption_send_failed",
				"",
			)
			// The media send has already succeeded, so the overall request cannot
			// truthfully be classified as not dispatched even if a wrapped caption
			// error carries that narrower certainty.
			failure.Dispatch = ""
			return bridge.SendResult{}, failure
		}
		if captionResponse == nil {
			failure := a.classifyMediaTransportError(
				errors.New("send Google media caption: transport returned no response"),
				"google_caption_send_failed",
				"",
			)
			return bridge.SendResult{}, failure
		}
		if captionResponse.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
			// The connection just delivered the media; a rejected caption is not a
			// lifecycle event.
			return bridge.SendResult{}, bridge.OpError{
				Class:       bridge.FailureTransient,
				Operation:   "send_media",
				Fingerprint: "google_caption_send_rejected",
				Cause: fmt.Errorf(
					"Google media caption send returned %s",
					captionResponse.GetStatus().String(),
				),
			}
		}
	}

	return bridge.SendResult{
		RemoteMessageID: payload.GetTmpID(),
		EchoExpected:    true,
	}, nil
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
	switch failure.Class {
	case bridge.FailureCredentialsExpired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired:
		a.ReportError(failure)
	}
}

var _ bridge.TextSender = (*Adapter)(nil)
var _ bridge.ReactionSender = (*Adapter)(nil)
var _ bridge.ReadReceiptSender = (*Adapter)(nil)
var _ bridge.MediaSender = (*Adapter)(nil)
