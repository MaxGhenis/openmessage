package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/localapi"
)

// markReadBulkDelay throttles the remote calls in a bulk mark-read.
//
// The motivating case is a one-off cleanup of a long-stale unread backlog
// (hundreds of threads). Each thread is a separate round trip to Google, and
// firing them back to back is the shape most likely to get the account
// throttled — which would cost a re-pair, not just a failed call. 250ms is
// slow enough to stay unremarkable and still clears several hundred threads in
// a couple of minutes.
const markReadBulkDelay = 250 * time.Millisecond

func markReadTool() mcp.Tool {
	return mcp.NewTool("mark_read",
		mcp.WithDescription(
			"Mark one or more conversations as read, clearing the unread badge on the phone as well as in OpenMessage. "+
				"Pass a single conversation_id or a list of them for a bulk cleanup. "+
				"On RCS threads this sends a read receipt, so the other person can see that you read the message — "+
				"only use it when the user has asked you to.",
		),
		mcp.WithString("conversation_id", mcp.Description("Conversation to mark read. Use conversation_ids for several at once.")),
		mcp.WithArray("conversation_ids",
			mcp.Description("Conversations to mark read, for a bulk cleanup. Throttled to stay under Google's rate limits."),
			mcp.WithStringItems(),
		),
		mcp.WithString("message_id", mcp.Description(
			"Optional message to advance the read cursor to. Defaults to the newest incoming message in the thread. Ignored for a bulk call.")),
		// Not destructive (nothing is deleted) but not idempotent either: a
		// repeat call re-sends a read receipt.
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

// markReadTargets reads the conversation ids from either argument shape.
func markReadTargets(args map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	add(strArg(args, "conversation_id"))
	if raw, ok := args["conversation_ids"]; ok {
		switch list := raw.(type) {
		case []string:
			for _, s := range list {
				add(s)
			}
		case []any:
			for _, item := range list {
				if s, ok := item.(string); ok {
					add(s)
				}
			}
		}
	}
	return out
}

func markReadHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		targets := markReadTargets(args)
		if len(targets) == 0 {
			return errorResult("conversation_id (or conversation_ids) is required"), nil
		}
		// A caller-supplied message_id only makes sense for a single thread.
		messageID := ""
		if len(targets) == 1 {
			messageID = strArg(args, "message_id")
		}

		var (
			synced      []string
			localOnly   []map[string]any
			failedLocal []map[string]any
		)

		for i, conversationID := range targets {
			if i > 0 {
				select {
				case <-ctx.Done():
					return errorResult(fmt.Sprintf(
						"cancelled after %d of %d conversations: %v", i, len(targets), ctx.Err())), nil
				case <-time.After(markReadBulkDelay):
				}
			}

			result, err := a.SyncConversationRead(conversationID, messageID)
			switch {
			case err != nil:
				failedLocal = append(failedLocal, map[string]any{
					"conversation_id": conversationID,
					"error":           err.Error(),
				})
			case result.RemoteSynced:
				synced = append(synced, conversationID)
			default:
				reason := result.RemoteSkipped
				if result.RemoteErr != nil {
					reason = result.RemoteErr.Error()
				}
				localOnly = append(localOnly, map[string]any{
					"conversation_id": conversationID,
					"reason":          reason,
				})
			}
		}

		payload := map[string]any{
			"requested":       len(targets),
			"synced_to_phone": synced,
			"local_only":      localOnly,
			"failed":          failedLocal,
		}

		// One thread, one clear sentence — the common case.
		if len(targets) == 1 {
			switch {
			case len(failedLocal) == 1:
				return errorResult(fmt.Sprintf("could not mark read: %s", failedLocal[0]["error"])), nil
			case len(localOnly) == 1:
				return errorResult(fmt.Sprintf(
					"marked read in OpenMessage, but NOT on the phone: %s", localOnly[0]["reason"])), nil
			default:
				return structuredResult(payload, fmt.Sprintf(
					"Marked %s read; the unread badge should clear on the phone.", targets[0])), nil
			}
		}

		summary := fmt.Sprintf("Marked %d of %d conversations read on the phone.", len(synced), len(targets))
		if len(localOnly) > 0 {
			summary += fmt.Sprintf(" %d cleared locally only.", len(localOnly))
		}
		if len(failedLocal) > 0 {
			summary += fmt.Sprintf(" %d failed.", len(failedLocal))
		}
		return structuredResult(payload, summary), nil
	}
}

// daemonMarkReadHandler routes mark_read through the running daemon.
//
// This is not optional: `serve --mcp-stdio` — the shape MCP hosts spawn per
// session — is a transportless client whose a.GetClient() is nil, so the
// legacy handler above could never reach Google from there. The daemon owns
// the live connection, so the work is posted to its /api/mark-read instead.
func daemonMarkReadHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		targets := markReadTargets(args)
		if len(targets) == 0 {
			return errorResult("conversation_id (or conversation_ids) is required"), nil
		}
		messageID := ""
		if len(targets) == 1 {
			messageID = strArg(args, "message_id")
		}

		var (
			ok     []string
			failed []map[string]any
		)
		for i, conversationID := range targets {
			if i > 0 {
				select {
				case <-ctx.Done():
					return errorResult(fmt.Sprintf(
						"cancelled after %d of %d conversations: %v", i, len(targets), ctx.Err())), nil
				case <-time.After(markReadBulkDelay):
				}
			}
			if err := daemon.MarkRead(ctx, conversationID, messageID); err != nil {
				if responseErr, isResponse := localapi.AsResponseError(err); isResponse {
					failed = append(failed, map[string]any{
						"conversation_id": conversationID,
						"error":           fmt.Sprintf("HTTP %d: %s", responseErr.StatusCode, responseErr.Body),
					})
					continue
				}
				// The daemon itself is unreachable; further calls will fail
				// the same way, so stop rather than grinding through the list.
				return daemonDownResult(err), nil
			}
			ok = append(ok, conversationID)
		}

		payload := map[string]any{
			"requested": len(targets),
			"marked":    ok,
			"failed":    failed,
			"via":       "app",
		}
		if len(targets) == 1 && len(failed) == 1 {
			return errorResult(fmt.Sprintf("the app could not mark it read: %s", failed[0]["error"])), nil
		}
		summary := fmt.Sprintf("Marked %d of %d conversations read via the running OpenMessage app.", len(ok), len(targets))
		if len(failed) > 0 {
			summary += fmt.Sprintf(" %d failed.", len(failed))
		}
		return structuredResult(payload, summary), nil
	}
}
