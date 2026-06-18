package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/web"
)

const (
	defaultPort        = 7010
	pagedConversation  = "conv-paged"
	pagedConversationN = 150
)

func main() {
	logger := zerolog.Nop()
	store, err := db.New(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	if err := seedFixture(store); err != nil {
		panic(err)
	}

	events := web.NewEventBroker()
	var nextID atomic.Int64
	nextID.Store(time.Now().UnixNano())
	type mediaBlob struct {
		data []byte
		mime string
	}
	var mediaStore sync.Map
	base := web.APIHandlerWithOptions(store, nil, logger, nil, web.APIOptions{
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
		WhatsAppStatus: func() any {
			return map[string]any{"connected": true, "paired": true}
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
		events.PublishMessages(req.ConversationID)
		events.PublishConversations()
		writeJSON(w, map[string]any{
			"message_id": msg.MessageID,
			"success":    true,
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
		events.PublishDrafts(req.ConversationID)
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
		events.PublishTyping(req.ConversationID, req.SenderName, req.SenderNumber, req.Typing)
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

	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			base.ServeHTTP(w, r)
			return
		}
		var req struct {
			ConversationID string `json:"conversation_id"`
			Message        string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ConversationID == "" || req.Message == "" {
			http.Error(w, "conversation_id and message are required", http.StatusBadRequest)
			return
		}
		msg, err := upsertSyntheticMessage(store, req.ConversationID, req.Message, time.Now().UnixMilli(), true, false, "Me", "+15551234567", "", nextID.Add(1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events.PublishMessages(req.ConversationID)
		events.PublishConversations()
		writeJSON(w, map[string]any{
			"message_id": msg.MessageID,
			"status":     "SUCCESS",
			"success":    true,
		})
	})

	mux.HandleFunc("/api/drafts/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			base.ServeHTTP(w, r)
			return
		}
		var req struct {
			Body    string `json:"body"`
			DraftID string `json:"draft_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.DraftID == "" || req.Body == "" {
			http.Error(w, "draft_id and body are required", http.StatusBadRequest)
			return
		}
		draft, err := store.GetDraft(req.DraftID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if draft == nil {
			http.Error(w, "draft not found", http.StatusNotFound)
			return
		}
		msg, err := upsertSyntheticMessage(store, draft.ConversationID, req.Body, time.Now().UnixMilli(), true, false, "Me", "+15551234567", "", nextID.Add(1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := store.DeleteDraft(req.DraftID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		events.PublishMessages(draft.ConversationID)
		events.PublishDrafts(draft.ConversationID)
		events.PublishConversations()
		writeJSON(w, map[string]any{
			"message_id": msg.MessageID,
			"status":     "SUCCESS",
			"success":    true,
		})
	})

	mux.Handle("/", base)

	addr := "127.0.0.1:" + strconv.Itoa(serverPort())
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
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
		Participants:   `[{"name":"Jordan Rivera","number":"+14155550199"}]`,
		LastMessageTS:  1738959300000,
		SourcePlatform: "sms",
	}, []*db.Message{
		{
			MessageID:      "m9a",
			ConversationID: "conv9",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14155550199",
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
		ConversationID: "conv10",
		Name:           "Jordan Rivera",
		Participants:   `[{"name":"Jordan Rivera","number":"+14155550199"}]`,
		LastMessageTS:  1738959900000,
		UnreadCount:    1,
		SourcePlatform: "whatsapp",
	}, []*db.Message{
		{
			MessageID:      "m10a",
			ConversationID: "conv10",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14155550199",
			Body:           "Sent the menu here too in case WhatsApp is easier.",
			TimestampMS:    1738959000000,
			Status:         "delivered",
			SourcePlatform: "whatsapp",
		},
		{
			MessageID:      "m10b",
			ConversationID: "conv10",
			SenderName:     "Jordan Rivera",
			SenderNumber:   "+14155550199",
			Body:           "Also, do you want me to bring dessert?",
			TimestampMS:    1738959900000,
			Status:         "delivered",
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
