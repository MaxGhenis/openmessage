package tools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func downloadMediaTool() mcp.Tool {
	return mcp.NewTool("download_media",
		mcp.WithDescription("Resolve a message attachment and return its local API URL (/api/media/<message-id>, or /api/v1/messages/<message-id>/attachments/<n> on a v2 install) and metadata. Accepts a message ID (its first attachment) or the media_id that get_messages and get_conversation report for one attachment (v2msg:<message-id>:<n>). On a v2 install a pending attachment is downloaded into the local store first, and one its platform can no longer deliver is reported as unavailable with the reason. Pass save=true, or an output_path, to also write the bytes to a private file under the export directory (OPENMESSAGES_EXPORT_DIR, default ~/Documents/OpenMessage) and get its path and SHA-256. Supports Google Messages, WhatsApp, and Signal."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("The message ID, or one attachment's media_id (v2msg:<message-id>:<n>)")),
		mcp.WithBoolean("save", mcp.Description("Also write the attachment to a private file under the export directory and return its path")),
		mcp.WithString("output_path", mcp.Description("Where to save the attachment, relative to the export directory; implies save")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
}

// mediaReference is a download_media argument split into the message and
// the attachment ordinal it names.
type mediaReference struct {
	MessageID string
	Ordinal   int64
	// MediaID reports a v2msg:<message>:<n> media_id, which only v2 reads
	// hand out.
	MediaID bool
}

func parseMediaReference(raw string) mediaReference {
	raw = strings.TrimSpace(raw)
	rest, ok := strings.CutPrefix(raw, "v2msg:")
	if !ok {
		return mediaReference{MessageID: raw}
	}
	cut := strings.LastIndexByte(rest, ':')
	if cut <= 0 {
		return mediaReference{MessageID: rest, MediaID: true}
	}
	ordinal, err := strconv.ParseInt(rest[cut+1:], 10, 64)
	if err != nil || ordinal < 0 {
		return mediaReference{MessageID: raw}
	}
	return mediaReference{MessageID: rest[:cut], Ordinal: ordinal, MediaID: true}
}

func downloadMediaHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		msgID := strings.TrimSpace(strArg(args, "message_id"))
		if msgID == "" {
			return errorResult("message_id is required"), nil
		}
		outputPath := strings.TrimSpace(strArg(args, "output_path"))
		save := outputPath != ""
		if raw, ok := args["save"].(bool); ok && raw {
			save = true
		}

		ref := parseMediaReference(msgID)
		if options.V2Primary || ref.MediaID {
			return downloadV2Media(ctx, options, ref, save, outputPath), nil
		}
		return downloadLegacyMedia(ctx, a, options, msgID, save, outputPath), nil
	}
}

// fetchedMedia is one attachment's bytes plus how the serving side
// describes them.
type fetchedMedia struct {
	data     []byte
	mime     string
	filename string
}

// downloadV2Media resolves a v2 attachment: through the running app in
// transportless client mode, otherwise through this process's media
// service. Either way a pending attachment is downloaded into the v2 store
// before it is described.
func downloadV2Media(
	ctx context.Context,
	options Options,
	ref mediaReference,
	save bool,
	outputPath string,
) *mcp.CallToolResult {
	var (
		fetched fetchedMedia
		failure *mcp.CallToolResult
	)
	switch {
	case options.Daemon != nil:
		fetched, failure = fetchV2MediaFromDaemon(ctx, options.Daemon, ref)
	case options.V2 != nil && options.V2.Media != nil && options.V2.V2Store != nil:
		fetched, failure = fetchV2MediaInProcess(ctx, options.V2, ref)
	default:
		return errorResult("v2 attachments are not available in this serve mode: run the MCP server in client mode against the OpenMessage app (serve --mcp-stdio)")
	}
	if failure != nil {
		return failure
	}
	mediaURL := "/api/v1/messages/" + url.PathEscape(ref.MessageID) + "/attachments/" + strconv.FormatInt(ref.Ordinal, 10)
	return describeFetchedMedia(ref, true, mediaURL, fetched, save, outputPath)
}

func fetchV2MediaFromDaemon(
	ctx context.Context,
	daemon *localapi.Client,
	ref mediaReference,
) (fetchedMedia, *mcp.CallToolResult) {
	attachment, err := daemon.MessageAttachment(ctx, ref.MessageID, ref.Ordinal, messaging.DefaultMaxMediaBytes)
	if err != nil {
		return fetchedMedia{}, daemonMediaFailure(err)
	}
	if attachment.SHA256 != "" {
		sum := sha256.Sum256(attachment.Data)
		if got := hex.EncodeToString(sum[:]); got != attachment.SHA256 {
			return fetchedMedia{}, errorResult(fmt.Sprintf(
				"the app served attachment bytes with SHA-256 %s, but reported %s", got, attachment.SHA256,
			))
		}
	}
	return fetchedMedia{data: attachment.Data, mime: attachment.MIME, filename: attachment.Filename}, nil
}

// daemonMediaFailure turns a failed daemon attachment request into the
// reason the app gave. The app's error text already says whether the
// attachment is unknown, gone for good, or only unreachable right now.
func daemonMediaFailure(err error) *mcp.CallToolResult {
	responseErr, ok := localapi.AsResponseError(err)
	if !ok {
		if localapi.IsConnectionRefused(err) {
			return daemonDownResult(err)
		}
		return errorResult(fmt.Sprintf("fetch attachment from the OpenMessage app: %v", err))
	}
	if localapi.IsAuthError(err) {
		return daemonProbeFailureResult(err)
	}
	detail := responseErrorDetail(responseErr)
	switch {
	case responseErr.StatusCode == 404 && detail == "not found":
		return errorResult("message or attachment not found")
	case responseErr.StatusCode == 404:
		return errorResult("attachment unavailable: " + detail)
	case responseErr.StatusCode == 503:
		return errorResult("attachment not downloadable right now (retry later): " + detail)
	default:
		return errorResult(fmt.Sprintf("fetch attachment from the OpenMessage app (HTTP %d): %s", responseErr.StatusCode, detail))
	}
}

func responseErrorDetail(responseErr *localapi.ResponseError) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(responseErr.Body), &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error)
	}
	return strings.TrimSpace(responseErr.Body)
}

func fetchV2MediaInProcess(
	ctx context.Context,
	v2 *V2Dependencies,
	ref mediaReference,
) (fetchedMedia, *mcp.CallToolResult) {
	messages, err := sqlite.NewMessageRepository(v2.V2Store, time.Now)
	if err != nil {
		return fetchedMedia{}, errorResult(fmt.Sprintf("open v2 messages: %v", err))
	}
	message, err := messages.GetMessage(ctx, ref.MessageID)
	if errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return fetchedMedia{}, errorResult("message or attachment not found")
	}
	if err != nil {
		return fetchedMedia{}, errorResult(fmt.Sprintf("get message: %v", err))
	}
	descriptor, err := v2.Media.Resolve(ctx, media.DownloadCommand{
		AccountID: message.AccountID,
		MessageID: message.MessageID,
		Ordinal:   ref.Ordinal,
	})
	if err != nil {
		switch {
		case errors.Is(err, media.ErrNotFound):
			return fetchedMedia{}, errorResult("message or attachment not found")
		case errors.Is(err, media.ErrGone):
			return fetchedMedia{}, errorResult("attachment unavailable: " + err.Error())
		default:
			return fetchedMedia{}, errorResult(fmt.Sprintf("resolve attachment: %v", err))
		}
	}
	reader, err := v2.Media.Open(descriptor.Ref)
	if err != nil {
		return fetchedMedia{}, errorResult(fmt.Sprintf("open attachment: %v", err))
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, messaging.DefaultMaxMediaBytes+1))
	if err != nil {
		return fetchedMedia{}, errorResult(fmt.Sprintf("read attachment: %v", err))
	}
	return fetchedMedia{data: data, mime: descriptor.MIME, filename: descriptor.Filename}, nil
}

// downloadLegacyMedia keeps the legacy-primary contract: describe the
// message's media and its /api/media URL without fetching it, unless the
// caller asked to save it and a running app can serve the bytes.
func downloadLegacyMedia(
	ctx context.Context,
	a *app.App,
	options Options,
	msgID string,
	save bool,
	outputPath string,
) *mcp.CallToolResult {
	msg, err := a.Store.GetMessageByID(msgID)
	if err != nil {
		return errorResult(fmt.Sprintf("get message: %v", err))
	}
	if msg == nil {
		return errorResult("message not found")
	}
	if msg.MediaID == "" {
		return errorResult("this message has no media attachment")
	}

	mimeType := strings.TrimSpace(msg.MimeType)
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "application/octet-stream"
	}
	ext := extensionForMime(mimeType)
	safeID := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(msgID)
	filename := fmt.Sprintf("openmessage-%s%s", safeID, ext)
	mediaURL := "/api/media/" + url.PathEscape(msgID)

	if save {
		if options.Daemon == nil {
			return errorResult("save needs the running OpenMessage app: fetch " + mediaURL + " from it instead")
		}
		attachment, err := options.Daemon.LegacyMedia(ctx, msgID, messaging.DefaultMaxMediaBytes)
		if err != nil {
			return daemonMediaFailure(err)
		}
		return describeFetchedMedia(
			mediaReference{MessageID: msgID},
			false,
			mediaURL,
			fetchedMedia{data: attachment.Data, mime: firstNonBlank(attachment.MIME, mimeType), filename: filename},
			true,
			outputPath,
		)
	}

	return structuredResult(map[string]any{
		"message_id": msgID,
		"url":        mediaURL,
		"filename":   filename,
		"mime_type":  mimeType,
		// Legacy message rows do not retain attachment size. The serving
		// endpoint resolves it lazily (including projected v2blob: refs), so
		// report the value as unknown instead of fetching or inventing bytes.
		"size_bytes": nil,
	}, fmt.Sprintf("Media is available at %s (%s, %s; size unknown)", mediaURL, filename, mimeType))
}

// describeFetchedMedia reports fetched bytes and, when asked, writes them to
// a private (0600) file under the export root.
func describeFetchedMedia(
	ref mediaReference,
	v2 bool,
	mediaURL string,
	fetched fetchedMedia,
	save bool,
	outputPath string,
) *mcp.CallToolResult {
	mimeType := firstNonBlank(fetched.mime, "application/octet-stream")
	filename := strings.TrimSpace(fetched.filename)
	if filename == "" {
		filename = "attachment" + extensionForMime(mimeType)
	}
	sum := sha256.Sum256(fetched.data)
	digest := hex.EncodeToString(sum[:])
	payload := map[string]any{
		"message_id": ref.MessageID,
		"url":        mediaURL,
		"filename":   filename,
		"mime_type":  mimeType,
		"size_bytes": len(fetched.data),
		"sha256":     digest,
	}
	if v2 {
		payload["ordinal"] = ref.Ordinal
		payload["media_id"] = fmt.Sprintf("v2msg:%s:%d", ref.MessageID, ref.Ordinal)
	}
	if !save {
		return structuredResult(payload, fmt.Sprintf(
			"Attachment %s (%s, %d bytes, sha256 %s) is available at %s; pass save=true to write it to a file",
			filename, mimeType, len(fetched.data), digest, mediaURL,
		))
	}
	if outputPath == "" {
		outputPath = filepath.Join("media", defaultMediaFileName(ref, filename))
	}
	path, err := resolveExportWritePath(outputPath)
	if err != nil {
		return errorResult(fmt.Sprintf("resolve output_path: %v", err))
	}
	if err := writePrivateExportFile(path, fetched.data); err != nil {
		return errorResult(fmt.Sprintf("save attachment: %v", err))
	}
	payload["path"] = path
	return structuredResult(payload, fmt.Sprintf(
		"Saved %s (%s, %d bytes, sha256 %s) to %s",
		filename, mimeType, len(fetched.data), digest, path,
	))
}

// defaultMediaFileName names a saved attachment after its message and
// ordinal, so two attachments never collide, keeping the served filename
// (already sanitized by the serving side) for its extension and legibility.
func defaultMediaFileName(ref mediaReference, filename string) string {
	safeID := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(ref.MessageID)
	name := strings.NewReplacer("/", "_", "\\", "_").Replace(filepath.Base(filename))
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	return fmt.Sprintf("%s-%d-%s", safeID, ref.Ordinal, name)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func extensionForMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "audio/ogg"):
		return ".ogg"
	case strings.HasPrefix(mime, "audio/aac"):
		return ".aac"
	case strings.HasPrefix(mime, "audio/mp4"), strings.HasPrefix(mime, "audio/m4a"):
		return ".m4a"
	case strings.HasPrefix(mime, "audio/mpeg"):
		return ".mp3"
	case strings.HasPrefix(mime, "audio/amr"):
		return ".amr"
	case strings.HasPrefix(mime, "audio/"):
		return ".audio"
	case strings.HasPrefix(mime, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mime, "image/png"):
		return ".png"
	case strings.HasPrefix(mime, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mime, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mime, "image/"):
		return ".img"
	case strings.HasPrefix(mime, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mime, "video/3gpp"):
		return ".3gp"
	case strings.HasPrefix(mime, "video/"):
		return ".video"
	default:
		return ".bin"
	}
}
