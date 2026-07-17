package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
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

const v2DeliveryDescription = " With v2 sending enabled, this waits for a settled delivery result. Every result includes outbox_id and idempotency_key. If settled is false (the wait was interrupted or the app is retrying automatically), the message is still durably queued and the app finishes sending it in the background: never send it again in response. An uncertain result means the transport may have accepted the message; do not retry automatically. Send again only as a deliberate new intent, reusing the returned idempotency_key when repeating the exact same send after a lost response."

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
	submission, err := v2.submitText(ctx, a, v2wire.TextInput{
		ConversationID: conversationID,
		Body:           body,
		IdempotencyKey: key,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("failed to submit message: %v", err))
	}
	return waitForV2Delivery(ctx, v2, submission, key)
}

// waitForV2Delivery reports the durable send's outcome. The intent is already
// enqueued when this runs, so no path below may return an IsError result: a
// tool error invites the calling agent to resend, and the outbox will finish
// the first send regardless. Interrupted waits and auto-retrying states are
// reported as non-settled statuses with explicit do-not-resend guidance.
func waitForV2Delivery(
	ctx context.Context,
	v2 *V2Dependencies,
	submission messaging.Submission,
	idempotencyKey string,
) *mcp.CallToolResult {
	if v2.Service == nil {
		return errorResult("v2 send service is unavailable")
	}
	delivery, err := v2.Service.Wait(ctx, submission.OutboxID)
	if err != nil {
		return v2InterruptedResult(submission, idempotencyKey, delivery, err)
	}

	settled := delivery.State != messaging.OutboxNotDispatched
	payload := map[string]any{
		"ok":               v2DeliveryOK(delivery.State),
		"settled":          settled,
		"outbox_id":        delivery.OutboxID,
		"state":            delivery.State,
		"deduplicated":     submission.Deduplicated,
		"local_message_id": delivery.LocalMessageID,
		"idempotency_key":  idempotencyKey,
	}
	if !settled {
		payload["auto_retry"] = true
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

	return structuredResult(payload, v2DeliveryText(delivery))
}

// v2InterruptedResult handles Wait ending before the delivery settled (request
// context canceled or timed out, or a transient read failure). The durable row
// is untouched by the interruption and the dispatcher runs on its own context,
// so the send still completes in the background.
func v2InterruptedResult(
	submission messaging.Submission,
	idempotencyKey string,
	lastKnown messaging.Delivery,
	waitErr error,
) *mcp.CallToolResult {
	outboxID := lastKnown.OutboxID
	if outboxID == "" {
		outboxID = submission.OutboxID
	}
	state := string(lastKnown.State)
	if state == "" {
		state = string(messaging.OutboxQueued)
	}
	payload := map[string]any{
		"ok":              false,
		"settled":         false,
		"outbox_id":       outboxID,
		"state":           state,
		"deduplicated":    submission.Deduplicated,
		"idempotency_key": idempotencyKey,
		"wait_error":      waitErr.Error(),
	}
	if lastKnown.LocalMessageID != "" {
		payload["local_message_id"] = lastKnown.LocalMessageID
	}
	text := fmt.Sprintf(
		"The send is durably queued (outbox %s, state %s) and the app will finish sending it in the background; this wait was interrupted (%v) before the outcome settled. Do NOT send this message again. To repeat the exact same send deliberately, reuse idempotency_key %s.",
		outboxID, state, waitErr, idempotencyKey,
	)
	return structuredResult(payload, text)
}

// v2DeliveryOK reports whether the message reached the transport. store_failed
// means the transport accepted the send and only the local record needs repair,
// which the dispatcher performs automatically.
func v2DeliveryOK(state messaging.OutboxState) bool {
	return state == messaging.OutboxConfirmed || state == messaging.OutboxStoreFailed
}

func v2DeliveryText(delivery messaging.Delivery) string {
	switch delivery.State {
	case messaging.OutboxConfirmed:
		if delivery.RemoteMessageID != "" {
			return fmt.Sprintf("Message delivery confirmed (outbox %s, remote message %s).", delivery.OutboxID, delivery.RemoteMessageID)
		}
		return fmt.Sprintf("Message delivery confirmed (outbox %s).", delivery.OutboxID)
	case messaging.OutboxUncertain:
		return fmt.Sprintf("Message delivery is uncertain (outbox %s): the transport may have accepted it. Do not retry automatically; send again only as a deliberate new intent.", delivery.OutboxID)
	case messaging.OutboxNotDispatched:
		return fmt.Sprintf("Delivery has not succeeded yet (outbox %s, error class %s). The app is retrying it automatically; do NOT send this message again.", delivery.OutboxID, firstNonEmpty(delivery.ErrorClass, "unknown"))
	case messaging.OutboxStoreFailed:
		return fmt.Sprintf("The transport accepted the message (outbox %s, remote message %s); the local record is being repaired automatically. Do not resend.", delivery.OutboxID, delivery.RemoteMessageID)
	case messaging.OutboxRejected:
		return fmt.Sprintf("Message delivery was rejected (outbox %s, error class %s). The app will not retry it; sending again creates a new message and may fail the same way.", delivery.OutboxID, firstNonEmpty(delivery.ErrorClass, "unknown"))
	case messaging.OutboxCanceled:
		return fmt.Sprintf("Message delivery was canceled (outbox %s).", delivery.OutboxID)
	default:
		return fmt.Sprintf("Message delivery settled as %s (outbox %s).", delivery.State, delivery.OutboxID)
	}
}
