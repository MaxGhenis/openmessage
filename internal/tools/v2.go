package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

// V2Dependencies is the optional durable-send seam used by the four MCP send
// tools. A nil/disabled value leaves their legacy descriptors and handlers
// untouched.
type V2Dependencies struct {
	Enabled   bool
	V2Primary bool
	Service   *messaging.MessageService
	V2Store   *sqlite.Store
	Registry  bridge.Registry
}

const v2DeliveryDescription = " With v2 sending enabled, this reports truthful transport state: transport_state is queued (has NOT left this machine), transmitted (the platform transport accepted it — NOT proof of delivery), delivered (a delivery receipt was observed), uncertain, failed, or canceled. settled/transmitted are true only on transport acknowledgment; while they are false the message is still durably queued and the app keeps sending it in the background — never send it again in response. An uncertain result means the transport may have accepted the message; do not retry automatically. Results include the platform actually used and the conversation_id written to; there is never a silent fallback to another platform. Sends carry a default ~10-minute send window (ttl_seconds; 0 = never expire) after which a still-queued message cancels as expired instead of sending stale. Near-identical resends within a few minutes are blocked unless force=true. Set wait_for_transmit=true (with wait_seconds, max 120) to keep waiting for transport acknowledgment before returning. Reuse the returned idempotency_key only to replay the exact same send after a lost response."

const v2IdempotencyDescription = "Optional retry key for the exact same send. Every result echoes the key in use; reuse the same key only when repeating a send whose response was lost. Omit it to mint a new intent."

func activeV2(options []*V2Dependencies) *V2Dependencies {
	if len(options) == 0 || options[0] == nil || !options[0].Enabled {
		return nil
	}
	return options[0]
}

func v2Requested(enabled []bool) bool {
	return len(enabled) > 0 && enabled[0]
}

func (v *V2Dependencies) submitDeps(a *app.App) v2wire.Deps {
	return v2wire.Deps{
		Legacy:   a.Store,
		V2:       v.V2Store,
		Service:  v.Service,
		Registry: v.Registry,
	}
}

func (v *V2Dependencies) nativeDeps() v2wire.NativeDeps {
	return v2wire.NativeDeps{
		V2:       v.V2Store,
		Service:  v.Service,
		Registry: v.Registry,
	}
}

func (v *V2Dependencies) submitText(
	ctx context.Context,
	a *app.App,
	input v2wire.TextInput,
) (messaging.Submission, error) {
	if v.V2Primary {
		return v2wire.SubmitTextV2(ctx, v.nativeDeps(), input)
	}
	return v2wire.SubmitText(ctx, v.submitDeps(a), input)
}

func (v *V2Dependencies) submitMedia(
	ctx context.Context,
	a *app.App,
	input v2wire.MediaInput,
) (messaging.Submission, error) {
	if v.V2Primary {
		return v2wire.SubmitMediaV2(ctx, v.nativeDeps(), input)
	}
	return v2wire.SubmitMedia(ctx, v.submitDeps(a), input)
}

func v2IdempotencyKey(args map[string]any) (string, error) {
	if raw, present := args["idempotency_key"]; present {
		key, ok := raw.(string)
		if !ok {
			return "", errors.New("idempotency_key must be a string")
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return "", errors.New("idempotency_key must not be blank")
		}
		if len(key) > 128 {
			return "", errors.New("idempotency_key is too long")
		}
		for i := 0; i < len(key); i++ {
			c := key[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				continue
			}
			switch c {
			case '_', '-', '.', ':':
				continue
			default:
				return "", errors.New("idempotency_key contains unsupported characters")
			}
		}
		return key, nil
	}
	return newMCPIdempotencyKey()
}

func newMCPIdempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate MCP idempotency key: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(value[0:4]),
		hex.EncodeToString(value[4:6]),
		hex.EncodeToString(value[6:8]),
		hex.EncodeToString(value[8:10]),
		hex.EncodeToString(value[10:16]),
	}, "-"), nil
}

// sendPlatform resolves the send platform for a conversation ID. Prefixed
// IDs are authoritative; otherwise the serving store's conversation row
// decides. Empty when unresolvable — the submit path still validates the
// conversation, so an unknown platform never blocks a legitimate send.
func (v *V2Dependencies) sendPlatform(a *app.App, conversationID string) string {
	switch {
	case strings.HasPrefix(conversationID, "whatsapp:"):
		return "whatsapp"
	case strings.HasPrefix(conversationID, "signal:"), strings.HasPrefix(conversationID, "signal-group:"):
		return "signal"
	}
	if v.V2Primary && v.V2Store != nil {
		conversation, err := v.V2Store.GetConversation(conversationID)
		if err != nil {
			return ""
		}
		account, err := v.V2Store.GetAccount(conversation.AccountID)
		if err != nil {
			return ""
		}
		if account.BridgeKey == "google" {
			return "sms"
		}
		return account.BridgeKey
	}
	if a != nil && a.Store != nil {
		if conversation, err := a.Store.GetConversation(conversationID); err == nil && conversation != nil {
			return normalizedPlatform(conversation.SourcePlatform)
		}
	}
	return ""
}

func submitV2Text(
	ctx context.Context,
	a *app.App,
	v2 *V2Dependencies,
	args map[string]any,
	conversationID string,
	body string,
) *mcp.CallToolResult {
	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	ttl, err := parseSendTTL(args)
	if err != nil {
		return errorResult(err.Error())
	}
	force, err := parseSendForce(args)
	if err != nil {
		return errorResult(err.Error())
	}
	wait, err := parseSendWaitOptions(args)
	if err != nil {
		return errorResult(err.Error())
	}
	platform := v2.sendPlatform(a, conversationID)
	if failure := checkPlatformSendable(localSendCapability(a, v2), platform); failure != nil {
		return failure
	}
	submission, err := v2.submitText(ctx, a, v2wire.TextInput{
		ConversationID: conversationID,
		Body:           body,
		IdempotencyKey: key,
		TTL:            ttl,
		Force:          force,
	})
	if err != nil {
		var duplicate *messaging.DuplicateSendError
		if errors.As(err, &duplicate) {
			return duplicateBlockedResult(duplicate)
		}
		if errors.Is(err, v2wire.ErrPlatformNotSendable) {
			return platformUnavailableResult(firstNonEmpty(platform, "the requested platform"), err.Error())
		}
		return errorResult(fmt.Sprintf("failed to submit message: %v", err))
	}
	return waitForV2Delivery(ctx, a, v2, submission, key, platform, conversationID, wait)
}

// waitForV2Delivery reports the durable send's outcome. The intent is already
// enqueued when this runs, so no path below may return an IsError result: a
// tool error invites the calling agent to resend, and the outbox will finish
// the first send regardless. Interrupted waits and auto-retrying states are
// reported as non-settled statuses with explicit do-not-resend guidance.
func waitForV2Delivery(
	ctx context.Context,
	a *app.App,
	v2 *V2Dependencies,
	submission messaging.Submission,
	idempotencyKey string,
	platform string,
	conversationID string,
	wait sendWaitOptions,
) *mcp.CallToolResult {
	if v2.Service == nil {
		return errorResult("v2 send service is unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait.Wait)
	defer cancel()

	var delivery messaging.Delivery
	var waitErr error
	for {
		delivery, waitErr = v2.Service.Get(waitCtx, submission.OutboxID)
		if waitErr != nil {
			break
		}
		if sendSettled(delivery.State) || delivery.State == messaging.OutboxUncertain {
			break
		}
		// not_dispatched is stable-but-retrying: report it unless the caller
		// asked to hold out for transport acknowledgment.
		if delivery.State == messaging.OutboxNotDispatched && !wait.WaitForTransmit {
			break
		}
		changed := v2.Service.Changes()
		select {
		case <-waitCtx.Done():
			waitErr = waitCtx.Err()
		case <-changed:
		case <-time.After(250 * time.Millisecond):
		}
		if waitErr != nil {
			break
		}
	}
	if waitErr != nil {
		return v2InterruptedResult(a, v2, submission, idempotencyKey, platform, conversationID, waitErr)
	}

	outcome := sendOutcome{
		Delivery:          delivery,
		IdempotencyKey:    idempotencyKey,
		Deduplicated:      submission.Deduplicated,
		Platform:          platform,
		ConversationID:    conversationID,
		WaitedForTransmit: wait.WaitForTransmit,
	}
	if sendTransmitted(delivery.State) {
		outcome.Delivered = deliveryReceiptObserved(legacyReads(a), delivery.RemoteMessageID)
	}
	return sendOutcomeResult(outcome)
}

// v2InterruptedResult handles the wait ending before the delivery settled
// (request context canceled or timed out, or a transient read failure). The
// durable row is untouched by the interruption and the dispatcher runs on its
// own context, so the send still completes in the background — unless its
// send window expires first.
func v2InterruptedResult(
	a *app.App,
	v2 *V2Dependencies,
	submission messaging.Submission,
	idempotencyKey string,
	platform string,
	conversationID string,
	waitErr error,
) *mcp.CallToolResult {
	// Best-effort fresh read on a detached context: the wait's own context is
	// typically the thing that just expired.
	lastKnown := messaging.Delivery{
		OutboxID:       submission.OutboxID,
		ConversationID: conversationID,
		State:          messaging.OutboxQueued,
		LocalMessageID: submission.LocalMessageID,
		ExpiresAt:      submission.ExpiresAt,
	}
	if v2.Service != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 2*time.Second)
		if delivery, err := v2.Service.Get(readCtx, submission.OutboxID); err == nil {
			lastKnown = delivery
		}
		cancel()
		if sendSettled(lastKnown.State) || lastKnown.State == messaging.OutboxUncertain {
			outcome := sendOutcome{
				Delivery:       lastKnown,
				IdempotencyKey: idempotencyKey,
				Deduplicated:   submission.Deduplicated,
				Platform:       platform,
				ConversationID: conversationID,
			}
			if sendTransmitted(lastKnown.State) {
				outcome.Delivered = deliveryReceiptObserved(legacyReads(a), lastKnown.RemoteMessageID)
			}
			return sendOutcomeResult(outcome)
		}
	}

	outcome := sendOutcome{
		Delivery:       lastKnown,
		IdempotencyKey: idempotencyKey,
		Deduplicated:   submission.Deduplicated,
		Platform:       platform,
		ConversationID: conversationID,
	}
	payload := buildSendPayload(outcome)
	payload["wait_error"] = waitErr.Error()
	text := fmt.Sprintf(
		"The send is durably queued (outbox %s, state %s) and has NOT been transmitted; this wait was interrupted (%v) before the outcome was known. The app keeps sending it in the background. Do NOT send this message again — check progress with list_outbox, or repeat the exact same send deliberately by reusing idempotency_key %s.",
		lastKnown.OutboxID, lastKnown.State, waitErr, idempotencyKey,
	)
	return structuredResult(payload, text)
}

// legacyReads returns the legacy store for delivery-receipt lookups. Receipt
// statuses (OUTGOING_DELIVERED, DELIVERED, READ) are recorded by the live
// event handlers on the legacy store regardless of serving mode.
func legacyReads(a *app.App) readsource.ReadSource {
	if a == nil || a.Store == nil {
		return nil
	}
	return a.Store
}
