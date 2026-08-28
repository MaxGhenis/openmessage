package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
)

// list_outbox and cancel_outbox give agents direct custody of durable sends:
// see what is still queued or retrying, and stop a stale send before it
// transmits (2026-08-05: an overnight-queued send flushed ~15 hours later
// with no way to see or stop it from the MCP surface).

func listOutboxTool() mcp.Tool {
	return mcp.NewTool("list_outbox",
		mcp.WithDescription("List durable outbox sends that have not completed: queued, dispatching, auto-retrying, uncertain, or awaiting local repair. Use after any send that did not report transmitted, and before retrying anything — a queued predecessor here means do NOT resend."),
		mcp.WithString("conversation_id", mcp.Description("Only list sends for this conversation")),
		mcp.WithNumber("limit", mcp.Description("Maximum items to return (default 50, max 200)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func cancelOutboxTool() mcp.Tool {
	return mcp.NewTool("cancel_outbox",
		mcp.WithDescription("Cancel one durable outbox send that has NOT crossed the transport boundary (state queued or not_dispatched). Once canceled it will never transmit. Sends already handed to the transport (dispatching/uncertain/confirmed) cannot be canceled."),
		mcp.WithString("outbox_id", mcp.Required(), mcp.Description("Outbox ID from a send result or list_outbox")),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
}

const outboxUnavailableText = "the durable outbox is not available in this serving mode: legacy direct sends transmit synchronously and leave nothing queued. This tool works with v2 sending enabled or when routing through the running OpenMessage app."

func outboxUnavailableHandler() server.ToolHandlerFunc {
	return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return errorResult(outboxUnavailableText), nil
	}
}

type outboxRow struct {
	OutboxID       string `json:"outbox_id"`
	ConversationID string `json:"conversation_id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	TransportState string `json:"transport_state"`
	ScheduledForMS int64  `json:"scheduled_for_ms"`
	NextAttemptMS  int64  `json:"next_attempt_at_ms,omitempty"`
	ExpiresAtMS    int64  `json:"expires_at_ms,omitempty"`
	AttemptCount   int64  `json:"attempt_count"`
	CreatedAtMS    int64  `json:"created_at_ms"`
	Summary        string `json:"summary,omitempty"`
	ErrorClass     string `json:"error_class,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
}

func transportStateForOutboxState(state messaging.OutboxState) string {
	return sendOutcome{Delivery: messaging.Delivery{State: state}}.transportState()
}

func outboxListResult(rows []outboxRow, conversationID string) *mcp.CallToolResult {
	scope := ""
	if conversationID != "" {
		scope = fmt.Sprintf(" for conversation %s", conversationID)
	}
	if len(rows) == 0 {
		return structuredResult(map[string]any{
			"count": 0,
			"items": []outboxRow{},
		}, fmt.Sprintf("The outbox has no incomplete sends%s: nothing is queued, retrying, or uncertain.", scope))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d incomplete send(s)%s. Queued/retrying items have NOT been transmitted — do not resend them; cancel_outbox stops one before it transmits.\n", len(rows), scope)
	for _, row := range rows {
		fmt.Fprintf(&sb, "- %s [%s → %s] conversation=%s created=%s attempts=%d",
			row.OutboxID,
			row.State,
			row.TransportState,
			row.ConversationID,
			time.UnixMilli(row.CreatedAtMS).UTC().Format(time.RFC3339),
			row.AttemptCount,
		)
		if row.ExpiresAtMS > 0 {
			fmt.Fprintf(&sb, " expires=%s", time.UnixMilli(row.ExpiresAtMS).UTC().Format(time.RFC3339))
		}
		if row.Summary != "" {
			fmt.Fprintf(&sb, " %q", row.Summary)
		}
		sb.WriteByte('\n')
	}
	return structuredResult(map[string]any{
		"count": len(rows),
		"items": rows,
	}, sb.String())
}

func outboxListLimit(args map[string]any) int {
	limit := intArg(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return limit
}

func v2ListOutboxHandler(v2 *V2Dependencies) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v2 == nil || v2.Service == nil {
			return errorResult(outboxUnavailableText), nil
		}
		args := req.GetArguments()
		conversationID := strings.TrimSpace(strArg(args, "conversation_id"))
		pending, err := v2.Service.ListPending(ctx, messaging.ListPendingQuery{
			ConversationID: conversationID,
			Limit:          outboxListLimit(args),
		})
		if err != nil {
			return errorResult(fmt.Sprintf("list outbox: %v", err)), nil
		}
		rows := make([]outboxRow, 0, len(pending))
		for _, delivery := range pending {
			row := outboxRow{
				OutboxID:       delivery.OutboxID,
				ConversationID: delivery.ConversationID,
				Kind:           string(delivery.Kind),
				State:          string(delivery.State),
				TransportState: transportStateForOutboxState(delivery.State),
				ScheduledForMS: delivery.ScheduledFor.UnixMilli(),
				AttemptCount:   delivery.AttemptCount,
				CreatedAtMS:    delivery.CreatedAt.UnixMilli(),
				Summary:        delivery.Summary,
				ErrorClass:     delivery.ErrorClass,
				ErrorCode:      delivery.ErrorCode,
			}
			if !delivery.NextAttemptAt.IsZero() {
				row.NextAttemptMS = delivery.NextAttemptAt.UnixMilli()
			}
			if !delivery.ExpiresAt.IsZero() {
				row.ExpiresAtMS = delivery.ExpiresAt.UnixMilli()
			}
			rows = append(rows, row)
		}
		return outboxListResult(rows, conversationID), nil
	}
}

func daemonListOutboxHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strings.TrimSpace(strArg(args, "conversation_id"))
		pending, err := daemon.ListPending(ctx, conversationID, outboxListLimit(args))
		if err != nil {
			if responseErr, ok := localapi.AsResponseError(err); ok && responseErr.StatusCode == 503 {
				return errorResult("the running app does not have v2 sending enabled, so it keeps no durable outbox (legacy sends transmit synchronously)."), nil
			}
			return daemonDownResult(err), nil
		}
		rows := make([]outboxRow, 0, len(pending))
		for _, delivery := range pending {
			row := outboxRow{
				OutboxID:       delivery.OutboxID,
				ConversationID: delivery.ConversationID,
				Kind:           delivery.Kind,
				State:          delivery.State,
				TransportState: transportStateForOutboxState(messaging.OutboxState(delivery.State)),
				ScheduledForMS: delivery.ScheduledForMS,
				ExpiresAtMS:    delivery.ExpiresAtMS,
				AttemptCount:   delivery.AttemptCount,
				CreatedAtMS:    delivery.CreatedAtMS,
				Summary:        delivery.Summary,
				ErrorClass:     delivery.ErrorClass,
				ErrorCode:      delivery.ErrorCode,
			}
			if delivery.NextAttemptMS != nil {
				row.NextAttemptMS = *delivery.NextAttemptMS
			}
			rows = append(rows, row)
		}
		return outboxListResult(rows, conversationID), nil
	}
}

func outboxCancelSuccessResult(delivery messaging.Delivery) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"Canceled: outbox %s will never transmit. The message was NOT sent.",
		delivery.OutboxID,
	)
	return structuredResult(map[string]any{
		"ok":              true,
		"outbox_id":       delivery.OutboxID,
		"state":           string(delivery.State),
		"transport_state": transportStateForOutboxState(delivery.State),
		"canceled":        true,
	}, text)
}

func outboxCancelInvalidStateText(outboxID string, state string) string {
	return fmt.Sprintf(
		"cannot cancel outbox %s from state %q: it already crossed (or is crossing) the transport boundary, so canceling could no longer prevent delivery. Check the conversation to see whether the message arrived.",
		outboxID, state,
	)
}

func v2CancelOutboxHandler(v2 *V2Dependencies) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if v2 == nil || v2.Service == nil {
			return errorResult(outboxUnavailableText), nil
		}
		outboxID := strings.TrimSpace(strArg(req.GetArguments(), "outbox_id"))
		if outboxID == "" {
			return errorResult("outbox_id is required"), nil
		}
		delivery, err := v2.Service.Cancel(ctx, outboxID)
		if err != nil {
			if errors.Is(err, messaging.ErrInvalidState) {
				return errorResult(outboxCancelInvalidStateText(outboxID, string(delivery.State))), nil
			}
			return errorResult(fmt.Sprintf("cancel outbox %s: %v", outboxID, err)), nil
		}
		return outboxCancelSuccessResult(delivery), nil
	}
}

func daemonCancelOutboxHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		outboxID := strings.TrimSpace(strArg(req.GetArguments(), "outbox_id"))
		if outboxID == "" {
			return errorResult("outbox_id is required"), nil
		}
		delivery, err := daemon.CancelDelivery(ctx, outboxID)
		if err != nil {
			if responseErr, ok := localapi.AsResponseError(err); ok {
				switch responseErr.StatusCode {
				case 409:
					return errorResult(outboxCancelInvalidStateText(outboxID, "already dispatched or terminal")), nil
				case 404:
					return errorResult(fmt.Sprintf("outbox %s was not found on the app", outboxID)), nil
				case 503:
					return errorResult("the running app does not have v2 sending enabled, so it keeps no durable outbox."), nil
				}
			}
			return daemonDownResult(err), nil
		}
		return outboxCancelSuccessResult(deliveryFromLocalAPI(delivery)), nil
	}
}
