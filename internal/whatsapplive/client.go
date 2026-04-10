package whatsapplive

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

const recentIncomingWindow = 2 * time.Minute
const avatarCacheTTL = 30 * time.Minute
const unavailablePlaceholderRepairCooldown = 30 * time.Minute
const maxUnavailablePlaceholderRepairs = 25
const recentlyLeftGroupTTL = 6 * time.Hour

var ErrProfilePhotoNotFound = errors.New("whatsapp profile photo not found")

var sendTextMessage = func(cli *whatsmeow.Client, ctx context.Context, to watypes.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return cli.SendMessage(ctx, to, message, extra...)
}

var uploadMedia = func(cli *whatsmeow.Client, ctx context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return cli.Upload(ctx, plaintext, mediaType)
}

var downloadMediaWithPath = func(cli *whatsmeow.Client, ctx context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, fileLength int, mediaType whatsmeow.MediaType, fileEncSHA256B64 string) ([]byte, error) {
	return cli.DownloadMediaWithPath(ctx, directPath, encFileHash, fileHash, mediaKey, fileLength, mediaType, fileEncSHA256B64)
}

var connectClient = func(cli *whatsmeow.Client) error {
	return cli.Connect()
}

var launchConnect = func(b *Bridge, cli *whatsmeow.Client) {
	go b.runConnect(cli)
}

var clientIsConnected = func(cli *whatsmeow.Client) bool {
	return cli != nil && cli.IsConnected()
}

var getProfilePictureInfo = func(cli *whatsmeow.Client, ctx context.Context, jid watypes.JID, params *whatsmeow.GetProfilePictureParams) (*watypes.ProfilePictureInfo, error) {
	return cli.GetProfilePictureInfo(ctx, jid, params)
}

var leaveGroup = func(cli *whatsmeow.Client, ctx context.Context, jid watypes.JID) error {
	return cli.LeaveGroup(ctx, jid)
}

var downloadProfilePhoto = func(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, "", err
	}
	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mime == "" && len(data) > 0 {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

type storedMediaRef struct {
	URL           string `json:"url,omitempty"`
	DirectPath    string `json:"direct_path,omitempty"`
	FileSHA256    string `json:"file_sha256,omitempty"`
	FileEncSHA256 string `json:"file_enc_sha256,omitempty"`
	FileLength    uint64 `json:"file_length,omitempty"`
}

type Callbacks struct {
	OnConversationsChange func()
	OnIncomingMessage     func(*db.Message)
	OnMessagesChange      func(string)
	OnStatusChange        func()
	OnTypingChange        func(conversationID, senderName, senderNumber string, typing bool)
}

type StatusSnapshot struct {
	Connected   bool   `json:"connected"`
	Connecting  bool   `json:"connecting"`
	Paired      bool   `json:"paired"`
	Pairing     bool   `json:"pairing"`
	AccountJID  string `json:"account_jid,omitempty"`
	PushName    string `json:"push_name,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	QRAvailable bool   `json:"qr_available"`
	QREvent     string `json:"qr_event,omitempty"`
	QRUpdatedAt int64  `json:"qr_updated_at,omitempty"`
}

type QRSnapshot struct {
	Event      string `json:"event,omitempty"`
	Error      string `json:"error,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	PNGDataURL string `json:"png_data_url,omitempty"`

	Code string `json:"-"`
}

type participantJSON struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	IsMe   bool   `json:"is_me,omitempty"`
}

type storedReaction struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

type avatarCacheEntry struct {
	ProfileID string
	MIME      string
	Data      []byte
	FetchedAt time.Time
	Missing   bool
}

type unavailableRepairTarget struct {
	chatJID   watypes.JID
	senderJID watypes.JID
}

type Bridge struct {
	mu          sync.RWMutex
	store       *db.Store
	logger      zerolog.Logger
	sessionPath string
	callbacks   Callbacks

	container *sqlstore.Container
	client    *whatsmeow.Client

	connected                 bool
	connecting                bool
	pairing                   bool
	lastError                 string
	qr                        QRSnapshot
	avatars                   map[string]avatarCacheEntry
	unavailableRepairRequests map[string]time.Time
	recentlyLeftGroups        map[string]time.Time
}

func New(sessionPath string, store *db.Store, logger zerolog.Logger, callbacks Callbacks) (*Bridge, error) {
	bridge := &Bridge{
		store:              store,
		logger:             logger,
		sessionPath:        sessionPath,
		callbacks:          callbacks,
		recentlyLeftGroups: make(map[string]time.Time),
	}
	if err := bridge.initClientLocked(); err != nil {
		return nil, err
	}
	return bridge, nil
}

func (b *Bridge) initClientLocked() error {
	if b.client != nil {
		return nil
	}
	container, err := sqlstore.New(context.Background(), "sqlite", sessionStoreDSN(b.sessionPath), waLog.Noop)
	if err != nil {
		return fmt.Errorf("open WhatsApp session store: %w", err)
	}
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		container.Close()
		return fmt.Errorf("load WhatsApp device store: %w", err)
	}
	cli := whatsmeow.NewClient(deviceStore, waLog.Noop)
	cli.EnableAutoReconnect = true
	cli.InitialAutoReconnect = true
	cli.AddEventHandler(b.handleEvent)
	b.container = container
	b.client = cli
	return nil
}

func sessionStoreDSN(path string) string {
	return (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
	}).String()
}

func (b *Bridge) resetClientLocked() error {
	if b.client != nil {
		b.client.Disconnect()
		b.client = nil
	}
	if b.container != nil {
		if err := b.container.Close(); err != nil {
			b.logger.Warn().Err(err).Msg("Failed to close WhatsApp session store")
		}
		b.container = nil
	}
	return b.initClientLocked()
}

func (b *Bridge) recoverPersistedSessionLocked() error {
	if err := b.initClientLocked(); err != nil {
		return err
	}
	if b.client == nil || b.client.Store == nil || b.client.Store.ID != nil {
		return nil
	}
	return b.resetClientLocked()
}

func (b *Bridge) pairedDeviceStoreLocked() *wastore.Device {
	if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
		return b.client.Store
	}
	if b.container == nil {
		return nil
	}
	deviceStore, err := b.container.GetFirstDevice(context.Background())
	if err != nil {
		b.logger.Debug().Err(err).Msg("Failed to inspect persisted WhatsApp session state")
		return nil
	}
	if deviceStore == nil || deviceStore.ID == nil {
		return nil
	}
	return deviceStore
}

func (b *Bridge) ConnectIfPaired() error {
	b.mu.Lock()
	if err := b.recoverPersistedSessionLocked(); err != nil {
		b.mu.Unlock()
		return err
	}
	if b.client == nil || clientIsConnected(b.client) || b.client.Store.ID == nil || b.pairing || b.connecting {
		b.mu.Unlock()
		return nil
	}
	cli := b.client
	b.lastError = ""
	b.connecting = true
	b.mu.Unlock()
	b.emitStatusChange()
	launchConnect(b, cli)
	return nil
}

func (b *Bridge) Connect() error {
	b.mu.Lock()
	if err := b.recoverPersistedSessionLocked(); err != nil {
		b.mu.Unlock()
		return err
	}
	if b.client == nil || clientIsConnected(b.client) || b.pairing || b.connecting {
		b.mu.Unlock()
		b.emitStatusChange()
		return nil
	}
	cli := b.client
	paired := cli.Store.ID != nil
	b.lastError = ""

	if !paired {
		qrChan, err := cli.GetQRChannel(context.Background())
		if err != nil {
			b.lastError = err.Error()
			b.mu.Unlock()
			b.emitStatusChange()
			return fmt.Errorf("start WhatsApp pairing: %w", err)
		}
		b.pairing = true
		b.connecting = true
		b.qr = QRSnapshot{}
		b.mu.Unlock()
		b.emitStatusChange()
		go b.consumeQR(qrChan)
		launchConnect(b, cli)
		return nil
	}

	b.connecting = true
	b.mu.Unlock()
	b.emitStatusChange()
	launchConnect(b, cli)
	return nil
}

func (b *Bridge) runConnect(cli *whatsmeow.Client) {
	if err := connectClient(cli); err != nil {
		b.logger.Warn().Err(err).Msg("WhatsApp connect failed")
		b.mu.Lock()
		if b.client == cli {
			b.connecting = false
			b.lastError = err.Error()
			if b.client.Store.ID == nil {
				b.pairing = false
			}
		}
		b.mu.Unlock()
		b.emitStatusChange()
	}
}

func (b *Bridge) consumeQR(ch <-chan whatsmeow.QRChannelItem) {
	for item := range ch {
		snap := QRSnapshot{
			Event:     item.Event,
			UpdatedAt: time.Now().UnixMilli(),
		}
		if item.Timeout > 0 {
			snap.ExpiresAt = time.Now().Add(item.Timeout).UnixMilli()
		}
		if item.Error != nil {
			snap.Error = item.Error.Error()
		}
		if item.Event == whatsmeow.QRChannelEventCode {
			snap.Code = item.Code
		}

		b.mu.Lock()
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			b.qr = snap
		default:
			b.qr = snap
			b.pairing = false
			b.connecting = false
			if snap.Error != "" {
				b.lastError = snap.Error
			} else if item.Event != whatsmeow.QRChannelSuccess.Event {
				b.lastError = item.Event
			}
		}
		b.mu.Unlock()
		b.emitStatusChange()
	}
}

func (b *Bridge) Unpair() error {
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()

	if cli != nil && cli.Store != nil && cli.Store.ID != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if clientIsConnected(cli) {
			if err := cli.Logout(ctx); err != nil {
				b.logger.Warn().Err(err).Msg("WhatsApp logout failed; clearing local session anyway")
			}
		} else if err := cli.Store.Delete(ctx); err != nil {
			b.logger.Warn().Err(err).Msg("Failed to clear WhatsApp session store")
		}
	}

	b.mu.Lock()
	b.connected = false
	b.connecting = false
	b.pairing = false
	b.lastError = ""
	b.qr = QRSnapshot{}
	err := b.resetClientLocked()
	b.mu.Unlock()
	b.emitStatusChange()
	if err != nil {
		return fmt.Errorf("reset WhatsApp bridge: %w", err)
	}
	return nil
}

func (b *Bridge) Status() StatusSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	status := StatusSnapshot{
		Connected:  b.connected && clientIsConnected(b.client),
		Connecting: b.connecting,
		Pairing:    b.pairing,
		LastError:  b.lastError,
	}
	if deviceStore := b.pairedDeviceStoreLocked(); deviceStore != nil {
		status.Paired = true
		status.AccountJID = deviceStore.ID.String()
		status.PushName = strings.TrimSpace(deviceStore.PushName)
	}
	status.QRAvailable = b.qr.Code != ""
	status.QREvent = b.qr.Event
	status.QRUpdatedAt = b.qr.UpdatedAt
	return status
}

func (b *Bridge) ensureSendClient(wait time.Duration, reason string) (*whatsmeow.Client, error) {
	if cli := b.currentSendClient(); cli != nil {
		return cli, nil
	}
	if err := b.beginReconnect(reason, false); err != nil {
		return nil, err
	}
	return b.waitForConnectedClient(wait)
}

func (b *Bridge) currentSendClient() *whatsmeow.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.client == nil || !b.connected || !clientIsConnected(b.client) {
		return nil
	}
	return b.client
}

func (b *Bridge) beginReconnect(reason string, force bool) error {
	b.mu.Lock()
	if err := b.recoverPersistedSessionLocked(); err != nil {
		b.mu.Unlock()
		return err
	}
	if b.client == nil || b.client.Store == nil || b.client.Store.ID == nil {
		b.mu.Unlock()
		return errors.New("whatsapp live sync is not connected")
	}
	if !force && b.connected && clientIsConnected(b.client) {
		b.mu.Unlock()
		return nil
	}
	if b.connecting {
		b.mu.Unlock()
		return nil
	}
	cli := b.client
	b.connected = false
	b.connecting = true
	b.pairing = false
	if reason != "" {
		b.lastError = reason
	}
	b.mu.Unlock()
	b.emitStatusChange()
	launchConnect(b, cli)
	return nil
}

func (b *Bridge) waitForConnectedClient(wait time.Duration) (*whatsmeow.Client, error) {
	deadline := time.Now().Add(wait)
	for {
		b.mu.RLock()
		cli := b.client
		connected := b.connected && clientIsConnected(cli)
		connecting := b.connecting
		lastError := b.lastError
		paired := cli != nil && cli.Store != nil && cli.Store.ID != nil
		b.mu.RUnlock()

		if connected && cli != nil {
			return cli, nil
		}
		if !paired {
			if lastError != "" {
				return nil, errors.New(lastError)
			}
			return nil, errors.New("whatsapp live sync is not connected")
		}
		if time.Now().After(deadline) {
			if lastError != "" {
				return nil, fmt.Errorf("whatsapp reconnect: %s", lastError)
			}
			if connecting {
				return nil, errors.New("whatsapp reconnect timed out")
			}
			return nil, errors.New("whatsapp live sync is not connected")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func shouldReconnectWhatsAppSend(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "websocket") ||
		strings.Contains(msg, "not connected")
}

func (b *Bridge) QRCode() (QRSnapshot, error) {
	b.mu.RLock()
	snap := b.qr
	b.mu.RUnlock()
	if snap.Code == "" {
		return snap, fmt.Errorf("no active WhatsApp QR code")
	}
	code, err := qr.Encode(snap.Code, qr.M)
	if err != nil {
		return QRSnapshot{}, fmt.Errorf("encode WhatsApp QR: %w", err)
	}
	snap.PNGDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	return snap, nil
}

func (b *Bridge) UsesLiveSession() bool {
	status := b.Status()
	return status.Paired || status.Connected || status.Pairing
}

func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		b.client.Disconnect()
		b.client = nil
	}
	b.connected = false
	b.connecting = false
	b.pairing = false
	if b.container != nil {
		err := b.container.Close()
		b.container = nil
		return err
	}
	return nil
}

func (b *Bridge) SendText(conversationID, body, replyToID string) (*db.Message, error) {
	body = strings.TrimSpace(body)
	if conversationID == "" || body == "" {
		return nil, errors.New("conversation_id and body are required")
	}

	chatJID, err := parseConversationJID(conversationID)
	if err != nil {
		return nil, err
	}

	cli, err := b.ensureSendClient(15*time.Second, "reconnecting WhatsApp before send")
	if err != nil {
		return nil, err
	}

	reqID := cli.GenerateMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp, err := sendTextMessage(cli, ctx, chatJID, outgoingTextMessage(body, replyToID), whatsmeow.SendRequestExtra{
		ID: reqID,
	})
	if err != nil {
		if shouldReconnectWhatsAppSend(err) {
			if reconnectErr := b.beginReconnect("WhatsApp send timed out; reconnecting", true); reconnectErr != nil {
				b.logger.Debug().Err(reconnectErr).Msg("Failed to start WhatsApp reconnect after send error")
			}
		}
		return nil, fmt.Errorf("send WhatsApp message: %w", err)
	}

	now := time.Now().UnixMilli()
	if !resp.Timestamp.IsZero() {
		now = resp.Timestamp.UnixMilli()
	}
	messageID := string(resp.ID)
	if messageID == "" {
		messageID = string(reqID)
	}

	senderName := "Me"
	senderNumber := ""
	if cli.Store != nil {
		if push := strings.TrimSpace(cli.Store.PushName); push != "" {
			senderName = push
		}
		if cli.Store.ID != nil {
			senderNumber = jidToPhone(*cli.Store.ID)
		}
	}

	return &db.Message{
		MessageID:      "whatsapp:" + messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           body,
		TimestampMS:    now,
		Status:         "sent",
		IsFromMe:       true,
		ReplyToID:      normalizeReplyToID(replyToID),
		SourcePlatform: "whatsapp",
		SourceID:       messageID,
	}, nil
}

func (b *Bridge) SendMedia(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	if conversationID == "" || len(data) == 0 {
		return nil, errors.New("conversation_id and file are required")
	}
	caption = strings.TrimSpace(caption)
	replyToID = normalizeReplyToID(replyToID)
	chatJID, err := parseConversationJID(conversationID)
	if err != nil {
		return nil, err
	}
	mediaType, err := mediaTypeForMIME(mime)
	if err != nil {
		return nil, err
	}

	cli, err := b.ensureSendClient(30*time.Second, "reconnecting WhatsApp before media send")
	if err != nil {
		return nil, err
	}

	uploadCtx, cancelUpload := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelUpload()

	upload, err := uploadMedia(cli, uploadCtx, data, mediaType)
	if err != nil {
		if shouldReconnectWhatsAppSend(err) {
			if reconnectErr := b.beginReconnect("WhatsApp media upload timed out; reconnecting", true); reconnectErr != nil {
				b.logger.Debug().Err(reconnectErr).Msg("Failed to start WhatsApp reconnect after media upload error")
			}
		}
		return nil, fmt.Errorf("upload WhatsApp media: %w", err)
	}
	reqID := cli.GenerateMessageID()
	sendCtx, cancelSend := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelSend()

	resp, err := sendTextMessage(cli, sendCtx, chatJID, outgoingMediaMessage(upload, mime, filename, mediaType, caption, replyToID), whatsmeow.SendRequestExtra{
		ID: reqID,
	})
	if err != nil {
		if shouldReconnectWhatsAppSend(err) {
			if reconnectErr := b.beginReconnect("WhatsApp media send timed out; reconnecting", true); reconnectErr != nil {
				b.logger.Debug().Err(reconnectErr).Msg("Failed to start WhatsApp reconnect after media send error")
			}
		}
		return nil, fmt.Errorf("send WhatsApp media: %w", err)
	}

	now := time.Now().UnixMilli()
	if !resp.Timestamp.IsZero() {
		now = resp.Timestamp.UnixMilli()
	}
	messageID := string(resp.ID)
	if messageID == "" {
		messageID = string(reqID)
	}

	senderName := "Me"
	senderNumber := ""
	if cli.Store != nil {
		if push := strings.TrimSpace(cli.Store.PushName); push != "" {
			senderName = push
		}
		if cli.Store.ID != nil {
			senderNumber = jidToPhone(*cli.Store.ID)
		}
	}

	return &db.Message{
		MessageID:      "whatsapp:" + messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           caption,
		TimestampMS:    now,
		Status:         "sent",
		IsFromMe:       true,
		MediaID:        encodeStoredMediaRef(storedMediaRefFromUpload(upload)),
		MimeType:       mime,
		DecryptionKey:  encodeBytes(upload.MediaKey),
		ReplyToID:      replyToID,
		SourcePlatform: "whatsapp",
		SourceID:       messageID,
	}, nil
}

func (b *Bridge) SendReaction(conversationID, targetMessageID, emoji, action string) error {
	targetMessageID = strings.TrimSpace(targetMessageID)
	if targetMessageID == "" {
		return errors.New("whatsapp target message is required")
	}
	emoji = strings.TrimSpace(emoji)
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "add"
	}
	if emoji == "" {
		return errors.New("whatsapp reaction emoji is required")
	}

	target, err := b.store.GetMessageByID(targetMessageID)
	if err != nil {
		return fmt.Errorf("load WhatsApp reaction target: %w", err)
	}
	if target == nil && !strings.HasPrefix(targetMessageID, "whatsapp:") {
		target, err = b.store.GetMessageByID("whatsapp:" + targetMessageID)
		if err != nil {
			return fmt.Errorf("load WhatsApp reaction target: %w", err)
		}
	}
	if target == nil || target.SourcePlatform != "whatsapp" {
		return errors.New("whatsapp reaction target not found")
	}

	targetConversationID := strings.TrimSpace(target.ConversationID)
	if targetConversationID == "" {
		targetConversationID = strings.TrimSpace(conversationID)
	}
	if targetConversationID == "" {
		return errors.New("whatsapp reaction conversation is required")
	}

	chatJID, err := parseConversationJID(targetConversationID)
	if err != nil {
		return err
	}
	chatJID = b.normalizeConversationJID(chatJID)

	targetSourceID := strings.TrimSpace(target.SourceID)
	if targetSourceID == "" {
		targetSourceID = strings.TrimSpace(strings.TrimPrefix(target.MessageID, "whatsapp:"))
	}
	if targetSourceID == "" {
		return errors.New("whatsapp reaction target id is unavailable")
	}

	cli, err := b.ensureSendClient(15*time.Second, "reconnecting WhatsApp before reaction")
	if err != nil {
		return err
	}

	reactionText := emoji
	if action == "remove" {
		reactionText = ""
	}
	reqID := cli.GenerateMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	_, err = sendTextMessage(cli, ctx, chatJID, cli.BuildReaction(chatJID, b.reactionTargetSenderJID(target, chatJID), watypes.MessageID(targetSourceID), reactionText), whatsmeow.SendRequestExtra{
		ID: reqID,
	})
	if err != nil {
		if shouldReconnectWhatsAppSend(err) {
			if reconnectErr := b.beginReconnect("WhatsApp reaction timed out; reconnecting", true); reconnectErr != nil {
				b.logger.Debug().Err(reconnectErr).Msg("Failed to start WhatsApp reconnect after reaction error")
			}
		}
		return fmt.Errorf("send WhatsApp reaction: %w", err)
	}

	nextReactions, changed, err := updateStoredReactions(target.Reactions, b.reactionActorIDForClient(cli), reactionText)
	if err != nil {
		return fmt.Errorf("update local WhatsApp reaction state: %w", err)
	}
	if !changed {
		return nil
	}
	target.Reactions = nextReactions
	if err := b.store.UpsertMessage(target); err != nil {
		return fmt.Errorf("store WhatsApp reaction update: %w", err)
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(target.ConversationID)
	}
	return nil
}

func (b *Bridge) LeaveGroup(conversationID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation_id is required")
	}
	chatJID, err := parseConversationJID(conversationID)
	if err != nil {
		return err
	}
	chatJID = b.normalizeConversationJID(chatJID)
	if chatJID.Server != watypes.GroupServer {
		return errors.New("conversation is not a WhatsApp group")
	}

	cli, err := b.ensureSendClient(30*time.Second, "reconnecting WhatsApp before leaving group")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := leaveGroup(cli, ctx, chatJID); err != nil {
		if shouldReconnectWhatsAppSend(err) {
			if reconnectErr := b.beginReconnect("WhatsApp leave group timed out; reconnecting", true); reconnectErr != nil {
				b.logger.Debug().Err(reconnectErr).Msg("Failed to start WhatsApp reconnect after leave-group error")
			}
		}
		return fmt.Errorf("leave WhatsApp group: %w", err)
	}

	b.markLeftGroup(conversationID)
	if err := b.store.DeleteConversation(conversationID); err != nil {
		return fmt.Errorf("remove local WhatsApp group thread: %w", err)
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(conversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}

	return nil
}

func (b *Bridge) DownloadStoredMedia(msg *db.Message) ([]byte, string, error) {
	if msg == nil {
		return nil, "", errors.New("message not found")
	}
	if whatsappmedia.IsLocalMediaRef(msg.MediaID) {
		return downloadLocalWhatsAppMedia(msg.MediaID, msg.MimeType)
	}
	ref, err := decodeStoredMediaRef(msg.MediaID)
	if err != nil {
		return nil, "", err
	}
	mediaType, err := mediaTypeForMIME(msg.MimeType)
	if err != nil {
		return nil, "", err
	}
	mediaKey, err := decodeHexBytes(msg.DecryptionKey)
	if err != nil {
		return nil, "", fmt.Errorf("invalid WhatsApp media key: %w", err)
	}
	fileEncSHA256, err := decodeHexBytes(ref.FileEncSHA256)
	if err != nil {
		return nil, "", fmt.Errorf("invalid WhatsApp media enc hash: %w", err)
	}
	fileSHA256, err := decodeHexBytes(ref.FileSHA256)
	if err != nil {
		return nil, "", fmt.Errorf("invalid WhatsApp media hash: %w", err)
	}

	b.mu.RLock()
	cli := b.client
	connected := b.connected
	b.mu.RUnlock()
	if cli == nil || !connected || !clientIsConnected(cli) {
		return nil, "", errors.New("whatsapp live sync is not connected")
	}
	if ref.DirectPath == "" {
		return nil, "", errors.New("whatsapp media is missing a download path")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := downloadMediaWithPath(cli, ctx, ref.DirectPath, fileEncSHA256, fileSHA256, mediaKey, int(ref.FileLength), mediaType, "")
	if err != nil {
		return nil, "", fmt.Errorf("download WhatsApp media: %w", err)
	}
	return data, msg.MimeType, nil
}

func downloadLocalWhatsAppMedia(mediaID, currentMIME string) ([]byte, string, error) {
	path, err := whatsappmedia.ResolveLocalMediaPath("", mediaID)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read local WhatsApp media: %w", err)
	}
	mimeType := strings.TrimSpace(currentMIME)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if mimeType == "" && len(data) > 0 {
		mimeType = http.DetectContentType(data)
	}
	return data, mimeType, nil
}

func (b *Bridge) ProfilePhoto(conversationID string) ([]byte, string, error) {
	jid, err := parseConversationJID(conversationID)
	if err != nil {
		return nil, "", err
	}
	jid = b.canonicalJID(jid)
	cacheKey := waConversationID(jid)

	cached, hasCached := b.avatarCacheEntry(cacheKey)
	if hasCached && time.Since(cached.FetchedAt) < avatarCacheTTL {
		if cached.Missing {
			return nil, "", ErrProfilePhotoNotFound
		}
		if len(cached.Data) > 0 {
			return cloneBytes(cached.Data), cached.MIME, nil
		}
	}

	b.mu.RLock()
	cli := b.client
	connected := b.connected
	b.mu.RUnlock()
	if cli == nil || !connected || !clientIsConnected(cli) {
		if hasCached && len(cached.Data) > 0 {
			return cloneBytes(cached.Data), cached.MIME, nil
		}
		return nil, "", errors.New("whatsapp live sync is not connected")
	}

	params := &whatsmeow.GetProfilePictureParams{Preview: true}
	if hasCached && cached.ProfileID != "" {
		params.ExistingID = cached.ProfileID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	info, err := getProfilePictureInfo(cli, ctx, jid, params)
	if err != nil {
		if hasCached && len(cached.Data) > 0 {
			return cloneBytes(cached.Data), cached.MIME, nil
		}
		return nil, "", fmt.Errorf("fetch WhatsApp profile photo: %w", err)
	}
	if info == nil {
		if hasCached && cached.ProfileID != "" && len(cached.Data) > 0 {
			b.storeAvatarCache(cacheKey, avatarCacheEntry{
				ProfileID: cached.ProfileID,
				MIME:      cached.MIME,
				Data:      cloneBytes(cached.Data),
				FetchedAt: time.Now(),
			})
			return cloneBytes(cached.Data), cached.MIME, nil
		}
		b.storeAvatarCache(cacheKey, avatarCacheEntry{
			FetchedAt: time.Now(),
			Missing:   true,
		})
		return nil, "", ErrProfilePhotoNotFound
	}
	if strings.TrimSpace(info.URL) == "" {
		b.storeAvatarCache(cacheKey, avatarCacheEntry{
			ProfileID: strings.TrimSpace(info.ID),
			FetchedAt: time.Now(),
			Missing:   true,
		})
		return nil, "", ErrProfilePhotoNotFound
	}

	data, mime, err := downloadProfilePhoto(ctx, info.URL)
	if err != nil {
		if hasCached && len(cached.Data) > 0 {
			return cloneBytes(cached.Data), cached.MIME, nil
		}
		return nil, "", fmt.Errorf("download WhatsApp profile photo: %w", err)
	}
	if len(data) == 0 {
		b.storeAvatarCache(cacheKey, avatarCacheEntry{
			ProfileID: strings.TrimSpace(info.ID),
			FetchedAt: time.Now(),
			Missing:   true,
		})
		return nil, "", ErrProfilePhotoNotFound
	}

	entry := avatarCacheEntry{
		ProfileID: strings.TrimSpace(info.ID),
		MIME:      mime,
		Data:      cloneBytes(data),
		FetchedAt: time.Now(),
	}
	b.storeAvatarCache(cacheKey, entry)
	return data, mime, nil
}

func (b *Bridge) handleEvent(raw any) {
	switch evt := raw.(type) {
	case *waevents.Connected:
		b.handleConnected()
	case *waevents.Disconnected:
		b.handleDisconnected("")
	case *waevents.LoggedOut:
		b.handleDisconnected(evt.PermanentDisconnectDescription())
		go b.reinitializeAfterLogout()
	case *waevents.StreamReplaced:
		b.handleDisconnected("stream replaced")
	case *waevents.ClientOutdated:
		b.handleDisconnected("client outdated")
	case *waevents.ConnectFailure:
		b.handleDisconnected(evt.PermanentDisconnectDescription())
	case *waevents.TemporaryBan:
		b.handleDisconnected(evt.PermanentDisconnectDescription())
	case *waevents.PairSuccess:
		b.handlePairSuccess(evt)
	case *waevents.PairError:
		b.handlePairError(evt.Error)
	case *waevents.QRScannedWithoutMultidevice:
		b.handlePairError(fmt.Errorf("scan the QR code again after enabling multi-device support in WhatsApp"))
	case *waevents.Message:
		b.handleMessage(evt)
	case *waevents.Receipt:
		b.handleReceipt(evt)
	case *waevents.ChatPresence:
		b.handleChatPresence(evt)
	case *waevents.GroupInfo:
		b.handleGroupInfo(evt)
	case *waevents.HistorySync:
		b.handleHistorySync(evt)
	default:
		b.logger.Debug().Type("type", evt).Msg("Unhandled WhatsApp event")
	}
}

func (b *Bridge) handleConnected() {
	b.mu.Lock()
	b.connected = true
	b.connecting = false
	b.pairing = false
	b.lastError = ""
	b.qr = QRSnapshot{}
	cli := b.client
	b.mu.Unlock()

	if cli != nil {
		if err := cli.SendPresence(context.Background(), watypes.PresenceAvailable); err != nil {
			b.logger.Debug().Err(err).Msg("Failed to mark WhatsApp bridge as online for typing updates")
		}
	}
	go b.reconcileStoredDirectChats()
	go b.repairUnavailableMediaPlaceholdersAsync()
	b.emitStatusChange()
}

func (b *Bridge) handleDisconnected(reason string) {
	b.mu.Lock()
	b.connected = false
	b.connecting = false
	if b.client != nil && b.client.Store != nil && b.client.Store.ID == nil {
		b.pairing = false
	}
	if reason != "" {
		b.lastError = reason
	}
	b.mu.Unlock()
	b.emitStatusChange()
}

func (b *Bridge) handlePairSuccess(_ *waevents.PairSuccess) {
	b.mu.Lock()
	b.pairing = false
	b.lastError = ""
	b.qr = QRSnapshot{}
	b.mu.Unlock()
	b.emitStatusChange()
}

func (b *Bridge) handlePairError(err error) {
	b.mu.Lock()
	b.pairing = false
	b.connecting = false
	b.connected = false
	b.qr = QRSnapshot{}
	if err != nil {
		b.lastError = err.Error()
	}
	b.mu.Unlock()
	b.emitStatusChange()
}

func (b *Bridge) reinitializeAfterLogout() {
	b.mu.Lock()
	if err := b.resetClientLocked(); err != nil {
		b.lastError = err.Error()
		b.mu.Unlock()
		b.emitStatusChange()
		return
	}
	b.mu.Unlock()
	b.emitStatusChange()
}

func (b *Bridge) handleMessage(evt *waevents.Message) {
	if evt == nil || evt.Message == nil {
		return
	}
	chatJID := b.normalizeConversationJID(evt.Info.Chat)
	if evt.Info.IsGroup && b.shouldSuppressLeftGroup(waConversationID(chatJID)) {
		return
	}
	if b.handleReactionMessage(evt) {
		return
	}

	body := extractMessageBody(evt.Message)
	if body == "" {
		return
	}

	conv, err := b.upsertConversationForMessage(evt)
	if err != nil {
		b.logger.Warn().Err(err).Str("chat", evt.Info.Chat.String()).Msg("Failed to upsert WhatsApp conversation")
	}
	senderName, senderNumber := b.resolveSender(evt, conv)
	msg := &db.Message{
		MessageID:      "whatsapp:" + string(evt.Info.ID),
		ConversationID: waConversationID(chatJID),
		SenderName:     senderName,
		SenderNumber:   senderNumber,
		Body:           body,
		TimestampMS:    evt.Info.Timestamp.UnixMilli(),
		Status:         "delivered",
		IsFromMe:       evt.Info.IsFromMe,
		MentionsMe:     b.messageMentionsOwnAccount(evt.Message),
		ReplyToID:      extractReplyToID(evt.Message),
		SourcePlatform: "whatsapp",
		SourceID:       string(evt.Info.ID),
	}
	if mediaRef, mediaKey, mimeType, ok := extractStoredMediaRef(evt.Message); ok {
		msg.MediaID = encodeStoredMediaRef(mediaRef)
		msg.DecryptionKey = encodeBytes(mediaKey)
		msg.MimeType = mimeType
	}

	if err := b.store.UpsertMessage(msg); err != nil {
		b.logger.Error().Err(err).Str("msg_id", msg.MessageID).Msg("Failed to store WhatsApp message")
		return
	}
	if msg.MediaID != "" {
		if placeholder, err := b.store.FindUnresolvedWhatsAppPlaceholderAlias(msg.ConversationID, msg.TimestampMS, msg.Body, msg.SourceID); err != nil {
			b.logger.Debug().Err(err).Str("msg_id", msg.MessageID).Msg("Failed to look for WhatsApp placeholder alias")
		} else if placeholder != nil {
			if err := b.store.DeleteMessageByID(placeholder.MessageID); err != nil {
				b.logger.Debug().Err(err).Str("placeholder_msg_id", placeholder.MessageID).Msg("Failed to delete superseded WhatsApp placeholder")
			}
		}
	}
	b.clearUnavailableRepairRequestsForSource(msg.SourceID)
	if err := b.store.BumpConversationTimestamp(msg.ConversationID, msg.TimestampMS); err != nil {
		b.logger.Warn().Err(err).Str("conv_id", msg.ConversationID).Msg("Failed to update WhatsApp conversation timestamp")
	}

	if !msg.IsFromMe && isRecentMessage(evt.Info.Timestamp) && b.callbacks.OnIncomingMessage != nil {
		b.callbacks.OnIncomingMessage(msg)
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(msg.ConversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
}

func (b *Bridge) handleReactionMessage(evt *waevents.Message) bool {
	reaction := extractReactionMessage(evt.Message)
	if reaction == nil {
		return false
	}

	targetID := strings.TrimSpace(reaction.GetKey().GetID())
	if targetID == "" {
		return true
	}

	msg, err := b.store.GetMessageByID("whatsapp:" + targetID)
	if err != nil {
		b.logger.Warn().Err(err).Str("target_msg_id", targetID).Msg("Failed to load WhatsApp reaction target")
		return true
	}
	if msg == nil {
		b.logger.Debug().Str("target_msg_id", targetID).Msg("Skipping WhatsApp reaction for unknown target message")
		return true
	}

	nextReactions, changed, err := updateStoredReactions(msg.Reactions, b.reactionActorID(evt), reaction.GetText())
	if err != nil {
		b.logger.Warn().Err(err).Str("target_msg_id", targetID).Msg("Failed to apply WhatsApp reaction")
		return true
	}
	if !changed {
		return true
	}

	msg.Reactions = nextReactions
	if err := b.store.UpsertMessage(msg); err != nil {
		b.logger.Warn().Err(err).Str("target_msg_id", targetID).Msg("Failed to store WhatsApp reaction update")
		return true
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(msg.ConversationID)
	}
	return true
}

func (b *Bridge) handleReceipt(evt *waevents.Receipt) {
	if evt == nil || len(evt.MessageIDs) == 0 {
		return
	}
	nextStatus, ok := receiptStatus(evt.Type)
	if !ok {
		return
	}

	updatedConversations := map[string]struct{}{}
	for _, id := range evt.MessageIDs {
		msgID := "whatsapp:" + string(id)
		msg, err := b.store.GetMessageByID(msgID)
		if err != nil {
			b.logger.Warn().Err(err).Str("msg_id", msgID).Msg("Failed to load WhatsApp message for receipt")
			continue
		}
		if msg == nil || !msg.IsFromMe {
			continue
		}
		if !shouldUpgradeStatus(msg.Status, nextStatus) {
			continue
		}
		msg.Status = nextStatus
		if err := b.store.UpsertMessage(msg); err != nil {
			b.logger.Warn().Err(err).Str("msg_id", msgID).Str("status", nextStatus).Msg("Failed to update WhatsApp message receipt status")
			continue
		}
		updatedConversations[msg.ConversationID] = struct{}{}
	}

	if len(updatedConversations) == 0 {
		return
	}
	if b.callbacks.OnMessagesChange != nil {
		for conversationID := range updatedConversations {
			b.callbacks.OnMessagesChange(conversationID)
		}
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
}

func receiptStatus(receiptType watypes.ReceiptType) (string, bool) {
	switch receiptType {
	case watypes.ReceiptTypeDelivered, watypes.ReceiptTypeSender:
		return "delivered", true
	case watypes.ReceiptTypeRead, watypes.ReceiptTypeReadSelf, watypes.ReceiptTypePlayed, watypes.ReceiptTypePlayedSelf:
		return "read", true
	case watypes.ReceiptTypeServerError:
		return "failed", true
	default:
		return "", false
	}
}

func shouldUpgradeStatus(current, next string) bool {
	if strings.EqualFold(strings.TrimSpace(next), "failed") {
		return statusRank(current) <= 1
	}
	return statusRank(next) > statusRank(current)
}

func statusRank(status string) int {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "", "OUTGOING_SENDING":
		return 0
	case "SENT", "OUTGOING_SENT":
		return 1
	case "DELIVERED", "OUTGOING_DELIVERED":
		return 2
	case "READ", "OUTGOING_READ":
		return 3
	case "FAILED", "OUTGOING_FAILED":
		return 1
	default:
		return 0
	}
}

func (b *Bridge) handleChatPresence(evt *waevents.ChatPresence) {
	if evt == nil || b.callbacks.OnTypingChange == nil {
		return
	}
	conversationID := waConversationID(b.canonicalJID(evt.Chat))
	senderJID := b.canonicalJID(evt.Sender)
	senderNumber := jidToPhone(senderJID)
	senderName := b.contactDisplayName(senderJID, "")
	if senderName == "" {
		senderName = strings.TrimSpace(senderJID.User)
	}
	typing := evt.State == watypes.ChatPresenceComposing
	b.callbacks.OnTypingChange(conversationID, senderName, senderNumber, typing)
}

func (b *Bridge) handleGroupInfo(evt *waevents.GroupInfo) {
	if evt == nil {
		return
	}
	chatJID := b.normalizeConversationJID(evt.JID)
	conversationID := waConversationID(chatJID)
	if b.didOwnAccountJoinGroup(evt) {
		b.clearLeftGroup(conversationID)
	} else if b.shouldSuppressLeftGroup(conversationID) {
		if err := b.store.DeleteConversation(conversationID); err != nil {
			b.logger.Debug().Err(err).Str("chat", chatJID.String()).Msg("Failed to delete suppressed WhatsApp group")
		}
		return
	}
	if b.didOwnAccountLeaveGroup(evt) {
		b.markLeftGroup(conversationID)
		if err := b.store.DeleteConversation(conversationID); err != nil {
			b.logger.Debug().Err(err).Str("chat", chatJID.String()).Msg("Failed to delete WhatsApp group after leave event")
			return
		}
		if b.callbacks.OnMessagesChange != nil {
			b.callbacks.OnMessagesChange(conversationID)
		}
		if b.callbacks.OnConversationsChange != nil {
			b.callbacks.OnConversationsChange()
		}
		return
	}
	name := ""
	if evt.Name != nil {
		name = evt.Name.Name
	}
	if err := b.upsertGroupConversation(chatJID, name, nil); err != nil {
		b.logger.Debug().Err(err).Str("chat", chatJID.String()).Msg("Failed to update WhatsApp group metadata")
		return
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
}

func (b *Bridge) handleHistorySync(evt *waevents.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()
	if cli == nil {
		return
	}
	for _, conversation := range evt.Data.GetConversations() {
		for _, item := range conversation.GetMessages() {
			webMsg := item.GetMessage()
			if webMsg == nil {
				continue
			}
			msgEvt, err := cli.ParseWebMessage(watypes.EmptyJID, webMsg)
			if err != nil {
				b.logger.Debug().Err(err).Msg("Failed to parse WhatsApp history sync message")
				continue
			}
			b.handleMessage(msgEvt)
		}
	}
}

func (b *Bridge) upsertConversationForMessage(evt *waevents.Message) (*db.Conversation, error) {
	chatJID := b.normalizeConversationJID(evt.Info.Chat)
	conversationID := waConversationID(chatJID)
	existing, _ := b.store.GetConversation(conversationID)
	lastTS := evt.Info.Timestamp.UnixMilli()

	convo := &db.Conversation{
		ConversationID: conversationID,
		IsGroup:        evt.Info.IsGroup,
		LastMessageTS:  lastTS,
		SourcePlatform: "whatsapp",
		Participants:   "[]",
	}
	if existing != nil {
		*convo = *existing
		convo.LastMessageTS = maxInt64(existing.LastMessageTS, lastTS)
		convo.IsGroup = evt.Info.IsGroup
		convo.SourcePlatform = "whatsapp"
	}

	if evt.Info.IsGroup {
		if convo.Name == "" {
			convo.Name = fallbackChatName(chatJID)
		}
		if convo.Participants == "" {
			convo.Participants = "[]"
		}
		if err := b.store.UpsertConversation(convo); err != nil {
			return nil, err
		}
		if existing == nil || looksLikeRawIdentifier(existing.Name) || existing.Participants == "" || existing.Participants == "[]" {
			go b.enrichGroupConversation(chatJID)
		}
		return convo, nil
	}

	name := b.contactDisplayName(chatJID, evt.Info.PushName)
	if shouldReplaceConversationName(convo.Name, name) {
		convo.Name = name
	}
	if participants, err := marshalParticipants([]participantJSON{{
		Name:   firstNonEmpty(name, fallbackChatName(chatJID)),
		Number: b.phoneForJID(chatJID),
	}}); err == nil && participants != "" {
		convo.Participants = participants
	}
	if err := b.store.UpsertConversation(convo); err != nil {
		return nil, err
	}
	return convo, nil
}

func (b *Bridge) enrichGroupConversation(chatJID watypes.JID) {
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()
	if cli == nil || !clientIsConnected(cli) {
		return
	}
	info, err := cli.GetGroupInfo(context.Background(), chatJID)
	if err != nil {
		b.logger.Debug().Err(err).Str("chat", chatJID.String()).Msg("Failed to fetch WhatsApp group info")
		return
	}
	if err := b.upsertGroupConversation(chatJID, info.Name, info.Participants); err != nil {
		b.logger.Debug().Err(err).Str("chat", chatJID.String()).Msg("Failed to store WhatsApp group info")
		return
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
}

func (b *Bridge) upsertGroupConversation(chatJID watypes.JID, groupName string, participants []watypes.GroupParticipant) error {
	conversationID := waConversationID(chatJID)
	existing, _ := b.store.GetConversation(conversationID)
	convo := &db.Conversation{
		ConversationID: conversationID,
		Name:           fallbackChatName(chatJID),
		IsGroup:        true,
		Participants:   "[]",
		SourcePlatform: "whatsapp",
	}
	if existing != nil {
		*convo = *existing
		convo.IsGroup = true
		convo.SourcePlatform = "whatsapp"
	}
	if shouldReplaceConversationName(convo.Name, strings.TrimSpace(groupName)) {
		convo.Name = strings.TrimSpace(groupName)
	}
	if serialized, err := b.groupParticipantsJSON(participants); err == nil && serialized != "" {
		convo.Participants = serialized
	}
	return b.store.UpsertConversation(convo)
}

func (b *Bridge) resolveSender(evt *waevents.Message, convo *db.Conversation) (string, string) {
	if evt.Info.IsFromMe {
		b.mu.RLock()
		defer b.mu.RUnlock()
		if b.client != nil && strings.TrimSpace(b.client.Store.PushName) != "" {
			return strings.TrimSpace(b.client.Store.PushName), ""
		}
		return "Me", ""
	}

	if !evt.Info.IsGroup {
		chatJID := b.canonicalJID(evt.Info.Chat)
		name := b.contactDisplayName(chatJID, evt.Info.PushName)
		return firstNonEmpty(name, convoName(convo), fallbackChatName(chatJID)), b.phoneForJID(chatJID)
	}

	senderJID := b.canonicalJID(evt.Info.Sender)
	name := b.contactDisplayName(senderJID, evt.Info.PushName)
	return firstNonEmpty(name, fallbackChatName(senderJID)), b.phoneForJID(senderJID)
}

func (b *Bridge) contactDisplayName(jid watypes.JID, pushName string) string {
	jid = b.canonicalJID(jid)
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()
	if cli != nil && cli.Store != nil && cli.Store.Contacts != nil {
		if contact, err := cli.Store.Contacts.GetContact(context.Background(), jid); err == nil && contact.Found {
			return firstNonEmpty(strings.TrimSpace(contact.FullName), strings.TrimSpace(contact.FirstName), strings.TrimSpace(contact.PushName), strings.TrimSpace(contact.BusinessName), strings.TrimSpace(contact.RedactedPhone))
		}
	}
	return firstNonEmpty(strings.TrimSpace(pushName), b.phoneForJID(jid))
}

func (b *Bridge) phoneForJID(jid watypes.JID) string {
	jid = b.canonicalJID(jid)
	phone := jidToPhone(jid)
	if phone != "" {
		return phone
	}
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()
	if cli != nil && cli.Store != nil && cli.Store.Contacts != nil {
		if contact, err := cli.Store.Contacts.GetContact(context.Background(), jid); err == nil && contact.Found {
			return strings.TrimSpace(contact.RedactedPhone)
		}
	}
	return ""
}

func (b *Bridge) didOwnAccountLeaveGroup(evt *waevents.GroupInfo) bool {
	if evt == nil {
		return false
	}
	return b.groupInfoIncludesOwnAccount(evt.Leave)
}

func (b *Bridge) didOwnAccountJoinGroup(evt *waevents.GroupInfo) bool {
	if evt == nil {
		return false
	}
	return b.groupInfoIncludesOwnAccount(evt.Join)
}

func (b *Bridge) groupInfoIncludesOwnAccount(members []watypes.JID) bool {
	if len(members) == 0 {
		return false
	}
	b.mu.RLock()
	var ownJID watypes.JID
	if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
		ownJID = b.client.Store.ID.ToNonAD()
	}
	b.mu.RUnlock()
	if ownJID.IsEmpty() {
		return false
	}
	ownCanonical := b.canonicalJID(ownJID)
	for _, participantJID := range members {
		participantCanonical := b.canonicalJID(participantJID)
		if sameWhatsAppIdentity(participantJID, ownJID) || sameWhatsAppIdentity(participantCanonical, ownCanonical) {
			return true
		}
	}
	return false
}

func (b *Bridge) markLeftGroup(conversationID string) {
	if strings.TrimSpace(conversationID) == "" {
		return
	}
	b.mu.Lock()
	if b.recentlyLeftGroups == nil {
		b.recentlyLeftGroups = make(map[string]time.Time)
	}
	b.recentlyLeftGroups[conversationID] = time.Now().Add(recentlyLeftGroupTTL)
	b.mu.Unlock()
}

func (b *Bridge) clearLeftGroup(conversationID string) {
	if strings.TrimSpace(conversationID) == "" {
		return
	}
	b.mu.Lock()
	delete(b.recentlyLeftGroups, conversationID)
	b.mu.Unlock()
}

func (b *Bridge) shouldSuppressLeftGroup(conversationID string) bool {
	if strings.TrimSpace(conversationID) == "" {
		return false
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	expiresAt, ok := b.recentlyLeftGroups[conversationID]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(b.recentlyLeftGroups, conversationID)
		return false
	}
	return true
}

func (b *Bridge) canonicalJID(jid watypes.JID) watypes.JID {
	jid = jid.ToNonAD()
	if jid.Server != watypes.HiddenUserServer {
		return jid
	}
	b.mu.RLock()
	cli := b.client
	b.mu.RUnlock()
	if cli == nil || cli.Store == nil {
		return jid
	}
	alt, err := cli.Store.GetAltJID(context.Background(), jid)
	if err == nil && !alt.IsEmpty() {
		return alt.ToNonAD()
	}
	if fallback, fallbackErr := b.lookupPNFromSessionStore(jid.User); fallbackErr == nil && !fallback.IsEmpty() {
		return fallback.ToNonAD()
	}
	return jid
}

func (b *Bridge) normalizeConversationJID(jid watypes.JID) watypes.JID {
	rawJID := jid.ToNonAD()
	canonical := b.canonicalJID(rawJID)
	if rawJID != canonical {
		rawID := waConversationID(rawJID)
		canonicalID := waConversationID(canonical)
		if err := b.store.MergeConversationIDs(rawID, canonicalID); err != nil {
			b.logger.Warn().Err(err).Str("source", rawID).Str("target", canonicalID).Msg("Failed to merge WhatsApp conversation aliases")
		}
	}
	return canonical
}

func (b *Bridge) reconcileStoredDirectChats() {
	conversations, err := b.store.ListConversationsByPlatform("whatsapp", 1000000)
	if err != nil {
		b.logger.Debug().Err(err).Msg("Failed to list WhatsApp conversations for reconciliation")
		return
	}

	changed := false
	for _, convo := range conversations {
		if convo == nil || convo.IsGroup || !strings.HasPrefix(convo.ConversationID, "whatsapp:") || !strings.HasSuffix(convo.ConversationID, "@lid") {
			continue
		}
		jid, err := parseConversationJID(convo.ConversationID)
		if err != nil {
			continue
		}
		if b.normalizeConversationJID(jid) != jid {
			changed = true
		}
	}

	if changed {
		if b.callbacks.OnConversationsChange != nil {
			b.callbacks.OnConversationsChange()
		}
		if b.callbacks.OnMessagesChange != nil {
			b.callbacks.OnMessagesChange("")
		}
	}
}

func (b *Bridge) lookupPNFromSessionStore(lidUser string) (watypes.JID, error) {
	if strings.TrimSpace(lidUser) == "" || strings.TrimSpace(b.sessionPath) == "" {
		return watypes.EmptyJID, nil
	}
	dbh, err := sql.Open("sqlite", (&url.URL{
		Scheme:   "file",
		Path:     b.sessionPath,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String())
	if err != nil {
		return watypes.EmptyJID, err
	}
	defer dbh.Close()
	dbh.SetMaxOpenConns(1)

	var pn string
	err = dbh.QueryRow(`SELECT pn FROM whatsmeow_lid_map WHERE lid = ?`, lidUser).Scan(&pn)
	if err == sql.ErrNoRows {
		return watypes.EmptyJID, nil
	}
	if err != nil {
		return watypes.EmptyJID, err
	}
	return watypes.NewJID(strings.TrimSpace(pn), watypes.DefaultUserServer), nil
}

func (b *Bridge) lookupLIDFromSessionStore(pnUser string) watypes.JID {
	if strings.TrimSpace(pnUser) == "" || strings.TrimSpace(b.sessionPath) == "" {
		return watypes.EmptyJID
	}
	dbh, err := sql.Open("sqlite", (&url.URL{
		Scheme:   "file",
		Path:     b.sessionPath,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String())
	if err != nil {
		return watypes.EmptyJID
	}
	defer dbh.Close()
	dbh.SetMaxOpenConns(1)

	var lid string
	err = dbh.QueryRow(`SELECT lid FROM whatsmeow_lid_map WHERE pn = ?`, pnUser).Scan(&lid)
	if err != nil {
		return watypes.EmptyJID
	}
	return watypes.NewJID(strings.TrimSpace(lid), watypes.HiddenUserServer)
}

func (b *Bridge) avatarCacheEntry(key string) (avatarCacheEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.avatars == nil {
		return avatarCacheEntry{}, false
	}
	entry, ok := b.avatars[key]
	if !ok {
		return avatarCacheEntry{}, false
	}
	return avatarCacheEntry{
		ProfileID: entry.ProfileID,
		MIME:      entry.MIME,
		Data:      cloneBytes(entry.Data),
		FetchedAt: entry.FetchedAt,
		Missing:   entry.Missing,
	}, true
}

func (b *Bridge) storeAvatarCache(key string, entry avatarCacheEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.avatars == nil {
		b.avatars = map[string]avatarCacheEntry{}
	}
	entry.Data = cloneBytes(entry.Data)
	b.avatars[key] = entry
}

func (b *Bridge) groupParticipantsJSON(participants []watypes.GroupParticipant) (string, error) {
	if len(participants) == 0 {
		return "[]", nil
	}
	items := make([]participantJSON, 0, len(participants))
	for _, participant := range participants {
		jid := participant.JID
		if participant.PhoneNumber.User != "" {
			jid = participant.PhoneNumber
		}
		name := firstNonEmpty(strings.TrimSpace(participant.DisplayName), b.contactDisplayName(jid, ""))
		items = append(items, participantJSON{
			Name:   firstNonEmpty(name, fallbackChatName(jid)),
			Number: b.phoneForJID(jid),
		})
	}
	return marshalParticipants(items)
}

func (b *Bridge) RepairUnavailableMediaPlaceholders(limit int) error {
	if limit <= 0 {
		limit = maxUnavailablePlaceholderRepairs
	}

	b.mu.RLock()
	cli := b.client
	connected := b.connected
	b.mu.RUnlock()
	if cli == nil || !connected {
		return errors.New("whatsapp live sync is not connected")
	}

	placeholders, err := b.store.ListLegacyWhatsAppMediaPlaceholders(limit)
	if err != nil {
		return fmt.Errorf("list unavailable WhatsApp placeholders: %w", err)
	}

	for _, msg := range placeholders {
		if !shouldRequestUnavailablePlaceholder(msg) {
			continue
		}
		targets := b.unavailablePlaceholderTargets(msg)
		if len(targets) == 0 {
			continue
		}
		sourceID := strings.TrimSpace(msg.SourceID)
		for _, target := range targets {
			requestKey := unavailableRepairRequestKey(sourceID, target.chatJID)
			if !b.markUnavailableRepairRequest(requestKey) {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := sendTextMessage(cli, ctx, target.chatJID, cli.BuildUnavailableMessageRequest(target.chatJID, target.senderJID, sourceID), whatsmeow.SendRequestExtra{
				Peer: true,
			})
			cancel()
			if err != nil {
				b.clearUnavailableRepairRequest(requestKey)
				b.logger.Debug().
					Err(err).
					Str("conversation_id", msg.ConversationID).
					Str("chat_jid", target.chatJID.String()).
					Str("source_id", sourceID).
					Msg("Failed to request WhatsApp placeholder resend")
				continue
			}

			b.logger.Debug().
				Str("conversation_id", msg.ConversationID).
				Str("chat_jid", target.chatJID.String()).
				Str("source_id", sourceID).
				Msg("Requested WhatsApp placeholder resend for legacy media repair")

			b.requestHistorySyncAroundPlaceholder(cli, msg, target)
		}
	}

	return nil
}

func (b *Bridge) repairUnavailableMediaPlaceholdersAsync() {
	if err := b.RepairUnavailableMediaPlaceholders(maxUnavailablePlaceholderRepairs); err != nil && !strings.Contains(err.Error(), "not connected") {
		b.logger.Debug().Err(err).Msg("Failed to request WhatsApp placeholder resends")
	}
}

func shouldRequestUnavailablePlaceholder(msg *db.Message) bool {
	if msg == nil {
		return false
	}
	if strings.TrimSpace(msg.SourcePlatform) != "whatsapp" {
		return false
	}
	if strings.TrimSpace(msg.MediaID) != "" {
		return false
	}
	if strings.TrimSpace(msg.SourceID) == "" {
		return false
	}
	switch strings.TrimSpace(msg.Body) {
	case "[Photo]", "[Video]", "[Audio]", "[Voice note]":
		return true
	default:
		return false
	}
}

func (b *Bridge) unavailablePlaceholderTargets(msg *db.Message) []unavailableRepairTarget {
	chatJID, err := parseConversationJID(msg.ConversationID)
	if err != nil {
		return nil
	}
	chatJID = b.canonicalJID(chatJID)
	if chatJID.Server != watypes.DefaultUserServer && chatJID.Server != watypes.HiddenUserServer {
		return nil
	}

	targets := []unavailableRepairTarget{{
		chatJID: chatJID,
	}}
	if !msg.IsFromMe {
		targets[0].senderJID = chatJID
	}
	if lid := b.lookupLIDFromSessionStore(chatJID.User); !lid.IsEmpty() && lid.ToNonAD() != chatJID.ToNonAD() {
		alt := unavailableRepairTarget{chatJID: lid.ToNonAD()}
		if !msg.IsFromMe {
			alt.senderJID = alt.chatJID
		}
		targets = append(targets, alt)
	}
	return targets
}

func unavailableRepairRequestKey(sourceID string, chatJID watypes.JID) string {
	sourceID = strings.TrimSpace(sourceID)
	chat := strings.TrimSpace(chatJID.String())
	if sourceID == "" || chat == "" {
		return ""
	}
	return chat + "|" + sourceID
}

func (b *Bridge) requestHistorySyncAroundPlaceholder(cli *whatsmeow.Client, placeholder *db.Message, target unavailableRepairTarget) {
	anchorMessages, err := b.store.GetMessagesByConversationAfter(placeholder.ConversationID, placeholder.TimestampMS, placeholder.MessageID, 1)
	if err != nil || len(anchorMessages) == 0 {
		return
	}
	anchor := anchorMessages[0]
	if anchor == nil || strings.TrimSpace(anchor.SourceID) == "" {
		return
	}

	requestKey := "history|" + unavailableRepairRequestKey(strings.TrimSpace(placeholder.SourceID), target.chatJID)
	if !b.markUnavailableRepairRequest(requestKey) {
		return
	}

	anchorInfo := &watypes.MessageInfo{
		MessageSource: watypes.MessageSource{
			Chat:     target.chatJID,
			Sender:   watypes.EmptyJID,
			IsFromMe: anchor.IsFromMe,
			IsGroup:  false,
		},
		ID:        watypes.MessageID(strings.TrimSpace(anchor.SourceID)),
		Timestamp: time.UnixMilli(anchor.TimestampMS),
	}
	if !anchor.IsFromMe {
		anchorInfo.Sender = target.chatJID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := sendTextMessage(cli, ctx, target.chatJID, cli.BuildHistorySyncRequest(anchorInfo, 10), whatsmeow.SendRequestExtra{
		Peer: true,
	}); err != nil {
		b.clearUnavailableRepairRequest(requestKey)
		b.logger.Debug().
			Err(err).
			Str("conversation_id", placeholder.ConversationID).
			Str("chat_jid", target.chatJID.String()).
			Str("source_id", strings.TrimSpace(placeholder.SourceID)).
			Msg("Failed to request WhatsApp history sync for legacy media repair")
		return
	}

	b.logger.Debug().
		Str("conversation_id", placeholder.ConversationID).
		Str("chat_jid", target.chatJID.String()).
		Str("source_id", strings.TrimSpace(placeholder.SourceID)).
		Msg("Requested WhatsApp history sync for legacy media repair")
}

func (b *Bridge) markUnavailableRepairRequest(requestKey string) bool {
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unavailableRepairRequests == nil {
		b.unavailableRepairRequests = map[string]time.Time{}
	}
	if requestedAt, ok := b.unavailableRepairRequests[requestKey]; ok && time.Since(requestedAt) < unavailablePlaceholderRepairCooldown {
		return false
	}
	b.unavailableRepairRequests[requestKey] = time.Now()
	return true
}

func (b *Bridge) clearUnavailableRepairRequest(requestKey string) {
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unavailableRepairRequests == nil {
		return
	}
	delete(b.unavailableRepairRequests, requestKey)
}

func (b *Bridge) clearUnavailableRepairRequestsForSource(sourceID string) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unavailableRepairRequests == nil {
		return
	}
	suffix := "|" + sourceID
	for requestKey := range b.unavailableRepairRequests {
		if strings.HasSuffix(requestKey, suffix) {
			delete(b.unavailableRepairRequests, requestKey)
		}
	}
}

func (b *Bridge) emitStatusChange() {
	if b.callbacks.OnStatusChange != nil {
		b.callbacks.OnStatusChange()
	}
}

func marshalParticipants(items []participantJSON) (string, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func encodeStoredMediaRef(ref storedMediaRef) string {
	data, err := json.Marshal(ref)
	if err != nil {
		return ""
	}
	return "wa:" + base64.RawURLEncoding.EncodeToString(data)
}

func decodeStoredMediaRef(value string) (storedMediaRef, error) {
	var ref storedMediaRef
	if !strings.HasPrefix(value, "wa:") {
		return ref, errors.New("invalid WhatsApp media reference")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "wa:"))
	if err != nil {
		return ref, fmt.Errorf("decode WhatsApp media reference: %w", err)
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ref, fmt.Errorf("unmarshal WhatsApp media reference: %w", err)
	}
	return ref, nil
}

func parseConversationJID(conversationID string) (watypes.JID, error) {
	if !strings.HasPrefix(conversationID, "whatsapp:") {
		return watypes.JID{}, fmt.Errorf("invalid WhatsApp conversation id: %s", conversationID)
	}
	jid, err := watypes.ParseJID(strings.TrimPrefix(conversationID, "whatsapp:"))
	if err != nil {
		return watypes.JID{}, fmt.Errorf("parse WhatsApp conversation id: %w", err)
	}
	return jid, nil
}

func mediaTypeForMIME(mime string) (whatsmeow.MediaType, error) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(mime, "image/"):
		return whatsmeow.MediaImage, nil
	case strings.HasPrefix(mime, "audio/"):
		return whatsmeow.MediaAudio, nil
	case strings.HasPrefix(mime, "video/"):
		return whatsmeow.MediaVideo, nil
	default:
		return whatsmeow.MediaDocument, nil
	}
}

func outgoingMediaMessage(upload whatsmeow.UploadResponse, mime, filename string, mediaType whatsmeow.MediaType, caption, replyToID string) *waE2E.Message {
	contextInfo := outgoingReplyContext(replyToID)
	switch mediaType {
	case whatsmeow.MediaImage:
		return &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Mimetype:      proto.String(mime),
				Caption:       optionalProtoString(caption),
				URL:           proto.String(upload.URL),
				DirectPath:    proto.String(upload.DirectPath),
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    proto.Uint64(upload.FileLength),
				ContextInfo:   contextInfo,
			},
		}
	case whatsmeow.MediaVideo:
		return &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				Mimetype:      proto.String(mime),
				Caption:       optionalProtoString(caption),
				URL:           proto.String(upload.URL),
				DirectPath:    proto.String(upload.DirectPath),
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    proto.Uint64(upload.FileLength),
				ContextInfo:   contextInfo,
			},
		}
	case whatsmeow.MediaAudio:
		return &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				Mimetype:      proto.String(mime),
				URL:           proto.String(upload.URL),
				DirectPath:    proto.String(upload.DirectPath),
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    proto.Uint64(upload.FileLength),
				ContextInfo:   contextInfo,
			},
		}
	default:
		displayName := filepath.Base(strings.TrimSpace(filename))
		if displayName == "." || displayName == "" {
			displayName = "Attachment"
		}
		return &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				Mimetype:      proto.String(mime),
				Caption:       optionalProtoString(caption),
				FileName:      proto.String(displayName),
				Title:         proto.String(displayName),
				URL:           proto.String(upload.URL),
				DirectPath:    proto.String(upload.DirectPath),
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    proto.Uint64(upload.FileLength),
				ContextInfo:   contextInfo,
			},
		}
	}
}

func storedMediaRefFromUpload(upload whatsmeow.UploadResponse) storedMediaRef {
	return storedMediaRef{
		URL:           strings.TrimSpace(upload.URL),
		DirectPath:    strings.TrimSpace(upload.DirectPath),
		FileSHA256:    encodeBytes(upload.FileSHA256),
		FileEncSHA256: encodeBytes(upload.FileEncSHA256),
		FileLength:    upload.FileLength,
	}
}

func cloneBytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	return append([]byte(nil), data...)
}

func extractStoredMediaRef(msg *waE2E.Message) (storedMediaRef, []byte, string, bool) {
	msg = unwrapWhatsAppMessage(msg)
	switch {
	case msg == nil:
		return storedMediaRef{}, nil, "", false
	case msg.GetImageMessage() != nil:
		part := msg.GetImageMessage()
		return storedMediaRef{
			URL:           strings.TrimSpace(part.GetURL()),
			DirectPath:    strings.TrimSpace(part.GetDirectPath()),
			FileSHA256:    encodeBytes(part.GetFileSHA256()),
			FileEncSHA256: encodeBytes(part.GetFileEncSHA256()),
			FileLength:    part.GetFileLength(),
		}, part.GetMediaKey(), part.GetMimetype(), true
	case msg.GetVideoMessage() != nil:
		part := msg.GetVideoMessage()
		return storedMediaRef{
			URL:           strings.TrimSpace(part.GetURL()),
			DirectPath:    strings.TrimSpace(part.GetDirectPath()),
			FileSHA256:    encodeBytes(part.GetFileSHA256()),
			FileEncSHA256: encodeBytes(part.GetFileEncSHA256()),
			FileLength:    part.GetFileLength(),
		}, part.GetMediaKey(), part.GetMimetype(), true
	case msg.GetAudioMessage() != nil:
		part := msg.GetAudioMessage()
		return storedMediaRef{
			URL:           strings.TrimSpace(part.GetURL()),
			DirectPath:    strings.TrimSpace(part.GetDirectPath()),
			FileSHA256:    encodeBytes(part.GetFileSHA256()),
			FileEncSHA256: encodeBytes(part.GetFileEncSHA256()),
			FileLength:    part.GetFileLength(),
		}, part.GetMediaKey(), part.GetMimetype(), true
	case msg.GetDocumentMessage() != nil:
		part := msg.GetDocumentMessage()
		return storedMediaRef{
			URL:           strings.TrimSpace(part.GetURL()),
			DirectPath:    strings.TrimSpace(part.GetDirectPath()),
			FileSHA256:    encodeBytes(part.GetFileSHA256()),
			FileEncSHA256: encodeBytes(part.GetFileEncSHA256()),
			FileLength:    part.GetFileLength(),
		}, part.GetMediaKey(), part.GetMimetype(), true
	default:
		return storedMediaRef{}, nil, "", false
	}
}

func encodeBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", value)
}

func decodeHexBytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	return hex.DecodeString(value)
}

func normalizeReplyToID(replyToID string) string {
	replyToID = strings.TrimSpace(replyToID)
	if replyToID == "" {
		return ""
	}
	return "whatsapp:" + strings.TrimPrefix(replyToID, "whatsapp:")
}

func optionalProtoString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return proto.String(value)
}

func outgoingReplyContext(replyToID string) *waE2E.ContextInfo {
	replyToID = normalizeReplyToID(replyToID)
	if replyToID == "" {
		return nil
	}
	return &waE2E.ContextInfo{
		StanzaID: proto.String(strings.TrimPrefix(replyToID, "whatsapp:")),
	}
}

func outgoingTextMessage(body, replyToID string) *waE2E.Message {
	contextInfo := outgoingReplyContext(replyToID)
	if contextInfo == nil {
		return &waE2E.Message{Conversation: proto.String(body)}
	}
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(body),
			ContextInfo: contextInfo,
		},
	}
}

func extractMessageBody(msg *waE2E.Message) string {
	msg = unwrapWhatsAppMessage(msg)
	switch {
	case msg == nil:
		return ""
	case strings.TrimSpace(msg.GetConversation()) != "":
		return strings.TrimSpace(msg.GetConversation())
	case strings.TrimSpace(msg.GetExtendedTextMessage().GetText()) != "":
		return strings.TrimSpace(msg.GetExtendedTextMessage().GetText())
	case msg.GetImageMessage() != nil:
		return firstNonEmpty(strings.TrimSpace(msg.GetImageMessage().GetCaption()), "[Photo]")
	case msg.GetVideoMessage() != nil:
		return firstNonEmpty(strings.TrimSpace(msg.GetVideoMessage().GetCaption()), "[Video]")
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "[Voice note]"
		}
		return "[Audio]"
	case msg.GetDocumentMessage() != nil:
		return firstNonEmpty(strings.TrimSpace(msg.GetDocumentMessage().GetCaption()), "[Document]")
	case msg.GetAudioMessage() != nil:
		return "[Audio]"
	case msg.GetStickerMessage() != nil:
		return "[Sticker]"
	case msg.GetContactMessage() != nil || msg.GetContactsArrayMessage() != nil:
		return "[Contact]"
	default:
		return ""
	}
}

func extractReactionMessage(msg *waE2E.Message) *waE2E.ReactionMessage {
	msg = unwrapWhatsAppMessage(msg)
	if msg == nil {
		return nil
	}
	return msg.GetReactionMessage()
}

func extractReplyToID(msg *waE2E.Message) string {
	ctx := messageContextInfo(msg)
	if ctx == nil || strings.TrimSpace(ctx.GetStanzaID()) == "" {
		return ""
	}
	return "whatsapp:" + strings.TrimSpace(ctx.GetStanzaID())
}

func (b *Bridge) messageMentionsOwnAccount(msg *waE2E.Message) bool {
	ctx := messageContextInfo(msg)
	if ctx == nil {
		return false
	}
	mentioned := ctx.GetMentionedJID()
	if len(mentioned) == 0 {
		return false
	}

	b.mu.RLock()
	var ownJID watypes.JID
	if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
		ownJID = b.client.Store.ID.ToNonAD()
	}
	b.mu.RUnlock()
	if ownJID.IsEmpty() {
		return false
	}
	ownCanonical := b.canonicalJID(ownJID)

	for _, rawJID := range mentioned {
		mentionedJID, err := watypes.ParseJID(strings.TrimSpace(rawJID))
		if err != nil {
			continue
		}
		mentionedCanonical := b.canonicalJID(mentionedJID)
		if sameWhatsAppIdentity(mentionedJID, ownJID) || sameWhatsAppIdentity(mentionedCanonical, ownCanonical) {
			return true
		}
	}
	return false
}

func sameWhatsAppIdentity(a, b watypes.JID) bool {
	a = a.ToNonAD()
	b = b.ToNonAD()
	if a.IsEmpty() || b.IsEmpty() {
		return false
	}
	if a.String() == b.String() {
		return true
	}
	if strings.TrimSpace(a.User) == "" || strings.TrimSpace(b.User) == "" || a.User != b.User {
		return false
	}
	return isWhatsAppPersonServer(a.Server) && isWhatsAppPersonServer(b.Server)
}

func isWhatsAppPersonServer(server string) bool {
	switch server {
	case watypes.DefaultUserServer, watypes.HiddenUserServer:
		return true
	default:
		return false
	}
}

func messageContextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	msg = unwrapWhatsAppMessage(msg)
	switch {
	case msg == nil:
		return nil
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	default:
		return nil
	}
}

func unwrapWhatsAppMessage(msg *waE2E.Message) *waE2E.Message {
	for msg != nil {
		switch {
		case msg.GetDeviceSentMessage() != nil && msg.GetDeviceSentMessage().GetMessage() != nil:
			msg = msg.GetDeviceSentMessage().GetMessage()
		case msg.GetEphemeralMessage() != nil && msg.GetEphemeralMessage().GetMessage() != nil:
			msg = msg.GetEphemeralMessage().GetMessage()
		case msg.GetViewOnceMessage() != nil && msg.GetViewOnceMessage().GetMessage() != nil:
			msg = msg.GetViewOnceMessage().GetMessage()
		case msg.GetViewOnceMessageV2() != nil && msg.GetViewOnceMessageV2().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2().GetMessage()
		case msg.GetViewOnceMessageV2Extension() != nil && msg.GetViewOnceMessageV2Extension().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2Extension().GetMessage()
		case msg.GetDocumentWithCaptionMessage() != nil && msg.GetDocumentWithCaptionMessage().GetMessage() != nil:
			msg = msg.GetDocumentWithCaptionMessage().GetMessage()
		case msg.GetEditedMessage() != nil && msg.GetEditedMessage().GetMessage() != nil:
			msg = msg.GetEditedMessage().GetMessage()
		default:
			return msg
		}
	}
	return nil
}

func waConversationID(jid watypes.JID) string {
	return "whatsapp:" + jid.String()
}

func jidToPhone(jid watypes.JID) string {
	user := strings.TrimSpace(jid.User)
	if user == "" {
		return ""
	}
	for _, r := range user {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "+" + user
}

func fallbackChatName(jid watypes.JID) string {
	if phone := jidToPhone(jid); phone != "" {
		return phone
	}
	return firstNonEmpty(strings.TrimSpace(jid.User), jid.String())
}

func shouldReplaceConversationName(existing, candidate string) bool {
	existing = strings.TrimSpace(existing)
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	if existing == "" {
		return true
	}
	return looksLikeRawIdentifier(existing) && !looksLikeRawIdentifier(candidate)
}

func looksLikeRawIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || strings.Contains(value, "@") || strings.HasPrefix(value, "+")
}

func convoName(convo *db.Conversation) string {
	if convo == nil {
		return ""
	}
	return strings.TrimSpace(convo.Name)
}

func isRecentMessage(ts time.Time) bool {
	if ts.IsZero() {
		return false
	}
	return time.Since(ts) <= recentIncomingWindow
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func maxInt64(a, c int64) int64 {
	if a > c {
		return a
	}
	return c
}

func (b *Bridge) reactionActorID(evt *waevents.Message) string {
	if evt == nil {
		return ""
	}
	if evt.Info.IsFromMe {
		return b.reactionActorIDForClient(nil)
	}
	if evt.Info.IsGroup && !evt.Info.Sender.IsEmpty() {
		return b.canonicalJID(evt.Info.Sender).String()
	}
	if !evt.Info.Chat.IsEmpty() {
		return b.canonicalJID(evt.Info.Chat).String()
	}
	if !evt.Info.Sender.IsEmpty() {
		return b.canonicalJID(evt.Info.Sender).String()
	}
	return ""
}

func (b *Bridge) reactionActorIDForClient(cli *whatsmeow.Client) string {
	if cli == nil {
		b.mu.RLock()
		cli = b.client
		b.mu.RUnlock()
	}
	if cli != nil && cli.Store != nil && cli.Store.ID != nil {
		return b.canonicalJID(*cli.Store.ID).String()
	}
	return "me"
}

func (b *Bridge) reactionTargetSenderJID(msg *db.Message, chatJID watypes.JID) watypes.JID {
	if msg == nil || msg.IsFromMe {
		return watypes.EmptyJID
	}
	if senderJID := parseWhatsAppSenderJID(msg.SenderNumber); !senderJID.IsEmpty() {
		return b.canonicalJID(senderJID)
	}
	if chatJID.Server == watypes.DefaultUserServer || chatJID.Server == watypes.HiddenUserServer {
		return b.canonicalJID(chatJID)
	}
	return watypes.EmptyJID
}

func parseWhatsAppSenderJID(number string) watypes.JID {
	number = strings.TrimSpace(strings.TrimPrefix(number, "+"))
	if number == "" {
		return watypes.EmptyJID
	}
	return watypes.NewJID(number, watypes.DefaultUserServer)
}

func updateStoredReactions(existingJSON, actorID, emoji string) (string, bool, error) {
	reactions, err := parseStoredReactions(existingJSON)
	if err != nil {
		return "", false, err
	}

	actorID = strings.TrimSpace(actorID)
	emoji = strings.TrimSpace(emoji)
	changed := false

	if actorID != "" {
		for i := range reactions {
			if idx := reactionActorIndex(reactions[i].Actors, actorID); idx >= 0 {
				reactions[i].Actors = append(reactions[i].Actors[:idx], reactions[i].Actors[idx+1:]...)
				if reactions[i].Count > 0 {
					reactions[i].Count--
				}
				changed = true
			}
		}
	}

	if emoji != "" {
		found := false
		for i := range reactions {
			if strings.TrimSpace(reactions[i].Emoji) != emoji {
				continue
			}
			found = true
			if actorID != "" && reactionActorIndex(reactions[i].Actors, actorID) < 0 {
				reactions[i].Actors = append(reactions[i].Actors, actorID)
			}
			reactions[i].Emoji = emoji
			reactions[i].Count++
			changed = true
			break
		}
		if !found {
			entry := storedReaction{
				Emoji: emoji,
				Count: 1,
			}
			if actorID != "" {
				entry.Actors = []string{actorID}
			}
			reactions = append(reactions, entry)
			changed = true
		}
	}

	compacted := make([]storedReaction, 0, len(reactions))
	for _, reaction := range reactions {
		reaction.Emoji = strings.TrimSpace(reaction.Emoji)
		if reaction.Emoji == "" || reaction.Count <= 0 {
			continue
		}
		compacted = append(compacted, reaction)
	}
	reactions = compacted

	sort.Slice(reactions, func(i, j int) bool {
		return reactions[i].Emoji < reactions[j].Emoji
	})

	if !changed {
		return existingJSON, false, nil
	}
	if len(reactions) == 0 {
		return "", true, nil
	}

	data, err := json.Marshal(reactions)
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func parseStoredReactions(value string) ([]storedReaction, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(value), &reactions); err != nil {
		return nil, err
	}
	return reactions, nil
}

func reactionActorIndex(actors []string, actorID string) int {
	for i, actor := range actors {
		if actor == actorID {
			return i
		}
	}
	return -1
}
