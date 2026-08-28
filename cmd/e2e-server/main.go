package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/migration"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/v2wire"
	"github.com/maxghenis/openmessage/internal/web"
)

const (
	defaultPort        = 7010
	pagedConversation  = "conv-paged"
	pagedConversationN = 150
)

func main() {
	logger := zerolog.Nop()
	handler, cleanup, err := newE2EServer(logger)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	addr := "127.0.0.1:" + strconv.Itoa(serverPort())
	if err := http.ListenAndServe(addr, handler); err != nil {
		panic(err)
	}
}

type e2eServer struct {
	handler  http.Handler
	v2Store  *sqlite.Store
	adapters map[string]*scripted.Adapter
}

type e2eAdapter struct {
	*scripted.Adapter
}

func (e2eAdapter) DeclaredCapabilities() bridge.CapabilitySet {
	return bridge.CapabilitySet{TextSend: true, MediaSend: true}
}

func (s *e2eServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func newE2EServer(logger zerolog.Logger) (_ *e2eServer, cleanup func(), resultErr error) {
	dataDir, err := os.MkdirTemp("", "openmessage-e2e-")
	if err != nil {
		return nil, nil, err
	}
	controlAuth, err := web.NewControlAuth(dataDir, logger)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}
	legacyPath := filepath.Join(dataDir, "messages.db")
	store, err := db.New(legacyPath)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}

	if err := seedFixture(store); err != nil {
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}

	v2StorePath := filepath.Join(dataDir, "store.sqlite3")
	blobPath := filepath.Join(dataDir, "blobs")
	if _, err := migration.Transform(context.Background(), migration.Options{
		SourcePath:      legacyPath,
		TempStorePath:   v2StorePath,
		TempBlobPath:    blobPath,
		TargetPath:      dataDir,
		TargetStorePath: v2StorePath,
	}); err != nil {
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}
	v2Store, err := sqlite.Open(v2StorePath)
	if err != nil {
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}
	blobs, err := blob.New(blobPath)
	if err != nil {
		_ = v2Store.Close()
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}

	events := web.NewEventBroker()
	registry := bridge.NewRegistry()
	adapters := map[string]*scripted.Adapter{
		"google-primary":   scripted.New("google-primary", bridge.PlatformGoogle),
		"whatsapp-primary": scripted.New("whatsapp-primary", bridge.PlatformWhatsApp),
		"signal-primary":   scripted.New("signal-primary", bridge.PlatformSignal),
	}
	for _, adapter := range adapters {
		for i := 0; i < 128; i++ {
			adapter.EnqueueMediaResult(bridge.SendResult{
				RemoteMessageID: fmt.Sprintf("e2e-media-remote-%s-%d", adapter.AccountID(), i),
				AcceptedAt:      time.Now(),
			})
		}
		if err := registry.Register(e2eAdapter{Adapter: adapter}); err != nil {
			_ = v2Store.Close()
			_ = store.Close()
			_ = os.RemoveAll(dataDir)
			return nil, nil, err
		}
	}
	clock := messaging.SystemClock{}
	messageService, err := messaging.NewMessageService(v2Store, registry, blobs, clock, messaging.CryptoIDSource{})
	if err != nil {
		_ = v2Store.Close()
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}
	mediaService, err := media.NewService(v2Store, registry, blobs, clock)
	if err != nil {
		_ = v2Store.Close()
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
		return nil, nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e2eChanges := make(chan struct{}, 1)
	var runWG sync.WaitGroup
	runWG.Add(2)
	go func() {
		defer runWG.Done()
		_ = messageService.Run(runCtx)
	}()
	go func() {
		defer runWG.Done()
		notifier := &v2wire.PrimaryNotifier{Sources: []func() <-chan struct{}{
			messageService.Changes,
			func() <-chan struct{} { return e2eChanges },
		}, Events: events, Logger: logger}
		_ = notifier.Run(runCtx)
	}()
	var nextID atomic.Int64
	nextID.Store(time.Now().UnixNano())
	type mediaBlob struct {
		data []byte
		mime string
	}
	var mediaStore sync.Map
	var syntheticReadReceipts sync.Map
	mediaStore.Store("m10media", mediaBlob{
		data: []byte{
			0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
			0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
			0x08, 0x04, 0x00, 0x00, 0x00, 0xb5, 0x1c, 0x0c,
			0x02, 0x00, 0x00, 0x00, 0x0b, 0x49, 0x44, 0x41,
			0x54, 0x78, 0xda, 0x63, 0xfc, 0xff, 0x1f, 0x00,
			0x03, 0x03, 0x02, 0x00, 0xee, 0xd9, 0xf7, 0x00,
			0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
			0x42, 0x60, 0x82,
		},
		mime: "image/png",
	})
	base := web.APIHandlerWithOptions(store, nil, logger, nil, web.APIOptions{
		Auth:      controlAuth,
		Reads:     v2read.New(v2Store),
		V2Primary: true,
		V2: &web.V2Options{
			Service: messageService, Media: mediaService, V2Store: v2Store, Blobs: blobs, Registry: registry,
		},
		Events:       events,
		IdentityName: "Max Ghenis",
		IsConnected:  func() bool { return true },
		FetchLinkPreview: func(ctx context.Context, rawURL string) (*web.LinkPreview, error) {
			switch rawURL {
			case "https://example.com/story":
				return &web.LinkPreview{
					URL:         rawURL,
					Title:       "Example Story",
					Description: "A compact social preview for the seeded test link.",
					SiteName:    "Example",
					ImageURL:    "https://images.example.com/story.png",
					Domain:      "example.com",
				}, nil
			case "https://openai.com/research":
				return &web.LinkPreview{
					URL:         rawURL,
					Title:       "OpenAI Research",
					Description: "Updates and papers from the research team.",
					SiteName:    "OpenAI",
					ImageURL:    "https://images.example.com/openai-research.png",
					Domain:      "openai.com",
				}, nil
			default:
				return nil, web.ErrNoLinkPreview
			}
		},
		FetchLinkPreviewImage: func(ctx context.Context, rawURL string) ([]byte, string, error) {
			switch rawURL {
			case "https://images.example.com/story.png", "https://images.example.com/openai-research.png":
				return []byte{
					0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
					0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
					0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
					0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
					0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
					0x54, 0x08, 0xd7, 0x63, 0xf8, 0xff, 0xff, 0x3f,
					0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59,
					0xe7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
					0x44, 0xae, 0x42, 0x60, 0x82,
				}, "image/png", nil
			default:
				return nil, "", web.ErrNoLinkPreview
			}
		},
		WhatsAppStatus: func() any {
			return map[string]any{"connected": true, "paired": true}
		},
		PairWhatsAppPhone: func(phone string) (string, error) {
			return "E2EPAIR1", nil
		},
		SignalStatus: func() any {
			return map[string]any{"connected": true, "paired": true, "account": "+15551234567"}
		},
		ConnectSignal: func() error { return nil },
		UnpairSignal:  func() error { return nil },
		SignalQRCode: func() (any, error) {
			return map[string]any{"png_data_url": "data:image/png;base64,ZmFrZQ=="}, nil
		},
		LeaveWhatsAppGroup: func(conversationID string) error {
			return store.DeleteConversation(conversationID)
		},
		SendWhatsAppMedia: func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
			messageID := fmt.Sprintf("whatsapp:e2e-media-%d", nextID.Add(1))
			now := time.Now().UnixMilli()
			mediaStore.Store(messageID, mediaBlob{
				data: append([]byte(nil), data...),
				mime: mime,
			})
			return &db.Message{
				MessageID:      messageID,
				ConversationID: conversationID,
				SenderName:     "Me",
				SenderNumber:   "+15551234567",
				Body:           caption,
				TimestampMS:    now,
				Status:         "OUTGOING_COMPLETE",
				IsFromMe:       true,
				MediaID:        "wa:e2e-media",
				MimeType:       mime,
				DecryptionKey:  "e2e",
				ReplyToID:      replyToID,
				SourcePlatform: "whatsapp",
				SourceID:       strings.TrimPrefix(messageID, "whatsapp:"),
			}, nil
		},
		SendSignalMedia: func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
			messageID := fmt.Sprintf("signal:e2e-media-%d", nextID.Add(1))
			now := time.Now().UnixMilli()
			mediaStore.Store(messageID, mediaBlob{
				data: append([]byte(nil), data...),
				mime: mime,
			})
			body := caption
			if body == "" {
				body = "[Attachment]"
			}
			return &db.Message{
				MessageID:      messageID,
				ConversationID: conversationID,
				SenderName:     "Me",
				SenderNumber:   "+15551234567",
				Body:           body,
				TimestampMS:    now,
				Status:         "sent",
				IsFromMe:       true,
				MediaID:        "signalatt:e2e-media",
				MimeType:       mime,
				ReplyToID:      replyToID,
				SourcePlatform: "signal",
				SourceID:       strings.TrimPrefix(messageID, "signal:"),
			}, nil
		},
		DownloadWhatsAppMedia: func(msg *db.Message) ([]byte, string, error) {
			raw, ok := mediaStore.Load(msg.MessageID)
			if !ok {
				return nil, "", fmt.Errorf("media %s not found", msg.MessageID)
			}
			blob := raw.(mediaBlob)
			return append([]byte(nil), blob.data...), blob.mime, nil
		},
		DownloadSignalMedia: func(msg *db.Message) ([]byte, string, error) {
			raw, ok := mediaStore.Load(msg.MessageID)
			if !ok {
				return nil, "", fmt.Errorf("media %s not found", msg.MessageID)
			}
			blob := raw.(mediaBlob)
			return append([]byte(nil), blob.data...), blob.mime, nil
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/_e2e/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Body           string `json:"body"`
			ConversationID string `json:"conversation_id"`
			IsFromMe       bool   `json:"is_from_me"`
			MentionsMe     bool   `json:"mentions_me"`
			SenderName     string `json:"sender_name"`
			SenderNumber   string `json:"sender_number"`
			TimestampMS    int64  `json:"timestamp_ms"`
			Status         string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ConversationID == "" || req.Body == "" {
			http.Error(w, "conversation_id and body are required", http.StatusBadRequest)
			return
		}
		if req.TimestampMS == 0 {
			req.TimestampMS = time.Now().UnixMilli()
		}
		msg, err := upsertSyntheticMessage(store, req.ConversationID, req.Body, req.TimestampMS, req.IsFromMe, req.MentionsMe, req.SenderName, req.SenderNumber, req.Status, nextID.Add(1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		v2ConversationID, v2MessageID, err := importSyntheticV2Message(v2Store, store, msg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.EqualFold(strings.TrimSpace(req.Status), "read") {
			syntheticReadReceipts.Store(v2MessageID, struct{}{})
		}
		events.PublishMessages(v2ConversationID)
		events.PublishConversations()
		select {
		case e2eChanges <- struct{}{}:
		default:
		}
		writeJSON(w, map[string]any{
			"message_id":      msg.MessageID,
			"conversation_id": v2ConversationID,
			"success":         true,
		})
	})

	mux.HandleFunc("/_e2e/drafts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Body           string `json:"body"`
			ConversationID string `json:"conversation_id"`
			DraftID        string `json:"draft_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ConversationID == "" || req.Body == "" {
			http.Error(w, "conversation_id and body are required", http.StatusBadRequest)
			return
		}
		if req.DraftID == "" {
			req.DraftID = fmt.Sprintf("draft-%d", nextID.Add(1))
		}
		if err := store.UpsertDraft(&db.Draft{
			DraftID:        req.DraftID,
			ConversationID: req.ConversationID,
			Body:           req.Body,
			CreatedAt:      time.Now().UnixMilli(),
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events.PublishDrafts(v2ConversationIDForLegacy(store, req.ConversationID))
		writeJSON(w, map[string]any{
			"draft_id": req.DraftID,
			"success":  true,
		})
	})

	mux.HandleFunc("/_e2e/typing", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
			SenderName     string `json:"sender_name"`
			SenderNumber   string `json:"sender_number"`
			Typing         bool   `json:"typing"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ConversationID == "" {
			http.Error(w, "conversation_id is required", http.StatusBadRequest)
			return
		}
		events.PublishTyping(v2ConversationIDForLegacy(store, req.ConversationID), req.SenderName, req.SenderNumber, req.Typing)
		writeJSON(w, map[string]any{"success": true})
	})

	mux.HandleFunc("/_e2e/avatar", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SourcePlatform string `json:"source_platform"`
			ParticipantID  string `json:"participant_id"`
			ContactID      string `json:"contact_id"`
			PhoneNumber    string `json:"phone_number"`
			DisplayName    string `json:"display_name"`
			MimeType       string `json:"mime_type"`
			ImageBase64    string `json:"image_base64"`
			ImageHash      string `json:"image_hash"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := []byte("avatar-bytes")
		if req.ImageBase64 != "" {
			encoded := req.ImageBase64
			if idx := strings.Index(encoded, ","); idx >= 0 {
				encoded = encoded[idx+1:]
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				http.Error(w, "invalid image_base64", http.StatusBadRequest)
				return
			}
			data = decoded
		}
		mimeType := strings.TrimSpace(req.MimeType)
		if mimeType == "" {
			mimeType = "image/png"
		}
		imageHash := strings.TrimSpace(req.ImageHash)
		if imageHash == "" {
			imageHash = fmt.Sprintf("e2e-avatar-%d", nextID.Add(1))
		}
		if err := store.UpsertContactAvatar(db.ContactAvatarCandidate{
			SourcePlatform: req.SourcePlatform,
			ParticipantID:  req.ParticipantID,
			ContactID:      req.ContactID,
			PhoneNumber:    req.PhoneNumber,
			DisplayName:    req.DisplayName,
			Source:         "e2e",
		}, data, mimeType, imageHash, time.Now().UnixMilli()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"success": true, "image_hash": imageHash})
	})

	mux.HandleFunc("POST /_e2e/bridges/{account}/next-result", func(w http.ResponseWriter, r *http.Request) {
		adapter := adapters[r.PathValue("account")]
		if adapter == nil {
			http.Error(w, "unknown bridge account", http.StatusNotFound)
			return
		}
		var req struct {
			Result string `json:"next_result"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Result {
		case "uncertain":
			adapter.EnqueueTextError(bridge.OpError{Class: bridge.FailureTransient, Operation: "send_text", Fingerprint: "e2e_uncertain", Dispatch: bridge.DispatchUncertain, Cause: fmt.Errorf("scripted uncertain result")})
		case "success":
			adapter.EnqueueTextResult(bridge.SendResult{RemoteMessageID: fmt.Sprintf("e2e-remote-%d", nextID.Add(1)), AcceptedAt: time.Now()})
		default:
			http.Error(w, "next_result must be success or uncertain", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"success": true})
	})

	mux.Handle("/", withPrimaryFixtureProjection(
		withConfirmedMediaProjection(
			withSyntheticReadReceipts(base, &syntheticReadReceipts),
			v2Store,
		),
		store,
	))

	server := &e2eServer{handler: mux, v2Store: v2Store, adapters: adapters}
	cleanup = func() {
		cancel()
		runWG.Wait()
		_ = v2Store.Close()
		_ = store.Close()
		_ = os.RemoveAll(dataDir)
	}
	return server, cleanup, nil
}

// withConfirmedMediaProjection closes the e2e adapter's confirmation seam.
// The production media outbox durably owns the uploaded blob, but a confirmed
// outgoing message is not yet given a message_attachments row. V2-primary
// reads therefore cannot produce the final /api/media URL. Project the already
// confirmed durable attachment before serving a thread; no transport result or
// state is fabricated here.
func withConfirmedMediaProjection(next http.Handler, store *sqlite.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages") {
			if err := projectConfirmedMediaAttachments(r.Context(), store); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func projectConfirmedMediaAttachments(ctx context.Context, store *sqlite.Store) error {
	outbox, err := sqlite.NewOutboxRepository(store, time.Now)
	if err != nil {
		return err
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		return err
	}
	attachments, err := sqlite.NewMessageAttachmentRepository(store, time.Now)
	if err != nil {
		return err
	}
	rows, err := outbox.ListConfirmedSince(ctx, -1, 1000)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Kind != sqlite.OutboxKindMedia || row.LocalMessageID == nil {
			continue
		}
		message, err := messages.GetMessage(ctx, *row.LocalMessageID)
		if err != nil {
			return fmt.Errorf("project confirmed media %q: get message: %w", row.OutboxID, err)
		}
		attachment, err := outbox.GetOutboxAttachment(ctx, row.OutboxID)
		if err != nil {
			return fmt.Errorf("project confirmed media %q: get attachment: %w", row.OutboxID, err)
		}
		size := attachment.SizeBytes
		if err := messages.ImportMessage(ctx, sqlite.MessageProjection{
			Message: message,
			Attachments: []sqlite.MessageAttachment{{
				MessageID: message.MessageID,
				Ordinal:   attachment.Ordinal,
				Filename:  attachment.Filename,
				MIME:      attachment.MIME,
				SizeBytes: &size,
			}},
		}); err != nil {
			return fmt.Errorf("project confirmed media %q: import attachment: %w", row.OutboxID, err)
		}
		if err := attachments.MarkDownloaded(
			ctx, message.MessageID, attachment.Ordinal, attachment.BlobHash,
			attachment.SizeBytes, attachment.MIME,
		); err != nil {
			return fmt.Errorf("project confirmed media %q: publish blob: %w", row.OutboxID, err)
		}
	}
	return nil
}

func v2ConversationIDForLegacy(store *db.Store, legacyID string) string {
	conversation, err := store.GetConversation(legacyID)
	if err != nil || conversation == nil {
		return legacyID
	}
	accountID := "google-primary"
	switch strings.ToLower(strings.TrimSpace(conversation.SourcePlatform)) {
	case "whatsapp":
		accountID = "whatsapp-primary"
	case "signal":
		accountID = "signal-primary"
	}
	return v2keys.DeriveID("conversation", accountID, legacyID)
}

func legacyConversationIDForV2(store *db.Store, v2ID string) string {
	conversations, err := store.ListConversations(1000)
	if err != nil {
		return v2ID
	}
	for _, conversation := range conversations {
		if v2ConversationIDForLegacy(store, conversation.ConversationID) == v2ID {
			return conversation.ConversationID
		}
	}
	return v2ID
}

// withPrimaryFixtureProjection preserves fixture-only legacy metadata at the
// v2-primary read boundary. Production remains authoritative for message and
// conversation state; this adapter only translates IDs for legacy draft
// fixtures and restores metadata intentionally absent from the clean schema.
func withPrimaryFixtureProjection(next http.Handler, legacy *db.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/drafts" {
			clone := r.Clone(r.Context())
			query := clone.URL.Query()
			query.Set("conversation_id", legacyConversationIDForV2(legacy, query.Get("conversation_id")))
			clone.URL.RawQuery = query.Encode()
			next.ServeHTTP(w, clone)
			return
		}

		if r.Method != http.MethodGet || (r.URL.Path != "/api/conversations" && r.URL.Path != "/api/search") {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &responseRecorder{header: make(http.Header), status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		if recorder.status != http.StatusOK {
			copyRecordedResponse(w, recorder)
			return
		}

		var rows []map[string]any
		if err := json.Unmarshal(recorder.body, &rows); err != nil {
			copyRecordedResponse(w, recorder)
			return
		}
		if r.URL.Path == "/api/search" {
			matches, err := legacy.SearchConversationsByMetadata(r.URL.Query().Get("q"), 500)
			if err == nil {
				rows = rows[:0]
				for _, conversation := range matches {
					rows = append(rows, fixtureConversationMap(conversation))
				}
			}
		}
		for _, row := range rows {
			v2ID, _ := row["ConversationID"].(string)
			legacyID := legacyConversationIDForV2(legacy, v2ID)
			conversation, err := legacy.GetConversation(legacyID)
			if err != nil || conversation == nil {
				continue
			}
			row["ConversationID"] = v2ConversationIDForLegacy(legacy, legacyID)
			row["Participants"] = conversation.Participants
			row["DisplayProtocol"] = conversation.DisplayProtocol
			row["source_platform"] = conversation.SourcePlatform
		}
		for key, values := range recorder.header {
			w.Header()[key] = values
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(recorder.status)
		_ = json.NewEncoder(w).Encode(rows)
	})
}

func fixtureConversationMap(conversation *db.Conversation) map[string]any {
	return map[string]any{
		"ConversationID": conversation.ConversationID,
		"Name":           conversation.Name, "IsGroup": conversation.IsGroup,
		"Participants": conversation.Participants, "LastMessageTS": conversation.LastMessageTS,
		"UnreadCount": conversation.UnreadCount, "source_platform": conversation.SourcePlatform,
		"DisplayProtocol": conversation.DisplayProtocol, "IsFavorite": conversation.IsFavorite,
	}
}

func copyRecordedResponse(w http.ResponseWriter, recorder *responseRecorder) {
	for key, values := range recorder.header {
		w.Header()[key] = values
	}
	w.WriteHeader(recorder.status)
	_, _ = w.Write(recorder.body)
}

// importSyntheticV2Message mirrors an e2e transport arrival into the same
// durable store used by v2-primary reads. The legacy row remains useful to
// legacy-only fixture endpoints, but it is never used as the primary signal.
func importSyntheticV2Message(v2Store *sqlite.Store, legacy *db.Store, msg *db.Message) (string, string, error) {
	legacyConversation, err := legacy.GetConversation(msg.ConversationID)
	if err != nil {
		return "", "", fmt.Errorf("get synthetic conversation %q: %w", msg.ConversationID, err)
	}
	if legacyConversation == nil {
		return "", "", fmt.Errorf("synthetic conversation %q not found", msg.ConversationID)
	}
	accountID := "google-primary"
	switch strings.ToLower(strings.TrimSpace(legacyConversation.SourcePlatform)) {
	case "whatsapp":
		accountID = "whatsapp-primary"
	case "signal":
		accountID = "signal-primary"
	}
	conversationID := v2keys.DeriveID("conversation", accountID, msg.ConversationID)
	conversation, err := v2Store.GetConversation(conversationID)
	if err != nil {
		return "", "", fmt.Errorf("get v2 synthetic conversation %q: %w", conversationID, err)
	}

	var senderIdentityID *string
	identities, err := v2Store.ListIdentities(accountID)
	if err != nil {
		return "", "", fmt.Errorf("list v2 identities for %q: %w", accountID, err)
	}
	for _, identity := range identities {
		matches := msg.IsFromMe && identity.IsSelf
		if !matches && strings.TrimSpace(msg.SenderNumber) != "" {
			matches = identity.CanonicalValue == msg.SenderNumber || identity.RawValue == msg.SenderNumber
		}
		if matches {
			identityID := identity.IdentityID
			senderIdentityID = &identityID
			break
		}
	}

	remoteMessageID := strings.TrimSpace(msg.SourceID)
	if remoteMessageID == "" {
		remoteMessageID = msg.MessageID
	}
	messageID := v2keys.DeriveID("message", accountID, msg.ConversationID+"\x1f"+remoteMessageID)
	direction := sqlite.MessageDirectionIncoming
	if msg.IsFromMe {
		direction = sqlite.MessageDirectionOutgoing
	}
	repository, err := sqlite.NewMessageRepository(v2Store, time.Now)
	if err != nil {
		return "", "", err
	}
	if err := repository.ImportMessage(context.Background(), sqlite.MessageProjection{Message: sqlite.Message{
		MessageID:        messageID,
		ConversationID:   conversationID,
		AccountID:        accountID,
		RemoteMessageID:  remoteMessageID,
		SenderIdentityID: senderIdentityID,
		Direction:        direction,
		Body:             msg.Body,
		State:            sqlite.MessageStateActive,
		OccurredAtMS:     msg.TimestampMS,
	}}); err != nil {
		return "", "", fmt.Errorf("import synthetic v2 message: %w", err)
	}
	if msg.TimestampMS > conversation.LastMessageAtMS {
		if err := v2Store.BumpConversationRecency(conversationID, msg.TimestampMS); err != nil {
			return "", "", err
		}
	}
	return conversationID, messageID, nil
}

// withSyntheticReadReceipts preserves fixture-only outgoing delivery status.
// The durable v2 cursor models thread read state, not remote per-message status.
func withSyntheticReadReceipts(next http.Handler, receipts *sync.Map) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/messages") {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &responseRecorder{header: make(http.Header), status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		for key, values := range recorder.header {
			w.Header()[key] = values
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(recorder.status)
		if recorder.status != http.StatusOK {
			_, _ = w.Write(recorder.body)
			return
		}
		var messages []map[string]any
		if err := json.Unmarshal(recorder.body, &messages); err != nil {
			_, _ = w.Write(recorder.body)
			return
		}
		for _, message := range messages {
			messageID, _ := message["MessageID"].(string)
			if _, ok := receipts.Load(messageID); ok {
				message["Status"] = "READ"
			}
		}
		_ = json.NewEncoder(w).Encode(messages)
	})
}

type responseRecorder struct {
	header http.Header
	body   []byte
	status int
}

func (r *responseRecorder) Header() http.Header    { return r.header }
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
func (r *responseRecorder) Write(body []byte) (int, error) {
	r.body = append(r.body, body...)
	return len(body), nil
}

func seedFixture(store *db.Store) error {
	if err := store.SeedDemo(); err != nil {
		return err
	}
	if err := store.SetConversationDisplayProtocol("conv1", "RCS"); err != nil {
		return err
	}
	for _, convoID := range []string{"conv3", "conv5"} {
		if err := setConversationPlatform(store, convoID, "whatsapp"); err != nil {
			return err
		}
	}
	if err := seedSyntheticConversation(store, &db.Conversation{
		ConversationID: "conv9",
		Name:           "Jordan Rivera",
		Participants:   `[{"name":"Jordan Rivera","number":"+14699991654"}]`,
		LastMessageTS:  1738959300000,
		SourcePlatform: "sms",
	}, []*db.Message{
		{
			MessageID:      "m9a",
			ConversationID: "conv9",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14699991654",
			Body:           "Can you text me the gate code when you get a chance?",
			TimestampMS:    1738958400000,
			Status:         "delivered",
			SourcePlatform: "sms",
		},
		{
			MessageID:      "m9b",
			ConversationID: "conv9",
			SenderName:     "Me",
			SenderNumber:   "+15551234567",
			Body:           "Yep, I'll send it before you head over.",
			TimestampMS:    1738959300000,
			Status:         "delivered",
			IsFromMe:       true,
			SourcePlatform: "sms",
		},
	}); err != nil {
		return err
	}
	if err := seedSyntheticConversation(store, &db.Conversation{
		ConversationID: "wa1",
		Name:           "Jordan Rivera",
		Participants:   `[{"name":"Jordan Rivera","number":"+14699991654"}]`,
		LastMessageTS:  1738960200000,
		UnreadCount:    1,
		SourcePlatform: "whatsapp",
	}, []*db.Message{
		{
			MessageID:      "m10a",
			ConversationID: "wa1",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14699991654",
			Body:           "Sent the menu here too in case WhatsApp is easier.",
			TimestampMS:    1738959000000,
			Status:         "delivered",
			SourcePlatform: "whatsapp",
		},
		{
			MessageID:      "m10b",
			ConversationID: "wa1",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14699991654",
			Body:           "Also, do you want me to bring dessert?",
			TimestampMS:    1738959900000,
			Status:         "delivered",
			SourcePlatform: "whatsapp",
		},
		{
			MessageID:      "m10media",
			ConversationID: "wa1",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14699991654",
			Body:           "Lisbon photo",
			TimestampMS:    1738960200000,
			Status:         "delivered",
			MediaID:        "wa:seed-media",
			MimeType:       "image/png",
			SourcePlatform: "whatsapp",
		},
	}); err != nil {
		return err
	}
	if err := seedSyntheticConversation(store, &db.Conversation{
		ConversationID: "conv11",
		Name:           "Jordan Rivera",
		Participants:   `[{"name":"Jordan Rivera","number":"+14155550999"}]`,
		LastMessageTS:  1738959000000,
		SourcePlatform: "sms",
	}, []*db.Message{
		{
			MessageID:      "m11a",
			ConversationID: "conv11",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14155550999",
			Body:           "Wrong Jordan, different line.",
			TimestampMS:    1738959000000,
			Status:         "delivered",
			SourcePlatform: "sms",
		},
	}); err != nil {
		return err
	}
	if err := seedSyntheticConversation(store, &db.Conversation{
		ConversationID: "signal:+14155550333",
		Name:           "Taylor Price",
		Participants:   `[{"name":"Taylor Price","number":"+14155550333"}]`,
		LastMessageTS:  1738959605000,
		UnreadCount:    0,
		SourcePlatform: "signal",
	}, []*db.Message{
		{
			MessageID:      "signal:seed-1",
			ConversationID: "signal:+14155550333",
			SenderName:     "Taylor Price",
			SenderNumber:   "+14155550333",
			Body:           "Signal is easier for me if you want to reply here.",
			TimestampMS:    1738959605000,
			Status:         "received",
			SourcePlatform: "signal",
			SourceID:       "seed-1",
		},
	}); err != nil {
		return err
	}
	if err := seedSyntheticConversation(store, &db.Conversation{
		ConversationID: "signal-group:4E8CCQ1ArzxJpbH53gUdo7SyJ/3d7wXnjOW/nTUdqDw=",
		Name:           "Strategy Lab",
		Participants:   `[{"name":"Devon Hart","number":"a1a98e48-7fa6-402e-9f62-b687098fed68"}]`,
		LastMessageTS:  1738960200000,
		UnreadCount:    0,
		SourcePlatform: "signal",
		IsGroup:        true,
	}, []*db.Message{
		{
			MessageID:      "signal:seed-group-1",
			ConversationID: "signal-group:4E8CCQ1ArzxJpbH53gUdo7SyJ/3d7wXnjOW/nTUdqDw=",
			SenderName:     "Devon Hart",
			SenderNumber:   "a1a98e48-7fa6-402e-9f62-b687098fed68",
			Body:           "Not directly related, but the recent logistics outage should update everyone on how quickly coordination software is becoming a strategic lever:",
			TimestampMS:    1738960200000,
			Status:         "received",
			SourcePlatform: "signal",
			SourceID:       "seed-group-1",
		},
	}); err != nil {
		return err
	}
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: pagedConversation,
		Name:           "Paged Thread",
		Participants:   `[{"name":"Pat Page","number":"+15550001111"}]`,
		LastMessageTS:  pagedMessageTimestamp(pagedConversationN),
		SourcePlatform: "sms",
	}); err != nil {
		return err
	}
	for i := 1; i <= pagedConversationN; i++ {
		if err := store.UpsertMessage(&db.Message{
			MessageID:      fmt.Sprintf("paged-%03d", i),
			ConversationID: pagedConversation,
			SenderName:     pagedSenderName(i),
			SenderNumber:   pagedSenderNumber(i),
			Body:           fmt.Sprintf("Paged message %03d", i),
			TimestampMS:    pagedMessageTimestamp(i),
			Status:         "delivered",
			IsFromMe:       i%2 == 0,
			SourcePlatform: "sms",
		}); err != nil {
			return err
		}
	}
	return nil
}

func seedSyntheticConversation(store *db.Store, convo *db.Conversation, msgs []*db.Message) error {
	if err := store.UpsertConversation(convo); err != nil {
		return err
	}
	for _, msg := range msgs {
		if err := store.UpsertMessage(msg); err != nil {
			return err
		}
	}
	return nil
}

func upsertSyntheticMessage(store *db.Store, conversationID, body string, timestampMS int64, isFromMe, mentionsMe bool, senderName, senderNumber, status string, id int64) (*db.Message, error) {
	platform := "sms"
	if conv, err := store.GetConversation(conversationID); err == nil && conv != nil && conv.SourcePlatform != "" {
		platform = conv.SourcePlatform
	}
	if strings.TrimSpace(status) == "" {
		status = syntheticStatus(isFromMe)
	}
	msg := &db.Message{
		MessageID:      fmt.Sprintf("e2e-%d", id),
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           body,
		TimestampMS:    timestampMS,
		Status:         status,
		IsFromMe:       isFromMe,
		MentionsMe:     mentionsMe,
		SourcePlatform: platform,
	}
	if err := store.UpsertMessage(msg); err != nil {
		return nil, err
	}

	conv, err := store.GetConversation(conversationID)
	if err != nil {
		conv = &db.Conversation{
			ConversationID: conversationID,
			Name:           senderName,
			Participants:   "[]",
			SourcePlatform: platform,
		}
	}
	conv.LastMessageTS = timestampMS
	if !isFromMe {
		conv.UnreadCount++
	}
	if err := store.UpsertConversation(conv); err != nil {
		return nil, err
	}
	return msg, nil
}

func setConversationPlatform(store *db.Store, conversationID, platform string) error {
	conv, err := store.GetConversation(conversationID)
	if err != nil || conv == nil {
		return err
	}
	conv.SourcePlatform = platform
	if err := store.UpsertConversation(conv); err != nil {
		return err
	}
	msgs, err := store.GetMessagesByConversation(conversationID, 1000)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		msg.SourcePlatform = platform
		if err := store.UpsertMessage(msg); err != nil {
			return err
		}
	}
	return nil
}

func pagedMessageTimestamp(i int) int64 {
	base := time.Date(2025, time.February, 5, 8, 0, 0, 0, time.UTC).UnixMilli()
	return base + int64(i*60_000)
}

func pagedSenderName(i int) string {
	if i%2 == 0 {
		return "Me"
	}
	return "Pat Page"
}

func pagedSenderNumber(i int) string {
	if i%2 == 0 {
		return "+15551234567"
	}
	return "+15550001111"
}

func serverPort() int {
	if raw := os.Getenv("OPENMESSAGES_E2E_PORT"); raw != "" {
		if port, err := strconv.Atoi(raw); err == nil && port > 0 {
			return port
		}
	}
	return defaultPort
}

func syntheticStatus(isFromMe bool) string {
	if isFromMe {
		return "OUTGOING_COMPLETE"
	}
	return "delivered"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
