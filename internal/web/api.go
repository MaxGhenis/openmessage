package web

import (
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// maxUploadBytes bounds /api/send-media request bodies. 128MB sits above
// every platform's attachment cap (Signal ~100MB, WhatsApp ~64MB, MMS ~1MB)
// while keeping a runaway POST from exhausting memory.
const maxUploadBytes = 128 << 20

const maxTranscriptRequestBytes = db.MaxTranscriptBytes + db.MaxTranscriptModelBytes + 4096

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
	RecordGoogleSend      func(success bool) // tracks Google send outcomes for stuck-session detection
	RecordGoogleSendError func(error)        // tracks auth/dead-session send errors for needs_repair
	GooglePhoneResponding func() bool
	MarkGoogleAuthExpired func(error) bool
	ReconnectGoogle       func() error
	Unpair                UnpairFunc
	WhatsAppStatus        func() any
	ConnectWhatsApp       func() error
	UnpairWhatsApp        func() error
	SignalStatus          func() any
	ConnectSignal         func() error
	ReplaySignalRecovery  func() error
	UnpairSignal          func() error
	LeaveWhatsAppGroup    func(conversationID string) error
	WhatsAppQRCode        func() (any, error)
	SignalQRCode          func() (any, error)
	WhatsAppAvatar        func(conversationID string) ([]byte, string, error)
	FetchLinkPreview      LinkPreviewFetcher
	FetchLinkPreviewImage LinkPreviewImageFetcher
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
	SyncGoogleContacts    func() (int, error)
}

type SearchResult struct {
	ConversationID  string `json:"ConversationID"`
	Name            string `json:"Name"`
	IsGroup         bool   `json:"IsGroup"`
	Participants    string `json:"Participants,omitempty"`
	LastMessageTS   int64  `json:"LastMessageTS"`
	UnreadCount     int    `json:"UnreadCount"`
	SourcePlatform  string `json:"source_platform,omitempty"`
	DisplayProtocol string `json:"display_protocol,omitempty"`
	IsFavorite      bool   `json:"is_favorite,omitempty"`
	UnifiedID       string `json:"unified_id,omitempty"`
	UnifiedName     string `json:"unified_name,omitempty"`
	Preview         string `json:"preview,omitempty"`
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
	recordGoogleSend := func(success bool) {
		if opts.RecordGoogleSend != nil {
			opts.RecordGoogleSend(success)
		}
	}
	recordGoogleSendError := func(err error) {
		if opts.RecordGoogleSendError != nil {
			opts.RecordGoogleSendError(err)
		}
	}
	googlePhoneResponding := func() bool {
		if opts.GooglePhoneResponding == nil {
			return true
		}
		return opts.GooglePhoneResponding()
	}
	markGoogleAuthExpired := func(err error) bool {
		if opts.MarkGoogleAuthExpired == nil {
			return false
		}
		marked := opts.MarkGoogleAuthExpired(err)
		if marked {
			publishStatus(currentConnected())
		}
		return marked
	}
	// Per-platform data-freshness, used to catch "zombie" bridges that report
	// connected=true while no longer actually syncing (the connection flag
	// lies; the data doesn't). Computing this scans the messages table, and
	// /api/status is polled every few seconds, so cache it — staleness is a
	// multi-day signal, so a 30s cache is plenty fresh.
	var (
		freshnessMu       sync.Mutex
		freshnessComputed time.Time
		freshnessValue    map[string]any
	)
	computeFreshness := func() map[string]any {
		freshnessMu.Lock()
		defer freshnessMu.Unlock()
		if freshnessValue != nil && time.Since(freshnessComputed) < 30*time.Second {
			return freshnessValue
		}
		stats, err := store.PlatformStats()
		if err != nil {
			return freshnessValue // keep last good value on error
		}
		var newest int64
		for _, st := range stats {
			if st.LatestMS > newest {
				newest = st.LatestMS
			}
		}
		// Map storage platform → status-block key (Google Messages stores SMS/RCS).
		keyFor := map[string]string{"sms": "google", "rcs": "google", "whatsapp": "whatsapp", "signal": "signal"}
		out := map[string]any{"newest_ms": newest}
		for _, st := range stats {
			key := keyFor[st.Platform]
			if key == "" {
				continue
			}
			behind := daysBehind(st.LatestMS, newest)
			entry := map[string]any{
				"latest_ms":          st.LatestMS,
				"latest_received_ms": st.LatestRecvMS,
				"behind_days":        behind,
				"stale":              st.LatestMS > 0 && behind >= staleDaysThreshold,
			}
			// sms + rcs both map to "google"; keep the freshest.
			if existing, ok := out[key].(map[string]any); ok {
				if st.LatestMS > existing["latest_ms"].(int64) {
					out[key] = entry
				}
			} else {
				out[key] = entry
			}
		}
		freshnessValue = out
		freshnessComputed = time.Now()
		return out
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
		if f := computeFreshness(); f != nil {
			payload["freshness"] = f
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
	linkPreviewService := NewLinkPreviewService(logger)
	fetchLinkPreview := opts.FetchLinkPreview
	if fetchLinkPreview == nil {
		fetchLinkPreview = linkPreviewService.Fetch
	}
	fetchLinkPreviewImage := opts.FetchLinkPreviewImage
	if fetchLinkPreviewImage == nil {
		fetchLinkPreviewImage = linkPreviewService.FetchImage
	}
	sendMediaBytes := func(w http.ResponseWriter, convID string, data []byte, filename, mimeType, caption, replyToID string) {
		if isSignalConversation(convID) {
			msg, err := sendSignalMedia(convID, data, filename, mimeType, caption, replyToID)
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
			msg, err := sendWhatsAppMedia(convID, data, filename, mimeType, caption, replyToID)
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

		media, err := cli.GM.UploadMedia(data, filename, mimeType)
		if err != nil {
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("upload media", err), 502)
			return
		}

		conv, err := cli.GM.GetConversation(convID)
		if err != nil {
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("get conversation", err), 502)
			return
		}

		myParticipantID, simPayload := app.ExtractSIMAndParticipant(conv)
		payload := app.BuildSendMediaPayload(convID, media, myParticipantID, simPayload)

		logger.Info().
			Str("conv_id", convID).
			Str("mime", mimeType).
			Str("filename", filename).
			Int("size", len(data)).
			Msg("Sending media message")

		resp, err := cli.GM.SendMessage(payload)
		if err != nil {
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("send message", err), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if !success {
			now := time.Now().UnixMilli()
			_ = recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: convID,
				Body:           "",
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_FAILED:" + resp.GetStatus().String(),
				MediaID:        media.MediaID,
				MimeType:       media.MimeType,
				DecryptionKey:  hex.EncodeToString(media.DecryptionKey),
			}, "")
			publishMessages(convID)
			publishConversations()
			if googlePhoneResponding() {
				recordGoogleSend(false)
			}
			httpError(w, googleSendRejectedMessage(resp.GetStatus().String(), googlePhoneResponding()), 502)
			return
		}
		recordGoogleSend(true)
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
		writeJSON(w, map[string]any{
			"status":  resp.GetStatus().String(),
			"success": success,
		})
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
		limit := queryIntClamped(r, "limit", 50, 500)
		convos, err := store.ListConversations(limit)
		if err != nil {
			httpError(w, "list conversations: "+err.Error(), 500)
			return
		}
		if convos == nil {
			convos = []*db.Conversation{}
		}
		enrichUnifiedConversationIdentities(store, convos)
		if err := enrichConversationPreviews(store, convos); err != nil {
			httpError(w, "conversation previews: "+err.Error(), 500)
			return
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
		if action == "favorite" {
			if r.Method != http.MethodPost && r.Method != http.MethodPatch {
				httpError(w, "method not allowed", 405)
				return
			}
			var req struct {
				Favorite *bool `json:"favorite"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				httpError(w, "invalid JSON: "+err.Error(), 400)
				return
			}
			if req.Favorite == nil {
				httpError(w, "favorite is required", 400)
				return
			}
			if err := store.SetConversationFavorite(convID, *req.Favorite); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					httpError(w, "conversation not found", 404)
					return
				}
				httpError(w, "set favorite: "+err.Error(), 500)
				return
			}
			convo, err := store.GetConversation(convID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					httpError(w, "conversation not found", 404)
					return
				}
				httpError(w, "get conversation: "+err.Error(), 500)
				return
			}
			publishConversations()
			writeJSON(w, convo)
			return
		}
		if action == "tab" {
			if r.Method != http.MethodPost && r.Method != http.MethodPatch {
				httpError(w, "method not allowed", 405)
				return
			}
			var req struct {
				Tab string `json:"tab"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				httpError(w, "invalid JSON: "+err.Error(), 400)
				return
			}
			if err := store.SetConversationTab(convID, req.Tab); err != nil {
				httpError(w, "set tab: "+err.Error(), 400)
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
		if action == "search" {
			if r.Method != http.MethodGet {
				httpError(w, "method not allowed", 405)
				return
			}
			q := strings.TrimSpace(r.URL.Query().Get("q"))
			if q == "" {
				httpError(w, "query parameter 'q' is required", 400)
				return
			}
			limit := queryIntClamped(r, "limit", 50, 100)
			msgs, err := store.SearchMessagesFiltered(q, db.SearchFilter{
				ConversationID: convID,
				Limit:          limit,
			})
			if err != nil {
				httpError(w, "search: "+err.Error(), 500)
				return
			}
			if msgs == nil {
				msgs = []*db.Message{}
			}
			writeJSON(w, msgs)
			return
		}
		if action == "messages-around" {
			if r.Method != http.MethodGet {
				httpError(w, "method not allowed", 405)
				return
			}
			messageID := strings.TrimSpace(r.URL.Query().Get("message_id"))
			if messageID == "" {
				httpError(w, "message_id is required", 400)
				return
			}
			msgs, err := store.GetMessagesAroundMessage(convID, messageID, queryIntClamped(r, "before", 40, 200), queryIntClamped(r, "after", 40, 200))
			if err != nil {
				if errors.Is(err, db.ErrMessageNotFound) {
					httpError(w, "message not found", 404)
					return
				}
				httpError(w, "get messages around: "+err.Error(), 500)
				return
			}
			if msgs == nil {
				msgs = []*db.Message{}
			}
			writeJSON(w, msgs)
			return
		}
		if action != "messages" {
			httpError(w, "not found", 404)
			return
		}
		limit := queryIntClamped(r, "limit", 100, 1000)
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

	// Bulk-move multiple conversations into a tab ("" = Recent, "archive", or a custom tab id).
	mux.HandleFunc("/api/conversations/move", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			IDs []string `json:"ids"`
			Tab string   `json:"tab"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if len(req.IDs) == 0 {
			httpError(w, "ids is required", 400)
			return
		}
		if err := store.SetConversationsTab(req.IDs, req.Tab); err != nil {
			httpError(w, "move conversations: "+err.Error(), 400)
			return
		}
		publishConversations()
		writeJSON(w, map[string]any{"moved": len(req.IDs), "tab": strings.TrimSpace(req.Tab)})
	})

	// List (GET) or create (POST) custom tabs.
	mux.HandleFunc("/api/tabs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			tabs, err := store.ListTabs()
			if err != nil {
				httpError(w, "list tabs: "+err.Error(), 500)
				return
			}
			if tabs == nil {
				tabs = []*db.Tab{}
			}
			writeJSON(w, tabs)
		case http.MethodPost:
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				httpError(w, "invalid JSON: "+err.Error(), 400)
				return
			}
			tab, err := store.CreateTab(req.Name)
			if err != nil {
				httpError(w, "create tab: "+err.Error(), 400)
				return
			}
			publishConversations()
			writeJSON(w, tab)
		default:
			httpError(w, "method not allowed", 405)
		}
	})

	// Rename (POST) or delete (DELETE) a custom tab by id.
	mux.HandleFunc("/api/tabs/", func(w http.ResponseWriter, r *http.Request) {
		tabID := strings.TrimPrefix(r.URL.Path, "/api/tabs/")
		if tabID == "" {
			httpError(w, "tab id required", 400)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPatch:
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				httpError(w, "invalid JSON: "+err.Error(), 400)
				return
			}
			if err := store.RenameTab(tabID, req.Name); err != nil {
				httpError(w, "rename tab: "+err.Error(), 400)
				return
			}
			publishConversations()
			writeJSON(w, map[string]any{"tab_id": tabID, "name": strings.TrimSpace(req.Name)})
		case http.MethodDelete:
			if err := store.DeleteTab(tabID); err != nil {
				httpError(w, "delete tab: "+err.Error(), 400)
				return
			}
			publishConversations()
			writeJSON(w, map[string]any{"deleted": tabID})
		default:
			httpError(w, "method not allowed", 405)
		}
	})

	mux.HandleFunc("/api/contacts", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		limit := queryInt(r, "limit", 20)
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		// First try the explicit contacts table.
		contacts, err := store.ListContacts(q, limit)
		if err != nil {
			httpError(w, "contacts: "+err.Error(), 500)
			return
		}
		// Always merge in participants we've messaged before — these are the
		// people the user actually wants to autocomplete by name. The contacts
		// table is mostly empty for most users since the macOS Contacts.app
		// integration is avatar-only.
		seen := map[string]bool{}
		for _, c := range contacts {
			seen[normalizeContactKey(c.Name, c.Number)] = true
		}
		fromConvos, err := store.ListContactsFromConversations(q, limit*4)
		if err == nil {
			for _, c := range fromConvos {
				key := normalizeContactKey(c.Name, c.Number)
				if seen[key] {
					continue
				}
				seen[key] = true
				contacts = append(contacts, c)
				if len(contacts) >= limit {
					break
				}
			}
		}
		// Always return [] not null so the JS side can iterate without checks.
		if contacts == nil {
			contacts = []*db.Contact{}
		}
		writeJSON(w, contacts)
	})

	mux.HandleFunc("/api/contacts/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.SyncGoogleContacts == nil {
			httpError(w, "google contact sync unavailable", 501)
			return
		}
		count, err := opts.SyncGoogleContacts()
		if err != nil {
			httpError(w, "sync contacts: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"synced": count})
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			httpError(w, "query parameter 'q' is required", 400)
			return
		}
		limit := queryIntClamped(r, "limit", 50, 500)
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

	mux.HandleFunc("/api/avatar", func(w http.ResponseWriter, r *http.Request) {
		source := strings.TrimSpace(r.URL.Query().Get("source"))
		if source == "" {
			source = "sms"
		}
		avatar, err := store.GetContactAvatar(
			source,
			r.URL.Query().Get("participant_id"),
			r.URL.Query().Get("contact_id"),
			r.URL.Query().Get("phone"),
		)
		if err != nil {
			httpError(w, "get avatar: "+err.Error(), 500)
			return
		}
		if avatar == nil || len(avatar.ImageData) == 0 || avatar.ImageHash == "" {
			httpError(w, "avatar not found", 404)
			return
		}
		etag := `"` + avatar.ImageHash + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		mimeType := strings.TrimSpace(avatar.MimeType)
		if mimeType == "" {
			mimeType = http.DetectContentType(avatar.ImageData)
		}
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("ETag", etag)
		w.Write(avatar.ImageData)
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
		if strings.TrimSpace(preview.ImageURL) != "" {
			preview = cloneLinkPreview(preview)
			preview.ImageURL = "/api/link-preview-image?url=" + url.QueryEscape(preview.ImageURL)
		}
		writeJSON(w, preview)
	})

	mux.HandleFunc("/api/link-preview-image", func(w http.ResponseWriter, r *http.Request) {
		rawURL := r.URL.Query().Get("url")
		if rawURL == "" {
			httpError(w, "query parameter 'url' is required", 400)
			return
		}
		data, contentType, err := fetchLinkPreviewImage(r.Context(), rawURL)
		if err != nil {
			switch {
			case errors.Is(err, ErrNoLinkPreview):
				httpError(w, "link preview image unavailable", 404)
				return
			case errors.Is(err, ErrInvalidLinkPreviewURL), errors.Is(err, ErrBlockedLinkPreviewURL):
				httpError(w, err.Error(), 400)
				return
			default:
				httpError(w, "link preview image: "+err.Error(), 502)
				return
			}
		}
		if strings.TrimSpace(contentType) == "" {
			contentType = http.DetectContentType(data)
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
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
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("get conversation", err), 502)
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
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("send message", err), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if !success {
			// Google Messages returned a non-SUCCESS status (UNKNOWN,
			// FAILURE_2/3/4 — typically "not default SMS app" or RCS/SMS
			// delivery failure). Persist a FAILED placeholder row so the
			// user can see the attempt, and surface a clear HTTP error
			// to the UI instead of silently swallowing the failure.
			now := time.Now().UnixMilli()
			_ = recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: req.ConversationID,
				Body:           req.Message,
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_FAILED:" + resp.GetStatus().String(),
				ReplyToID:      req.ReplyToID,
			}, "")
			publishMessages(req.ConversationID)
			publishConversations()
			if googlePhoneResponding() {
				recordGoogleSend(false)
			}
			httpError(w, googleSendRejectedMessage(resp.GetStatus().String(), googlePhoneResponding()), 502)
			return
		}
		recordGoogleSend(true)
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
		writeJSON(w, map[string]any{
			"status":  resp.GetStatus().String(),
			"success": success,
		})
	})

	mux.HandleFunc("/api/gifs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			httpError(w, "method not allowed", 405)
			return
		}
		limit := queryIntClamped(r, "limit", defaultGIFSearchLimit, maxGIFSearchResults)
		page := queryIntClamped(r, "page", 1, 1000)
		results, err := searchKlipyGIFs(r.Context(), r.URL.Query().Get("q"), limit, page)
		if err != nil {
			if errors.Is(err, errGIFProviderNotConfigured) {
				httpError(w, err.Error(), 501)
				return
			}
			httpError(w, err.Error(), 502)
			return
		}
		for i := range results {
			results[i].PreviewURL = proxyGIFPreviewURL(results[i].PreviewURL)
		}
		writeJSON(w, map[string]any{
			"results":  results,
			"page":     page,
			"has_more": len(results) >= limit,
		})
	})

	mux.HandleFunc("/api/gifs/trending", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			httpError(w, "method not allowed", 405)
			return
		}
		limit := queryIntClamped(r, "limit", defaultGIFSearchLimit, maxGIFSearchResults)
		page := queryIntClamped(r, "page", 1, 1000)
		results, err := searchKlipyGIFs(r.Context(), defaultGIFSearchQuery, limit, page)
		if err != nil {
			if errors.Is(err, errGIFProviderNotConfigured) {
				httpError(w, err.Error(), 501)
				return
			}
			httpError(w, err.Error(), 502)
			return
		}
		for i := range results {
			results[i].PreviewURL = proxyGIFPreviewURL(results[i].PreviewURL)
		}
		writeJSON(w, map[string]any{
			"results":  results,
			"page":     page,
			"has_more": len(results) >= limit,
		})
	})

	mux.HandleFunc("/api/gifs/preview", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			httpError(w, "method not allowed", 405)
			return
		}
		data, _, mimeType, err := downloadGIFMedia(r.Context(), r.URL.Query().Get("url"), maxGIFPreviewBytes)
		if err != nil {
			httpError(w, err.Error(), 400)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Set("Content-Type", mimeType)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/send-gif", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
			URL            string `json:"url"`
			Caption        string `json:"caption"`
			ReplyToID      string `json:"reply_to_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		convID := strings.TrimSpace(req.ConversationID)
		if convID == "" {
			httpError(w, "conversation_id is required", 400)
			return
		}
		data, filename, mimeType, err := downloadGIFMedia(r.Context(), req.URL, maxGIFSendBytes)
		if err != nil {
			httpError(w, err.Error(), 400)
			return
		}
		sendMediaBytes(w, convID, data, filename, mimeType, strings.TrimSpace(req.Caption), strings.TrimSpace(req.ReplyToID))
	})

	mux.HandleFunc("/api/send-media", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		// Cap the whole request body: ParseMultipartForm's argument only
		// bounds the in-memory portion (the rest spills to disk), and the
		// attachment is read fully into memory below — without this cap a
		// single oversized POST can OOM the backend and take every live
		// sync down with it.
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				httpError(w, fmt.Sprintf("attachment too large (limit %d MB)", maxUploadBytes>>20), http.StatusRequestEntityTooLarge)
				return
			}
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
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				httpError(w, fmt.Sprintf("attachment too large (limit %d MB)", maxUploadBytes>>20), http.StatusRequestEntityTooLarge)
				return
			}
			httpError(w, "read file: "+err.Error(), 500)
			return
		}

		mime := header.Header.Get("Content-Type")
		if mime == "" {
			mime = "application/octet-stream"
		}
		sendMediaBytes(w, convID, data, header.Filename, mime, caption, replyToID)
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
			markGoogleAuthExpired(err)
			httpError(w, googleAPIErrorMessage("download media", err), 502)
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
			} else if markGoogleAuthExpired(err) {
				httpError(w, googleAPIErrorMessage("get conversation", err), 502)
				return
			}
		}

		payload := app.BuildReactionPayload(req.MessageID, req.Emoji, req.Action, sim)
		resp, err := cli.GM.SendReaction(payload)
		if err != nil {
			markGoogleAuthExpired(err)
			httpError(w, googleAPIErrorMessage("send reaction", err), 502)
			return
		}
		writeJSON(w, map[string]any{
			"success": resp.GetSuccess(),
		})
	})

	mux.HandleFunc("/api/transcript", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptRequestBytes)
		var req struct {
			MessageID  string  `json:"message_id"`
			Transcript *string `json:"transcript"`
			Model      *string `json:"model,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				httpError(w, fmt.Sprintf("transcript request too large (limit %d bytes)", maxTranscriptRequestBytes), http.StatusRequestEntityTooLarge)
				return
			}
			httpError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		if req.MessageID == "" {
			httpError(w, "message_id required", 400)
			return
		}
		if req.Transcript == nil {
			httpError(w, "transcript required", 400)
			return
		}
		if err := db.ValidateMessageTranscript(*req.Transcript, req.Model); err != nil {
			httpError(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		if err := store.SetMessageTranscript(req.MessageID, *req.Transcript, req.Model); err != nil {
			if errors.Is(err, db.ErrMessageNotFound) {
				httpError(w, "message not found", 404)
				return
			}
			httpError(w, err.Error(), 500)
			return
		}
		msg, err := store.GetMessageByID(req.MessageID)
		if err != nil {
			httpError(w, "load message: "+err.Error(), 500)
			return
		}
		if msg != nil {
			publishMessages(msg.ConversationID)
		}
		writeJSON(w, map[string]any{
			"success":           true,
			"message_id":        req.MessageID,
			"transcript_length": utf8.RuneCountInString(*req.Transcript),
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
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("failed to get/create conversation", err), 502)
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
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("get conversation", err), 502)
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
			if !markGoogleAuthExpired(err) {
				recordGoogleSendError(err)
			}
			httpError(w, googleAPIErrorMessage("send message", err), 502)
			return
		}
		success := resp.GetStatus() == gmproto.SendMessageResponse_SUCCESS
		if !success {
			now := time.Now().UnixMilli()
			_ = recordOutgoingMessage(&db.Message{
				MessageID:      payload.TmpID,
				ConversationID: draft.ConversationID,
				Body:           req.Body,
				IsFromMe:       true,
				TimestampMS:    now,
				Status:         "OUTGOING_FAILED:" + resp.GetStatus().String(),
			}, "")
			publishMessages(draft.ConversationID)
			publishConversations()
			if googlePhoneResponding() {
				recordGoogleSend(false)
			}
			httpError(w, googleSendRejectedMessage(resp.GetStatus().String(), googlePhoneResponding()), 502)
			return
		}
		recordGoogleSend(true)
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
		// Prefer the API key from a header — secrets in query strings leak into
		// logs, history, and proxies. The query param is kept only as a
		// deprecated fallback for existing callers.
		apiKey := strings.TrimSpace(r.Header.Get("X-Anthropic-Api-Key"))
		if apiKey == "" {
			apiKey = r.URL.Query().Get("api_key")
		}
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
				// Could be a deep backfill OR the shallow startup catch-up
				// holding the shared guard — keep the message generic and
				// consistent with /api/backfill/status (which now reports
				// running=true in both cases).
				httpError(w, "a sync is already running — try again in a moment", 409)
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
			markGoogleAuthExpired(err)
			httpError(w, googleAPIErrorMessage("backfill phone", err), 502)
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
			markGoogleAuthExpired(err)
			httpError(w, googleAPIErrorMessage("reconnect google messages", err), 502)
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

	mux.HandleFunc("/api/signal/recovery/replay", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", 405)
			return
		}
		if opts.ReplaySignalRecovery == nil || opts.SignalStatus == nil {
			httpError(w, "signal recovery replay not available", 404)
			return
		}
		if err := opts.ReplaySignalRecovery(); err != nil {
			httpError(w, "replay signal recovery: "+err.Error(), 502)
			return
		}
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
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json")
			w.Header().Set("Cache-Control", "no-cache")
		case "/sw.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		default:
			if strings.HasSuffix(r.URL.Path, ".png") {
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Cache-Control", "public, max-age=31536000")
			}
		}
		staticHandler.ServeHTTP(w, r)
	}))

	// Wrap the mux to intercept MCP requests before the mux's catch-all.
	var handler http.Handler = mux
	if mcpHandler != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
				mcpHandler.ServeHTTP(w, r)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}

	// Guard local control surfaces against cross-origin / DNS-rebinding abuse. The server
	// binds to 127.0.0.1, but that does NOT stop a malicious web page the user
	// visits from talking to http://127.0.0.1:<port>/api/... or /mcp/... .
	// Requiring a loopback Host and, when present, a same-origin loopback
	// Origin/Referer closes that vector without affecting native app requests.
	return ProtectLocalControl(handler)
}

// ProtectLocalControl rejects cross-origin browser access to local control
// surfaces such as /api/* and /mcp/*.
func ProtectLocalControl(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProtectedLocalPath(r.URL.Path) && !isLocalControlRequest(r) {
			httpError(w, "forbidden: API requests must originate from the local OpenMessage app", http.StatusForbidden)
			return
		}
		setSecurityHeaders(w)
		handler.ServeHTTP(w, r)
	})
}

func isProtectedLocalPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || path == "/mcp" || strings.HasPrefix(path, "/mcp/")
}

// setSecurityHeaders adds defense-in-depth headers. The UI relies on
// disciplined manual escaping against XSS; the CSP doesn't stop inline-
// handler injection (the app uses inline scripts/handlers throughout) but
// it does contain a missed sink's blast radius: scripts, fetches, images,
// and media can't reach non-loopback origins, so message history can't be
// exfiltrated to an attacker host. Fonts are the one external dependency.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline'; "+
			"worker-src 'self'; "+
			"manifest-src 'self'; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src 'self' https://fonts.gstatic.com; "+
			"img-src 'self' data: blob:; "+
			"media-src 'self' data: blob:; "+
			"connect-src 'self'; "+
			"object-src 'none'; "+
			"base-uri 'self'; "+
			"frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// isLoopbackHost reports whether a bare hostname refers to this machine's
// loopback interface.
func isLoopbackHost(hostname string) bool {
	switch strings.ToLower(strings.Trim(hostname, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// isLocalControlRequest validates that a protected local request originates
// from OpenMessage itself rather than a cross-origin browser page: Host must
// be loopback, and Origin/Referer must be the same local scheme and port.
func isLocalControlRequest(r *http.Request) bool {
	reqOrigin, ok := localRequestOrigin(r)
	if !ok {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	headerOrigin, ok := parseLocalOrigin(origin)
	return ok && headerOrigin == reqOrigin
}

type localOrigin struct {
	Scheme string
	Port   string
}

func localRequestOrigin(r *http.Request) (localOrigin, bool) {
	host, port, ok := splitHostPortDefault(r.Host, requestDefaultPort(r))
	// An absent Host header is treated as non-local: browsers always send
	// one, so an empty value only ever comes from a hand-rolled client —
	// which must not slip past the loopback check by omission.
	if !ok || host == "" || !isLoopbackHost(host) {
		return localOrigin{}, false
	}
	return localOrigin{Scheme: requestScheme(r), Port: port}, true
}

func parseLocalOrigin(raw string) (localOrigin, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return localOrigin{}, false
	}
	host, port, ok := splitHostPortDefault(u.Host, defaultPortForScheme(u.Scheme))
	if !ok || !isLoopbackHost(host) {
		return localOrigin{}, false
	}
	return localOrigin{Scheme: strings.ToLower(u.Scheme), Port: port}, true
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func requestDefaultPort(r *http.Request) string {
	return defaultPortForScheme(requestScheme(r))
}

func defaultPortForScheme(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}

func splitHostPortDefault(hostport, defaultPort string) (string, string, bool) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", "", false
	}
	if host, port, err := net.SplitHostPort(hostport); err == nil {
		if port == "" {
			port = defaultPort
		}
		return host, port, true
	}
	u, err := url.Parse("//" + hostport)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return u.Hostname(), port, true
}

func mergeSearchResults(store *db.Store, msgs []*db.Message, convos []*db.Conversation, limit int) []SearchResult {
	results := make([]SearchResult, 0, limit)
	seen := make(map[string]struct{}, limit)
	identityIndex := loadUnifiedIdentityIndex(store)

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
		appendResult(searchResultForConversation(conv, msg.TimestampMS, searchPreviewForMessage(msg), identityIndex))
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
		appendResult(searchResultForConversation(conv, conv.LastMessageTS, preview, identityIndex))
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

type unifiedConversationIdentity struct {
	ID   string
	Name string
}

type unifiedIdentifier struct {
	Platform string `json:"platform"`
	Value    string `json:"value"`
}

type conversationParticipant struct {
	Name      string `json:"name"`
	Number    string `json:"number"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	ID        string `json:"id"`
	IsMe      bool   `json:"is_me"`
	IsMeCamel bool   `json:"isMe"`
}

func enrichConversationPreviews(store *db.Store, convos []*db.Conversation) error {
	ids := make([]string, 0, len(convos))
	for _, conv := range convos {
		if conv == nil {
			continue
		}
		ids = append(ids, conv.ConversationID)
	}
	previews, err := store.LatestConversationPreviews(ids)
	if err != nil {
		return err
	}
	for _, conv := range convos {
		if conv == nil {
			continue
		}
		conv.LastMessagePreview = previews[conv.ConversationID]
	}
	return nil
}

func enrichUnifiedConversationIdentities(store *db.Store, convos []*db.Conversation) {
	identityIndex := loadUnifiedIdentityIndex(store)
	if len(identityIndex) == 0 {
		return
	}
	for _, conv := range convos {
		if identity, ok := unifiedIdentityForConversation(conv, identityIndex); ok {
			conv.UnifiedID = identity.ID
			conv.UnifiedName = identity.Name
		}
	}
}

func loadUnifiedIdentityIndex(store *db.Store) map[string]unifiedConversationIdentity {
	contacts, err := store.ListUnifiedContacts("", 10000)
	if err != nil || len(contacts) == 0 {
		return nil
	}
	index := make(map[string]unifiedConversationIdentity)
	for _, contact := range contacts {
		if contact == nil || strings.TrimSpace(contact.UnifiedID) == "" {
			continue
		}
		var identifiers []unifiedIdentifier
		if err := json.Unmarshal([]byte(contact.Identifiers), &identifiers); err != nil {
			continue
		}
		identity := unifiedConversationIdentity{
			ID:   strings.TrimSpace(contact.UnifiedID),
			Name: strings.TrimSpace(contact.DisplayName),
		}
		for _, identifier := range identifiers {
			key := unifiedIdentifierKey(identifier.Platform, identifier.Value)
			if key == "" {
				continue
			}
			index[key] = identity
		}
	}
	return index
}

func searchResultForConversation(conv *db.Conversation, timestamp int64, preview string, identityIndex map[string]unifiedConversationIdentity) SearchResult {
	result := SearchResult{
		ConversationID:  conv.ConversationID,
		Name:            conv.Name,
		IsGroup:         conv.IsGroup,
		Participants:    conv.Participants,
		LastMessageTS:   timestamp,
		UnreadCount:     conv.UnreadCount,
		SourcePlatform:  conv.SourcePlatform,
		DisplayProtocol: conv.DisplayProtocol,
		IsFavorite:      conv.IsFavorite,
		Preview:         preview,
	}
	if identity, ok := unifiedIdentityForConversation(conv, identityIndex); ok {
		result.UnifiedID = identity.ID
		result.UnifiedName = identity.Name
	}
	return result
}

func unifiedIdentityForConversation(conv *db.Conversation, identityIndex map[string]unifiedConversationIdentity) (unifiedConversationIdentity, bool) {
	if conv == nil || conv.IsGroup || len(identityIndex) == 0 {
		return unifiedConversationIdentity{}, false
	}
	participantID := primaryConversationParticipantID(conv)
	if participantID == "" {
		return unifiedConversationIdentity{}, false
	}
	identity, ok := identityIndex[unifiedIdentifierKey(conv.SourcePlatform, participantID)]
	return identity, ok
}

func primaryConversationParticipantID(conv *db.Conversation) string {
	if conv == nil || strings.TrimSpace(conv.Participants) == "" {
		return ""
	}
	var participants []conversationParticipant
	if err := json.Unmarshal([]byte(conv.Participants), &participants); err != nil {
		return ""
	}
	for _, participant := range participants {
		if participant.IsMe || participant.IsMeCamel {
			continue
		}
		if id := participantIdentifier(participant); id != "" {
			return id
		}
	}
	if len(participants) == 0 {
		return ""
	}
	return participantIdentifier(participants[0])
}

func participantIdentifier(participant conversationParticipant) string {
	for _, value := range []string{participant.Number, participant.Phone, participant.Email, participant.ID} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func unifiedIdentifierKey(platform, value string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	value = normalizeUnifiedIdentifier(value)
	if platform == "" || value == "" {
		return ""
	}
	return platform + "\x00" + value
}

func normalizeUnifiedIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	digits := make([]byte, 0, len(value))
	phoneLike := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		case c == '+' || c == '(' || c == ')' || c == '-' || c == '.' || c == ' ':
		default:
			phoneLike = false
		}
	}
	if phoneLike && len(digits) >= 7 {
		return string(digits)
	}
	return value
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

func googleAPIErrorMessage(action string, err error) string {
	if app.IsGoogleAuthExpiredError(err) {
		return "Google Messages session expired; refreshing and reconnecting. Try again in a few seconds."
	}
	if isGoogleNetworkError(err) {
		return "Google Messages is offline. Check your internet connection, then try again."
	}
	return action + ": " + err.Error()
}

// googleSendRejectedMessage builds the user-facing error for a non-SUCCESS
// Google send. UNKNOWN is what a silently-unlinked linked device returns on
// every send, so the message points at re-pairing rather than leaving the
// user with a bare status code.
func googleSendRejectedMessage(status string, phoneResponding bool) string {
	return app.GoogleSendRejectedMessage(status, phoneResponding)
}

func isGoogleNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	needles := []string{
		"instantmessaging-pa.clients6.google.com",
		"google.internal.communications.instantmessaging",
		"no such host",
		"temporary failure in name resolution",
		"server misbehaving",
		"dial tcp",
		"lookup ",
		"network is unreachable",
		"no route to host",
		"i/o timeout",
		"context deadline exceeded",
		"client.timeout exceeded",
		"tls handshake timeout",
		"connection refused",
		"connection reset by peer",
	}
	for _, needle := range needles {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// normalizeContactKey returns a stable dedup key for a contact entry. We
// fold names case-insensitively and reduce phone numbers to digits-only so
// that "+1 (650) 555-1234", "16505551234", and "650-555-1234" all collide.
func normalizeContactKey(name, number string) string {
	digits := make([]byte, 0, len(number))
	for i := 0; i < len(number); i++ {
		c := number[i]
		if c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	return strings.ToLower(strings.TrimSpace(name)) + "|" + string(digits)
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

// queryIntClamped reads an int "limit"-style param and clamps it to [1, max],
// falling back to def for missing/non-positive values. SQLite treats LIMIT -1
// as "no limit", so an unclamped negative/zero limit would return the entire
// table — an unbounded-response/DoS vector.
func queryIntClamped(r *http.Request, key string, def, max int) int {
	n := queryInt(r, key, def)
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

// staleDaysThreshold is how many whole days a platform's latest message may
// trail the newest message overall before it's flagged as not syncing.
const staleDaysThreshold = 3

// daysBehind returns how many whole days `older` trails `newer` (0 if not behind).
func daysBehind(older, newer int64) int {
	if older <= 0 || newer <= older {
		return 0
	}
	return int(time.UnixMilli(newer).Sub(time.UnixMilli(older)).Hours() / 24)
}
