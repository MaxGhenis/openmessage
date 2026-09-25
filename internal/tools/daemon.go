package tools

import (
	"context"
	"encoding/json"
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
	converted := messaging.Delivery{
		OutboxID:        delivery.OutboxID,
		AccountID:       delivery.AccountID,
		ConversationID:  delivery.ConversationID,
		State:           messaging.OutboxState(delivery.State),
		LocalMessageID:  delivery.LocalMessageID,
		RemoteMessageID: delivery.RemoteMessageID,
		ErrorClass:      delivery.ErrorClass,
		ErrorCode:       delivery.ErrorCode,
		Warning:         delivery.Warning,
	}
	if delivery.ExpiresAtMS > 0 {
		converted.ExpiresAt = time.UnixMilli(delivery.ExpiresAtMS)
	}
	return converted
}

// daemonSendPlatform resolves the send platform for a conversation routed at
// the daemon. Prefixed IDs are authoritative; otherwise the local read
// source's conversation row decides.
func daemonSendPlatform(reads readsource.ReadSource, conversationID string) string {
	switch {
	case strings.HasPrefix(conversationID, "whatsapp:"):
		return "whatsapp"
	case strings.HasPrefix(conversationID, "signal:"), strings.HasPrefix(conversationID, "signal-group:"):
		return "signal"
	}
	if reads != nil {
		if conversation, err := reads.GetConversation(conversationID); err == nil && conversation != nil {
			return normalizedPlatform(conversation.SourcePlatform)
		}
	}
	return ""
}

// daemonCheckPlatformSendable enforces the daemon's per-platform send
// capability before submitting. A daemon that predates the capability block
// (no "send" map) cannot be checked and passes through, as does a queueable
// outage (transient disconnect) — the durable outbox plus TTL handles those.
func daemonCheckPlatformSendable(status localapi.DaemonStatus, platform string) *mcp.CallToolResult {
	if platform == "" {
		return nil
	}
	capability, known := status.SendCapabilityFor(platform)
	if !known || capability.Available || capability.Queueable {
		return nil
	}
	return platformUnavailableResult(platform, capability.Reason)
}

// daemonRejectionResult renders a deterministic daemon refusal. A 404 on the
// outbox submit route means the daemon's serving store could not resolve the
// conversation — spelled out because a bare "HTTP 404: not found" reads like
// a transport bug and has sent agents down the wrong path (2026-08-05:
// WhatsApp sends 404ing while status showed the platform connected).
func daemonRejectionResult(err error, conversationID, platform string) *mcp.CallToolResult {
	if responseErr, ok := isDaemonDuplicateRejection(err); ok {
		return daemonDuplicateBlockedResult(responseErr)
	}
	if responseErr, ok := localapi.AsResponseError(err); ok && responseErr.StatusCode == 404 {
		platformNote := ""
		if platform != "" {
			platformNote = fmt.Sprintf(" The %s connection can be up for receiving while this send path has no usable conversation record.", platform)
		}
		return errorResult(fmt.Sprintf(
			"send rejected: the app could not resolve conversation %q in its serving store (HTTP 404). The message was NOT queued.%s Use resolve_contact_routes to find a sendable route for this contact, or send the first message from the app.",
			conversationID, platformNote,
		))
	}
	if responseErr, ok := localapi.AsResponseError(err); ok && responseErr.StatusCode == 501 {
		return platformUnavailableResult(firstNonEmpty(platform, "the requested platform"), responseErr.Body)
	}
	return errorResult(fmt.Sprintf("send rejected by the app: %v", err))
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
// and waits (bounded) for its outcome, mirroring the in-process v2 result
// contract so agents see identical semantics in both serve modes.
func daemonSubmitTextAndWait(
	ctx context.Context,
	options Options,
	args map[string]any,
	conversationID string,
	body string,
	platform string,
) *mcp.CallToolResult {
	daemon := options.Daemon
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
	submission := localapi.TextSubmission{
		ConversationID: conversationID,
		Body:           body,
		IdempotencyKey: key,
		Force:          force,
	}
	if ttl > 0 {
		ttlMS := ttl.Milliseconds()
		submission.TTLMS = &ttlMS
	}
	accepted, err := daemon.SubmitText(ctx, submission)
	if err != nil {
		if localapi.IsDeterministicRejection(err) {
			return daemonRejectionResult(err, conversationID, platform)
		}
		return daemonAmbiguousResult(key, err)
	}
	return daemonWaitForDelivery(ctx, options, accepted, key, platform, conversationID, wait)
}

// daemonAwaitOutcome polls the daemon until the send reaches a reportable
// outcome or the wait window closes. With WaitForTransmit it holds through
// auto-retrying not_dispatched states; otherwise those return immediately.
// The bool reports whether any state was ever observed.
func daemonAwaitOutcome(
	ctx context.Context,
	daemon *localapi.Client,
	outboxID string,
	wait sendWaitOptions,
) (localapi.Delivery, bool, error) {
	deadline := time.Now().Add(wait.Wait)
	var last localapi.Delivery
	var lastErr error
	observed := false
	for {
		delivery, err := daemon.Delivery(ctx, outboxID)
		if err == nil {
			observed = true
			last = delivery
			lastErr = nil
			state := messaging.OutboxState(delivery.State)
			if sendSettled(state) || state == messaging.OutboxUncertain {
				return last, true, nil
			}
			if state == messaging.OutboxNotDispatched && !wait.WaitForTransmit {
				return last, true, nil
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			if observed {
				return last, true, nil
			}
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return localapi.Delivery{}, false, lastErr
		}
		select {
		case <-ctx.Done():
			if observed {
				return last, true, nil
			}
			return localapi.Delivery{}, false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func daemonWaitForDelivery(
	ctx context.Context,
	options Options,
	submission localapi.Submission,
	idempotencyKey string,
	platform string,
	conversationID string,
	wait sendWaitOptions,
) *mcp.CallToolResult {
	delivery, observed, err := daemonAwaitOutcome(ctx, options.Daemon, submission.OutboxID, wait)
	if !observed {
		// The intent is durably queued on the daemon; only our view failed.
		lastKnown := messaging.Delivery{
			OutboxID:       submission.OutboxID,
			ConversationID: conversationID,
			State:          messaging.OutboxQueued,
			LocalMessageID: submission.LocalMessageID,
		}
		if submission.ExpiresAtMS > 0 {
			lastKnown.ExpiresAt = time.UnixMilli(submission.ExpiresAtMS)
		}
		outcome := sendOutcome{
			Delivery:       lastKnown,
			IdempotencyKey: idempotencyKey,
			Deduplicated:   submission.Deduplicated,
			Platform:       platform,
			ConversationID: conversationID,
		}
		payload := buildSendPayload(outcome)
		payload["wait_error"] = err.Error()
		text := fmt.Sprintf(
			"The send is durably queued on the app (outbox %s, state %s) and has NOT been confirmed as transmitted; this wait was interrupted (%v). The app keeps sending it in the background. Do NOT send this message again — check progress with list_outbox, or repeat the exact same send deliberately by reusing idempotency_key %s.",
			lastKnown.OutboxID, lastKnown.State, err, idempotencyKey,
		)
		return structuredResult(payload, text)
	}

	converted := deliveryFromLocalAPI(delivery)
	outcome := sendOutcome{
		Delivery:          converted,
		IdempotencyKey:    idempotencyKey,
		Deduplicated:      submission.Deduplicated,
		Platform:          firstNonEmpty(delivery.Platform, platform),
		ConversationID:    conversationID,
		WaitedForTransmit: wait.WaitForTransmit,
		WaitExpired:       wait.WaitForTransmit && !sendTransmitted(converted.State),
	}
	if sendTransmitted(converted.State) {
		outcome.Delivered = deliveryReceiptObserved(options.Reads, converted.RemoteMessageID)
	}
	return sendOutcomeResult(outcome)
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
		platform := daemonSendPlatform(options.Reads, conversationID)
		if requested := normalizeDirectSendPlatform(strArg(args, "platform")); strArg(args, "platform") != "" {
			if platform != "" && requested != platform {
				return platformMismatchResult(requested, platform, conversationID), nil
			}
			if platform == "" {
				platform = requested
			}
		}
		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		if failure := daemonCheckPlatformSendable(status, platform); failure != nil {
			return failure, nil
		}
		if status.SendsViaOutbox() {
			return daemonSubmitTextAndWait(ctx, options, args, conversationID, message, platform), nil
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
			return unsupportedSendPlatformResult(platform), nil
		}

		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		if failure := daemonCheckPlatformSendable(status, platform); failure != nil {
			return failure, nil
		}
		if status.SendsViaOutbox() {
			return daemonSubmitTextAndWait(ctx, options, args, conversationID, message, platform), nil
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

		platform := daemonSendPlatform(options.Reads, conversationID)
		status, failure := daemonStatusOrResult(ctx, daemon)
		if failure != nil {
			return failure, nil
		}
		if failure := daemonCheckPlatformSendable(status, platform); failure != nil {
			return failure, nil
		}
		key, err := v2IdempotencyKey(args)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		ttl, err := parseSendTTL(args)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		wait, err := parseSendWaitOptions(args)
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
		if ttl > 0 {
			ttlMS := ttl.Milliseconds()
			submission.TTLMS = &ttlMS
		}
		if status.SendsViaOutbox() {
			outboxSubmission, err := daemon.SubmitMedia(ctx, submission)
			if err != nil {
				if localapi.IsDeterministicRejection(err) {
					return daemonRejectionResult(err, conversationID, platform), nil
				}
				return daemonAmbiguousResult(key, err), nil
			}
			return daemonWaitForDelivery(ctx, options, outboxSubmission, key, platform, conversationID, wait), nil
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
// SMS/RCS thread with one participant whose number matches the phone.
// Matching compares each participant's number individually by digit suffix
// (so +1 (650) 555-0100, 6505550100, and +16505550100 all meet) — never a
// substring search over concatenated participant digits, which could match a
// sequence straddling two different numbers and route the send to the wrong
// person.
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
		if normalizedPlatform(conversation.SourcePlatform) != "sms" {
			continue
		}
		var participants []struct {
			Number string `json:"number"`
			Phone  string `json:"phone"`
		}
		if err := json.Unmarshal([]byte(conversation.Participants), &participants); err != nil {
			continue
		}
		for _, participant := range participants {
			if phoneDigitsMatch(digits, firstNonEmpty(participant.Number, participant.Phone)) {
				return conversation, nil
			}
		}
	}
	return nil, nil
}

// phoneDigitsMatch reports whether a stored participant number denotes the
// same line as the wanted digits (already trimmed to the last 10). One side
// may carry a country prefix the other lacks, so the shorter digit string
// must be a suffix of the longer; a 7-digit floor keeps short fragments from
// matching everything.
func phoneDigitsMatch(wantedDigits, participantNumber string) bool {
	participantDigits := digitsOnly(participantNumber)
	if len(participantDigits) < 7 || len(wantedDigits) < 7 {
		return false
	}
	return strings.HasSuffix(participantDigits, wantedDigits) ||
		strings.HasSuffix(wantedDigits, participantDigits)
}

// daemonGetStatusHandler serves get_status in client mode: daemon truth when
// the app is running, an explicit "app not running" report otherwise.
func daemonGetStatusHandler(a *app.App, options Options) server.ToolHandlerFunc {
	daemon := options.Daemon
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := daemon.RawStatus(ctx)
		if err != nil {
			// A ResponseError means something answered on the daemon port but
			// refused or garbled the status request — that is not the same as
			// "the app is closed", so say so.
			reachable := false
			if _, ok := localapi.AsResponseError(err); ok {
				reachable = true
			}
			var sb strings.Builder
			if reachable {
				fmt.Fprintf(&sb, "OpenMessage app: ANSWERING but status unavailable (%v)\n", err)
				sb.WriteString("Something is listening on the daemon port but did not return a usable status document. If this persists, check the app version and the control token (OPENMESSAGES_DATA_DIR must point at the app's data directory).\n")
			} else {
				sb.WriteString("OpenMessage app: NOT RUNNING\n")
			}
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
				"daemon_reachable": reachable,
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
		if send, ok := raw["send"].(map[string]any); ok {
			sb.WriteString("\nSend capability (daemon truth; \"connected\" above does NOT imply a platform can send):\n")
			for _, platform := range []string{"sms", "whatsapp", "signal"} {
				entry, ok := send[platform].(map[string]any)
				if !ok {
					continue
				}
				available, _ := entry["available"].(bool)
				queueable, _ := entry["queueable"].(bool)
				reason, _ := entry["reason"].(string)
				switch {
				case available:
					fmt.Fprintf(&sb, "  %s: available\n", platform)
				case queueable:
					fmt.Fprintf(&sb, "  %s: DEGRADED (sends queue, not transmit) — %s\n", platform, firstNonEmpty(reason, "reason unknown"))
				default:
					fmt.Fprintf(&sb, "  %s: UNAVAILABLE — %s\n", platform, firstNonEmpty(reason, "reason unknown"))
				}
			}
		}
		if v2Primary, ok := raw["v2_primary"].(bool); ok {
			fmt.Fprintf(&sb, "App v2 mode: primary=%v send=%v (v2_send is the send STACK flag, not per-platform capability — see the send capability block)\n", v2Primary, raw["v2_send"])
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
