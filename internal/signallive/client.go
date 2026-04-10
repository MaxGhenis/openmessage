package signallive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"rsc.io/qr"

	"github.com/maxghenis/openmessage/internal/db"
)

const (
	receiveTimeoutSeconds = 2
	receiveMaxMessages    = 100
	receiveFailureLimit   = 3
	sendTimeout           = 20 * time.Second
	syncRequestTimeout    = 10 * time.Second
	historySyncQuietAfter = 45 * time.Second
)

func isSignalIdleReceiveTimeout(err error, timedOut bool, output []byte) bool {
	if len(bytes.TrimSpace(output)) != 0 {
		return false
	}
	return timedOut || errors.Is(err, context.DeadlineExceeded)
}

var (
	now = time.Now

	signalCLILookPath = exec.LookPath
	signalCLIStat     = os.Stat

	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		commandArgs := append([]string{"--config", configDir}, args...)
		cmd := exec.CommandContext(ctx, signalCLIExecutable(), commandArgs...)
		return cmd.CombinedOutput()
	}

	startSignalLink = func(ctx context.Context, configDir string) (io.ReadCloser, func() error, error) {
		cmd := exec.CommandContext(ctx, "script", "-q", "/dev/null", signalCLIExecutable(), "--config", configDir, "link", "-n", "OpenMessage")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return stdout, cmd.Wait, nil
	}
)

type Callbacks struct {
	OnConversationsChange func()
	OnIncomingMessage     func(*db.Message)
	OnMessagesChange      func(string)
	OnStatusChange        func()
	OnTypingChange        func(conversationID, senderName, senderNumber string, typing bool)
}

type StatusSnapshot struct {
	Connected   bool                 `json:"connected"`
	Connecting  bool                 `json:"connecting"`
	Paired      bool                 `json:"paired"`
	Pairing     bool                 `json:"pairing"`
	Account     string               `json:"account,omitempty"`
	LastError   string               `json:"last_error,omitempty"`
	QRAvailable bool                 `json:"qr_available"`
	QRUpdatedAt int64                `json:"qr_updated_at,omitempty"`
	HistorySync *HistorySyncSnapshot `json:"history_sync,omitempty"`
}

type HistorySyncSnapshot struct {
	Running               bool  `json:"running"`
	StartedAt             int64 `json:"started_at,omitempty"`
	CompletedAt           int64 `json:"completed_at,omitempty"`
	ImportedConversations int   `json:"imported_conversations,omitempty"`
	ImportedMessages      int   `json:"imported_messages,omitempty"`
}

type QRSnapshot struct {
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	PNGDataURL string `json:"png_data_url,omitempty"`

	URI string `json:"-"`
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

type Bridge struct {
	mu        sync.RWMutex
	commandMu sync.Mutex

	store     *db.Store
	logger    zerolog.Logger
	configDir string
	callbacks Callbacks

	connected  bool
	connecting bool
	pairing    bool
	account    string
	lastError  string
	qr         QRSnapshot

	pairCancel    context.CancelFunc
	receiveCancel context.CancelFunc
	receiveToken  uint64
	groupNames    map[string]string
	contactByACI  map[string]string
	historySync   struct {
		startedAt             int64
		lastImportAt          int64
		importedConversations int
		importedMessages      int
	}
}

type signalReceivePayload struct {
	Account  string               `json:"account"`
	Envelope signalEnvelope       `json:"envelope"`
	Result   *signalReceiveResult `json:"result,omitempty"`
}

type signalReceiveResult struct {
	Account  string         `json:"account"`
	Envelope signalEnvelope `json:"envelope"`
}

type signalEnvelope struct {
	Source        string               `json:"source"`
	SourceName    string               `json:"sourceName"`
	SourceNumber  string               `json:"sourceNumber"`
	SourceUUID    string               `json:"sourceUuid"`
	Timestamp     int64                `json:"timestamp"`
	DataMessage   *signalDataMessage   `json:"dataMessage"`
	SyncMessage   *signalSyncMessage   `json:"syncMessage"`
	TypingMessage *signalTypingMessage `json:"typingMessage"`
}

type signalSyncMessage struct {
	SentMessage *signalSentMessage `json:"sentMessage"`
}

type signalDataMessage struct {
	Timestamp   int64                `json:"timestamp"`
	Message     string               `json:"message"`
	GroupInfo   *signalGroupInfo     `json:"groupInfo"`
	Attachments []signalAttachment   `json:"attachments"`
	Mentions    []signalMention      `json:"mentions"`
	Reaction    *signalReaction      `json:"reaction"`
	Quote       *signalQuotedMessage `json:"quote"`
}

type signalSentMessage struct {
	Timestamp         int64                `json:"timestamp"`
	Message           string               `json:"message"`
	Destination       string               `json:"destination"`
	DestinationNumber string               `json:"destinationNumber"`
	GroupInfo         *signalGroupInfo     `json:"groupInfo"`
	Attachments       []signalAttachment   `json:"attachments"`
	Mentions          []signalMention      `json:"mentions"`
	Reaction          *signalReaction      `json:"reaction"`
	Quote             *signalQuotedMessage `json:"quote"`
}

type signalGroupInfo struct {
	GroupID string `json:"groupId"`
	Title   string `json:"title"`
}

type signalAttachment struct {
	ContentType string `json:"contentType"`
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	Caption     string `json:"caption"`
}

type signalMention struct {
	Number          string `json:"number"`
	RecipientNumber string `json:"recipientNumber"`
	Recipient       string `json:"recipient"`
}

type signalReaction struct {
	Emoji               string `json:"emoji"`
	TargetAuthor        string `json:"targetAuthor"`
	TargetAuthorNumber  string `json:"targetAuthorNumber"`
	TargetAuthorUUID    string `json:"targetAuthorUuid"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	IsRemove            bool   `json:"isRemove"`
	Target              struct {
		Timestamp    int64  `json:"timestamp"`
		Author       string `json:"author"`
		AuthorNumber string `json:"authorNumber"`
		AuthorUUID   string `json:"authorUuid"`
	} `json:"target"`
}

type signalQuotedMessage struct {
	Timestamp int64  `json:"timestamp"`
	Author    string `json:"author"`
	Text      string `json:"text"`
}

type signalTypingMessage struct {
	Action    string           `json:"action"`
	GroupInfo *signalGroupInfo `json:"groupInfo"`
}

func New(configDir string, store *db.Store, logger zerolog.Logger, callbacks Callbacks) (*Bridge, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, fmt.Errorf("create Signal config dir: %w", err)
	}
	bridge := &Bridge{
		store:        store,
		logger:       logger,
		configDir:    configDir,
		callbacks:    callbacks,
		groupNames:   map[string]string{},
		contactByACI: map[string]string{},
	}
	bridge.account = bridge.firstStoredAccount()
	return bridge, nil
}

func signalCLIExecutable() string {
	if override := strings.TrimSpace(os.Getenv("OPENMESSAGES_SIGNAL_CLI")); override != "" {
		return override
	}
	if resolved, err := signalCLILookPath("signal-cli"); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/signal-cli",
		"/usr/local/bin/signal-cli",
		"/opt/local/bin/signal-cli",
	} {
		if _, err := signalCLIStat(candidate); err == nil {
			return candidate
		}
	}
	return "signal-cli"
}

func (b *Bridge) ConnectIfPaired() error {
	b.mu.Lock()
	if b.pairing || b.connecting || b.connected {
		b.mu.Unlock()
		return nil
	}
	if b.account == "" {
		b.account = b.firstStoredAccount()
	}
	if b.account == "" {
		b.mu.Unlock()
		return nil
	}
	b.connecting = true
	b.lastError = ""
	account := b.account
	b.mu.Unlock()
	b.emitStatusChange()
	go b.startReceiveLoop(account, false)
	return nil
}

func (b *Bridge) Connect() error {
	b.mu.Lock()
	if b.account == "" {
		b.account = b.firstStoredAccount()
	}
	if b.account != "" {
		if b.pairing || b.connecting || b.connected {
			b.mu.Unlock()
			return nil
		}
		b.connecting = true
		b.lastError = ""
		account := b.account
		b.mu.Unlock()
		b.emitStatusChange()
		go b.startReceiveLoop(account, false)
		return nil
	}
	if b.pairing || b.connecting {
		b.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.pairCancel = cancel
	b.pairing = true
	b.connecting = true
	b.lastError = ""
	b.qr = QRSnapshot{}
	b.mu.Unlock()
	b.emitStatusChange()
	go b.runLink(ctx)
	return nil
}

func (b *Bridge) Unpair() error {
	b.cancelBackgroundWork(true)
	if err := os.RemoveAll(b.configDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Signal config dir: %w", err)
	}
	b.mu.Lock()
	b.connected = false
	b.connecting = false
	b.pairing = false
	b.account = ""
	b.lastError = ""
	b.qr = QRSnapshot{}
	b.historySync = struct {
		startedAt             int64
		lastImportAt          int64
		importedConversations int
		importedMessages      int
	}{}
	b.mu.Unlock()
	b.emitStatusChange()
	return nil
}

func (b *Bridge) Status() StatusSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	account := b.account
	if account == "" {
		account = b.firstStoredAccount()
	}
	return StatusSnapshot{
		Connected:   b.connected,
		Connecting:  b.connecting,
		Paired:      account != "",
		Pairing:     b.pairing,
		Account:     account,
		LastError:   b.lastError,
		QRAvailable: b.qr.URI != "",
		QRUpdatedAt: b.qr.UpdatedAt,
		HistorySync: b.historySyncSnapshotLocked(),
	}
}

func (b *Bridge) QRCode() (QRSnapshot, error) {
	b.mu.RLock()
	snap := b.qr
	b.mu.RUnlock()
	if snap.URI == "" {
		return snap, fmt.Errorf("no active Signal QR code")
	}
	code, err := qr.Encode(snap.URI, qr.M)
	if err != nil {
		return QRSnapshot{}, fmt.Errorf("encode Signal QR: %w", err)
	}
	snap.PNGDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	return snap, nil
}

func (b *Bridge) SendText(conversationID, body, replyToID string) (*db.Message, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("signal message body is required")
	}

	account, err := b.usableAccount()
	if err != nil {
		return nil, err
	}
	target, isGroup, err := parseConversationTarget(conversationID)
	if err != nil {
		return nil, err
	}

	args := []string{"-a", account, "send", "-m", body}
	quoteArgs, err := b.signalQuoteArgs(replyToID, account)
	if err != nil {
		return nil, err
	}
	args = append(args, quoteArgs...)
	if isGroup {
		args = append(args, "--group-id", target)
	} else {
		args = append(args, target)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, args...)
	b.commandMu.Unlock()
	if err != nil {
		return nil, commandError("send Signal message", err, output)
	}

	timestamp := now().UnixMilli()
	messageID := localOutgoingMessageID(conversationID, timestamp, body)
	senderName := firstNonEmpty(os.Getenv("OPENMESSAGES_MY_NAME"), "Me")
	msg := &db.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   account,
		Body:           body,
		TimestampMS:    timestamp,
		Status:         "sent",
		IsFromMe:       true,
		ReplyToID:      strings.TrimSpace(replyToID),
		SourcePlatform: "signal",
	}
	return msg, nil
}

func (b *Bridge) SendMedia(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
	if len(data) == 0 {
		return nil, errors.New("signal attachment is required")
	}
	account, err := b.usableAccount()
	if err != nil {
		return nil, err
	}
	target, isGroup, err := parseConversationTarget(conversationID)
	if err != nil {
		return nil, err
	}

	attachmentPath, err := b.writeLocalAttachment(data, filename)
	if err != nil {
		return nil, err
	}

	caption = strings.TrimSpace(caption)
	args := []string{"-a", account, "send"}
	if caption != "" {
		args = append(args, "-m", caption)
	}
	args = append(args, "-a", attachmentPath)
	quoteArgs, err := b.signalQuoteArgs(replyToID, account)
	if err != nil {
		_ = os.Remove(attachmentPath)
		return nil, err
	}
	args = append(args, quoteArgs...)
	if isGroup {
		args = append(args, "--group-id", target)
	} else {
		args = append(args, target)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, args...)
	b.commandMu.Unlock()
	if err != nil {
		_ = os.Remove(attachmentPath)
		return nil, commandError("send Signal media", err, output)
	}

	timestamp := now().UnixMilli()
	body := caption
	if body == "" {
		body = signalAttachmentPlaceholder([]signalAttachment{{ContentType: mime}})
	}
	messageID := localOutgoingMessageID(conversationID, timestamp, body)
	senderName := firstNonEmpty(os.Getenv("OPENMESSAGES_MY_NAME"), "Me")
	msg := &db.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   account,
		Body:           body,
		TimestampMS:    timestamp,
		Status:         "sent",
		IsFromMe:       true,
		ReplyToID:      strings.TrimSpace(replyToID),
		SourcePlatform: "signal",
		MimeType:       strings.TrimSpace(mime),
		MediaID:        encodeSignalLocalAttachmentRef(attachmentPath),
	}
	return msg, nil
}

func (b *Bridge) SendReaction(conversationID, targetMessageID, emoji, action string) error {
	targetMessageID = strings.TrimSpace(targetMessageID)
	if targetMessageID == "" {
		return errors.New("signal target message is required")
	}
	emoji = strings.TrimSpace(emoji)
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "add"
	}
	if emoji == "" {
		return errors.New("signal reaction emoji is required")
	}

	target, err := b.store.GetMessageByID(targetMessageID)
	if err != nil {
		return fmt.Errorf("load Signal reaction target: %w", err)
	}
	if target == nil || target.SourcePlatform != "signal" {
		return errors.New("signal reaction target not found")
	}
	if strings.TrimSpace(conversationID) == "" {
		conversationID = target.ConversationID
	}

	account, err := b.usableAccount()
	if err != nil {
		return err
	}
	targetConversationID := strings.TrimSpace(target.ConversationID)
	if targetConversationID == "" {
		targetConversationID = strings.TrimSpace(conversationID)
	}
	recipient, isGroup, err := parseConversationTarget(targetConversationID)
	if err != nil {
		return err
	}
	targetAuthor := b.resolveContactAddress(target.SenderNumber)
	if targetAuthor == "" {
		return errors.New("signal reaction target author is unavailable")
	}

	args := []string{"-a", account, "sendReaction", "-e", emoji, "-a", targetAuthor, "-t", strconv.FormatInt(target.TimestampMS, 10)}
	if action == "remove" {
		args = append(args, "-r")
	}
	if isGroup {
		args = append(args, "--group-id", recipient)
	} else {
		args = append(args, recipient)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, args...)
	b.commandMu.Unlock()
	if err != nil {
		return commandError("send Signal reaction", err, output)
	}

	nextReactions, changed, err := updateStoredReactions(target.Reactions, account, signalReactionStoreEmoji(emoji, action))
	if err != nil {
		return fmt.Errorf("update local Signal reaction state: %w", err)
	}
	if !changed {
		return nil
	}
	target.Reactions = nextReactions
	if err := b.store.UpsertMessage(target); err != nil {
		return fmt.Errorf("store Signal reaction update: %w", err)
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(target.ConversationID)
	}
	return nil
}

func (b *Bridge) Close() error {
	b.cancelBackgroundWork(false)
	return nil
}

func (b *Bridge) runLink(ctx context.Context) {
	reader, wait, err := startSignalLink(ctx, b.configDir)
	if err != nil {
		b.mu.Lock()
		b.pairing = false
		b.connecting = false
		b.lastError = err.Error()
		b.pairCancel = nil
		b.mu.Unlock()
		b.emitStatusChange()
		return
	}
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lastLine := ""
	for scanner.Scan() {
		line := sanitizeSignalOutput(scanner.Text())
		if uri := extractSignalLinkURI(line); uri != "" {
			b.mu.Lock()
			b.qr = QRSnapshot{
				URI:       uri,
				UpdatedAt: now().UnixMilli(),
			}
			b.mu.Unlock()
			b.emitStatusChange()
			continue
		}
		if strings.TrimSpace(line) != "" {
			lastLine = strings.TrimSpace(line)
		}
	}
	waitErr := wait()
	if scanErr := scanner.Err(); scanErr != nil && waitErr == nil {
		waitErr = scanErr
	}

	account, accountErr := b.probeAccount(context.Background(), "")
	b.mu.Lock()
	b.pairing = false
	b.connecting = false
	b.pairCancel = nil
	b.qr = QRSnapshot{}
	if account != "" {
		b.account = account
		b.lastError = ""
	} else {
		switch {
		case accountErr != nil:
			b.lastError = accountErr.Error()
		case lastLine != "":
			b.lastError = lastLine
		case waitErr != nil:
			b.lastError = waitErr.Error()
		default:
			b.lastError = "Signal pairing cancelled"
		}
	}
	b.mu.Unlock()
	b.emitStatusChange()
	if account != "" {
		b.startReceiveLoop(account, true)
	}
}

func (b *Bridge) startReceiveLoop(account string, requestSync bool) {
	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	if b.receiveCancel != nil {
		b.receiveCancel()
	}
	b.receiveToken++
	token := b.receiveToken
	b.receiveCancel = cancel
	b.mu.Unlock()

	probedAccount, err := b.probeAccount(ctx, account)
	if err != nil || probedAccount == "" {
		b.mu.Lock()
		b.connected = false
		b.connecting = false
		if err != nil {
			b.lastError = err.Error()
		} else {
			b.lastError = "Signal account is not paired"
		}
		if b.receiveToken == token {
			b.receiveCancel = nil
		}
		b.mu.Unlock()
		b.emitStatusChange()
		return
	}

	b.mu.Lock()
	b.account = probedAccount
	b.connected = true
	b.connecting = false
	b.lastError = ""
	b.mu.Unlock()
	b.emitStatusChange()
	go b.refreshContacts()
	go b.refreshGroupNames()
	if requestSync {
		b.beginHistorySync()
		b.emitStatusChange()
		if err := b.requestSync(probedAccount); err != nil {
			b.logger.Debug().Err(err).Msg("Failed to request Signal device sync after pairing")
		}
	}

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			if b.receiveToken == token {
				b.receiveCancel = nil
			}
			if !b.pairing {
				b.connected = false
			}
			b.mu.Unlock()
			b.emitStatusChange()
			return
		default:
		}

		callCtx, callCancel := context.WithTimeout(ctx, time.Duration(receiveTimeoutSeconds+3)*time.Second)
		b.commandMu.Lock()
		output, err := runSignalCLI(callCtx, b.configDir, "-a", probedAccount, "--output", "json", "receive", "--timeout", strconv.Itoa(receiveTimeoutSeconds), "--max-messages", strconv.Itoa(receiveMaxMessages))
		b.commandMu.Unlock()
		timedOut := errors.Is(callCtx.Err(), context.DeadlineExceeded)
		callCancel()
		if ctx.Err() != nil {
			continue
		}
		if err != nil {
			if isSignalIdleReceiveTimeout(err, timedOut, output) {
				consecutiveFailures = 0
				continue
			}
			if isSignalAccountInvalid(err, output) {
				b.mu.Lock()
				if b.receiveToken == token {
					b.receiveCancel = nil
				}
				b.connected = false
				b.connecting = false
				b.account = ""
				b.lastError = cleanSignalCommandOutput(err, output)
				b.mu.Unlock()
				b.emitStatusChange()
				return
			}
			consecutiveFailures++
			receiveErr := commandError("receive Signal messages", err, output)
			if consecutiveFailures >= receiveFailureLimit {
				b.logger.Warn().Err(receiveErr).Int("failures", consecutiveFailures).Msg("Signal receive polling repeatedly failed; forcing reconnect")
				b.mu.Lock()
				if b.receiveToken == token {
					b.receiveCancel = nil
				}
				b.connected = false
				b.connecting = false
				b.lastError = cleanSignalCommandOutput(err, output)
				b.mu.Unlock()
				b.emitStatusChange()
				return
			}
			b.logger.Debug().Err(receiveErr).Int("failures", consecutiveFailures).Msg("Signal receive polling failed")
			time.Sleep(500 * time.Millisecond)
			continue
		}
		consecutiveFailures = 0
		if len(bytes.TrimSpace(output)) == 0 {
			continue
		}
		if err := b.handleReceiveOutput(probedAccount, output); err != nil {
			b.logger.Debug().Err(err).Msg("Failed to process Signal receive payload")
		}
	}
}

func (b *Bridge) requestSync(account string) error {
	account = normalizeSignalAddress(account)
	if account == "" {
		return errors.New("signal account is not paired")
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncRequestTimeout)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, "-a", account, "sendSyncRequest")
	b.commandMu.Unlock()
	if err != nil {
		return commandError("request Signal device sync", err, output)
	}
	return nil
}

func (b *Bridge) handleReceiveOutput(account string, output []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var payload signalReceivePayload
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}
		env := payload.Envelope
		payloadAccount := strings.TrimSpace(payload.Account)
		if payload.Result != nil {
			if payloadAccount == "" {
				payloadAccount = strings.TrimSpace(payload.Result.Account)
			}
			if env.Timestamp == 0 && env.Source == "" && env.SourceNumber == "" && env.DataMessage == nil && env.TypingMessage == nil {
				env = payload.Result.Envelope
			}
		}
		if payloadAccount == "" {
			payloadAccount = account
		}
		if env.TypingMessage != nil {
			b.handleTypingMessage(payloadAccount, &env)
		}
		if env.DataMessage != nil {
			if err := b.handleDataMessage(payloadAccount, &env); err != nil {
				b.logger.Debug().Err(err).Msg("Failed to store Signal message")
			}
		}
		if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
			if err := b.handleSentMessage(payloadAccount, &env); err != nil {
				b.logger.Debug().Err(err).Msg("Failed to store Signal sent sync message")
			}
		}
	}
	return scanner.Err()
}

func (b *Bridge) handleTypingMessage(account string, env *signalEnvelope) {
	if env == nil || env.TypingMessage == nil || b.callbacks.OnTypingChange == nil {
		return
	}
	source := b.resolveContactAddress(firstNonEmpty(env.SourceNumber, env.SourceUUID, env.Source))
	if source == "" || addressesMatch(source, account) {
		return
	}
	groupID := ""
	if env.TypingMessage.GroupInfo != nil {
		groupID = strings.TrimSpace(env.TypingMessage.GroupInfo.GroupID)
	}
	conversationID := signalConversationID(source, groupID)
	typing := strings.EqualFold(strings.TrimSpace(env.TypingMessage.Action), "started")
	b.callbacks.OnTypingChange(conversationID, firstNonEmpty(strings.TrimSpace(env.SourceName), source), source, typing)
}

func (b *Bridge) handleDataMessage(account string, env *signalEnvelope) error {
	if env == nil || env.DataMessage == nil {
		return nil
	}

	source := b.resolveContactAddress(firstNonEmpty(env.SourceNumber, env.SourceUUID, env.Source))
	groupID := ""
	groupTitle := ""
	if env.DataMessage.GroupInfo != nil {
		groupID = strings.TrimSpace(env.DataMessage.GroupInfo.GroupID)
		groupTitle = strings.TrimSpace(env.DataMessage.GroupInfo.Title)
	}
	if groupTitle == "" && groupID != "" {
		groupTitle = b.groupName(groupID)
	}
	if source == "" && groupID == "" {
		return nil
	}

	conversationID := signalConversationID(source, groupID)
	if env.DataMessage.Reaction != nil {
		return b.applyReactionToConversation(conversationID, env.DataMessage.Reaction, b.resolveContactAddress(signalReactionActorID(env)), account)
	}

	isFromMe := source != "" && addressesMatch(source, account)
	if isFromMe {
		return nil
	}

	timestamp := env.DataMessage.Timestamp
	if timestamp == 0 {
		timestamp = env.Timestamp
	}
	body := strings.TrimSpace(env.DataMessage.Message)
	if body == "" {
		body = signalAttachmentPlaceholder(env.DataMessage.Attachments)
	}
	name := firstNonEmpty(strings.TrimSpace(env.SourceName), source)
	sourceID := signalIncomingSourceID(conversationID, source, timestamp, body)
	messageID := "signal:" + sourceID
	existingMsg, _ := b.store.GetMessageByID(messageID)

	existing, _ := b.store.GetConversation(conversationID)
	convo := &db.Conversation{
		ConversationID: conversationID,
		Name:           name,
		IsGroup:        groupID != "",
		LastMessageTS:  timestamp,
		UnreadCount:    1,
		SourcePlatform: "signal",
		Participants:   "[]",
	}
	if existing != nil {
		*convo = *existing
		convo.LastMessageTS = maxInt64(existing.LastMessageTS, timestamp)
		convo.IsGroup = groupID != ""
		convo.SourcePlatform = "signal"
		convo.UnreadCount = existing.UnreadCount
		if existingMsg == nil {
			convo.UnreadCount = existing.UnreadCount + 1
		}
	}
	if convo.IsGroup {
		if groupTitle != "" {
			convo.Name = groupTitle
		} else if convo.Name == "" {
			convo.Name = "Signal Group"
		}
	} else {
		if convo.Name == "" {
			convo.Name = source
		}
		if participants, err := marshalParticipants([]participantJSON{{
			Name:   firstNonEmpty(name, source),
			Number: source,
		}}); err == nil {
			convo.Participants = participants
		}
	}
	if err := b.store.UpsertConversation(convo); err != nil {
		return err
	}

	msg := &db.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		SenderName:     firstNonEmpty(name, convo.Name, source),
		SenderNumber:   source,
		Body:           body,
		TimestampMS:    timestamp,
		Status:         "received",
		IsFromMe:       false,
		MentionsMe:     signalMentionsMe(env.DataMessage.Mentions, account),
		ReplyToID:      signalQuoteReplyID(conversationID, env.DataMessage.Quote),
		SourcePlatform: "signal",
		SourceID:       sourceID,
	}
	if len(env.DataMessage.Attachments) > 0 {
		msg.MimeType = strings.TrimSpace(env.DataMessage.Attachments[0].ContentType)
		msg.MediaID = encodeSignalAttachmentRef(env.DataMessage.Attachments[0].ID)
	}
	if err := b.store.UpsertMessage(msg); err != nil {
		return err
	}
	b.recordHistorySyncProgress(existing == nil, existingMsg == nil)
	if existingMsg == nil && b.callbacks.OnIncomingMessage != nil {
		b.callbacks.OnIncomingMessage(msg)
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(conversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
	return nil
}

func (b *Bridge) handleSentMessage(account string, env *signalEnvelope) error {
	if env == nil || env.SyncMessage == nil || env.SyncMessage.SentMessage == nil {
		return nil
	}

	sent := env.SyncMessage.SentMessage
	groupID := ""
	groupTitle := ""
	if sent.GroupInfo != nil {
		groupID = strings.TrimSpace(sent.GroupInfo.GroupID)
		groupTitle = strings.TrimSpace(sent.GroupInfo.Title)
	}
	if groupTitle == "" && groupID != "" {
		groupTitle = b.groupName(groupID)
	}
	target := b.resolveContactAddress(firstNonEmpty(sent.DestinationNumber, sent.Destination))
	if target == "" && groupID == "" {
		return nil
	}

	conversationID := signalConversationID(target, groupID)
	if sent.Reaction != nil {
		return b.applyReactionToConversation(conversationID, sent.Reaction, b.resolveContactAddress(account), account)
	}

	timestamp := sent.Timestamp
	if timestamp == 0 {
		timestamp = env.Timestamp
	}
	body := strings.TrimSpace(sent.Message)
	if body == "" {
		body = signalAttachmentPlaceholder(sent.Attachments)
	}
	if body == "" {
		return nil
	}

	existingMsg := b.matchLocalOutgoingMessage(conversationID, body, timestamp)
	messageID := localOutgoingMessageID(conversationID, timestamp, body)
	messageTimestamp := timestamp
	replyToID := signalQuoteReplyID(conversationID, sent.Quote)
	if existingMsg != nil {
		messageID = existingMsg.MessageID
		if existingMsg.TimestampMS > 0 {
			messageTimestamp = existingMsg.TimestampMS
		}
		if replyToID == "" {
			replyToID = existingMsg.ReplyToID
		}
	}

	existing, _ := b.store.GetConversation(conversationID)
	convo := &db.Conversation{
		ConversationID: conversationID,
		Name:           target,
		IsGroup:        groupID != "",
		LastMessageTS:  maxInt64(messageTimestamp, timestamp),
		UnreadCount:    0,
		SourcePlatform: "signal",
		Participants:   "[]",
	}
	if existing != nil {
		*convo = *existing
		convo.LastMessageTS = maxInt64(existing.LastMessageTS, maxInt64(messageTimestamp, timestamp))
		convo.IsGroup = groupID != ""
		convo.SourcePlatform = "signal"
	}
	if convo.IsGroup {
		if groupTitle != "" {
			convo.Name = groupTitle
		} else if convo.Name == "" {
			convo.Name = "Signal Group"
		}
	} else {
		if convo.Name == "" {
			convo.Name = target
		}
		if participants, err := marshalParticipants([]participantJSON{{
			Name:   firstNonEmpty(convo.Name, target),
			Number: target,
		}}); err == nil {
			convo.Participants = participants
		}
	}
	if err := b.store.UpsertConversation(convo); err != nil {
		return err
	}

	senderName := firstNonEmpty(os.Getenv("OPENMESSAGES_MY_NAME"), "Me")
	msg := &db.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   account,
		Body:           body,
		TimestampMS:    messageTimestamp,
		Status:         "sent",
		IsFromMe:       true,
		ReplyToID:      replyToID,
		SourcePlatform: "signal",
		SourceID:       strings.TrimPrefix(messageID, "signal:"),
	}
	if len(sent.Attachments) > 0 {
		if existingMsg != nil {
			cleanupLocalSignalAttachment(existingMsg.MediaID)
		}
		msg.MimeType = strings.TrimSpace(sent.Attachments[0].ContentType)
		msg.MediaID = encodeSignalAttachmentRef(sent.Attachments[0].ID)
	}
	if err := b.store.UpsertMessage(msg); err != nil {
		return err
	}
	b.recordHistorySyncProgress(existing == nil, existingMsg == nil)
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(conversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
	return nil
}

func (b *Bridge) applyReactionToConversation(conversationID string, reaction *signalReaction, actorID, account string) error {
	if reaction == nil || b == nil || b.store == nil {
		return nil
	}
	targetMessage, err := b.findReactionTarget(conversationID, reaction, account)
	if err != nil {
		return err
	}
	if targetMessage == nil {
		return nil
	}

	action := ""
	if reaction.IsRemove {
		action = "remove"
	}
	nextReactions, changed, err := updateStoredReactions(targetMessage.Reactions, actorID, signalReactionStoreEmoji(reaction.Emoji, action))
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	targetMessage.Reactions = nextReactions
	if err := b.store.UpsertMessage(targetMessage); err != nil {
		return err
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(targetMessage.ConversationID)
	}
	return nil
}

func (b *Bridge) findReactionTarget(conversationID string, reaction *signalReaction, account string) (*db.Message, error) {
	targetTimestamp := signalReactionTargetTimestamp(reaction)
	if reaction == nil || targetTimestamp == 0 {
		return nil, nil
	}
	messages, err := b.store.GetMessagesByConversationAtTimestamp(conversationID, targetTimestamp, 10)
	if err != nil || len(messages) == 0 {
		return nil, err
	}
	if len(messages) == 1 {
		return messages[0], nil
	}
	targetAuthor := b.resolveContactAddress(signalReactionTargetAuthor(reaction, account))
	if targetAuthor != "" {
		for _, message := range messages {
			if message != nil && addressesMatch(b.resolveContactAddress(message.SenderNumber), targetAuthor) {
				return message, nil
			}
		}
	}
	return messages[0], nil
}

func (b *Bridge) emitStatusChange() {
	if b.callbacks.OnStatusChange != nil {
		b.callbacks.OnStatusChange()
	}
}

func (b *Bridge) beginHistorySync() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.historySync.startedAt = now().UnixMilli()
	b.historySync.lastImportAt = 0
	b.historySync.importedConversations = 0
	b.historySync.importedMessages = 0
}

func (b *Bridge) recordHistorySyncProgress(newConversation, newMessage bool) {
	if !newConversation && !newMessage {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.historySync.startedAt == 0 {
		return
	}
	if newConversation {
		b.historySync.importedConversations++
	}
	if newMessage {
		b.historySync.importedMessages++
	}
	b.historySync.lastImportAt = now().UnixMilli()
}

func (b *Bridge) historySyncSnapshotLocked() *HistorySyncSnapshot {
	if b.historySync.startedAt == 0 {
		return nil
	}
	activityAt := b.historySync.startedAt
	if b.historySync.lastImportAt > activityAt {
		activityAt = b.historySync.lastImportAt
	}
	running := now().UnixMilli()-activityAt < int64(historySyncQuietAfter/time.Millisecond)
	snapshot := &HistorySyncSnapshot{
		Running:               running,
		StartedAt:             b.historySync.startedAt,
		ImportedConversations: b.historySync.importedConversations,
		ImportedMessages:      b.historySync.importedMessages,
	}
	if !running {
		snapshot.CompletedAt = activityAt
	}
	return snapshot
}

func (b *Bridge) cancelBackgroundWork(clearPairQR bool) {
	b.mu.Lock()
	if b.pairCancel != nil {
		b.pairCancel()
		b.pairCancel = nil
	}
	if b.receiveCancel != nil {
		b.receiveCancel()
		b.receiveCancel = nil
	}
	if clearPairQR {
		b.qr = QRSnapshot{}
	}
	b.mu.Unlock()
}

func (b *Bridge) usableAccount() (string, error) {
	b.mu.RLock()
	account := b.account
	connected := b.connected
	b.mu.RUnlock()
	if account == "" {
		account = b.firstStoredAccount()
	}
	if account == "" {
		return "", errors.New("signal is not paired")
	}
	if !connected {
		_ = b.ConnectIfPaired()
	}
	return account, nil
}

func (b *Bridge) probeAccount(ctx context.Context, expected string) (string, error) {
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, "--output", "json", "listAccounts")
	b.commandMu.Unlock()
	accounts := parseSignalAccounts(output)
	if err != nil && len(accounts) == 0 {
		return "", commandError("list Signal accounts", err, output)
	}
	if expected = normalizeSignalAddress(expected); expected != "" {
		for _, account := range accounts {
			if addressesMatch(account, expected) {
				return account, nil
			}
		}
	}
	if len(accounts) > 0 {
		return accounts[0], nil
	}
	return "", nil
}

func (b *Bridge) groupName(groupID string) string {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ""
	}
	b.mu.RLock()
	name := strings.TrimSpace(b.groupNames[groupID])
	b.mu.RUnlock()
	if name != "" {
		return name
	}
	b.refreshGroupNames()
	b.mu.RLock()
	defer b.mu.RUnlock()
	return strings.TrimSpace(b.groupNames[groupID])
}

func (b *Bridge) refreshGroupNames() {
	if b == nil || b.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, "listGroups")
	b.commandMu.Unlock()
	groups := parseSignalGroups(output)
	if err != nil && len(groups) == 0 {
		b.logger.Debug().Err(commandError("list Signal groups", err, output)).Msg("Failed to refresh Signal groups")
		return
	}
	if len(groups) == 0 {
		return
	}
	b.mu.Lock()
	for id, name := range groups {
		b.groupNames[id] = name
	}
	b.mu.Unlock()

	count, err := b.store.ConversationCount("signal")
	if err != nil || count == 0 {
		return
	}
	conversations, err := b.store.ListConversationsByPlatform("signal", count)
	if err != nil {
		return
	}
	changed := false
	for _, convo := range conversations {
		if convo == nil || !strings.HasPrefix(convo.ConversationID, "signal-group:") {
			continue
		}
		groupID := strings.TrimPrefix(convo.ConversationID, "signal-group:")
		name := strings.TrimSpace(groups[groupID])
		if name == "" || name == strings.TrimSpace(convo.Name) {
			continue
		}
		updated := *convo
		updated.Name = name
		if err := b.store.UpsertConversation(&updated); err != nil {
			continue
		}
		changed = true
	}
	if changed && b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
}

func (b *Bridge) resolveContactAddress(value string) string {
	value = normalizeSignalAddress(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "+") {
		return value
	}
	b.mu.RLock()
	resolved := normalizeSignalAddress(b.contactByACI[value])
	b.mu.RUnlock()
	if resolved != "" {
		return resolved
	}
	b.refreshContacts()
	b.mu.RLock()
	defer b.mu.RUnlock()
	if resolved = normalizeSignalAddress(b.contactByACI[value]); resolved != "" {
		return resolved
	}
	return value
}

func (b *Bridge) refreshContacts() {
	if b == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, "listContacts")
	b.commandMu.Unlock()
	contacts := parseSignalContacts(output)
	if err != nil && len(contacts) == 0 {
		b.logger.Debug().Err(commandError("list Signal contacts", err, output)).Msg("Failed to refresh Signal contacts")
		return
	}
	if len(contacts) == 0 {
		return
	}
	b.mu.Lock()
	for aci, number := range contacts {
		b.contactByACI[aci] = number
	}
	b.mu.Unlock()
}

func (b *Bridge) firstStoredAccount() string {
	path := filepath.Join(b.configDir, "data", "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return firstSignalAccount(raw)
}

func parseSignalAccounts(raw []byte) []string {
	accounts := decodedSignalAccounts(raw)
	if len(accounts) > 0 {
		sort.Strings(accounts)
		return accounts
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := normalizeSignalAddress(scanner.Text())
		if line != "" {
			accounts = append(accounts, line)
		}
	}
	sort.Strings(accounts)
	return accounts
}

func firstSignalAccount(raw []byte) string {
	accounts := decodedSignalAccounts(raw)
	if len(accounts) == 0 {
		return ""
	}
	return accounts[0]
}

func decodedSignalAccounts(raw []byte) []string {
	type signalAccount struct {
		Number string `json:"number"`
	}
	seen := map[string]struct{}{}
	accounts := make([]string, 0, 4)
	appendAccount := func(number string) {
		account := normalizeSignalAddress(number)
		if account == "" {
			return
		}
		if _, ok := seen[account]; ok {
			return
		}
		seen[account] = struct{}{}
		accounts = append(accounts, account)
	}

	var list []signalAccount
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, item := range list {
			appendAccount(item.Number)
		}
	}

	var wrapped struct {
		Accounts []signalAccount `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		for _, item := range wrapped.Accounts {
			appendAccount(item.Number)
		}
	}

	return accounts
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

func parseSignalGroups(raw []byte) map[string]string {
	groups := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Id: ") {
			continue
		}
		rest := strings.TrimPrefix(line, "Id: ")
		nameIndex := strings.Index(rest, " Name: ")
		if nameIndex == -1 {
			continue
		}
		groupID := strings.TrimSpace(rest[:nameIndex])
		namePart := rest[nameIndex+len(" Name: "):]
		activeIndex := strings.Index(namePart, "  Active: ")
		if activeIndex != -1 {
			namePart = namePart[:activeIndex]
		}
		name := strings.TrimSpace(namePart)
		if groupID == "" || name == "" {
			continue
		}
		groups[groupID] = name
	}
	return groups
}

func parseSignalContacts(raw []byte) map[string]string {
	contacts := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Number: ") {
			continue
		}
		rest := strings.TrimPrefix(line, "Number: ")
		aciIndex := strings.Index(rest, " ACI: ")
		if aciIndex == -1 {
			continue
		}
		number := normalizeSignalAddress(strings.TrimSpace(rest[:aciIndex]))
		remainder := rest[aciIndex+len(" ACI: "):]
		nameIndex := strings.Index(remainder, " Name: ")
		if nameIndex == -1 {
			continue
		}
		aci := normalizeSignalAddress(strings.TrimSpace(remainder[:nameIndex]))
		if aci == "" || number == "" {
			continue
		}
		contacts[aci] = number
	}
	return contacts
}

func parseConversationTarget(conversationID string) (target string, isGroup bool, err error) {
	conversationID = strings.TrimSpace(conversationID)
	switch {
	case strings.HasPrefix(conversationID, "signal-group:"):
		target = strings.TrimSpace(strings.TrimPrefix(conversationID, "signal-group:"))
		isGroup = true
	case strings.HasPrefix(conversationID, "signal:"):
		target = normalizeSignalAddress(strings.TrimPrefix(conversationID, "signal:"))
	default:
		err = fmt.Errorf("invalid Signal conversation id %q", conversationID)
	}
	if strings.TrimSpace(target) == "" && err == nil {
		err = fmt.Errorf("missing Signal conversation target")
	}
	return
}

func signalConversationID(address, groupID string) string {
	if groupID = strings.TrimSpace(groupID); groupID != "" {
		return "signal-group:" + groupID
	}
	return "signal:" + normalizeSignalAddress(address)
}

func normalizeSignalAddress(value string) string {
	return strings.TrimSpace(value)
}

func signalIncomingSourceID(conversationID, sender string, timestamp int64, body string) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		strings.TrimSpace(conversationID),
		strings.TrimSpace(sender),
		strconv.FormatInt(timestamp, 10),
		strings.TrimSpace(body),
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func localOutgoingMessageID(conversationID string, timestamp int64, body string) string {
	return "signal:local:" + signalIncomingSourceID(conversationID, "me", timestamp, body)
}

func (b *Bridge) matchLocalOutgoingMessage(conversationID, body string, timestamp int64) *db.Message {
	if b == nil || b.store == nil {
		return nil
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	msgs, err := b.store.GetMessagesByConversation(conversationID, 25)
	if err != nil {
		return nil
	}
	for _, msg := range msgs {
		if msg == nil || !msg.IsFromMe || msg.SourcePlatform != "signal" {
			continue
		}
		if strings.TrimSpace(msg.Body) != body {
			continue
		}
		if !strings.HasPrefix(msg.MessageID, "signal:local:") {
			continue
		}
		if absInt64(msg.TimestampMS-timestamp) > int64(15*time.Second/time.Millisecond) {
			continue
		}
		return msg
	}
	return nil
}

func signalQuoteReplyID(conversationID string, quote *signalQuotedMessage) string {
	if quote == nil || quote.Timestamp == 0 {
		return ""
	}
	author := normalizeSignalAddress(quote.Author)
	if author == "" {
		author = "unknown"
	}
	sourceID := signalIncomingSourceID(conversationID, author, quote.Timestamp, strings.TrimSpace(quote.Text))
	return "signal:" + sourceID
}

func signalMentionsMe(mentions []signalMention, account string) bool {
	account = normalizeSignalAddress(account)
	if account == "" {
		return false
	}
	for _, mention := range mentions {
		targets := []string{
			mention.Number,
			mention.RecipientNumber,
			mention.Recipient,
		}
		for _, target := range targets {
			if addressesMatch(target, account) {
				return true
			}
		}
	}
	return false
}

func signalReactionActorID(env *signalEnvelope) string {
	if env == nil {
		return ""
	}
	return firstNonEmpty(env.SourceNumber, env.SourceUUID, env.Source)
}

func signalReactionTargetAuthor(reaction *signalReaction, account string) string {
	if reaction == nil {
		return ""
	}
	return firstNonEmpty(
		reaction.Target.AuthorNumber,
		reaction.Target.AuthorUUID,
		reaction.Target.Author,
		reaction.TargetAuthorNumber,
		reaction.TargetAuthorUUID,
		reaction.TargetAuthor,
		account,
	)
}

func signalReactionTargetTimestamp(reaction *signalReaction) int64 {
	if reaction == nil {
		return 0
	}
	if reaction.TargetSentTimestamp != 0 {
		return reaction.TargetSentTimestamp
	}
	return reaction.Target.Timestamp
}

func signalReactionStoreEmoji(emoji, action string) string {
	if strings.EqualFold(strings.TrimSpace(action), "remove") {
		return ""
	}
	return strings.TrimSpace(emoji)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func signalAttachmentPlaceholder(attachments []signalAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	mime := strings.ToLower(strings.TrimSpace(attachments[0].ContentType))
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "[Photo]"
	case strings.HasPrefix(mime, "video/"):
		return "[Video]"
	case strings.HasPrefix(mime, "audio/"):
		return "[Audio]"
	default:
		return "[Attachment]"
	}
}

const signalAttachmentPrefix = "signalatt:"
const signalLocalAttachmentPrefix = "signallocal:"

func encodeSignalAttachmentRef(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return signalAttachmentPrefix + id
}

func encodeSignalLocalAttachmentRef(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return signalLocalAttachmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(path))
}

func decodeSignalAttachmentRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, signalAttachmentPrefix) {
		return "", errors.New("invalid Signal attachment reference")
	}
	id := strings.TrimSpace(strings.TrimPrefix(value, signalAttachmentPrefix))
	if id == "" {
		return "", errors.New("empty Signal attachment reference")
	}
	return id, nil
}

func decodeSignalLocalAttachmentRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, signalLocalAttachmentPrefix) {
		return "", errors.New("invalid Signal local attachment reference")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(value, signalLocalAttachmentPrefix))
	if raw == "" {
		return "", errors.New("empty Signal local attachment reference")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode Signal local attachment reference: %w", err)
	}
	path := strings.TrimSpace(string(decoded))
	if path == "" {
		return "", errors.New("empty Signal local attachment path")
	}
	return path, nil
}

func (b *Bridge) DownloadMedia(msg *db.Message) ([]byte, string, error) {
	if msg == nil {
		return nil, "", errors.New("signal media message is required")
	}
	if localPath, err := decodeSignalLocalAttachmentRef(msg.MediaID); err == nil {
		data, readErr := os.ReadFile(localPath)
		if readErr != nil {
			return nil, "", fmt.Errorf("read local Signal attachment: %w", readErr)
		}
		mimeType := strings.TrimSpace(msg.MimeType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return data, mimeType, nil
	}
	attachmentID, err := decodeSignalAttachmentRef(msg.MediaID)
	if err != nil {
		return nil, "", err
	}
	account, err := b.usableAccount()
	if err != nil {
		return nil, "", err
	}

	args := []string{"-a", account, "getAttachment", "--id", attachmentID}
	target, isGroup, err := parseConversationTarget(msg.ConversationID)
	if err != nil {
		return nil, "", err
	}
	if isGroup {
		args = append(args, "--group-id", target)
	} else {
		args = append(args, "--recipient", target)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	b.commandMu.Lock()
	output, err := runSignalCLI(ctx, b.configDir, args...)
	b.commandMu.Unlock()
	if err != nil {
		return nil, "", commandError("download Signal attachment", err, output)
	}
	payload := strings.TrimSpace(string(output))
	if payload == "" {
		return nil, "", errors.New("signal attachment is empty")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", fmt.Errorf("decode Signal attachment: %w", err)
		}
	}
	return data, msg.MimeType, nil
}

func (b *Bridge) signalQuoteArgs(replyToID, account string) ([]string, error) {
	replyToID = strings.TrimSpace(replyToID)
	if replyToID == "" {
		return nil, nil
	}
	if b == nil || b.store == nil {
		return nil, nil
	}
	target, err := b.store.GetMessageByID(replyToID)
	if err != nil {
		return nil, fmt.Errorf("load Signal reply target: %w", err)
	}
	if target == nil || target.SourcePlatform != "signal" {
		return nil, errors.New("signal reply target not found")
	}
	if target.TimestampMS == 0 {
		return nil, errors.New("signal reply target timestamp is unavailable")
	}
	author := normalizeSignalAddress(target.SenderNumber)
	if target.IsFromMe || addressesMatch(author, account) || author == "" {
		author = account
	} else {
		author = b.resolveContactAddress(author)
	}
	if author == "" {
		return nil, errors.New("signal reply target author is unavailable")
	}
	quoteBody := strings.TrimSpace(target.Body)
	if quoteBody == "" && target.MediaID != "" {
		quoteBody = signalAttachmentPlaceholder([]signalAttachment{{ContentType: target.MimeType}})
	}
	if quoteBody == "" {
		quoteBody = "Attachment"
	}
	return []string{
		"--quote-timestamp", strconv.FormatInt(target.TimestampMS, 10),
		"--quote-author", author,
		"--quote-message", quoteBody,
	}, nil
}

func (b *Bridge) writeLocalAttachment(data []byte, filename string) (string, error) {
	cacheDir := filepath.Join(b.configDir, "outgoing-attachments")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", fmt.Errorf("create Signal attachment cache: %w", err)
	}
	pattern := "signal-*"
	if ext := strings.TrimSpace(filepath.Ext(filename)); ext != "" {
		pattern += ext
	}
	file, err := os.CreateTemp(cacheDir, pattern)
	if err != nil {
		return "", fmt.Errorf("create Signal attachment temp file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("write Signal attachment temp file: %w", err)
	}
	return file.Name(), nil
}

func cleanupLocalSignalAttachment(mediaID string) {
	path, err := decodeSignalLocalAttachmentRef(mediaID)
	if err != nil || path == "" {
		return
	}
	_ = os.Remove(path)
}

func sanitizeSignalOutput(line string) string {
	line = strings.ReplaceAll(line, "\r", "")
	for {
		start := strings.Index(line, "\x1b")
		if start == -1 {
			return strings.TrimSpace(line)
		}
		end := start + 1
		for end < len(line) {
			ch := line[end]
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
				end++
				break
			}
			end++
		}
		line = line[:start] + line[end:]
	}
}

func extractSignalLinkURI(line string) string {
	idx := strings.Index(line, "sgnl://linkdevice?")
	if idx == -1 {
		return ""
	}
	return strings.TrimSpace(line[idx:])
}

func commandError(prefix string, err error, output []byte) error {
	return fmt.Errorf("%s: %s", prefix, cleanSignalCommandOutput(err, output))
}

func cleanSignalCommandOutput(err error, output []byte) string {
	lines := []string{}
	if err != nil {
		lines = append(lines, strings.TrimSpace(err.Error()))
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = sanitizeSignalOutput(line)
		if line == "" || strings.HasPrefix(line, "████") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(uniqueStrings(lines), ": "))
}

func isSignalAccountInvalid(err error, output []byte) bool {
	text := strings.ToLower(cleanSignalCommandOutput(err, output))
	return strings.Contains(text, "not registered") ||
		strings.Contains(text, "authorization failed") ||
		strings.Contains(text, "invalid account")
}

func uniqueStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func marshalParticipants(items []participantJSON) (string, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func addressesMatch(a, b string) bool {
	a = normalizeSignalAddress(a)
	b = normalizeSignalAddress(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
