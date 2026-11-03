package tools

import (
	"context"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
)

// transcriptsUnavailableWhileV2Serves explains the refusal on a v2-primary
// install. Transcripts live only in the legacy store: v2 has no place for
// them, and the legacy->v2 migration counts and drops them. The legacy store
// still takes writes, but the only places that show a transcript, the read
// tools and the web UI's conversation view, read v2 there, so none would show
// one saved to legacy. Agents hold v2 message IDs, which the legacy store
// doesn't know.
const transcriptsUnavailableWhileV2Serves = "not available while v2 is the serving store: " +
	"get_messages and the other message reads come from the v2 store, which keeps no transcripts, " +
	"so none of them would show a transcript saved to the legacy store."

func setMessageTranscriptTool() mcp.Tool {
	return mcp.NewTool("set_message_transcript",
		mcp.WithDescription(
			"Save a transcript for an existing message. The original body and media metadata are preserved, and calling again overwrites the prior transcript. Unavailable while v2 is the serving store, which keeps no transcripts.",
		),
		mcp.WithString("message_id",
			mcp.Required(),
			mcp.Description("The message_id of the message to annotate with transcript text."),
		),
		mcp.WithString("transcript",
			mcp.Required(),
			mcp.Description("The transcribed text. Empty string clears any existing transcript."),
		),
		mcp.WithString("model",
			mcp.Description("Free-form model identifier (for example faster-whisper:base.en)."),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
	)
}

func setMessageTranscriptHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if options.V2Primary {
			return errorResult("set_message_transcript: " + transcriptsUnavailableWhileV2Serves), nil
		}
		args := req.GetArguments()
		messageID := strArg(args, "message_id")
		rawTranscript, ok := args["transcript"]
		if !ok {
			return errorResult("set_message_transcript: transcript is required"), nil
		}
		transcript, ok := rawTranscript.(string)
		if !ok {
			return errorResult("set_message_transcript: transcript must be a string"), nil
		}
		var model *string
		if _, ok := args["model"]; ok {
			modelValue, ok := args["model"].(string)
			if !ok {
				return errorResult("set_message_transcript: model must be a string"), nil
			}
			model = &modelValue
		}
		if messageID == "" {
			return errorResult("set_message_transcript: message_id is required"), nil
		}
		if err := db.ValidateMessageTranscript(transcript, model); err != nil {
			return errorResult(fmt.Sprintf("set_message_transcript: %v", err)), nil
		}
		// In client mode the running app saves the transcript, as it does
		// sends and reactions, so this process never writes the live store.
		if options.Daemon != nil {
			return daemonSetMessageTranscript(ctx, options.Daemon, messageID, transcript, model), nil
		}
		if err := a.Store.SetMessageTranscript(messageID, transcript, model); err != nil {
			return errorResult(fmt.Sprintf("set_message_transcript: %v", err)), nil
		}
		msg, err := a.Store.GetMessageByID(messageID)
		if err != nil {
			return errorResult(fmt.Sprintf("set_message_transcript: reload message: %v", err)), nil
		}
		if msg != nil && a.OnMessagesChange != nil {
			a.OnMessagesChange(msg.ConversationID)
		}
		storedModel := ""
		if msg != nil {
			storedModel = msg.TranscriptModel
		}
		return textResult(fmt.Sprintf(
			"Transcript saved for message %s (%d chars, model=%q).",
			messageID, utf8.RuneCountInString(transcript), storedModel,
		)), nil
	}
}

// daemonSetMessageTranscript saves a transcript through the running app's
// /api/transcript. A v2-primary app refuses it (HTTP 409) with its reason.
// A repeat save of the same transcript and model leaves the message as it
// was, so a request that failed mid-flight is safe to send again.
func daemonSetMessageTranscript(ctx context.Context, daemon *localapi.Client, messageID, transcript string, model *string) *mcp.CallToolResult {
	saved, err := daemon.SetTranscript(ctx, messageID, transcript, model)
	if err != nil {
		responseErr, ok := localapi.AsResponseError(err)
		switch {
		case !ok && localapi.IsConnectionRefused(err):
			return daemonDownResult(err)
		case !ok:
			return errorResult(fmt.Sprintf("set_message_transcript: the request to the OpenMessage app failed: %v. Saving the same transcript again is safe.", err))
		case localapi.IsAuthError(err):
			return daemonProbeFailureResult(err)
		case responseErr.StatusCode == http.StatusNotFound:
			return errorResult("set_message_transcript: message not found")
		case responseErr.StatusCode == http.StatusConflict:
			return errorResult("set_message_transcript: " + responseErrorDetail(responseErr))
		default:
			return errorResult(fmt.Sprintf(
				"set_message_transcript: the OpenMessage app could not save the transcript (HTTP %d): %s",
				responseErr.StatusCode, responseErrorDetail(responseErr),
			))
		}
	}
	chars := utf8.RuneCountInString(transcript)
	if saved.TranscriptModel == nil {
		return textResult(fmt.Sprintf(
			"Transcript saved for message %s via the running OpenMessage app (%d chars).",
			messageID, chars,
		))
	}
	return textResult(fmt.Sprintf(
		"Transcript saved for message %s via the running OpenMessage app (%d chars, model=%q).",
		messageID, chars, *saved.TranscriptModel,
	))
}
