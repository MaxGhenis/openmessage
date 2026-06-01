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
	"runtime/debug"
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
	receiveRecoveryWindow = 30 * time.Second
	reactionMatchWindow   = 15 * time.Second
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
	Connected   bool   `json:"connected"`
	Connecting  bool   `json:"connecting"`
	Paired      bool   `json:"paired"`
	Pairing     bool   `json:"pairing"`
	Account     string `json:"account,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	QRAvailable bool   `json:"qr_available"`
	QRUpdatedAt int64  `json:"qr_updated_at,omitempty"`
	// NeedsReauth is set when signal-cli reports that the stored account is
	// no longer registered / authorized (e.g. user re-registered Signal on a
	// new phone, or the linked device was unlinked remotely). When true,
	// automatic reconnects should stop — the user has to visit Platforms and
	// re-pair manually. The UI should surface this prominently; otherwise
	// Signal silently stops receiving with no indication.
	NeedsReauth     bool                   `json:"needs_reauth,omitempty"`
	HistorySync     *HistorySyncSnapshot   `json:"history_sync,omitempty"`
	ReceiveRecovery *ReceiveRecoveryStatus `json:"receive_recovery,omitempty"`
}

type HistorySyncSnapshot struct {
	Running               bool  `json:"running"`
	StartedAt             int64 `json:"started_at,omitempty"`
	CompletedAt           int64 `json:"completed_at,omitempty"`
	ImportedConversations int   `json:"imported_conversations,omitempty"`
	ImportedMessages      int   `json:"imported_messages,omitempty"`
}

type ReceiveRecoveryStatus struct {
	PendingCount    int    `json:"pending_count"`
	LastIssueAt     int64  `json:"last_issue_at,omitempty"`
	LastIssueReason string `json:"last_issue_reason,omitempty"`
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
	mu         sync.RWMutex
	commandMu  sync.Mutex
	recoveryMu sync.Mutex

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
	// needsReauth is set when signal-cli reports the stored account is no
	// longer registered / authorized. Cleared on successful pair or
	// reconnect. While set, the reconnect ticker skips this bridge so we
	// don't hammer signal-cli with a known-bad account every 5 seconds.
	needsReauth bool

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
	lastReceiveRecoveryAt int64
}

type signalReceiveRecoveryRecord struct {
	TimestampMS int64  `json:"timestamp_ms"`
	Account     string `json:"account,omitempty"`
	Reason      string `json:"reason"`
	Error       string `json:"error,omitempty"`
	Raw         string `json:"raw"`
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
	Source          string               `json:"source"`
	SourceName      string               `json:"sourceName"`
	SourceNumber    string               `json:"sourceNumber"`
	SourceUUID      string               `json:"sourceUuid"`
	SourceServiceID string               `json:"sourceServiceId"`
	Timestamp       int64                `json:"timestamp"`
	DataMessage     *signalDataMessage   `json:"dataMessage"`
	EditMessage     *signalEditMessage   `json:"editMessage"`
	SyncMessage     *signalSyncMessage   `json:"syncMessage"`
	TypingMessage   *signalTypingMessage `json:"typingMessage"`
}

type signalSyncMessage struct {
	SentMessage *signalSentMessage `json:"sentMessage"`
}

type signalDataMessage struct {
	Timestamp          int64                `json:"timestamp"`
	Message            string               `json:"message"`
	GroupInfo          *signalGroupInfo     `json:"groupInfo"`
	Attachments        []signalAttachment   `json:"attachments"`
	Mentions           []signalMention      `json:"mentions"`
	Reaction           *signalReaction      `json:"reaction"`
	Quote              *signalQuotedMessage `json:"quote"`
	IsExpirationUpdate bool                 `json:"isExpirationUpdate"`
	ViewOnce           bool                 `json:"viewOnce"`
	Payment            json.RawMessage      `json:"payment"`
	Previews           []json.RawMessage    `json:"previews"`
	Sticker            json.RawMessage      `json:"sticker"`
	RemoteDelete       json.RawMessage      `json:"remoteDelete"`
	Contacts           []json.RawMessage    `json:"contacts"`
	PollCreate         json.RawMessage      `json:"pollCreate"`
	PollVote           json.RawMessage      `json:"pollVote"`
	PollTerminate      json.RawMessage      `json:"pollTerminate"`
	StoryContext       json.RawMessage      `json:"storyContext"`
	PinMessage         json.RawMessage      `json:"pinMessage"`
	UnpinMessage       json.RawMessage      `json:"unpinMessage"`
	AdminDelete        json.RawMessage      `json:"adminDelete"`
}

type signalSentMessage struct {
	Timestamp            int64                `json:"timestamp"`
	Message              string               `json:"message"`
	Destination          string               `json:"destination"`
	DestinationNumber    string               `json:"destinationNumber"`
	DestinationE164      string               `json:"destinationE164"`
	DestinationUUID      string               `json:"destinationUuid"`
	DestinationServiceID string               `json:"destinationServiceId"`
	EditMessage          *signalEditMessage   `json:"editMessage"`
	GroupInfo            *signalGroupInfo     `json:"groupInfo"`
	Attachments          []signalAttachment   `json:"attachments"`
	Mentions             []signalMention      `json:"mentions"`
	Reaction             *signalReaction      `json:"reaction"`
	Quote                *signalQuotedMessage `json:"quote"`
	IsExpirationUpdate   bool                 `json:"isExpirationUpdate"`
	ViewOnce             bool                 `json:"viewOnce"`
	Payment              json.RawMessage      `json:"payment"`
	Previews             []json.RawMessage    `json:"previews"`
	Sticker              json.RawMessage      `json:"sticker"`
	RemoteDelete         json.RawMessage      `json:"remoteDelete"`
	Contacts             []json.RawMessage    `json:"contacts"`
	PollCreate           json.RawMessage      `json:"pollCreate"`
	PollVote             json.RawMessage      `json:"pollVote"`
	PollTerminate        json.RawMessage      `json:"pollTerminate"`
	StoryContext         json.RawMessage      `json:"storyContext"`
	PinMessage           json.RawMessage      `json:"pinMessage"`
	UnpinMessage         json.RawMessage      `json:"unpinMessage"`
	AdminDelete          json.RawMessage      `json:"adminDelete"`
}

type signalGroupInfo struct {
	GroupID   string `json:"groupId"`
	Title     string `json:"title"`
	GroupName string `json:"groupName"`
	Type      string `json:"type"`
}

type signalEditMessage struct {
	TargetSentTimestamp int64              `json:"targetSentTimestamp"`
	DataMessage         *signalDataMessage `json:"dataMessage"`
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
	Emoji                 string `json:"emoji"`
	TargetAuthor          string `json:"targetAuthor"`
	TargetAuthorNumber    string `json:"targetAuthorNumber"`
	TargetAuthorUUID      string `json:"targetAuthorUuid"`
	TargetAuthorACI       string `json:"targetAuthorAci"`
	TargetAuthorServiceID string `json:"targetAuthorServiceId"`
	TargetSentTimestamp   int64  `json:"targetSentTimestamp"`
	IsRemove              bool   `json:"isRemove"`
	Target                struct {
		Timestamp       int64  `json:"timestamp"`
		Author          string `json:"author"`
		AuthorNumber    string `json:"authorNumber"`
		AuthorUUID      string `json:"authorUuid"`
		AuthorACI       string `json:"authorAci"`
		AuthorServiceID string `json:"authorServiceId"`
	} `json:"target"`
}

type signalQuotedMessage struct {
	Timestamp int64  `json:"timestamp"`
	Author    string `json:"author"`
	AuthorACI string `json:"authorAci"`
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
		b.needsReauth = false
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
	b.needsReauth = false
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
	b.needsReauth = false
	b.lastError = ""
	b.qr = QRSnapshot{}
	b.historySync = struct {
		startedAt             int64
		lastImportAt          int64
		importedConversations int
		importedMessages      int
	}{}
	b.lastReceiveRecoveryAt = 0
	b.mu.Unlock()
	b.emitStatusChange()
	return nil
}

func (b *Bridge) Status() StatusSnapshot {
	b.mu.RLock()
	account := b.account
	if account == "" {
		account = b.firstStoredAccount()
	}
	snapshot := StatusSnapshot{
		Connected:   b.connected,
		Connecting:  b.connecting,
		Paired:      account != "",
		Pairing:     b.pairing,
		Account:     account,
		LastError:   b.lastError,
		QRAvailable: b.qr.URI != "",
		QRUpdatedAt: b.qr.UpdatedAt,
		NeedsReauth: b.needsReauth,
		HistorySync: b.historySyncSnapshotLocked(),
	}
	b.mu.RUnlock()
	snapshot.ReceiveRecovery = b.receiveRecoveryStatus()
	return snapshot
}

func (b *Bridge) ReplayReceiveRecoveryQueue() error {
	account, err := b.usableAccount()
	if err != nil {
		return err
	}
	b.replayReceiveRecoveryQueue(account)
	return nil
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
		SourceID:       strings.TrimPrefix(messageID, "signal:"),
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
	// Use `--attachment=path` (not `-a path`). signal-cli's send subcommand
	// defines -a/--attachment with nargs='*', which makes argparse greedily
	// consume every following token as another attachment — including the
	// positional recipient. Anchoring the value with `=` prevents that and
	// keeps the recipient available for parsing as the positional.
	// Reproduced with "No recipients given" when sending to an ACI-only contact.
	args = append(args, "--attachment="+attachmentPath)
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
		SourceID:       strings.TrimPrefix(messageID, "signal:"),
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
	if err := b.store.UpdateMessageReactions(target.MessageID, nextReactions); err != nil {
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
	// A panic while parsing an attacker-influenced envelope (unchecked indexes
	// into attachments/reactions/quotes) would otherwise kill this goroutine
	// and leave connected=true — Signal silently freezes. Recover, reset the
	// connection state, and let the reconnect watchdog re-spawn the loop.
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error().
				Interface("panic", r).
				Bytes("stack", debug.Stack()).
				Msg("Recovered from panic in Signal receive loop")
			b.mu.Lock()
			b.connected = false
			b.connecting = false
			b.mu.Unlock()
			b.emitStatusChange()
		}
	}()
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
	b.needsReauth = false // successful connect clears any prior re-auth flag
	b.lastError = ""
	b.mu.Unlock()
	b.emitStatusChange()
	go b.refreshMetadataAndReplay(probedAccount)
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
				// Signal-side says the account is no longer registered /
				// authorized. Don't clear b.account — we want the UI to
				// know *which* account needs re-pairing. Instead flip
				// needsReauth so the reconnect ticker stops retrying and
				// the UI surfaces a clear "re-pair Signal" banner.
				b.mu.Lock()
				if b.receiveToken == token {
					b.receiveCancel = nil
				}
				b.connected = false
				b.connecting = false
				b.needsReauth = true
				b.lastError = cleanSignalCommandOutput(err, output)
				b.logger.Warn().Str("account", b.account).Msg("Signal account needs re-pairing (signal-cli reports unregistered/unauthorized)")
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

func (b *Bridge) refreshMetadataAndReplay(account string) {
	b.refreshContacts()
	b.refreshGroupNames()
	b.drainReceiveWAL(account)
	b.replayReceiveRecoveryQueue(account)
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
	// Durability: signal-cli ACKs the batch to Signal's servers as it
	// streams it to stdout. If we crash between reading `output` and
	// committing the DB rows, those messages are gone from the server
	// forever. Persist the raw batch to a write-ahead log before we
	// process any of it; drainReceiveWAL on startup replays anything
	// we didn't finish processing cleanly. DB writes are idempotent
	// (source_id uniqueness), so replay is safe even for lines we did
	// commit before the crash.
	walPath := b.receiveWALPath()
	if err := appendReceiveWAL(walPath, account, output); err != nil {
		b.logger.Warn().Err(err).Msg("Failed to persist signal-cli batch to WAL — continuing with best-effort processing")
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		if _, err := b.processReceiveLine(account, scanner.Bytes(), true); err != nil {
			b.logger.Debug().Err(err).Msg("Failed to process Signal receive payload")
		}
	}
	if err := scanner.Err(); err != nil {
		// Leave the WAL in place — startup drain will retry.
		return err
	}
	// All lines processed (or quarantined to recovery). Drop the WAL.
	_ = os.Remove(walPath)
	return nil
}

func (b *Bridge) receiveWALPath() string {
	return filepath.Join(b.configDir, "signal-receive-wal.ndjson")
}

// appendReceiveWAL writes every JSON line in `output` to the WAL under a
// shared lock so concurrent receive polls append atomically. fsync after
// the write so a crash between signal-cli ACK and DB commit doesn't lose
// the batch.
func appendReceiveWAL(path, account string, output []byte) error {
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	nowMS := now().UnixMilli()
	account = normalizeSignalAddress(account)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		record := signalReceiveRecoveryRecord{
			TimestampMS: nowMS,
			Account:     account,
			Reason:      "wal",
			Raw:         string(line),
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return file.Sync()
}

// drainReceiveWAL re-processes any WAL entries left over from a prior
// crash or shutdown. Called from startup (refreshMetadataAndReplay)
// before the receive loop begins polling signal-cli again.
func (b *Bridge) drainReceiveWAL(account string) {
	if b == nil {
		return
	}
	path := b.receiveWALPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			b.logger.Debug().Err(err).Msg("Failed to read Signal receive WAL")
		}
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		_ = os.Remove(path)
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	replayed := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record signalReceiveRecoveryRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		replayAccount := firstNonEmpty(strings.TrimSpace(record.Account), normalizeSignalAddress(account))
		if _, err := b.processReceiveLine(replayAccount, []byte(record.Raw), true); err != nil {
			b.logger.Debug().Err(err).Msg("Failed to replay Signal WAL entry")
		}
		replayed++
	}
	_ = os.Remove(path)
	if replayed > 0 {
		b.logger.Info().Int("replayed", replayed).Msg("Drained Signal receive WAL after restart")
	}
}

func (b *Bridge) processReceiveLine(account string, rawLine []byte, allowRecovery bool) (bool, error) {
	line := bytes.TrimSpace(rawLine)
	if len(line) == 0 {
		return true, nil
	}
	if line[0] != '{' {
		return true, nil
	}
	var payload signalReceivePayload
	if err := json.Unmarshal(line, &payload); err != nil {
		if allowRecovery {
			b.recordReceiveRecoveryIssue(account, line, "unmarshal_failed", err)
		}
		return false, nil
	}
	env := payload.Envelope
	payloadAccount := strings.TrimSpace(payload.Account)
	if payload.Result != nil {
		if payloadAccount == "" {
			payloadAccount = strings.TrimSpace(payload.Result.Account)
		}
		if env.Timestamp == 0 && env.Source == "" && env.SourceNumber == "" && env.SourceUUID == "" && env.SourceServiceID == "" && env.DataMessage == nil && env.EditMessage == nil && env.SyncMessage == nil && env.TypingMessage == nil {
			env = payload.Result.Envelope
		}
	}
	if payloadAccount == "" {
		payloadAccount = account
	}
	if reason := signalEnvelopeRecoveryReason(&env); reason != "" {
		if allowRecovery {
			b.recordReceiveRecoveryIssue(payloadAccount, line, reason, nil)
		}
		return false, nil
	}
	if env.TypingMessage != nil {
		b.handleTypingMessage(payloadAccount, &env)
	}
	if env.EditMessage != nil {
		if err := b.handleEditMessage(payloadAccount, &env, !allowRecovery); err != nil {
			if allowRecovery {
				b.recordReceiveRecoveryIssue(payloadAccount, line, "handle_edit_message_failed", err)
			}
			return false, fmt.Errorf("apply Signal edit: %w", err)
		}
	}
	if env.DataMessage != nil {
		if err := b.handleDataMessage(payloadAccount, &env); err != nil {
			if allowRecovery {
				b.recordReceiveRecoveryIssue(payloadAccount, line, "handle_data_message_failed", err)
			}
			return false, fmt.Errorf("store Signal message: %w", err)
		}
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		if err := b.handleSentMessage(payloadAccount, &env, !allowRecovery); err != nil {
			if allowRecovery {
				b.recordReceiveRecoveryIssue(payloadAccount, line, "handle_sent_message_failed", err)
			}
			return false, fmt.Errorf("store Signal sent sync message: %w", err)
		}
	}
	return true, nil
}

func (b *Bridge) recordReceiveRecoveryIssue(account string, rawLine []byte, reason string, err error) {
	if err := b.appendReceiveRecoveryRecord(account, rawLine, reason, err); err != nil {
		b.logger.Warn().Err(err).Str("reason", reason).Msg("Failed to quarantine Signal receive payload")
	}
	if !b.shouldTriggerReceiveRecovery() {
		return
	}
	account = normalizeSignalAddress(account)
	if account == "" {
		return
	}
	b.beginHistorySync()
	b.emitStatusChange()
	go func() {
		if syncErr := b.requestSync(account); syncErr != nil {
			b.logger.Debug().Err(syncErr).Str("reason", reason).Msg("Failed to request Signal recovery sync")
		}
	}()
}

func (b *Bridge) appendReceiveRecoveryRecord(account string, rawLine []byte, reason string, cause error) error {
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()
	account = normalizeSignalAddress(account)
	if err := os.MkdirAll(b.configDir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(b.receiveRecoveryPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	record := signalReceiveRecoveryRecord{
		TimestampMS: now().UnixMilli(),
		Account:     account,
		Reason:      strings.TrimSpace(reason),
		Raw:         string(rawLine),
	}
	if cause != nil {
		record.Error = cause.Error()
	}
	encoded, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		return marshalErr
	}
	if _, writeErr := file.Write(append(encoded, '\n')); writeErr != nil {
		return writeErr
	}
	return nil
}

func (b *Bridge) replayReceiveRecoveryQueue(account string) {
	if b == nil {
		return
	}
	path := b.receiveRecoveryPath()
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			b.logger.Debug().Err(err).Msg("Failed to read Signal receive recovery queue")
		}
		return
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	remaining := make([][]byte, 0)
	recovered := 0
	for scanner.Scan() {
		recordLine := bytes.TrimSpace(scanner.Bytes())
		if len(recordLine) == 0 {
			continue
		}
		var record signalReceiveRecoveryRecord
		if err := json.Unmarshal(recordLine, &record); err != nil {
			remaining = append(remaining, append([]byte(nil), recordLine...))
			continue
		}
		replayAccount := firstNonEmpty(strings.TrimSpace(record.Account), normalizeSignalAddress(account))
		resolved, err := b.processReceiveLine(replayAccount, []byte(record.Raw), false)
		if err != nil {
			b.logger.Debug().Err(err).Str("reason", record.Reason).Msg("Failed to replay Signal recovery payload")
		}
		if resolved {
			recovered++
			continue
		}
		remaining = append(remaining, append([]byte(nil), recordLine...))
	}
	if err := scanner.Err(); err != nil {
		b.logger.Debug().Err(err).Msg("Failed to scan Signal receive recovery queue")
		return
	}
	if err := rewriteRecoveryQueue(path, remaining); err != nil {
		b.logger.Warn().Err(err).Msg("Failed to rewrite Signal receive recovery queue")
		return
	}
	if recovered > 0 {
		b.logger.Debug().Int("recovered", recovered).Msg("Replayed Signal recovery payloads")
	}
}

func rewriteRecoveryQueue(path string, lines [][]byte) error {
	if len(lines) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tempPath := path + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if _, err := file.Write(bytes.TrimSpace(line)); err != nil {
			file.Close()
			return err
		}
		if _, err := file.Write([]byte{'\n'}); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (b *Bridge) receiveRecoveryPath() string {
	return filepath.Join(b.configDir, "signal-receive-recovery.ndjson")
}

func (b *Bridge) receiveRecoveryStatus() *ReceiveRecoveryStatus {
	if b == nil {
		return nil
	}
	b.recoveryMu.Lock()
	defer b.recoveryMu.Unlock()
	raw, err := os.ReadFile(b.receiveRecoveryPath())
	if err != nil {
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	status := &ReceiveRecoveryStatus{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		status.PendingCount++
		var record signalReceiveRecoveryRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if status.LastIssueAt == 0 && status.LastIssueReason == "" {
				status.LastIssueReason = "invalid_record"
			}
			continue
		}
		if record.TimestampMS >= status.LastIssueAt {
			status.LastIssueAt = record.TimestampMS
			status.LastIssueReason = strings.TrimSpace(record.Reason)
		}
	}
	if status.PendingCount == 0 {
		return nil
	}
	return status
}

func (b *Bridge) shouldTriggerReceiveRecovery() bool {
	nowMS := now().UnixMilli()
	b.mu.Lock()
	defer b.mu.Unlock()
	if nowMS-b.lastReceiveRecoveryAt < int64(receiveRecoveryWindow/time.Millisecond) {
		return false
	}
	b.lastReceiveRecoveryAt = nowMS
	return true
}

func signalEnvelopeRecoveryReason(env *signalEnvelope) string {
	if env == nil {
		return ""
	}
	if env.EditMessage != nil {
		groupID := ""
		if info := signalEditGroupInfo(env.EditMessage); info != nil {
			groupID = strings.TrimSpace(info.GroupID)
		}
		if signalEnvelopeSource(env) == "" && groupID == "" {
			return "missing_edit_message_source"
		}
	}
	if env.DataMessage != nil {
		groupID := ""
		if env.DataMessage.GroupInfo != nil {
			groupID = strings.TrimSpace(env.DataMessage.GroupInfo.GroupID)
		}
		if signalEnvelopeSource(env) == "" && groupID == "" {
			return "missing_data_message_source"
		}
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		groupID := ""
		if info := signalSentGroupInfo(env.SyncMessage.SentMessage); info != nil {
			groupID = strings.TrimSpace(info.GroupID)
		}
		if signalSentTarget(env.SyncMessage.SentMessage) == "" && groupID == "" {
			return "missing_sent_message_target"
		}
	}
	return ""
}

func (b *Bridge) handleTypingMessage(account string, env *signalEnvelope) {
	if env == nil || env.TypingMessage == nil || b.callbacks.OnTypingChange == nil {
		return
	}
	source := b.resolveContactAddress(signalEnvelopeSource(env))
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

	source := b.resolveContactAddress(signalEnvelopeSource(env))
	groupID := ""
	groupTitle := ""
	if env.DataMessage.GroupInfo != nil {
		groupID = strings.TrimSpace(env.DataMessage.GroupInfo.GroupID)
		groupTitle = signalGroupTitle(env.DataMessage.GroupInfo)
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
	body := env.DataMessage.displayBody()
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, source, false); placeholder != nil {
		body = firstNonEmpty(strings.TrimSpace(placeholder.Body), body)
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
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, source, false); placeholder != nil {
		mergeSignalMissingEditPlaceholder(msg, placeholder)
	}
	if err := b.store.UpsertMessage(msg); err != nil {
		return err
	}
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, source, false); placeholder != nil && placeholder.MessageID != msg.MessageID {
		if err := b.store.DeleteMessageByID(placeholder.MessageID); err != nil {
			b.logger.Debug().Err(err).Str("placeholder_msg_id", placeholder.MessageID).Msg("Failed to delete superseded Signal missing-edit placeholder")
		}
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

func (b *Bridge) handleEditMessage(account string, env *signalEnvelope, synthesizeOnMissing bool) error {
	if env == nil || env.EditMessage == nil || env.EditMessage.DataMessage == nil {
		return nil
	}

	source := b.resolveContactAddress(signalEnvelopeSource(env))
	groupID := ""
	groupTitle := ""
	if info := signalEditGroupInfo(env.EditMessage); info != nil {
		groupID = strings.TrimSpace(info.GroupID)
		groupTitle = signalGroupTitle(info)
	}
	if groupTitle == "" && groupID != "" {
		groupTitle = b.groupName(groupID)
	}
	if source == "" && groupID == "" {
		return nil
	}
	if source != "" && addressesMatch(source, account) {
		return nil
	}
	conversationID := signalConversationID(source, groupID)
	if err := b.ensureSignalConversation(conversationID, source, groupID, groupTitle, firstNonEmpty(strings.TrimSpace(env.SourceName), source), env.Timestamp, "signal", 0); err != nil {
		return err
	}
	err := b.applySignalEdit(conversationID, env.EditMessage.TargetSentTimestamp, source, env.EditMessage.DataMessage, account)
	if err == nil || !errors.Is(err, errSignalEditTargetNotFound) || !synthesizeOnMissing {
		return err
	}
	return b.materializeMissingSignalEdit(signalMissingEditArgs{
		ConversationID: conversationID,
		TimestampMS:    env.EditMessage.TargetSentTimestamp,
		SenderName:     firstNonEmpty(strings.TrimSpace(env.SourceName), source),
		SenderNumber:   source,
		Body:           env.EditMessage.DataMessage.displayBody(),
		ReplyToID:      signalQuoteReplyID(conversationID, env.EditMessage.DataMessage.Quote),
		DataMessage:    env.EditMessage.DataMessage,
		Account:        account,
		IsFromMe:       false,
		Status:         "received",
	})
}

func (b *Bridge) handleSentMessage(account string, env *signalEnvelope, synthesizeOnMissing bool) error {
	if env == nil || env.SyncMessage == nil || env.SyncMessage.SentMessage == nil {
		return nil
	}

	sent := env.SyncMessage.SentMessage
	if sent.EditMessage != nil {
		return b.handleSentEditMessage(account, env, synthesizeOnMissing)
	}
	groupID := ""
	groupTitle := ""
	if info := signalSentGroupInfo(sent); info != nil {
		groupID = strings.TrimSpace(info.GroupID)
		groupTitle = signalGroupTitle(info)
	}
	if groupTitle == "" && groupID != "" {
		groupTitle = b.groupName(groupID)
	}
	target := b.resolveContactAddress(signalSentTarget(sent))
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
	body := sent.displayBody()
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, account, true); placeholder != nil {
		body = firstNonEmpty(strings.TrimSpace(placeholder.Body), body)
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
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, account, true); placeholder != nil {
		mergeSignalMissingEditPlaceholder(msg, placeholder)
	}
	if err := b.store.UpsertMessage(msg); err != nil {
		return err
	}
	if placeholder := b.findSignalMissingEditAlias(conversationID, timestamp, account, true); placeholder != nil && placeholder.MessageID != msg.MessageID {
		if err := b.store.DeleteMessageByID(placeholder.MessageID); err != nil {
			b.logger.Debug().Err(err).Str("placeholder_msg_id", placeholder.MessageID).Msg("Failed to delete superseded Signal missing-edit placeholder")
		}
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

func (b *Bridge) handleSentEditMessage(account string, env *signalEnvelope, synthesizeOnMissing bool) error {
	if env == nil || env.SyncMessage == nil || env.SyncMessage.SentMessage == nil || env.SyncMessage.SentMessage.EditMessage == nil {
		return nil
	}

	sent := env.SyncMessage.SentMessage
	groupID := ""
	groupTitle := ""
	if info := signalSentGroupInfo(sent); info != nil {
		groupID = strings.TrimSpace(info.GroupID)
		groupTitle = signalGroupTitle(info)
	}
	if groupTitle == "" && groupID != "" {
		groupTitle = b.groupName(groupID)
	}
	target := b.resolveContactAddress(signalSentTarget(sent))
	if target == "" && groupID == "" {
		return nil
	}
	conversationID := signalConversationID(target, groupID)
	if err := b.ensureSignalConversation(conversationID, target, groupID, groupTitle, target, env.Timestamp, "signal", 0); err != nil {
		return err
	}
	err := b.applySignalEdit(conversationID, sent.EditMessage.TargetSentTimestamp, b.resolveContactAddress(account), sent.EditMessage.DataMessage, account)
	if err == nil || !errors.Is(err, errSignalEditTargetNotFound) || !synthesizeOnMissing {
		return err
	}
	return b.materializeMissingSignalEdit(signalMissingEditArgs{
		ConversationID: conversationID,
		TimestampMS:    sent.EditMessage.TargetSentTimestamp,
		SenderName:     firstNonEmpty(os.Getenv("OPENMESSAGES_MY_NAME"), "Me"),
		SenderNumber:   account,
		Body:           sent.EditMessage.DataMessage.displayBody(),
		ReplyToID:      signalQuoteReplyID(conversationID, sent.EditMessage.DataMessage.Quote),
		DataMessage:    sent.EditMessage.DataMessage,
		Account:        account,
		IsFromMe:       true,
		Status:         "sent",
	})
}

func (b *Bridge) ensureSignalConversation(conversationID, target, groupID, groupTitle, fallbackName string, timestamp int64, platform string, unreadDelta int) error {
	if b == nil || b.store == nil || conversationID == "" {
		return nil
	}
	existing, _ := b.store.GetConversation(conversationID)
	convo := &db.Conversation{
		ConversationID: conversationID,
		Name:           fallbackName,
		IsGroup:        groupID != "",
		LastMessageTS:  timestamp,
		UnreadCount:    maxInt(0, unreadDelta),
		SourcePlatform: platform,
		Participants:   "[]",
	}
	if existing != nil {
		*convo = *existing
		convo.LastMessageTS = maxInt64(existing.LastMessageTS, timestamp)
		convo.IsGroup = groupID != ""
		convo.SourcePlatform = platform
		convo.UnreadCount = maxInt(0, existing.UnreadCount+unreadDelta)
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
			Name:   firstNonEmpty(convo.Name, fallbackName, target),
			Number: target,
		}}); err == nil {
			convo.Participants = participants
		}
	}
	return b.store.UpsertConversation(convo)
}

func (b *Bridge) applySignalEdit(conversationID string, targetTimestamp int64, targetAuthor string, dataMessage *signalDataMessage, account string) error {
	if b == nil || b.store == nil || targetTimestamp == 0 || dataMessage == nil {
		return nil
	}
	targetMessage, err := b.findTimestampTarget(conversationID, targetTimestamp, b.resolveContactAddress(targetAuthor))
	if err != nil {
		return err
	}
	if targetMessage == nil {
		return errSignalEditTargetNotFound
	}
	updated := *targetMessage
	updated.Body = dataMessage.displayBody()
	updated.MentionsMe = signalMentionsMe(dataMessage.Mentions, account)
	if replyToID := signalQuoteReplyID(conversationID, dataMessage.Quote); replyToID != "" {
		updated.ReplyToID = replyToID
	}
	if len(dataMessage.Attachments) > 0 {
		updated.MimeType = strings.TrimSpace(dataMessage.Attachments[0].ContentType)
		updated.MediaID = encodeSignalAttachmentRef(dataMessage.Attachments[0].ID)
	}
	if err := b.store.UpsertMessage(&updated); err != nil {
		return err
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(conversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
	return nil
}

var errSignalEditTargetNotFound = errors.New("signal edit target not found")

type signalMissingEditArgs struct {
	ConversationID string
	TimestampMS    int64
	SenderName     string
	SenderNumber   string
	Body           string
	ReplyToID      string
	DataMessage    *signalDataMessage
	Account        string
	IsFromMe       bool
	Status         string
}

func (b *Bridge) materializeMissingSignalEdit(args signalMissingEditArgs) error {
	if b == nil || b.store == nil || strings.TrimSpace(args.ConversationID) == "" || args.DataMessage == nil {
		return nil
	}
	timestamp := args.TimestampMS
	if timestamp == 0 {
		timestamp = args.DataMessage.Timestamp
	}
	if timestamp == 0 {
		timestamp = now().UnixMilli()
	}
	sourceID := signalMissingEditSourceID(args.ConversationID, args.SenderNumber, timestamp)
	msg := &db.Message{
		MessageID:      "signal:" + sourceID,
		ConversationID: args.ConversationID,
		SenderName:     firstNonEmpty(strings.TrimSpace(args.SenderName), strings.TrimSpace(args.SenderNumber)),
		SenderNumber:   strings.TrimSpace(args.SenderNumber),
		Body:           strings.TrimSpace(args.Body),
		TimestampMS:    timestamp,
		Status:         firstNonEmpty(strings.TrimSpace(args.Status), "received"),
		IsFromMe:       args.IsFromMe,
		MentionsMe:     signalMentionsMe(args.DataMessage.Mentions, args.Account),
		ReplyToID:      strings.TrimSpace(args.ReplyToID),
		SourcePlatform: "signal",
		SourceID:       sourceID,
	}
	if msg.Body == "" {
		msg.Body = args.DataMessage.displayBody()
	}
	if len(args.DataMessage.Attachments) > 0 {
		msg.MimeType = strings.TrimSpace(args.DataMessage.Attachments[0].ContentType)
		msg.MediaID = encodeSignalAttachmentRef(args.DataMessage.Attachments[0].ID)
	}
	if err := b.store.UpsertMessage(msg); err != nil {
		return err
	}
	if b.callbacks.OnMessagesChange != nil {
		b.callbacks.OnMessagesChange(args.ConversationID)
	}
	if b.callbacks.OnConversationsChange != nil {
		b.callbacks.OnConversationsChange()
	}
	return nil
}

func (b *Bridge) findSignalMissingEditAlias(conversationID string, timestampMS int64, senderNumber string, isFromMe bool) *db.Message {
	if b == nil || b.store == nil || strings.TrimSpace(conversationID) == "" || timestampMS == 0 {
		return nil
	}
	messages, err := b.store.GetMessagesByConversationAtTimestamp(conversationID, timestampMS, 10)
	if err != nil {
		return nil
	}
	senderNumber = normalizeSignalAddress(senderNumber)
	var alias *db.Message
	for _, message := range messages {
		if message == nil || !strings.HasPrefix(strings.TrimSpace(message.SourceID), signalMissingEditSourcePrefix) {
			continue
		}
		if message.IsFromMe != isFromMe {
			continue
		}
		if senderNumber != "" && !addressesMatch(normalizeSignalAddress(message.SenderNumber), senderNumber) {
			continue
		}
		if alias != nil {
			return nil
		}
		alias = message
	}
	return alias
}

func mergeSignalMissingEditPlaceholder(target, placeholder *db.Message) {
	if target == nil || placeholder == nil {
		return
	}
	target.Body = firstNonEmpty(strings.TrimSpace(placeholder.Body), strings.TrimSpace(target.Body))
	target.ReplyToID = firstNonEmpty(strings.TrimSpace(placeholder.ReplyToID), strings.TrimSpace(target.ReplyToID))
	if placeholder.MentionsMe {
		target.MentionsMe = true
	}
	if strings.TrimSpace(target.MediaID) == "" {
		target.MediaID = strings.TrimSpace(placeholder.MediaID)
	}
	if strings.TrimSpace(target.MimeType) == "" {
		target.MimeType = strings.TrimSpace(placeholder.MimeType)
	}
	if strings.TrimSpace(target.DecryptionKey) == "" {
		target.DecryptionKey = strings.TrimSpace(placeholder.DecryptionKey)
	}
	if strings.TrimSpace(target.Reactions) == "" {
		target.Reactions = strings.TrimSpace(placeholder.Reactions)
	}
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
	if err := b.store.UpdateMessageReactions(targetMessage.MessageID, nextReactions); err != nil {
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
	targetAuthor := b.resolveContactAddress(signalReactionTargetAuthor(reaction, account))
	return b.findTimestampTarget(conversationID, targetTimestamp, targetAuthor)
}

func (b *Bridge) findTimestampTarget(conversationID string, targetTimestamp int64, targetAuthor string) (*db.Message, error) {
	if b == nil || b.store == nil || targetTimestamp == 0 {
		return nil, nil
	}
	messages, err := b.store.GetMessagesByConversationAtTimestamp(conversationID, targetTimestamp, 10)
	if err != nil {
		return nil, err
	}
	if target := pickReactionTargetMessage(messages, targetAuthor, targetTimestamp); target != nil {
		return target, nil
	}
	windowMS := int64(reactionMatchWindow / time.Millisecond)
	messages, err = b.store.GetMessagesByConversationBetween(conversationID, targetTimestamp-windowMS, targetTimestamp+windowMS, 50)
	if err != nil {
		return nil, err
	}
	return pickReactionTargetMessage(messages, targetAuthor, targetTimestamp), nil
}

func pickReactionTargetMessage(messages []*db.Message, targetAuthor string, targetTimestamp int64) *db.Message {
	if len(messages) == 0 {
		return nil
	}
	var best *db.Message
	bestDelta := int64(-1)
	bestAuthorMatch := false
	for _, message := range messages {
		if message == nil {
			continue
		}
		authorMatch := targetAuthor != "" && addressesMatch(normalizeSignalAddress(message.SenderNumber), targetAuthor)
		if targetAuthor != "" && !authorMatch {
			continue
		}
		delta := absInt64(message.TimestampMS - targetTimestamp)
		if best == nil || delta < bestDelta || (!bestAuthorMatch && authorMatch) {
			best = message
			bestDelta = delta
			bestAuthorMatch = authorMatch
		}
	}
	if best != nil || targetAuthor != "" {
		return best
	}
	for _, message := range messages {
		if message != nil {
			return message
		}
	}
	return nil
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
		if isSignalAccountAddress(line) {
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
		if !isSignalAccountAddress(account) {
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

func isSignalAccountAddress(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 3 || value[0] != '+' {
		return false
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// signalIncomingSourceID computes a stable SHA-1 message id from a Signal
// envelope. The body is deliberately NOT part of the hash: Signal identifies
// a message by (sender, sent-timestamp) and an edit arrives as an update to
// that same logical message. Hashing in the body would produce a different
// id for each edit and manifest as duplicate rows in the thread. The body
// argument remains for call-site symmetry but is unused — retained so all
// call sites still pass it as a reminder that body changes must not shift
// the identity.
func signalIncomingSourceID(conversationID, sender string, timestamp int64, body string) string {
	_ = body
	sum := sha1.Sum([]byte(strings.Join([]string{
		strings.TrimSpace(conversationID),
		strings.TrimSpace(sender),
		strconv.FormatInt(timestamp, 10),
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func localOutgoingMessageID(conversationID string, timestamp int64, body string) string {
	return "signal:local:" + signalIncomingSourceID(conversationID, "me", timestamp, body)
}

func signalMissingEditSourceID(conversationID, sender string, timestamp int64) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		"missing-edit",
		strings.TrimSpace(conversationID),
		strings.TrimSpace(sender),
		strconv.FormatInt(timestamp, 10),
	}, "\x1f")))
	return signalMissingEditSourcePrefix + hex.EncodeToString(sum[:])
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
	var nearestDriftMS int64 = -1
	bodyMismatchSample := ""
	for _, msg := range msgs {
		if msg == nil || !msg.IsFromMe || msg.SourcePlatform != "signal" {
			continue
		}
		if !strings.HasPrefix(msg.MessageID, "signal:local:") {
			continue
		}
		trimmed := strings.TrimSpace(msg.Body)
		drift := absInt64(msg.TimestampMS - timestamp)
		if trimmed != body {
			if bodyMismatchSample == "" && drift <= int64(30*time.Second/time.Millisecond) {
				bodyMismatchSample = trimmed
			}
			continue
		}
		if drift > int64(15*time.Second/time.Millisecond) {
			if nearestDriftMS < 0 || drift < nearestDriftMS {
				nearestDriftMS = drift
			}
			continue
		}
		return msg
	}
	if nearestDriftMS > 0 || bodyMismatchSample != "" {
		b.logger.Warn().
			Str("conversation", conversationID).
			Int64("incoming_ts_ms", timestamp).
			Int64("nearest_drift_ms", nearestDriftMS).
			Str("body_preview", truncateForLog(body, 80)).
			Str("local_body_preview", truncateForLog(bodyMismatchSample, 80)).
			Msg("Signal outgoing dedup missed — may produce duplicate media/text row")
	}
	return nil
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func signalQuoteReplyID(conversationID string, quote *signalQuotedMessage) string {
	if quote == nil || quote.Timestamp == 0 {
		return ""
	}
	author := normalizeSignalAddress(firstNonEmpty(quote.AuthorACI, quote.Author))
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
	return signalEnvelopeSource(env)
}

func signalReactionTargetAuthor(reaction *signalReaction, account string) string {
	if reaction == nil {
		return ""
	}
	return firstNonEmpty(
		reaction.Target.AuthorNumber,
		reaction.Target.AuthorACI,
		reaction.Target.AuthorServiceID,
		reaction.Target.AuthorUUID,
		reaction.Target.Author,
		reaction.TargetAuthorNumber,
		reaction.TargetAuthorACI,
		reaction.TargetAuthorServiceID,
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

func signalEnvelopeSource(env *signalEnvelope) string {
	if env == nil {
		return ""
	}
	return firstNonEmpty(
		strings.TrimSpace(env.SourceNumber),
		strings.TrimSpace(env.SourceServiceID),
		strings.TrimSpace(env.SourceUUID),
		strings.TrimSpace(env.Source),
	)
}

func signalSentTarget(sent *signalSentMessage) string {
	if sent == nil {
		return ""
	}
	return firstNonEmpty(
		strings.TrimSpace(sent.DestinationNumber),
		strings.TrimSpace(sent.DestinationE164),
		strings.TrimSpace(sent.DestinationUUID),
		strings.TrimSpace(sent.DestinationServiceID),
		strings.TrimSpace(sent.Destination),
	)
}

func signalEditGroupInfo(edit *signalEditMessage) *signalGroupInfo {
	if edit == nil || edit.DataMessage == nil {
		return nil
	}
	return edit.DataMessage.GroupInfo
}

func signalSentGroupInfo(sent *signalSentMessage) *signalGroupInfo {
	if sent == nil {
		return nil
	}
	if sent.GroupInfo != nil {
		return sent.GroupInfo
	}
	return signalEditGroupInfo(sent.EditMessage)
}

func signalGroupTitle(info *signalGroupInfo) string {
	if info == nil {
		return ""
	}
	return firstNonEmpty(strings.TrimSpace(info.GroupName), strings.TrimSpace(info.Title))
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const (
	signalUnsupportedMessagePlaceholder = "[Unsupported Signal message]"
	signalMissingEditSourcePrefix       = "missing-edit:"
)

type signalUnsupportedContentFlags struct {
	IsExpirationUpdate bool
	ViewOnce           bool
	HasPayment         bool
	HasPreview         bool
	HasSticker         bool
	HasRemoteDelete    bool
	HasContacts        bool
	HasPollCreate      bool
	HasPollVote        bool
	HasPollTerminate   bool
	HasStoryContext    bool
	HasPinMessage      bool
	HasUnpinMessage    bool
	HasAdminDelete     bool
	HasGroupUpdate     bool
}

func (m *signalDataMessage) displayBody() string {
	if m == nil {
		return signalUnsupportedMessagePlaceholder
	}
	body := strings.TrimSpace(m.Message)
	if body != "" {
		return body
	}
	return signalUnsupportedContentPlaceholder(m.Attachments, signalUnsupportedContentFlags{
		IsExpirationUpdate: m.IsExpirationUpdate,
		ViewOnce:           m.ViewOnce,
		HasPayment:         signalRawMessagePresent(m.Payment),
		HasPreview:         len(m.Previews) > 0,
		HasSticker:         signalRawMessagePresent(m.Sticker),
		HasRemoteDelete:    signalRawMessagePresent(m.RemoteDelete),
		HasContacts:        len(m.Contacts) > 0,
		HasPollCreate:      signalRawMessagePresent(m.PollCreate),
		HasPollVote:        signalRawMessagePresent(m.PollVote),
		HasPollTerminate:   signalRawMessagePresent(m.PollTerminate),
		HasStoryContext:    signalRawMessagePresent(m.StoryContext),
		HasPinMessage:      signalRawMessagePresent(m.PinMessage),
		HasUnpinMessage:    signalRawMessagePresent(m.UnpinMessage),
		HasAdminDelete:     signalRawMessagePresent(m.AdminDelete),
		HasGroupUpdate:     signalIsGroupUpdate(m.GroupInfo),
	})
}

func (m *signalSentMessage) displayBody() string {
	if m == nil {
		return signalUnsupportedMessagePlaceholder
	}
	body := strings.TrimSpace(m.Message)
	if body != "" {
		return body
	}
	return signalUnsupportedContentPlaceholder(m.Attachments, signalUnsupportedContentFlags{
		IsExpirationUpdate: m.IsExpirationUpdate,
		ViewOnce:           m.ViewOnce,
		HasPayment:         signalRawMessagePresent(m.Payment),
		HasPreview:         len(m.Previews) > 0,
		HasSticker:         signalRawMessagePresent(m.Sticker),
		HasRemoteDelete:    signalRawMessagePresent(m.RemoteDelete),
		HasContacts:        len(m.Contacts) > 0,
		HasPollCreate:      signalRawMessagePresent(m.PollCreate),
		HasPollVote:        signalRawMessagePresent(m.PollVote),
		HasPollTerminate:   signalRawMessagePresent(m.PollTerminate),
		HasStoryContext:    signalRawMessagePresent(m.StoryContext),
		HasPinMessage:      signalRawMessagePresent(m.PinMessage),
		HasUnpinMessage:    signalRawMessagePresent(m.UnpinMessage),
		HasAdminDelete:     signalRawMessagePresent(m.AdminDelete),
		HasGroupUpdate:     signalIsGroupUpdate(m.GroupInfo),
	})
}

func signalUnsupportedContentPlaceholder(attachments []signalAttachment, flags signalUnsupportedContentFlags) string {
	if body := signalAttachmentPlaceholder(attachments); body != "" {
		return body
	}
	switch {
	case flags.HasSticker:
		return "[Sticker]"
	case flags.HasContacts:
		return "[Contact]"
	case flags.HasPayment:
		return "[Payment]"
	case flags.HasPollCreate:
		return "[Poll]"
	case flags.HasPollVote:
		return "[Poll vote]"
	case flags.HasPollTerminate:
		return "[Poll closed]"
	case flags.HasRemoteDelete:
		return "[Deleted message]"
	case flags.HasPinMessage:
		return "[Pinned message]"
	case flags.HasUnpinMessage:
		return "[Unpinned message]"
	case flags.HasAdminDelete:
		return "[Deleted by admin]"
	case flags.HasGroupUpdate:
		return "[Group updated]"
	case flags.HasStoryContext:
		return "[Story reply]"
	case flags.IsExpirationUpdate:
		return "[Disappearing messages updated]"
	case flags.ViewOnce:
		return "[View-once message]"
	case flags.HasPreview:
		return "[Link preview]"
	default:
		return signalUnsupportedMessagePlaceholder
	}
}

func signalRawMessagePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func signalIsGroupUpdate(info *signalGroupInfo) bool {
	return info != nil && strings.EqualFold(strings.TrimSpace(info.Type), "UPDATE")
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
