package tools

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/sendcap"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

// This file is the single place that translates durable outbox state into
// what agents are told about a send. The contract, after the 2026-08-05
// incident (a send reported as "confirmed" flushed ~15 hours later and
// double-texted the recipient):
//
//	queued      → the message has NOT left this machine
//	transmitted → the platform transport ACCEPTED it (remote message ID);
//	              acceptance is not delivery to the recipient's device
//	delivered   → a delivery receipt for it was observed
//
// "settled" is true only when the outcome is known and the dispatcher will
// not advance the intent further: transmitted (confirmed/store_failed) or
// terminal failure (rejected/canceled). It is never true while the intent is
// queued, retrying, or uncertain.

// Transport-state labels of the agent-facing send contract.
const (
	transportStateQueued      = "queued"
	transportStateTransmitted = "transmitted"
	transportStateDelivered   = "delivered"
	transportStateUncertain   = "uncertain"
	transportStateFailed      = "failed"
	transportStateCanceled    = "canceled"
)

// sendTTLEnvVar overrides the default send window for interactive MCP sends.
const sendTTLEnvVar = "OPENMESSAGES_SEND_TTL_SECONDS"

// defaultSendTTL is the interactive-send window: a message still queued this
// long after submission is canceled instead of transmitted stale. Agents opt
// out per send with ttl_seconds=0 (never expire) or choose another window.
const defaultSendTTL = 10 * time.Minute

const maxSendTTL = 24 * time.Hour

// Wait bounds for the settle wait. The default matches the previous
// daemon-routed behavior; the cap keeps one MCP call from hanging a session.
const (
	defaultSendWait = 25 * time.Second
	maxSendWait     = 120 * time.Second
)

const sendVerifyGuidance = "Transport acceptance is not delivery: verify the message appears in-thread (search_messages / get_conversation with a fresh timestamp) before reporting it as sent."

// sendWaitOptions are the agent-facing wait knobs shared by the send tools.
type sendWaitOptions struct {
	// WaitForTransmit keeps waiting through auto-retrying states until the
	// transport acknowledges (or the intent fails terminally), instead of
	// returning at the first stable-but-untransmitted state.
	WaitForTransmit bool
	// Wait bounds the whole wait.
	Wait time.Duration
}

func parseSendWaitOptions(args map[string]any) (sendWaitOptions, error) {
	options := sendWaitOptions{Wait: defaultSendWait}
	if raw, present := args["wait_for_transmit"]; present {
		value, ok := raw.(bool)
		if !ok {
			return options, errors.New("wait_for_transmit must be a boolean")
		}
		options.WaitForTransmit = value
	}
	if raw, present := args["wait_seconds"]; present {
		seconds, ok := numberArg(raw)
		if !ok {
			return options, errors.New("wait_seconds must be a number")
		}
		if seconds < 0 {
			return options, errors.New("wait_seconds must not be negative")
		}
		options.Wait = time.Duration(seconds * float64(time.Second))
	}
	if options.Wait > maxSendWait {
		options.Wait = maxSendWait
	}
	return options, nil
}

// parseSendTTL resolves the send window for one submission: the explicit
// ttl_seconds argument, else the OPENMESSAGES_SEND_TTL_SECONDS override,
// else the interactive default. ttl_seconds=0 means the send never expires.
func parseSendTTL(args map[string]any) (time.Duration, error) {
	if raw, present := args["ttl_seconds"]; present {
		seconds, ok := numberArg(raw)
		if !ok {
			return 0, errors.New("ttl_seconds must be a number")
		}
		if seconds < 0 {
			return 0, errors.New("ttl_seconds must not be negative")
		}
		ttl := time.Duration(seconds * float64(time.Second))
		if ttl > maxSendTTL {
			return 0, fmt.Errorf("ttl_seconds must not exceed %d (24 hours)", int(maxSendTTL/time.Second))
		}
		return ttl, nil
	}
	if raw := strings.TrimSpace(os.Getenv(sendTTLEnvVar)); raw != "" {
		seconds, err := strconv.ParseFloat(raw, 64)
		if err != nil || seconds < 0 || time.Duration(seconds*float64(time.Second)) > maxSendTTL {
			return 0, fmt.Errorf("%s must be a number of seconds between 0 and %d", sendTTLEnvVar, int(maxSendTTL/time.Second))
		}
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return defaultSendTTL, nil
}

func parseSendForce(args map[string]any) (bool, error) {
	raw, present := args["force"]
	if !present {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, errors.New("force must be a boolean")
	}
	return value, nil
}

func numberArg(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	default:
		return 0, false
	}
}

// sendTransmitted reports transport acceptance: the transport returned a
// result for this intent (or did so and only the local record needs repair).
func sendTransmitted(state messaging.OutboxState) bool {
	return state == messaging.OutboxConfirmed || state == messaging.OutboxStoreFailed
}

// sendSettled reports a known final outcome. Uncertain is deliberately NOT
// settled: the outcome is unknown, and reporting it settled invites agents
// to treat the send as done.
func sendSettled(state messaging.OutboxState) bool {
	switch state {
	case messaging.OutboxConfirmed, messaging.OutboxStoreFailed,
		messaging.OutboxRejected, messaging.OutboxCanceled:
		return true
	default:
		return false
	}
}

// sendOutcome is everything the send tools know about one durable send when
// they answer the agent.
type sendOutcome struct {
	Delivery       messaging.Delivery
	IdempotencyKey string
	Deduplicated   bool
	// Platform is the send platform actually used ("sms", "whatsapp",
	// "signal"); empty when it could not be resolved.
	Platform string
	// ConversationID is the conversation the send was written to. Falls back
	// to the submitted conversation ID when the delivery record lacks one.
	ConversationID string
	// Delivered is set when a delivery receipt for the transmitted message
	// was observed in the local store.
	Delivered bool
	// WaitedForTransmit and WaitExpired report an unfinished
	// wait_for_transmit wait, so the text can say "still queued after Ns".
	WaitedForTransmit bool
	WaitExpired       bool
}

func (o sendOutcome) conversationID() string {
	if strings.TrimSpace(o.Delivery.ConversationID) != "" {
		return o.Delivery.ConversationID
	}
	return o.ConversationID
}

func (o sendOutcome) transportState() string {
	state := o.Delivery.State
	switch {
	case o.Delivered:
		return transportStateDelivered
	case sendTransmitted(state):
		return transportStateTransmitted
	case state == messaging.OutboxUncertain:
		return transportStateUncertain
	case state == messaging.OutboxRejected:
		return transportStateFailed
	case state == messaging.OutboxCanceled:
		return transportStateCanceled
	default:
		return transportStateQueued
	}
}

// buildSendPayload is the structured result for every durable send answer.
func buildSendPayload(o sendOutcome) map[string]any {
	delivery := o.Delivery
	payload := map[string]any{
		"ok":              sendTransmitted(delivery.State),
		"settled":         sendSettled(delivery.State),
		"transmitted":     sendTransmitted(delivery.State),
		"transport_state": o.transportState(),
		"state":           string(delivery.State),
		"outbox_id":       delivery.OutboxID,
		"idempotency_key": o.IdempotencyKey,
		"deduplicated":    o.Deduplicated,
	}
	if conversationID := o.conversationID(); conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	if o.Platform != "" {
		payload["platform"] = o.Platform
	}
	if o.Delivered {
		payload["delivered"] = true
	}
	if delivery.LocalMessageID != "" {
		payload["local_message_id"] = delivery.LocalMessageID
	}
	if delivery.RemoteMessageID != "" {
		payload["remote_message_id"] = delivery.RemoteMessageID
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
	if !delivery.ExpiresAt.IsZero() {
		payload["expires_at_ms"] = delivery.ExpiresAt.UnixMilli()
	}
	if delivery.Expired() {
		payload["expired"] = true
	}
	if delivery.State == messaging.OutboxNotDispatched {
		payload["auto_retry"] = true
	}
	if delivery.State == messaging.OutboxUncertain {
		payload["uncertain"] = true
	}
	return payload
}

// sendResultText is the human/agent-readable line matching buildSendPayload.
func sendResultText(o sendOutcome) string {
	delivery := o.Delivery
	platform := o.Platform
	if platform == "" {
		platform = "the platform"
	}
	switch {
	case o.Delivered:
		return fmt.Sprintf(
			"Message transmitted on %s and a delivery receipt was observed (outbox %s, remote message %s).",
			platform, delivery.OutboxID, delivery.RemoteMessageID,
		)
	case delivery.State == messaging.OutboxConfirmed:
		return fmt.Sprintf(
			"Message transmitted: the %s transport accepted it (outbox %s, remote message %s). %s",
			platform, delivery.OutboxID, delivery.RemoteMessageID, sendVerifyGuidance,
		)
	case delivery.State == messaging.OutboxStoreFailed:
		return fmt.Sprintf(
			"Message transmitted: the %s transport accepted it (outbox %s, remote message %s); the local record is being repaired automatically. Do not resend. %s",
			platform, delivery.OutboxID, delivery.RemoteMessageID, sendVerifyGuidance,
		)
	case delivery.State == messaging.OutboxUncertain:
		return fmt.Sprintf(
			"Send outcome UNCERTAIN (outbox %s): the transport may or may not have accepted it, and this will not resolve on its own. Do not retry automatically — check the conversation for the message before doing anything, and resend only as a deliberate new intent.",
			delivery.OutboxID,
		)
	case delivery.Expired():
		return fmt.Sprintf(
			"NOT SENT: the send window expired before the message reached the transport (outbox %s). Nothing was transmitted and nothing will be. Submit a new send if the message is still wanted.",
			delivery.OutboxID,
		)
	case delivery.State == messaging.OutboxCanceled:
		return fmt.Sprintf("NOT SENT: the send was canceled before reaching the transport (outbox %s).", delivery.OutboxID)
	case delivery.State == messaging.OutboxRejected:
		return fmt.Sprintf(
			"NOT SENT: the send was rejected (outbox %s, error class %s). The app will not retry it; sending again creates a new message and may fail the same way.",
			delivery.OutboxID, firstNonEmpty(delivery.ErrorClass, "unknown"),
		)
	case delivery.State == messaging.OutboxNotDispatched:
		return fmt.Sprintf(
			"NOT YET TRANSMITTED: the last attempt failed (outbox %s, error class %s) and the app is retrying it automatically — do NOT send this message again; monitor with list_outbox or cancel with cancel_outbox. %s%s",
			delivery.OutboxID,
			firstNonEmpty(delivery.ErrorClass, "unknown"),
			sendQueuedExpiryText(delivery),
			sendWaitExpiredSuffix(o),
		)
	default:
		return fmt.Sprintf(
			"NOT YET TRANSMITTED: the message is durably queued (outbox %s, state %s) and has not left this machine. The app keeps trying in the background — do NOT send this message again; monitor with list_outbox or cancel with cancel_outbox. %s%s",
			delivery.OutboxID, delivery.State, sendQueuedExpiryText(delivery), sendWaitExpiredSuffix(o),
		)
	}
}

func sendQueuedExpiryText(delivery messaging.Delivery) string {
	if delivery.ExpiresAt.IsZero() {
		return "If it cannot transmit, it stays queued until canceled."
	}
	return fmt.Sprintf(
		"If it has not transmitted by %s it is canceled as expired instead of sending stale.",
		delivery.ExpiresAt.UTC().Format(time.RFC3339),
	)
}

func sendWaitExpiredSuffix(o sendOutcome) string {
	if o.WaitedForTransmit && o.WaitExpired {
		return " The wait_for_transmit window elapsed without transport acceptance."
	}
	return ""
}

// sendOutcomeResult renders one durable-send outcome. Never an IsError
// result: the intent is durably owned by the outbox, and a tool error
// invites the calling agent to resend.
func sendOutcomeResult(o sendOutcome) *mcp.CallToolResult {
	return structuredResult(buildSendPayload(o), sendResultText(o))
}

// withSendControlOptions appends the durable-send control arguments shared
// by the send tools (v2/daemon modes only — legacy direct sends have no
// outbox for these to act on).
func withSendControlOptions(options []mcp.ToolOption, includeForce bool) []mcp.ToolOption {
	options = append(options,
		mcp.WithNumber("ttl_seconds", mcp.Description(
			"Send window in seconds: if the message is still queued when the window closes, it is canceled as expired instead of transmitting stale. Default 600 (10 minutes; installation override via OPENMESSAGES_SEND_TTL_SECONDS). 0 = never expire. Max 86400.",
		)),
		mcp.WithBoolean("wait_for_transmit", mcp.Description(
			"Keep waiting (bounded by wait_seconds) until the transport acknowledges the send — or it fails terminally — instead of returning while it is queued or auto-retrying. Use this when you must report truthfully whether the message actually went out.",
		)),
		mcp.WithNumber("wait_seconds", mcp.Description(
			"How long to wait for the send outcome before reporting the durable queued state (default 25, max 120).",
		)),
	)
	if includeForce {
		options = append(options, mcp.WithBoolean("force", mcp.Description(
			"Bypass the near-duplicate guard: submit even though a very similar message was sent to this conversation within the last few minutes. Use only for a deliberate repeat.",
		)))
	}
	return options
}

// messageByIDReader is the optional read-source capability used to check for
// delivery receipts. The legacy store satisfies it; when the active read
// source does not, sends simply never report "delivered".
type messageByIDReader interface {
	GetMessageByID(messageID string) (*db.Message, error)
}

// deliveryReceiptObserved reports whether the local store has seen a
// delivery/read receipt for the transmitted message. Google Messages stores
// OUTGOING_DELIVERED/OUTGOING_READ/OUTGOING_DISPLAYED on the message row;
// WhatsApp receipts normalize to DELIVERED/READ. Absence of a receipt is not
// evidence of non-delivery — many paths never record one.
func deliveryReceiptObserved(reads readsource.ReadSource, remoteMessageID string) bool {
	if reads == nil || strings.TrimSpace(remoteMessageID) == "" {
		return false
	}
	reader, ok := reads.(messageByIDReader)
	if !ok {
		return false
	}
	message, err := reader.GetMessageByID(remoteMessageID)
	if err != nil || message == nil {
		return false
	}
	status := strings.ToUpper(message.Status)
	return strings.Contains(status, "DELIVERED") ||
		strings.Contains(status, "READ") ||
		strings.Contains(status, "DISPLAYED")
}

// platformUnavailableResult refuses a send whose platform cannot send right
// now. Nothing is queued; there is deliberately no cross-platform fallback —
// the channel is part of the instruction.
func platformUnavailableResult(platform, reason string) *mcp.CallToolResult {
	if reason == "" {
		reason = "the platform cannot send right now"
	}
	text := fmt.Sprintf(
		"Cannot send on %s: %s. The message was NOT queued. No fallback to another platform is attempted — the requested channel is part of the instruction. Use resolve_contact_routes to list this contact's sendable routes and choose one explicitly, or fix the platform and retry.",
		platform, reason,
	)
	result := structuredResult(map[string]any{
		"ok":         false,
		"error":      text,
		"error_kind": "platform_unsendable",
		"platform":   platform,
		"reason":     reason,
	}, text)
	result.IsError = true
	return result
}

// platformMismatchResult refuses a send whose conversation belongs to a
// different platform than the caller demanded.
func platformMismatchResult(requested, actual, conversationID string) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"Platform mismatch: conversation %s is a %s conversation, but platform=%q was requested. The message was NOT queued. Re-check the route (resolve_contact_routes) or omit the platform argument to send on the conversation's own platform.",
		conversationID, actual, requested,
	)
	result := structuredResult(map[string]any{
		"ok":                 false,
		"error":              text,
		"error_kind":         "platform_mismatch",
		"requested_platform": requested,
		"actual_platform":    actual,
		"conversation_id":    conversationID,
	}, text)
	result.IsError = true
	return result
}

// duplicateBlockedResult surfaces the near-duplicate guard. Nothing was
// queued, so an error result is safe (it cannot cause a double-send; it
// prevents one).
func duplicateBlockedResult(err *messaging.DuplicateSendError) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"NOT QUEUED: a very similar message was submitted to this conversation %s ago (outbox %s, state %s) and may still reach the recipient. Sending this too would risk a double-text. Check that prior send first (list_outbox / get_conversation). If both messages are genuinely intended, resend with force=true.",
		(time.Duration(err.PriorAgeMS) * time.Millisecond).Round(time.Second),
		err.PriorOutboxID,
		err.PriorState,
	)
	result := structuredResult(map[string]any{
		"ok":                     false,
		"error":                  text,
		"error_kind":             "near_duplicate_blocked",
		"duplicate_of_outbox_id": err.PriorOutboxID,
		"duplicate_state":        string(err.PriorState),
	}, text)
	result.IsError = true
	return result
}

// daemonDuplicateBlockedResult recognizes the daemon's HTTP 409 for the
// near-duplicate guard and renders the same guidance as the in-process path.
func daemonDuplicateBlockedResult(responseErr *localapi.ResponseError) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"NOT QUEUED: the app blocked this as a near-duplicate of a message submitted moments ago that may still reach the recipient (%s). Check that prior send first (list_outbox / get_conversation). If both messages are genuinely intended, resend with force=true.",
		responseErr.Body,
	)
	result := structuredResult(map[string]any{
		"ok":         false,
		"error":      text,
		"error_kind": "near_duplicate_blocked",
	}, text)
	result.IsError = true
	return result
}

func isDaemonDuplicateRejection(err error) (*localapi.ResponseError, bool) {
	responseErr, ok := localapi.AsResponseError(err)
	if !ok || responseErr.StatusCode != 409 {
		return nil, false
	}
	if !strings.Contains(responseErr.Body, "near-duplicate") {
		return nil, false
	}
	return responseErr, true
}

// localSendCapability computes per-platform send capability for a process
// that owns its own transports (standalone serve). Client mode uses daemon
// truth instead.
func localSendCapability(a *app.App, v2 *V2Dependencies) map[string]sendcap.Capability {
	inputs := sendcap.Inputs{
		TransportsEnabled: true,
		Google:            googleStatus(a),
		WhatsApp:          whatsAppStatus(a),
		Signal:            signalStatus(a),
	}
	if v2 != nil && v2.Registry != nil {
		inputs.AdapterTextSend = func(platform string) bool {
			accountID := v2wire.AccountIDForPlatform(platform)
			if accountID == "" {
				return false
			}
			return v2.Registry.Capabilities(accountID).TextSend
		}
	}
	return sendcap.Compute(inputs)
}

// checkPlatformSendable enforces route sendability at send time against a
// capability map. Unknown platforms pass through (the submit path validates
// them); missing maps mean "capability unknown", which must not block. A
// queueable outage (transient disconnect) also passes: the durable outbox
// exists exactly for that case, and the result reports queued/not-transmitted
// truthfully with a TTL bounding staleness.
func checkPlatformSendable(capabilities map[string]sendcap.Capability, platform string) *mcp.CallToolResult {
	if capabilities == nil || platform == "" {
		return nil
	}
	capability, known := capabilities[platform]
	if !known || capability.Available || capability.Queueable {
		return nil
	}
	return platformUnavailableResult(platform, capability.Reason)
}
