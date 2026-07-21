package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/readsource"
)

// Daemon-routed MCP handlers back the transportless "client mode" serve. The
// MCP process never opens platform transports of its own — a second process
// holding the same WhatsApp device credentials or signal-cli account logs the
// daemon out — so every live-network action is routed at the running app's
// local HTTP API instead, exactly like the CLI send path.

const daemonDownText = "the OpenMessage app isn't running, and this MCP server runs in transportless client mode (it never opens its own WhatsApp/Signal/Google connections — a second connection would log the app out). Start the OpenMessage app, then retry. Local reads (search, conversations, history) keep working without the app."

// daemonSettleTimeout bounds how long a daemon-routed send waits for the
// outbox item to settle before reporting the durable queued state.
const daemonSettleTimeout = 25 * time.Second

func daemonDownResult(err error) *mcp.CallToolResult {
	if err != nil {
		return errorResult(fmt.Sprintf("%s (probe error: %v)", daemonDownText, err))
	}
	return errorResult(daemonDownText)
}

func daemonProbeFailureResult(err error) *mcp.CallToolResult {
	if localapi.IsAuthError(err) {
		return errorResult(fmt.Sprintf(
			"the OpenMessage app rejected this MCP server's control token: %v. Point OPENMESSAGES_DATA_DIR at the app's data directory (macOS: ~/Library/Application Support/OpenMessage) in the MCP server config so it reads the app's control token.", err,
		))
	}
	return errorResult(fmt.Sprintf("check running OpenMessage mode: %v", err))
}

// daemonStatusOrResult probes the daemon and translates failures into
// ready-to-return tool results.
func daemonStatusOrResult(ctx context.Context, daemon *localapi.Client) (localapi.DaemonStatus, *mcp.CallToolResult) {
	status, reachable, err := daemon.Status(ctx)
	if err != nil {
		if !reachable {
			return localapi.DaemonStatus{}, daemonDownResult(err)
		}
		return localapi.DaemonStatus{}, daemonProbeFailureResult(err)
	}
	return status, nil
}

func deliveryFromLocalAPI(delivery localapi.Delivery) messaging.Delivery {
	return messaging.Delivery{
		OutboxID:        delivery.OutboxID,
		State:           messaging.OutboxState(delivery.State),
		LocalMessageID:  delivery.LocalMessageID,
		RemoteMessageID: delivery.RemoteMessageID,
		ErrorClass:      delivery.ErrorClass,
		ErrorCode:       delivery.ErrorCode,
		Warning:         delivery.Warning,
	}
}

// daemonAmbiguousResult reports a send whose outcome the daemon may or may
// not have recorded (the request failed mid-flight). It is intentionally not
// an IsError result: an error invites the calling agent to resend, and the
// daemon may already own this intent. The idempotency key is the safe replay
// handle.
func daemonAmbiguousResult(idempotencyKey string, cause error) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"The send outcome is unknown (%v). Do NOT send this message again with a new key. To replay-check this exact send, repeat it with the same idempotency_key: %s. If the app is not running, start it first.",
		cause, idempotencyKey,
	)
	return structuredResult(map[string]any{
		"ok":              false,
		"settled":         false,
		"ambiguous":       true,
		"idempotency_key": idempotencyKey,
		"error":           cause.Error(),
	}, text)
}

// daemonSubmitTextAndWait submits one durable text send to the daemon outbox
// and waits (bounded) for it to settle, mirroring the in-process v2 result
// contract so agents see identical semantics in both serve modes.
func daemonSubmitTextAndWait(
	ctx context.Context,
	daemon *localapi.Client,
	args map[string]any,
	conversationID string,
	body string,
) *mcp.CallToolResult {
	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	submission, err := daemon.SubmitText(ctx, localapi.TextSubmission{
		ConversationID: conversationID,
		Body:           body,
		IdempotencyKey: key,
	})
	if err != nil {
		if localapi.IsDeterministicRejection(err) {
			return errorResult(fmt.Sprintf("send rejected by the app: %v", err))
		}
		return daemonAmbiguousResult(key, err)
	}
	return daemonWaitForDelivery(ctx, daemon, submission, key)
}

func daemonWaitForDelivery(
	ctx context.Context,
	daemon *localapi.Client,
	submission localapi.Submission,
	idempotencyKey string,
) *mcp.CallToolResult {
	delivery, settled, err := daemon.WaitDelivery(ctx, submission.OutboxID, daemonSettleTimeout)
	if err != nil {
		// The intent is durably queued on the daemon; only our view failed.
		return v2InterruptedResult(
			messaging.Submission{OutboxID: submission.OutboxID, Deduplicated: submission.Deduplicated},
			idempotencyKey,
			messaging.Delivery{},
			err,
		)
	}
	converted := deliveryFromLocalAPI(delivery)
	if !settled {
		payload := map[string]any{
			"ok":              false,
			"settled":         false,
			"auto_retry":      true,
			"outbox_id":       delivery.OutboxID,
			"state":           delivery.State,
			"deduplicated":    submission.Deduplicated,
			"idempotency_key": idempotencyKey,
		}
		if delivery.LocalMessageID != "" {
			payload["local_message_id"] = delivery.LocalMessageID
		}
		text := fmt.Sprintf(
			"The send is durably queued on the app (outbox %s, state %s) and the app finishes sending it in the background. Do NOT send this message again. To repeat the exact same send deliberately, reuse idempotency_key %s.",
			delivery.OutboxID, delivery.State, idempotencyKey,
		)
		return structuredResult(payload, text)
	}

	settledState := converted.State != messaging.OutboxNotDispatched
	payload := map[string]any{
		"ok":               v2DeliveryOK(converted.State),
		"settled":          settledState,
		"outbox_id":        delivery.OutboxID,
		"state":            delivery.State,
		"deduplicated":     submission.Deduplicated,
		"local_message_id": delivery.LocalMessageID,
		"idempotency_key":  idempotencyKey,
	}
	if !settledState {
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
	return structuredResult(payload, v2DeliveryText(converted))
}

// daemonLegacySendText routes a text send through a legacy-mode daemon's
// /api/send surface.
func daemonLegacySendText(
	ctx context.Context,
	daemon *localapi.Client,
	args map[string]any,
	conversationID string,
	body string,
) *mcp.CallToolResult {
	key, err := v2IdempotencyKey(args)
	if err != nil {
		return errorResult(err.Error())
	}
	result, err := daemon.LegacySendText(ctx, conversationID, body, "", key)
	if err != nil {
		if localapi.IsDeterministicRejection(err) {
			return errorResult(fmt.Sprintf("send rejected by the app: %v", err))
		}
		if _, isResponse := localapi.AsResponseError(err); isResponse {
			return errorResult(fmt.Sprintf("the app could not send the message: %v", err))
		}
		return daemonAmbiguousResult(key, err)
	}
	return structuredResult(map[string]any{
		"ok":              true,
		"message_id":      result.MessageID,
		"conversation_id": conversationID,
		"idempotency_key": key,
		"via":             "app",
	}, fmt.Sprintf("Message sent via the running OpenMessage app (message %s).", result.MessageID))
}

func daemonSendToConversationHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strArg(args, "conversation_id")
		message := strArg(args, "message")
		if conversationID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}
		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		if status.SendsViaOutbox() {
			return daemonSubmitTextAndWait(ctx, daemon, args, conversationID, message), nil
		}
		return daemonLegacySendText(ctx, daemon, args, conversationID, message), nil
	}
}

func daemonSendMessageHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		recipient := firstNonEmpty(strings.TrimSpace(strArg(args, "recipient")), strings.TrimSpace(strArg(args, "phone_number")))
		platform := normalizeDirectSendPlatform(strArg(args, "platform"))
		message := strArg(args, "message")
		if recipient == "" {
			return errorResult("recipient or phone_number is required"), nil
		}
		if message == "" {
			return errorResult("message is required"), nil
		}

		var conversationID string
		switch platform {
		case "whatsapp":
			id, _, _, _, err := canonicalWhatsAppDirectConversation(recipient)
			if err != nil {
				return errorResult(err.Error()), nil
			}
			conversationID = id
		case "signal":
			id, _, _, _, err := canonicalSignalDirectConversation(recipient)
			if err != nil {
				return errorResult(err.Error()), nil
			}
			conversationID = id
		case "sms":
			if !looksLikePhoneNumber(recipient) {
				return errorResult(fmt.Sprintf("SMS recipient must be a phone number with country code (e.g. +15551234567), got %q", recipient)), nil
			}
			conversation, err := findDirectSMSConversation(options.Reads, recipient)
			if err != nil {
				return errorResult(fmt.Sprintf("resolve SMS conversation: %v", err)), nil
			}
			if conversation == nil {
				return errorResult(fmt.Sprintf(
					"no existing SMS conversation with %s. In transportless client mode, starting a brand-new SMS thread requires the OpenMessage app (send the first message there); replies to existing threads work here — use resolve_contact_routes to find the conversation, then send_to_conversation.", recipient,
				)), nil
			}
			conversationID = conversation.ConversationID
		default:
			return errorResult(fmt.Sprintf("unsupported platform %q (supported: sms, whatsapp, signal)", platform)), nil
		}

		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		if status.SendsViaOutbox() {
			return daemonSubmitTextAndWait(ctx, daemon, args, conversationID, message), nil
		}
		return daemonLegacySendText(ctx, daemon, args, conversationID, message), nil
	}
}

func daemonSendMediaToConversationHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationID := strArg(args, "conversation_id")
		filePath := strArg(args, "file_path")
		if conversationID == "" {
			return errorResult("conversation_id is required"), nil
		}
		if filePath == "" {
			return errorResult("file_path is required"), nil
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return errorResult(fmt.Sprintf("stat file: %v", err)), nil
		}
		if info.IsDir() {
			return errorResult("file_path must point to a file"), nil
		}
		if info.Size() > maxMediaUploadBytes {
			return errorResult(fmt.Sprintf("file too large (%d bytes; limit %d MB)", info.Size(), maxMediaUploadBytes>>20)), nil
		}
		filename := filepath.Base(filePath)
		if filename == "." || filename == string(filepath.Separator) || filename == "" {
			return errorResult("file_path must point to a file"), nil
		}

		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		key, err := v2IdempotencyKey(args)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return errorResult(fmt.Sprintf("read file: %v", err)), nil
		}
		defer file.Close()
		sniff := make([]byte, 512)
		n, _ := file.Read(sniff)
		if _, err := file.Seek(0, 0); err != nil {
			return errorResult(fmt.Sprintf("read file: %v", err)), nil
		}
		submission := localapi.MediaSubmission{
			ConversationID: conversationID,
			Filename:       filename,
			MIME:           detectMediaMimeType(filename, sniff[:n], strArg(args, "mime_type")),
			Caption:        strArg(args, "caption"),
			ReplyToID:      strArg(args, "reply_to_id"),
			IdempotencyKey: key,
			Content:        file,
		}
		if status.SendsViaOutbox() {
			outboxSubmission, err := daemon.SubmitMedia(ctx, submission)
			if err != nil {
				if localapi.IsDeterministicRejection(err) {
					return errorResult(fmt.Sprintf("media send rejected by the app: %v", err)), nil
				}
				return daemonAmbiguousResult(key, err), nil
			}
			return daemonWaitForDelivery(ctx, daemon, outboxSubmission, key), nil
		}
		result, err := daemon.LegacySendMedia(ctx, submission)
		if err != nil {
			if localapi.IsDeterministicRejection(err) {
				return errorResult(fmt.Sprintf("media send rejected by the app: %v", err)), nil
			}
			if _, isResponse := localapi.AsResponseError(err); isResponse {
				return errorResult(fmt.Sprintf("the app could not send the media: %v", err)), nil
			}
			return daemonAmbiguousResult(key, err), nil
		}
		return structuredResult(map[string]any{
			"ok":              true,
			"message_id":      result.MessageID,
			"conversation_id": conversationID,
			"idempotency_key": key,
			"via":             "app",
		}, fmt.Sprintf("Media sent via the running OpenMessage app (message %s): %s", result.MessageID, filename)), nil
	}
}

func daemonSendGroupMessageHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return errorResult(
			"creating a group conversation requires the OpenMessage app's live Google Messages connection, which transportless client mode does not hold. Send the first group message from the app; replies to existing groups work here via send_to_conversation.",
		), nil
	}
}

func daemonReactToMessageHandler(options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		messageID := strArg(args, "message_id")
		emoji := strArg(args, "emoji")
		action := strings.ToLower(strings.TrimSpace(strArg(args, "action")))
		conversationID := strArg(args, "conversation_id")
		if messageID == "" {
			return errorResult("message_id is required"), nil
		}
		if emoji == "" {
			return errorResult("emoji is required"), nil
		}
		if action == "" {
			action = "add"
		}
		if err := daemon.React(ctx, conversationID, messageID, emoji, action); err != nil {
			if responseErr, ok := localapi.AsResponseError(err); ok {
				return errorResult(fmt.Sprintf("the app could not send the reaction: HTTP %d: %s", responseErr.StatusCode, responseErr.Body)), nil
			}
			return daemonDownResult(err), nil
		}
		return structuredResult(map[string]any{
			"ok":         true,
			"message_id": messageID,
			"emoji":      emoji,
			"action":     action,
			"via":        "app",
		}, fmt.Sprintf("Reaction %s (%s) sent via the running OpenMessage app.", emoji, action)), nil
	}
}

// findDirectSMSConversation scans recent conversations for a non-group
// SMS/RCS thread whose participants include the phone number. Matching is by
// digit suffix so +1 (650) 555-0100, 6505550100, and +16505550100 all meet.
func findDirectSMSConversation(reads readsource.ReadSource, phone string) (*db.Conversation, error) {
	if reads == nil {
		return nil, fmt.Errorf("no read source available")
	}
	digits := digitsOnly(phone)
	if digits == "" {
		return nil, fmt.Errorf("recipient %q has no digits", phone)
	}
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	conversations, err := reads.ListConversations(2000)
	if err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		if conversation == nil || conversation.IsGroup {
			continue
		}
		platform := normalizedPlatform(conversation.SourcePlatform)
		if platform != "sms" {
			continue
		}
		participantDigits := digitsOnly(conversation.Participants)
		if participantDigits == "" {
			continue
		}
		if strings.Contains(participantDigits, digits) {
			return conversation, nil
		}
	}
	return nil, nil
}

// daemonGetStatusHandler serves get_status in client mode: daemon truth when
// the app is running, an explicit "app not running" report otherwise.
func daemonGetStatusHandler(a *app.App, options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := daemon.RawStatus(ctx)
		if err != nil {
			var sb strings.Builder
			sb.WriteString("OpenMessage app: NOT RUNNING\n")
			sb.WriteString("This MCP server runs in transportless client mode: local reads (search, conversations, history) work from the store, but sends and live platform status require the app. Start the OpenMessage app to send.\n")
			if stats, statsErr := options.Reads.PlatformStats(); statsErr == nil {
				sb.WriteString("\nStored messages by platform:\n")
				for _, stat := range stats {
					latest := ""
					if stat.LatestMS > 0 {
						latest = " (latest " + time.UnixMilli(stat.LatestMS).Format(time.RFC3339) + ")"
					}
					fmt.Fprintf(&sb, "  %s: %d messages%s\n", stat.Platform, stat.Count, latest)
				}
			}
			fmt.Fprintf(&sb, "\nData dir: %s\n", a.DataDir)
			return structuredResult(map[string]any{
				"mcp_mode":         "client",
				"daemon_reachable": false,
				"data_dir":         a.DataDir,
				"probe_error":      err.Error(),
			}, sb.String()), nil
		}

		var sb strings.Builder
		sb.WriteString("OpenMessage app: RUNNING (status below is daemon truth from /api/status)\n")
		sb.WriteString("This MCP server runs in transportless client mode; the app owns all live connections and sends are routed through it.\n\n")
		appendPlatform := func(label, key string) {
			platform, ok := raw[key].(map[string]any)
			if !ok {
				return
			}
			connected, _ := platform["connected"].(bool)
			paired, _ := platform["paired"].(bool)
			fmt.Fprintf(&sb, "%s: connected=%v paired=%v", label, connected, paired)
			if lastError, ok := platform["last_error"].(string); ok && lastError != "" {
				fmt.Fprintf(&sb, " last_error=%q", lastError)
			}
			sb.WriteString("\n")
		}
		if connected, ok := raw["connected"].(bool); ok {
			fmt.Fprintf(&sb, "Overall connected: %v\n", connected)
		}
		appendPlatform("Google Messages", "google")
		appendPlatform("WhatsApp", "whatsapp")
		appendPlatform("Signal", "signal")
		if v2Primary, ok := raw["v2_primary"].(bool); ok {
			fmt.Fprintf(&sb, "App v2 mode: primary=%v send=%v\n", v2Primary, raw["v2_send"])
		}
		fmt.Fprintf(&sb, "Client data dir: %s\n", a.DataDir)
		return structuredResult(map[string]any{
			"mcp_mode":         "client",
			"daemon_reachable": true,
			"data_dir":         a.DataDir,
			"daemon":           raw,
		}, sb.String()), nil
	}
}
