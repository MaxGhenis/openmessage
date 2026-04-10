package web

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/story"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

//go:embed static/*
var staticFS embed.FS

// APIHandler creates the HTTP handler with JSON API routes and static file serving.
// The client may be nil (disconnected state).
// mcpHandler is an optional http.Handler for the MCP SSE endpoint (mounted at /mcp/).
// StartDeepBackfill can optionally launch a guarded background backfill triggered by POST /api/backfill.
// StatusChecker returns whether the backend is connected.
type StatusChecker func() bool

// UnpairFunc deletes the session and disconnects.
type UnpairFunc func() error

// APIOptions holds optional callbacks for the API handler.
type APIOptions struct {
	Client                func() *client.Client
	Events                *EventBroker
	EventHeartbeat        time.Duration
	IdentityName          string
	IsConnected           StatusChecker
	GoogleStatus          func() any
	ReconnectGoogle       func() error
	Unpair                UnpairFunc
	WhatsAppStatus        func() any
	ConnectWhatsApp       func() error
	UnpairWhatsApp        func() error
	SignalStatus          func() any
	ConnectSignal         func() error
	UnpairSignal          func() error
	LeaveWhatsAppGroup    func(conversationID string) error
	WhatsAppQRCode        func() (any, error)
	SignalQRCode          func() (any, error)
	WhatsAppAvatar        func(conversationID string) ([]byte, string, error)
	FetchLinkPreview      LinkPreviewFetcher
	SendWhatsAppText      func(conversationID, body, replyToID string) (*db.Message, error)
	SendWhatsAppReaction  func(conversationID, messageID, emoji, action string) error
	SendSignalText        func(conversationID, body, replyToID string) (*db.Message, error)
	SendSignalMedia       func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error)
	SendSignalReaction    func(conversationID, messageID, emoji, action string) error
	SendWhatsAppMedia     func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error)
	DownloadWhatsAppMedia func(msg *db.Message) ([]byte, string, error)
	DownloadSignalMedia   func(msg *db.Message) ([]byte, string, error)
	StartDeepBackfill     func() bool
	BackfillStatus        func() any         // returns a JSON-serializable backfill progress snapshot
	BackfillPhone         func(string) error // targeted backfill for a single phone number
}

type SearchResult struct {
	ConversationID string `json:"ConversationID"`
	Name           string `json:"Name"`
	IsGroup        bool   `json:"IsGroup"`
	LastMessageTS  int64  `json:"LastMessageTS"`
	UnreadCount    int    `json:"UnreadCount"`
	SourcePlatform string `json:"source_platform,omitempty"`
	Preview        string `json:"preview,omitempty"`
}

// APIHandler creates a handler with minimal options (used by tests).
func APIHandler(store *db.Store, cli *client.Client, logger zerolog.Logger, mcpHandler http.Handler, onDeepBackfill ...func()) http.Handler {
	var cb func() bool
	if len(onDeepBackfill) > 0 {
		cb = func() bool {
			onDeepBackfill[0]()
			return true
		}
	}
	return APIHandlerWithOptions(store, cli, logger, mcpHandler, APIOptions{
		StartDeepBackfill: cb,
	})
}

func APIHandlerWithOptions(store *db.Store, cli *client.Client, logger zerolog.Logger, mcpHandler http.Handler, opts APIOptions) http.Handler {
	mux := http.NewServeMux()
	diagnosticsStartedAt := time.Now()
	getClient := func() *client.Client {
		if opts.Client != nil {
			return opts.Client()
		}
		return cli
	}
	currentConnected := func() bool {
		connected := getClient() != nil
		if opts.IsConnected != nil {
			connected = opts.IsConnected()
		}
		return connected
	}
	publishConversations := func() {
		if opts.Events != nil {
			opts.Events.PublishConversations()
		}
	}
	publishDrafts := func(conversationID string) {
		if opts.Events != nil {
			opts.Events.PublishDrafts(conversationID)
		}
	}
	publishMessages := func(conversationID string) {
		if opts.Events != nil {
			opts.Events.PublishMessages(conversationID)
		}
	}
	publishStatus := func(connected bool) {
		if opts.Events != nil {
			opts.Events.PublishStatus(connected)
		}
	}
	statusPayload := func(connected bool) map[string]any {
		payload := map[string]any{
			"connected": connected,
		}
		if strings.TrimSpace(opts.IdentityName) != "" {
			payload["identity_name"] = strings.TrimSpace(opts.IdentityName)
		}
		if opts.GoogleStatus != nil {
			payload["google"] = opts.GoogleStatus()
		}
		if opts.WhatsAppStatus != nil {
			payload["whatsapp"] = opts.WhatsAppStatus()
		}
		if opts.SignalStatus != nil {
			payload["signal"] = opts.SignalStatus()
		}
		if opts.BackfillStatus != nil {
			payload["backfill"] = opts.BackfillStatus()
		}
		return payload
	}
	diagnosticsPayload := func() map[string]any {
		now := time.Now()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		payload := statusPayload(currentConnected())
		payload["schema_version"] = 1
		payload["generated_at"] = now.UnixMilli()
		payload["generated_at_iso"] = now.UTC().Format(time.RFC3339Nano)
		payload["backend"] = map[string]any{
			"go_version": runtime.Version(),
			"goos":       runtime.GOOS,
			"goarch":     runtime.GOARCH,
			"goroutines": runtime.NumGoroutine(),
			"uptime_ms":  time.Since(diagnosticsStartedAt).Milliseconds(),
		}
		payload["memory"] = map[string]any{
			"alloc_bytes":         mem.Alloc,
			"total_alloc_bytes":   mem.TotalAlloc,
			"sys_bytes":           mem.Sys,
			"heap_alloc_bytes":    mem.HeapAlloc,
			"heap_inuse_bytes":    mem.HeapInuse,
			"heap_idle_bytes":     mem.HeapIdle,
			"heap_released_bytes": mem.HeapReleased,
			"next_gc_bytes":       mem.NextGC,
			"last_gc_unix_ms":     int64(mem.LastGC / uint64(time.Millisecond)),
			"gc_count":            mem.NumGC,
		}
		payload["capabilities"] = map[string]any{
			"google": map[string]bool{
				"status":    opts.GoogleStatus != nil,
				"reconnect": opts.ReconnectGoogle != nil,
				"unpair":    opts.Unpair != nil,
			},
			"whatsapp": map[string]bool{
				"status":         opts.WhatsAppStatus != nil,
				"connect":        opts.ConnectWhatsApp != nil,
				"unpair":         opts.UnpairWhatsApp != nil,
				"qr":             opts.WhatsAppQRCode != nil,
				"avatar":         opts.WhatsAppAvatar != nil,
				"send_text":      opts.SendWhatsAppText != nil,
				"send_media":     opts.SendWhatsAppMedia != nil,
				"send_reaction":  opts.SendWhatsAppReaction != nil,
				"download_media": opts.DownloadWhatsAppMedia != nil,
				"leave_group":    opts.LeaveWhatsAppGroup != nil,
			},
			"signal": map[string]bool{
				"status":         opts.SignalStatus != nil,
				"connect":        opts.ConnectSignal != nil,
				"unpair":         opts.UnpairSignal != nil,
				"qr":             opts.SignalQRCode != nil,
				"send_text":      opts.SendSignalText != nil,
				"send_media":     opts.SendSignalMedia != nil,
				"send_reaction":  opts.SendSignalReaction != nil,
				"download_media": opts.DownloadSignalMedia != nil,
			},
			"backfill": map[string]bool{
				"status":   opts.BackfillStatus != nil,
				"deep":     opts.StartDeepBackfill != nil,
				"targeted": opts.BackfillPhone != nil,
			},
			"link_preview": map[string]bool{
				"fetch": opts.FetchLinkPreview != nil,
			},
			"events": map[string]bool{
				"sse":       opts.Events != nil,
				"heartbeat": opts.EventHeartbeat > 0,
			},
		}

		platforms := []string{"sms", "whatsapp", "imessage", "gchat", "signal", "telegram"}
		convCounts := map[string]int{}
		msgCounts := map[string]int{}
		latestByPlatform := map[string]int64{}

		totalConversations, err := store.ConversationCount("")
		if err == nil {
			payload["conversation_count"] = totalConversations
		}
		totalMessages, err := store.MessageCount("")
		if err == nil {
			payload["message_count"] = totalMessages
		}
		for _, platform := range platforms {
			if count, err := store.ConversationCount(platform); err == nil && count > 0 {
				convCounts[platform] = count
			}
			if count, err := store.MessageCount(platform); err == nil && count > 0 {
				msgCounts[platform] = count
			}
			if latest, err := store.LatestTimestamp(platform); err == nil && latest > 0 {
				latestByPlatform[platform] = latest
			}
		}
		payload["conversation_counts"] = convCounts
		payload["message_counts"] = msgCounts
		payload["latest_message_ts"] = latestByPlatform
		return payload
	}
	recordOutgoingMessage := func(message *db.Message, deleteDraftID string) error {
		if err := store.RecordOutgoingMessage(message, deleteDraftID); err != nil {
			logger.Error().
				Err(err).
				Str("conv_id", message.ConversationID).
				Str("msg_id", message.MessageID).
				Msg("Failed to persist outgoing message locally")
			return err
		}
		return nil
	}
	isWhatsAppConversation := func(conversationID string) bool {
		if strings.HasPrefix(conversationID, "whatsapp:") {
			return true
		}
		conv, err := store.GetConversation(conversationID)
		return err == nil && conv != nil && conv.SourcePlatform == "whatsapp"
	}
	isSignalConversation := func(conversationID string) bool {
		if strings.HasPrefix(conversationID, "signal:") || strings.HasPrefix(conversationID, "signal-group:") {
			return true
		}
		conv, err := store.GetConversation(conversationID)
		return err == nil && conv != nil && conv.SourcePlatform == "signal"
	}
	var (
		errWhatsAppTextUnavailable  = errors.New("WhatsApp sending is not available")
		errWhatsAppMediaUnavailable = errors.New("WhatsApp media sending is not available")
		errWhatsAppLocalStore       = errors.New("whatsapp local store update failed")
		errSignalTextUnavailable    = errors.New("Signal sending is not available")
		errSignalMediaUnavailable   = errors.New("Signal media sending is not available")
		errSignalLocalStore         = errors.New("signal local store update failed")
	)
	sendWhatsAppText := func(conversationID, body, replyToID, deleteDraftID string) (*db.Message, error) {
		if opts.SendWhatsAppText == nil {
			return nil, errWhatsAppTextUnavailable
		}
		msg, err := opts.SendWhatsAppText(conversationID, body, replyToID)
		if err != nil {
			return nil, err
		}
		if err := recordOutgoingMessage(msg, deleteDraftID); err != nil {
			return nil, fmt.Errorf("%w: %v", errWhatsAppLocalStore, err)
		}
		return msg, nil
	}
	sendWhatsAppMedia := func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
		if opts.SendWhatsAppMedia == nil {
			return nil, errWhatsAppMediaUnavailable
		}
		msg, err := opts.SendWhatsAppMedia(conversationID, data, filename, mime, caption, replyToID)
		if err != nil {
			return nil, err
		}
		if err := recordOutgoingMessage(msg, ""); err != nil {
			return nil, fmt.Errorf("%w: %v", errWhatsAppLocalStore, err)
		}
		return msg, nil
	}
	sendSignalText := func(conversationID, body, replyToID, deleteDraftID string) (*db.Message, error) {
		if opts.SendSignalText == nil {
			return nil, errSignalTextUnavailable
		}
		msg, err := opts.SendSignalText(conversationID, body, replyToID)
		if err != nil {
			return nil, err
		}
		if err := recordOutgoingMessage(msg, deleteDraftID); err != nil {
			return nil, fmt.Errorf("%w: %v", errSignalLocalStore, err)
		}
		return msg, nil
	}
	sendSignalMedia := func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
		if opts.SendSignalMedia == nil {
			return nil, errSignalMediaUnavailable
		}
		msg, err := opts.SendSignalMedia(conversationID, data, filename, mime, caption, replyToID)
		if err != nil {
			return nil, err
		}
		if err := recordOutgoingMessage(msg, ""); err != nil {
			return nil, fmt.Errorf("%w: %v", errSignalLocalStore, err)
		}
		return msg, nil
	}
	fetchLinkPreview := opts.FetchLinkPreview
	if fetchLinkPreview == nil {
		fetchLinkPreview = NewLinkPreviewService(logger).Fetch
	}

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if opts.Events == nil {
			httpError(w, "event stream not available", 404)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpError(w, "streaming not supported", 500)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		connected := currentConnected()
		if err := writeSSEEvent(w, StreamEvent{
			Type:      EventTypeStatus,
			Connected: &connected,
		}); err != nil {
			return
		}
		flusher.Flush()

		subID, ch := opts.Events.Subscribe()
		defer opts.Events.Unsubscribe(subID)

		heartbeat := opts.EventHeartbeat
		if heartbeat <= 0 {
			heartbeat = 25 * time.Second
		}
		keepalive := time.NewTicker(heartbeat)
		defer keepalive.Stop()

		for {
			select {
			case evt := <-ch:
				if err := writeSSEEvent(w, evt); err != nil {
					return
				}
				flusher.Flush()
			case <-keepalive.C:
				if err := writeSSEEvent(w, StreamEvent{
					Type:      EventTypeHeartbeat,
					Timestamp: time.Now().UnixMilli(),
				}); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit", 50)
		convos, err := store.ListConversations(limit)
		if err != nil {
			httpError(w, "list conversations: "+err.Error(), 500)
			return
		}
		if convos == nil {
			convos = []*db.Conversation{}
		}
		writeJSON(w, convos)
	})

	mux.HandleFunc("/api/conversations/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/conversations/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			httpError(w, "not found", 404)
			return
		}
		action := parts[len(parts)-1]
		convID := strings.Join(parts[:len(parts)-1], "/")
		if action == "notification-mode" {
			if r.Method != http.MethodPost && r.Method != http.MethodPatch {
				httpError(w, "method not allowed", 405)
				return
			}
			var req struct {
				NotificationMode string `json:"notification_mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				httpError(w, "invalid JSON: "+err.Error(), 400)
				return
			}
			if err := store.SetConversationNotificationMode(convID, req.NotificationMode); err != nil {
				httpError(w, "set notification mode: "+err.Error(), 400)
				return
			}
			convo, err := store.GetConversation(convID)
			if err != nil {
				httpError(w, "get conversation: "+err.Error(), 500)
				return
			}
			publishConversations()
			writeJSON(w, convo)
			return
		}
		if action != "messages" {
			httpError(w, "not found", 404)
			return
		}
		limit := queryInt(r, "limit", 100)
		beforeMS := queryInt64(r, "before", 0)
		afterMS := queryInt64(r, "after", 0)
		beforeID := r.URL.Query().Get("before_id")
		afterID := r.URL.Query().Get("after_id")
		switch {
		case beforeMS > 0 && afterMS > 0:
			httpError(w, "before and after cannot be used together", 400)
			return
		case beforeMS == 0 && beforeID != "":
			httpError(w, "before_id requires before", 400)
			return
		case afterMS == 0 && afterID != "":
			httpError(w, "after_id requires after", 400)
			return
		}
		var msgs []*db.Message
		var err error
		switch {
		case afterMS > 0:
			msgs, err = store.GetMessagesByConversationAfter(convID, afterMS, afterID, limit)
		case beforeMS > 0:
			msgs, err = store.GetMessagesByConversationBefore(convID, beforeMS, beforeID, limit)
		default:
			msgs, err = store.GetMessagesByConversation(convID, limit)
		}
		if err != nil {
			httpError(w, "get messages: "+err.Error(), 500)
			return
		}
		if msgs == nil {
			msgs = []*db.Message{}
		}
		writeJSON(w, msgs)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			httpError(w, "query parameter 'q' is required", 400)
			return
		}
		limit := queryInt(r, "limit", 50)
		msgs, err := store.SearchMessages(q, "", limit)
		if err != nil {
			httpError(w, "search: "+err.Error(), 500)
			return
		}
		convos, err := store.SearchConversationsByMetadata(q, limit)
		if err != nil {
			httpError(w, "search: "+err.Error(), 500)
			return
		}
		results := mergeSearchResults(store, msgs, convos, limit)
		writeJSON(w, results)
	})

	mux.HandleFunc("/api/link-preview", func(w http.ResponseWriter, r *http.Request) {
		rawURL := r.URL.Query().Get("url")
		if rawURL == "" {
			httpError(w, "query parameter 'url' is required", 400)
			return
		}
		preview, err := fetchLinkPreview(r.Context(), rawURL)
		if err != nil {
			switch {
			case errors.Is(err, ErrNoLinkPreview):
				writeJSON(w, map[string]any{})
				return
			case errors.Is(err, ErrInvalidLinkPreviewURL), errors.Is(err, ErrBlockedLinkPreviewURL):
				httpError(w, err.Error(), 400)
				return
			default:
				httpError(w, "link preview: "+err.Error(), 502)
				return
			}
		}
		if preview == nil {
			writeJSON(w, map[string]any{})
			return
		}
		writeJSON(w, preview)
	})

	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
			Message        string `json:"message"`
			ReplyToID      string `json:"reply_to_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.ConversationID == "" || req.Message == "" {
			httpError(w, "conversation_id and message are required", 400)
			return
		}
		if isWhatsAppConversation(req.ConversationID) {
			msg, err := sendWhatsAppText(req.ConversationID, req.Message, req.ReplyToID, "")
			switch {
			case errors.Is(err, errWhatsAppTextUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errWhatsAppLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(req.ConversationID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		if isSignalConversation(req.ConversationID) {
			msg, err := sendSignalText(req.ConversationID, req.Message, req.ReplyToID, "")
			switch {
			case errors.Is(err, errSignalTextUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errSignalLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(req.ConversationID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}
		// Fetch conversation to get SIM and participant info
		conv, err := cli.GM.GetConversation(req.ConversationID)
		if err != nil {
			httpError(w, "get conversation: "+err.Error(), 502)
			return
		}

		myParticipantID, simPayload := app.ExtractSIMAndParticipant(conv)

		payload := app.BuildSendPayload(req.ConversationID, req.Message, req.ReplyToID, myParticipantID, simPayload)

		logger.Info().
			Str("conv_id", req.ConversationID).
			Str("participant_id", myParticipantID).
			Bool("has_sim", simPayload != nil).
			Msg("Sending message")

		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			httpError(w, "send message: "+err.Error(), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if success {
			// Store sent message in DB immediately so UI shows it
			now := time.Now().UnixMilli()
			if err := recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: req.ConversationID,
				Body:           req.Message,
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_SENDING",
				ReplyToID:      req.ReplyToID,
			}, ""); err != nil {
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			}
			publishMessages(req.ConversationID)
			publishConversations()
		}
		writeJSON(w, map[string]any{
			"status":  resp.GetStatus().String(),
			"success": success,
		})
	})

	mux.HandleFunc("/api/send-media", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		// Parse multipart form (max 10MB)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			httpError(w, "invalid multipart form: "+err.Error(), 400)
			return
		}

		convID := r.FormValue("conversation_id")
		caption := strings.TrimSpace(r.FormValue("caption"))
		replyToID := strings.TrimSpace(r.FormValue("reply_to_id"))
		if convID == "" {
			httpError(w, "conversation_id is required", 400)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			httpError(w, "file is required: "+err.Error(), 400)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			httpError(w, "read file: "+err.Error(), 500)
			return
		}

		mime := header.Header.Get("Content-Type")
		if mime == "" {
			mime = "application/octet-stream"
		}
		if isSignalConversation(convID) {
			msg, err := sendSignalMedia(convID, data, header.Filename, mime, caption, replyToID)
			switch {
			case errors.Is(err, errSignalMediaUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errSignalLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(convID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		if isWhatsAppConversation(convID) {
			msg, err := sendWhatsAppMedia(convID, data, header.Filename, mime, caption, replyToID)
			switch {
			case errors.Is(err, errWhatsAppMediaUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errWhatsAppLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(convID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}

		// Upload media via libgm
		media, err := cli.GM.UploadMedia(data, header.Filename, mime)
		if err != nil {
			httpError(w, "upload media: "+err.Error(), 502)
			return
		}

		// Get SIM and participant info
		conv, err := cli.GM.GetConversation(convID)
		if err != nil {
			httpError(w, "get conversation: "+err.Error(), 502)
			return
		}

		myParticipantID, simPayload := app.ExtractSIMAndParticipant(conv)

		payload := app.BuildSendMediaPayload(convID, media, myParticipantID, simPayload)

		logger.Info().
			Str("conv_id", convID).
			Str("mime", mime).
			Str("filename", header.Filename).
			Int("size", len(data)).
			Msg("Sending media message")

		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			httpError(w, "send message: "+err.Error(), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if success {
			now := time.Now().UnixMilli()
			if err := recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: convID,
				Body:           "",
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_SENDING",
				MediaID:        media.MediaID,
				MimeType:       media.MimeType,
				DecryptionKey:  hex.EncodeToString(media.DecryptionKey),
			}, ""); err != nil {
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			}
			publishMessages(convID)
			publishConversations()
		}
		writeJSON(w, map[string]any{
			"status":  resp.GetStatus().String(),
			"success": success,
		})
	})

	mux.HandleFunc("/api/media/", func(w http.ResponseWriter, r *http.Request) {
		msgID := strings.TrimPrefix(r.URL.Path, "/api/media/")
		if msgID == "" {
			httpError(w, "message_id required", 400)
			return
		}
		msg, err := store.GetMessageByID(msgID)
		if err != nil {
			httpError(w, "get message: "+err.Error(), 500)
			return
		}
		if msg == nil || msg.MediaID == "" {
			httpError(w, "no media for this message", 404)
			return
		}
		if msg.SourcePlatform == "whatsapp" || strings.HasPrefix(msg.MessageID, "whatsapp:") || strings.HasPrefix(msg.MediaID, "wa:") {
			if opts.DownloadWhatsAppMedia == nil {
				httpError(w, "whatsapp media not available", 404)
				return
			}
			data, mimeType, err := opts.DownloadWhatsAppMedia(msg)
			if err != nil {
				httpError(w, err.Error(), 502)
				return
			}
			if mimeType == "" {
				mimeType = msg.MimeType
			}
			w.Header().Set("Content-Type", mimeType)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(data)
			return
		}
		if msg.SourcePlatform == "signal" || strings.HasPrefix(msg.MessageID, "signal:") || strings.HasPrefix(msg.MediaID, "signalatt:") {
			if opts.DownloadSignalMedia == nil {
				httpError(w, "signal media not available", 404)
				return
			}
			data, mimeType, err := opts.DownloadSignalMedia(msg)
			if err != nil {
				httpError(w, err.Error(), 502)
				return
			}
			if mimeType == "" {
				mimeType = msg.MimeType
			}
			w.Header().Set("Content-Type", mimeType)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(data)
			return
		}
		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}
		// Decode hex decryption key
		key, err := hex.DecodeString(msg.DecryptionKey)
		if err != nil {
			httpError(w, "invalid decryption key", 500)
			return
		}
		data, err := cli.GM.DownloadMedia(msg.MediaID, key)
		if err != nil {
			httpError(w, "download media: "+err.Error(), 502)
			return
		}
		w.Header().Set("Content-Type", msg.MimeType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	})

	mux.HandleFunc("/api/react", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
			MessageID      string `json:"message_id"`
			Emoji          string `json:"emoji"`
			Action         string `json:"action"` // "add", "remove", "switch"; default "add"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.MessageID == "" || req.Emoji == "" {
			httpError(w, "message_id and emoji are required", 400)
			return
		}
		if isWhatsAppConversation(req.ConversationID) {
			if opts.SendWhatsAppReaction == nil {
				httpError(w, "whatsapp reactions are not available", 501)
				return
			}
			if err := opts.SendWhatsAppReaction(req.ConversationID, req.MessageID, req.Emoji, req.Action); err != nil {
				httpError(w, "send reaction: "+err.Error(), 502)
				return
			}
			publishMessages(req.ConversationID)
			writeJSON(w, map[string]any{"success": true})
			return
		}
		if isSignalConversation(req.ConversationID) {
			if opts.SendSignalReaction == nil {
				httpError(w, "signal reactions are not available", 501)
				return
			}
			if err := opts.SendSignalReaction(req.ConversationID, req.MessageID, req.Emoji, req.Action); err != nil {
				httpError(w, "send reaction: "+err.Error(), 502)
				return
			}
			publishMessages(req.ConversationID)
			writeJSON(w, map[string]any{"success": true})
			return
		}

		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}

		// Get SIM payload from conversation
		var sim *gmproto.SIMPayload
		if req.ConversationID != "" {
			if conv, err := cli.GM.GetConversation(req.ConversationID); err == nil {
				_, sim = app.ExtractSIMAndParticipant(conv)
			}
		}

		payload := BuildReactionPayload(req.MessageID, req.Emoji, req.Action, sim)
		resp, err := cli.GM.SendReaction(payload)
		if err != nil {
			httpError(w, "send reaction: "+err.Error(), 502)
			return
		}
		writeJSON(w, map[string]any{
			"success": resp.GetSuccess(),
		})
	})

	mux.HandleFunc("/api/new-conversation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			PhoneNumber string `json:"phone_number"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.PhoneNumber == "" {
			httpError(w, "phone_number is required", 400)
			return
		}
		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}

		convResp, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
			Numbers: app.NewContactNumbers([]string{req.PhoneNumber}),
		})
		if err != nil {
			httpError(w, "failed to get/create conversation: "+err.Error(), 502)
			return
		}
		conv := convResp.GetConversation()
		if conv == nil {
			httpError(w, "no conversation returned", 502)
			return
		}

		convoID := conv.GetConversationID()
		name := req.PhoneNumber
		// Try to get a name from participants
		for _, p := range conv.GetParticipants() {
			if !p.GetIsMe() {
				if fn := p.GetFormattedNumber(); fn != "" {
					name = fn
				}
				if cn := p.GetFullName(); cn != "" {
					name = cn
				}
			}
		}

		// Upsert into local DB so it shows in the sidebar
		store.UpsertConversation(&db.Conversation{
			ConversationID: convoID,
			Name:           name,
			LastMessageTS:  time.Now().UnixMilli(),
		})
		publishConversations()

		writeJSON(w, map[string]any{
			"conversation_id": convoID,
			"name":            name,
		})
	})

	mux.HandleFunc("/api/mark-read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.ConversationID == "" {
			httpError(w, "conversation_id is required", 400)
			return
		}
		if err := store.MarkConversationRead(req.ConversationID); err != nil {
			httpError(w, "mark read: "+err.Error(), 500)
			return
		}
		publishConversations()
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/drafts", func(w http.ResponseWriter, r *http.Request) {
		conversationID := r.URL.Query().Get("conversation_id")
		if conversationID == "" {
			httpError(w, "conversation_id is required", 400)
			return
		}
		drafts, err := store.ListDrafts(conversationID)
		if err != nil {
			httpError(w, "list drafts: "+err.Error(), 500)
			return
		}
		if drafts == nil {
			drafts = []*db.Draft{}
		}
		writeJSON(w, drafts)
	})

	mux.HandleFunc("/api/drafts/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			DraftID string `json:"draft_id"`
			Body    string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.DraftID == "" || req.Body == "" {
			httpError(w, "draft_id and body are required", 400)
			return
		}

		// Look up the draft to get conversation_id
		draft, err := store.GetDraft(req.DraftID)
		if err != nil {
			httpError(w, "get draft: "+err.Error(), 500)
			return
		}
		if draft == nil {
			httpError(w, "draft not found", 404)
			return
		}
		if isWhatsAppConversation(draft.ConversationID) {
			msg, err := sendWhatsAppText(draft.ConversationID, req.Body, "", req.DraftID)
			switch {
			case errors.Is(err, errWhatsAppTextUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errWhatsAppLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(draft.ConversationID)
			publishDrafts(draft.ConversationID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		if isSignalConversation(draft.ConversationID) {
			msg, err := sendSignalText(draft.ConversationID, req.Body, "", req.DraftID)
			switch {
			case errors.Is(err, errSignalTextUnavailable):
				httpError(w, err.Error(), 501)
				return
			case errors.Is(err, errSignalLocalStore):
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			case err != nil:
				httpError(w, err.Error(), 502)
				return
			}
			publishMessages(draft.ConversationID)
			publishDrafts(draft.ConversationID)
			publishConversations()
			writeJSON(w, map[string]any{
				"message_id": msg.MessageID,
				"status":     "SUCCESS",
				"success":    true,
			})
			return
		}
		cli := getClient()
		if cli == nil {
			httpError(w, app.ErrNotConnected, 503)
			return
		}

		// Use the same send logic as /api/send
		conv, err := cli.GM.GetConversation(draft.ConversationID)
		if err != nil {
			httpError(w, "get conversation: "+err.Error(), 502)
			return
		}

		myParticipantID, simPayload := app.ExtractSIMAndParticipant(conv)

		payload := app.BuildSendPayload(draft.ConversationID, req.Body, "", myParticipantID, simPayload)

		logger.Info().
			Str("conv_id", draft.ConversationID).
			Str("draft_id", req.DraftID).
			Msg("Sending draft message")

		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			httpError(w, "send message: "+err.Error(), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if success {
			now := time.Now().UnixMilli()
			if err := recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: draft.ConversationID,
				Body:           req.Body,
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_SENDING",
			}, req.DraftID); err != nil {
				httpError(w, "message sent remotely but failed to update local store: "+err.Error(), 500)
				return
			}
			publishMessages(draft.ConversationID)
			publishDrafts(draft.ConversationID)
			publishConversations()
		}
		writeJSON(w, map[string]any{
			"status":  resp.GetStatus().String(),
			"success": success,
		})
	})

	mux.HandleFunc("/api/drafts/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			httpError(w, "method not allowed", 405)
			return
		}
		draftID := strings.TrimPrefix(r.URL.Path, "/api/drafts/")
		if draftID == "" {
			httpError(w, "draft_id required", 400)
			return
		}
		draft, err := store.GetDraft(draftID)
		if err != nil {
			httpError(w, "get draft: "+err.Error(), 500)
			return
		}
		if err := store.DeleteDraft(draftID); err != nil {
			httpError(w, "delete draft: "+err.Error(), 500)
			return
		}
		if draft != nil {
			publishDrafts(draft.ConversationID)
			publishMessages(draft.ConversationID)
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/stats/", func(w http.ResponseWriter, r *http.Request) {
		convID := strings.TrimPrefix(r.URL.Path, "/api/stats/")
		if convID == "" {
			httpError(w, "conversation_id required", 400)
			return
		}
		msgs, err := store.GetMessagesByConversation(convID, 100000)
		if err != nil {
			httpError(w, "get messages: "+err.Error(), 500)
			return
		}
		if len(msgs) == 0 {
			httpError(w, "no messages found", 404)
			return
		}
		stats := story.ComputeStats(msgs, nil)
		writeJSON(w, stats)
	})

	mux.HandleFunc("/api/story/", func(w http.ResponseWriter, r *http.Request) {
		convID := strings.TrimPrefix(r.URL.Path, "/api/story/")
		if convID == "" {
			httpError(w, "conversation_id required", 400)
			return
		}
		msgs, err := store.GetMessagesByConversation(convID, 100000)
		if err != nil {
			httpError(w, "get messages: "+err.Error(), 500)
			return
		}
		if len(msgs) == 0 {
			httpError(w, "no messages found", 404)
			return
		}
		apiKey := r.URL.Query().Get("api_key")
		style := r.URL.Query().Get("style")
		s, err := story.Generate(msgs, story.GenerateConfig{
			Style:             style,
			APIKey:            apiKey,
			MaxSampleMessages: 200,
		})
		if err != nil {
			httpError(w, "generate story: "+err.Error(), 500)
			return
		}
		writeJSON(w, s)
	})

	mux.HandleFunc("/api/backfill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.StartDeepBackfill != nil {
			if !opts.StartDeepBackfill() {
				httpError(w, "deep backfill already running", 409)
				return
			}
			writeJSON(w, map[string]string{"status": "started"})
		} else {
			httpError(w, "deep backfill not available", 501)
		}
	})

	mux.HandleFunc("/api/backfill/status", func(w http.ResponseWriter, r *http.Request) {
		if opts.BackfillStatus != nil {
			writeJSON(w, opts.BackfillStatus())
		} else {
			writeJSON(w, map[string]any{
				"running": false,
				"phase":   "idle",
			})
		}
	})

	mux.HandleFunc("/api/backfill/phone", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			PhoneNumber string `json:"phone_number"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.PhoneNumber == "" {
			httpError(w, "phone_number is required", 400)
			return
		}
		if opts.BackfillPhone == nil {
			httpError(w, "phone backfill not available", 501)
			return
		}
		if err := opts.BackfillPhone(req.PhoneNumber); err != nil {
			httpError(w, "backfill phone: "+err.Error(), 502)
			return
		}
		publishConversations()
		publishMessages("")
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, statusPayload(currentConnected()))
	})

	mux.HandleFunc("/api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, diagnosticsPayload())
	})

	mux.HandleFunc("/api/google/reconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.ReconnectGoogle == nil {
			httpError(w, "google reconnect unavailable", 501)
			return
		}
		if err := opts.ReconnectGoogle(); err != nil {
			httpError(w, "reconnect google messages: "+err.Error(), 502)
			return
		}
		publishStatus(currentConnected())
		writeJSON(w, statusPayload(currentConnected()))
	})

	mux.HandleFunc("/api/whatsapp/status", func(w http.ResponseWriter, r *http.Request) {
		if opts.WhatsAppStatus == nil {
			httpError(w, "whatsapp live bridge not available", 404)
			return
		}
		writeJSON(w, opts.WhatsAppStatus())
	})

	mux.HandleFunc("/api/signal/status", func(w http.ResponseWriter, r *http.Request) {
		if opts.SignalStatus == nil {
			httpError(w, "signal live bridge not available", 404)
			return
		}
		writeJSON(w, opts.SignalStatus())
	})

	mux.HandleFunc("/api/signal/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.ConnectSignal == nil || opts.SignalStatus == nil {
			httpError(w, "signal live bridge not available", 404)
			return
		}
		if err := opts.ConnectSignal(); err != nil {
			httpError(w, "connect signal: "+err.Error(), 502)
			return
		}
		publishStatus(currentConnected())
		writeJSON(w, opts.SignalStatus())
	})

	mux.HandleFunc("/api/signal/qr", func(w http.ResponseWriter, r *http.Request) {
		if opts.SignalQRCode == nil {
			httpError(w, "signal qr not available", 404)
			return
		}
		qrPayload, err := opts.SignalQRCode()
		if err != nil {
			httpError(w, err.Error(), 404)
			return
		}
		writeJSON(w, qrPayload)
	})

	mux.HandleFunc("/api/signal/unpair", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.UnpairSignal == nil || opts.SignalStatus == nil {
			httpError(w, "signal live bridge not available", 404)
			return
		}
		if err := opts.UnpairSignal(); err != nil {
			httpError(w, "unpair signal: "+err.Error(), 500)
			return
		}
		publishStatus(currentConnected())
		writeJSON(w, opts.SignalStatus())
	})

	mux.HandleFunc("/api/whatsapp/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.ConnectWhatsApp == nil || opts.WhatsAppStatus == nil {
			httpError(w, "whatsapp live bridge not available", 404)
			return
		}
		if err := opts.ConnectWhatsApp(); err != nil {
			httpError(w, "connect whatsapp: "+err.Error(), 502)
			return
		}
		publishStatus(currentConnected())
		writeJSON(w, opts.WhatsAppStatus())
	})

	mux.HandleFunc("/api/whatsapp/qr", func(w http.ResponseWriter, r *http.Request) {
		if opts.WhatsAppQRCode == nil {
			httpError(w, "whatsapp qr not available", 404)
			return
		}
		qrPayload, err := opts.WhatsAppQRCode()
		if err != nil {
			httpError(w, err.Error(), 404)
			return
		}
		writeJSON(w, qrPayload)
	})

	mux.HandleFunc("/api/whatsapp/avatar", func(w http.ResponseWriter, r *http.Request) {
		if opts.WhatsAppAvatar == nil {
			httpError(w, "whatsapp avatar unavailable", 404)
			return
		}
		conversationID := strings.TrimSpace(r.URL.Query().Get("conversation_id"))
		if conversationID == "" {
			httpError(w, "query parameter 'conversation_id' is required", 400)
			return
		}
		data, mime, err := opts.WhatsAppAvatar(conversationID)
		if err != nil {
			switch {
			case errors.Is(err, whatsapplive.ErrProfilePhotoNotFound):
				httpError(w, "whatsapp avatar not found", 404)
			case strings.Contains(strings.ToLower(err.Error()), "not connected"):
				httpError(w, err.Error(), 503)
			default:
				httpError(w, "whatsapp avatar: "+err.Error(), 500)
			}
			return
		}
		if len(data) == 0 {
			httpError(w, "whatsapp avatar not found", 404)
			return
		}
		if strings.TrimSpace(mime) == "" {
			mime = "image/jpeg"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=1800")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/whatsapp/unpair", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.UnpairWhatsApp == nil || opts.WhatsAppStatus == nil {
			httpError(w, "whatsapp live bridge not available", 404)
			return
		}
		if err := opts.UnpairWhatsApp(); err != nil {
			httpError(w, "unpair whatsapp: "+err.Error(), 500)
			return
		}
		publishStatus(currentConnected())
		writeJSON(w, opts.WhatsAppStatus())
	})

	mux.HandleFunc("/api/whatsapp/leave-group", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.LeaveWhatsAppGroup == nil {
			httpError(w, "whatsapp group leave is not available", 404)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		req.ConversationID = strings.TrimSpace(req.ConversationID)
		if req.ConversationID == "" {
			httpError(w, "conversation_id is required", 400)
			return
		}
		convo, err := store.GetConversation(req.ConversationID)
		if err != nil {
			httpError(w, "get conversation: "+err.Error(), 500)
			return
		}
		if convo == nil {
			httpError(w, "conversation not found", 404)
			return
		}
		if convo.SourcePlatform != "whatsapp" {
			httpError(w, "conversation is not a WhatsApp thread", 400)
			return
		}
		if !convo.IsGroup {
			httpError(w, "conversation is not a WhatsApp group", 400)
			return
		}
		if err := opts.LeaveWhatsAppGroup(req.ConversationID); err != nil {
			httpError(w, err.Error(), 502)
			return
		}
		publishMessages(req.ConversationID)
		publishDrafts(req.ConversationID)
		publishConversations()
		writeJSON(w, map[string]any{
			"success":         true,
			"conversation_id": req.ConversationID,
		})
	})

	mux.HandleFunc("/api/unpair", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.Unpair != nil {
			if err := opts.Unpair(); err != nil {
				httpError(w, "unpair: "+err.Error(), 500)
				return
			}
		}
		publishStatus(false)
		writeJSON(w, map[string]string{"status": "ok"})
	})

	// Serve embedded static files at root
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to create static sub-filesystem")
	}
	staticHandler := http.FileServer(http.FS(staticContent))
	mux.Handle("/", staticHandler)

	// Wrap the mux to intercept /mcp/ requests before the mux's catch-all
	if mcpHandler != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/mcp/") {
				mcpHandler.ServeHTTP(w, r)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}

	return mux
}

// BuildReactionPayload constructs a SendReactionRequest using gmproto.MakeReactionData
// for proper emoji type mapping, matching the mautrix bridge format.
func BuildReactionPayload(messageID, emoji, action string, sim *gmproto.SIMPayload) *gmproto.SendReactionRequest {
	var a gmproto.SendReactionRequest_Action
	switch strings.ToLower(action) {
	case "remove":
		a = gmproto.SendReactionRequest_REMOVE
	case "switch":
		a = gmproto.SendReactionRequest_SWITCH
	default:
		a = gmproto.SendReactionRequest_ADD
	}
	return &gmproto.SendReactionRequest{
		MessageID:    messageID,
		ReactionData: gmproto.MakeReactionData(emoji),
		Action:       a,
		SIMPayload:   sim,
	}
}

func mergeSearchResults(store *db.Store, msgs []*db.Message, convos []*db.Conversation, limit int) []SearchResult {
	results := make([]SearchResult, 0, limit)
	seen := make(map[string]struct{}, limit)

	appendResult := func(result SearchResult) {
		if _, ok := seen[result.ConversationID]; ok {
			return
		}
		seen[result.ConversationID] = struct{}{}
		results = append(results, result)
	}

	for _, msg := range msgs {
		conv, err := store.GetConversation(msg.ConversationID)
		if err != nil || conv == nil {
			continue
		}
		appendResult(SearchResult{
			ConversationID: conv.ConversationID,
			Name:           conv.Name,
			IsGroup:        conv.IsGroup,
			LastMessageTS:  msg.TimestampMS,
			UnreadCount:    conv.UnreadCount,
			SourcePlatform: conv.SourcePlatform,
			Preview:        searchPreviewForMessage(msg),
		})
	}

	for _, conv := range convos {
		if _, ok := seen[conv.ConversationID]; ok {
			continue
		}
		preview := ""
		msgs, err := store.GetMessagesByConversation(conv.ConversationID, 1)
		if err == nil && len(msgs) > 0 {
			preview = searchPreviewForMessage(msgs[0])
		}
		appendResult(SearchResult{
			ConversationID: conv.ConversationID,
			Name:           conv.Name,
			IsGroup:        conv.IsGroup,
			LastMessageTS:  conv.LastMessageTS,
			UnreadCount:    conv.UnreadCount,
			SourcePlatform: conv.SourcePlatform,
			Preview:        preview,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].LastMessageTS != results[j].LastMessageTS {
			return results[i].LastMessageTS > results[j].LastMessageTS
		}
		return results[i].ConversationID < results[j].ConversationID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func searchPreviewForMessage(msg *db.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Body != "" {
		return msg.Body
	}
	if msg.MediaID != "" || msg.MimeType != "" {
		return "[Media]"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeSSEEvent(w http.ResponseWriter, evt StreamEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+evt.Type+"\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return n
}

func queryInt64(r *http.Request, key string, defaultVal int64) int64 {
	s := r.URL.Query().Get(key)
	if s == "" {
		return defaultVal
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultVal
	}
	return n
}
