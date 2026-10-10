package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

const v2ReactionDescription = " On a v2 install the reaction goes through the app's durable outbox, and this waits for it to settle. Use the conversation and message IDs the read tools return. The result carries outbox_id, state and idempotency_key; ok is true once the platform accepted the reaction. If settled is false the reaction is still queued and the app keeps retrying it in the background: do not react again (it is listed in the app's outbox, where it can be canceled). An uncertain result means the platform may have applied it; read the message's reactions before reacting again."

const v2ReactionIdempotencyDescription = "Optional retry key for the exact same reaction, used on a v2 install. Every v2 result echoes the key in use; reuse it only when repeating a reaction whose response was lost. Omit it to mint a new intent."

// submitV2Reaction queues a reaction on this process's own v2 outbox and waits
// for it to settle. It serves the in-process MCP surface of a v2-primary
// daemon.
func submitV2Reaction(
	ctx context.Context,
	v2 *V2Dependencies,
	args map[string]any,
	conversationID, messageID, emoji, action string,
) *mcp.CallToolResult {
	if v2.Service == nil {
		return errorResult("v2 send service is unavailable")
	}
	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	submission, err := v2wire.SubmitReactionV2(ctx, v2.nativeDeps(), v2wire.ReactionInput{
		ConversationID: conversationID,
		MessageID:      messageID,
		Emoji:          emoji,
		Action:         action,
		IdempotencyKey: key,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("failed to submit reaction: %v", err))
	}
	// Bounded like the client-mode wait: a reaction the dispatcher keeps
	// putting back (its platform has no lease to give) never settles, and the
	// caller still needs an answer.
	waitCtx, cancel := context.WithTimeout(ctx, daemonSettleTimeout)
	defer cancel()
	delivery, err := v2.Service.Wait(waitCtx, submission.OutboxID)
	if delivery.OutboxID == "" {
		delivery = messaging.Delivery{OutboxID: submission.OutboxID, State: submission.State}
	}
	return v2ReactionResult(delivery, submission.Deduplicated, key, err)
}

// daemonSubmitReactionAndWait is submitV2Reaction for transportless client
// mode: the running app owns the outbox, and this process follows the intent
// over the app's local API.
func daemonSubmitReactionAndWait(
	ctx context.Context,
	daemon *localapi.Client,
	args map[string]any,
	conversationID, messageID, emoji, action string,
) *mcp.CallToolResult {
	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	submission, err := daemon.SubmitReaction(ctx, localapi.ReactionSubmission{
		ConversationID: conversationID,
		MessageID:      messageID,
		Emoji:          emoji,
		Action:         action,
		IdempotencyKey: key,
	})
	if err != nil {
		if localapi.IsDeterministicRejection(err) {
			return errorResult(fmt.Sprintf("reaction rejected by the app: %v", err))
		}
		if responseErr, ok := localapi.AsResponseError(err); ok {
			return errorResult(fmt.Sprintf("the app could not queue the reaction: HTTP %d: %s", responseErr.StatusCode, responseErr.Body))
		}
		// The request failed mid-flight, so the app may or may not hold the
		// intent. Not an IsError result: an error invites a second reaction.
		return structuredResult(map[string]any{
			"ok":              false,
			"settled":         false,
			"ambiguous":       true,
			"idempotency_key": key,
			"error":           err.Error(),
		}, fmt.Sprintf(
			"The reaction's outcome is unknown (%v). Do NOT react again with a new key. To replay-check this exact reaction, repeat it with the same idempotency_key: %s. If the app is not running, start it first.",
			err, key,
		))
	}
	delivery, settled, err := daemon.WaitDelivery(ctx, submission.OutboxID, daemonSettleTimeout)
	if err != nil {
		// The intent is durably queued on the daemon; only our view failed.
		return v2ReactionResult(
			messaging.Delivery{OutboxID: submission.OutboxID, State: messaging.OutboxState(submission.State)},
			submission.Deduplicated, key, err,
		)
	}
	converted := deliveryFromLocalAPI(delivery)
	if !settled {
		return v2ReactionResult(converted, submission.Deduplicated, key, context.DeadlineExceeded)
	}
	return v2ReactionResult(converted, submission.Deduplicated, key, nil)
}

// v2ReactionResult reports a queued reaction's outcome. The intent is already
// stored when this runs, so no path here returns an IsError result: a tool
// error invites the calling agent to react again, and the outbox finishes the
// first reaction regardless. waitErr is why the wait ended before the
// reaction settled, if it did.
func v2ReactionResult(
	delivery messaging.Delivery,
	deduplicated bool,
	idempotencyKey string,
	waitErr error,
) *mcp.CallToolResult {
	state := delivery.State
	if state == "" {
		state = messaging.OutboxQueued
	}
	settled := v2ReactionSettled(state)
	payload := map[string]any{
		"ok":           v2DeliveryOK(state),
		"settled":      settled,
		"outbox_id":    delivery.OutboxID,
		"state":        state,
		"deduplicated": deduplicated,
	}
	// The app's compatibility route mints its own key and does not return it.
	if idempotencyKey != "" {
		payload["idempotency_key"] = idempotencyKey
	}
	if !settled {
		payload["auto_retry"] = true
		if waitErr != nil {
			payload["wait_error"] = waitErr.Error()
		}
	}
	if delivery.ErrorClass != "" {
		payload["error_class"] = delivery.ErrorClass
	}
	if delivery.ErrorCode != "" {
		payload["error_code"] = delivery.ErrorCode
	}
	if delivery.Warning != "" {
		payload["warning"] = delivery.Warning
	}
	return structuredResult(payload, v2ReactionText(delivery.OutboxID, state, delivery.ErrorClass, idempotencyKey))
}

// v2ReactionSettled reports whether the app is done with the reaction. A
// queued, in-flight or automatically retrying reaction is not.
func v2ReactionSettled(state messaging.OutboxState) bool {
	switch state {
	case messaging.OutboxConfirmed, messaging.OutboxStoreFailed, messaging.OutboxUncertain,
		messaging.OutboxRejected, messaging.OutboxCanceled:
		return true
	default:
		return false
	}
}

func v2ReactionText(outboxID string, state messaging.OutboxState, errorClass, idempotencyKey string) string {
	switch state {
	case messaging.OutboxConfirmed, messaging.OutboxStoreFailed:
		return fmt.Sprintf("Reaction delivered (outbox %s).", outboxID)
	case messaging.OutboxUncertain:
		return fmt.Sprintf("The reaction's outcome is unknown (outbox %s): the platform may have applied it. Read the message's reactions before reacting again.", outboxID)
	case messaging.OutboxRejected:
		return fmt.Sprintf("The reaction was rejected (outbox %s, error class %s). The app will not retry it.", outboxID, firstNonEmpty(errorClass, "unknown"))
	case messaging.OutboxCanceled:
		return fmt.Sprintf("The reaction was canceled before it was sent (outbox %s).", outboxID)
	default:
		text := fmt.Sprintf(
			"The reaction is durably queued (outbox %s, state %s) and the app keeps retrying it in the background. Do NOT react again; it is listed in the app's outbox, where it can be canceled.",
			outboxID, state,
		)
		if idempotencyKey != "" {
			text += fmt.Sprintf(" To repeat this exact reaction deliberately, reuse idempotency_key %s.", idempotencyKey)
		}
		return text
	}
}
