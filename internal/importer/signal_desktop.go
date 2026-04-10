package importer

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/maxghenis/openmessage/internal/db"
)

var (
	signalDesktopDefaultDir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Signal")
	signalDesktopLookPath   = exec.LookPath
	runSignalDesktopHelper  = func(ctx context.Context, helperPath, nodePath string, args ...string) ([]byte, error) {
		commandArgs := append([]string{helperPath}, args...)
		cmd := exec.CommandContext(ctx, nodePath, commandArgs...)
		return cmd.CombinedOutput()
	}
	runSignalSafeStoragePassword = func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "Signal Safe Storage", "-w")
		return cmd.Output()
	}
	signalDesktopNodePathFn    = signalDesktopNodePath
	signalDesktopAddonPathFn   = signalDesktopAddonPath
	signalDesktopSQLKeyFn      = signalDesktopSQLKey
	writeSignalDesktopHelperFn = writeSignalDesktopHelper
)

//go:embed signal_desktop_export.cjs
var signalDesktopExportFS embed.FS

const (
	signalDesktopHelperTimeout = 30 * time.Second
	signalAttachmentPrefix     = "signallocal:"
)

// SignalDesktop imports historical messages from Signal Desktop's local archive.
type SignalDesktop struct {
	// SupportDir overrides the default Signal Desktop support directory.
	SupportDir string
	// MyName is the local display name for outgoing messages.
	MyName string
	// MyAddress is the account identifier (usually phone number) to use for outgoing messages.
	MyAddress string
	// SinceMS limits the import to messages at or after this Unix millisecond timestamp.
	// When zero, the importer auto-detects the latest imported Signal timestamp and only imports newer rows.
	// Negative means force a full import.
	SinceMS int64
}

type signalDesktopExport struct {
	Conversations []signalDesktopConversationRow `json:"conversations"`
	Messages      []signalDesktopMessageRow      `json:"messages"`
}

type signalDesktopConversationRow struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	Name              string `json:"name"`
	ProfileName       string `json:"profile_name"`
	ProfileFamilyName string `json:"profile_family_name"`
	ProfileFullName   string `json:"profile_full_name"`
	E164              string `json:"e164"`
	ServiceID         string `json:"service_id"`
	GroupID           string `json:"group_id"`
	Members           string `json:"members"`
	ActiveAt          int64  `json:"active_at"`
}

type signalDesktopMessageRow struct {
	ID              string `json:"id"`
	ConversationID  string `json:"conversation_id"`
	Type            string `json:"type"`
	Body            string `json:"body"`
	SentAt          int64  `json:"sent_at"`
	ReceivedAt      int64  `json:"received_at"`
	Source          string `json:"source"`
	SourceServiceID string `json:"source_service_id"`
	QuoteJSON       string `json:"quote_json"`
	ReactionsJSON   string `json:"reactions_json"`
	ContentType     string `json:"content_type"`
	AttachmentPath  string `json:"attachment_path"`
	FileName        string `json:"file_name"`
	Caption         string `json:"caption"`
}

type signalDesktopIdentity struct {
	Address string
	Name    string
}

type signalDesktopQuote struct {
	ID        int64  `json:"id"`
	AuthorAci string `json:"authorAci"`
	Author    string `json:"author"`
	Text      string `json:"text"`
}

type signalDesktopReaction struct {
	Emoji  string `json:"emoji"`
	FromID string `json:"fromId"`
}

type signalStoredReaction struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

func (s *SignalDesktop) ImportFromDB(store *db.Store) (*ImportResult, error) {
	supportDir := strings.TrimSpace(s.SupportDir)
	if supportDir == "" {
		supportDir = signalDesktopDefaultDir
	}
	dbPath := filepath.Join(supportDir, "sql", "db.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("Signal Desktop database not found at %s", dbPath)
	}

	sinceMS := s.SinceMS
	if sinceMS < 0 {
		sinceMS = 0
	} else if sinceMS == 0 {
		if latest, err := store.LatestTimestamp("signal"); err == nil && latest > 0 {
			sinceMS = latest - 5*60*1000
		}
	}

	exported, err := s.exportDesktopArchive(supportDir, dbPath, sinceMS)
	if err != nil {
		return nil, err
	}

	byConversation := make(map[string]signalDesktopConversationRow, len(exported.Conversations))
	byServiceID := map[string]signalDesktopIdentity{}
	byE164 := map[string]signalDesktopIdentity{}
	for _, convo := range exported.Conversations {
		byConversation[convo.ID] = convo
		if !strings.EqualFold(strings.TrimSpace(convo.Type), "private") {
			continue
		}
		identity := signalDesktopIdentity{
			Address: signalDesktopBestAddress(convo),
			Name:    signalDesktopDisplayName(convo),
		}
		if id := strings.TrimSpace(convo.ServiceID); id != "" {
			byServiceID[id] = identity
		}
		if number := strings.TrimSpace(convo.E164); number != "" {
			byE164[number] = identity
		}
	}

	referenced := map[string]struct{}{}
	latestByConversation := map[string]int64{}
	for _, row := range exported.Messages {
		rawConvoID := strings.TrimSpace(row.ConversationID)
		if rawConvoID == "" {
			continue
		}
		if _, ok := byConversation[rawConvoID]; !ok {
			continue
		}
		referenced[rawConvoID] = struct{}{}
		ts := signalDesktopMessageTimestamp(row)
		if ts > latestByConversation[rawConvoID] {
			latestByConversation[rawConvoID] = ts
		}
	}

	result := &ImportResult{}
	for rawConvoID := range referenced {
		rawConvo := byConversation[rawConvoID]
		conversationID, conversation, ok := s.openMessageConversation(rawConvo, latestByConversation[rawConvoID], byServiceID, byE164)
		if !ok {
			continue
		}
		existing, _ := store.GetConversation(conversationID)
		if existing == nil {
			result.ConversationsCreated++
		}
		if err := store.UpsertConversation(conversation); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("conversation %s: %v", rawConvoID, err))
		}
	}

	for _, row := range exported.Messages {
		rawConvo, ok := byConversation[strings.TrimSpace(row.ConversationID)]
		if !ok {
			continue
		}
		msg, err := s.openMessageMessage(supportDir, rawConvo, row, byServiceID, byE164)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", row.ID, err))
			continue
		}
		if msg == nil {
			continue
		}
		existing, _ := store.GetMessageByID(msg.MessageID)
		if existing == nil {
			result.MessagesImported++
		} else {
			result.MessagesDuplicate++
		}
		if err := store.UpsertMessage(msg); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", row.ID, err))
		}
	}

	return result, nil
}

func (s *SignalDesktop) exportDesktopArchive(supportDir, dbPath string, sinceMS int64) (*signalDesktopExport, error) {
	nodePath, err := signalDesktopNodePathFn()
	if err != nil {
		return nil, err
	}
	addonPath, err := signalDesktopAddonPathFn()
	if err != nil {
		return nil, err
	}
	sqlKey, err := signalDesktopSQLKeyFn(filepath.Join(supportDir, "config.json"))
	if err != nil {
		return nil, err
	}
	helperPath, cleanup, err := writeSignalDesktopHelperFn()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), signalDesktopHelperTimeout)
	defer cancel()
	output, err := runSignalDesktopHelper(
		ctx,
		helperPath,
		nodePath,
		dbPath,
		sqlKey,
		addonPath,
		strconv.FormatInt(sinceMS, 10),
	)
	if err != nil {
		return nil, fmt.Errorf("export Signal Desktop archive: %w: %s", err, strings.TrimSpace(string(output)))
	}

	var exported signalDesktopExport
	if err := json.Unmarshal(output, &exported); err != nil {
		return nil, fmt.Errorf("decode Signal Desktop export: %w", err)
	}
	return &exported, nil
}

func (s *SignalDesktop) openMessageConversation(raw signalDesktopConversationRow, latestTS int64, byServiceID, byE164 map[string]signalDesktopIdentity) (string, *db.Conversation, bool) {
	openID := signalDesktopConversationID(raw)
	if openID == "" {
		return "", nil, false
	}
	participantsJSON := "[]"
	if strings.EqualFold(strings.TrimSpace(raw.Type), "group") {
		participants := make([]map[string]string, 0)
		for _, memberID := range strings.Fields(raw.Members) {
			identity := signalDesktopResolveIdentity(memberID, byServiceID, byE164)
			address := strings.TrimSpace(identity.Address)
			if address == "" {
				address = strings.TrimSpace(memberID)
			}
			name := strings.TrimSpace(identity.Name)
			if name == "" {
				name = address
			}
			participants = append(participants, map[string]string{
				"name":   name,
				"number": address,
			})
		}
		if data, err := json.Marshal(participants); err == nil {
			participantsJSON = string(data)
		}
	} else {
		address := signalDesktopBestAddress(raw)
		if address == "" {
			address = strings.TrimSpace(raw.ID)
		}
		name := signalDesktopDisplayName(raw)
		if data, err := json.Marshal([]map[string]string{{
			"name":   name,
			"number": address,
		}}); err == nil {
			participantsJSON = string(data)
		}
	}
	lastTS := latestTS
	if raw.ActiveAt > lastTS {
		lastTS = raw.ActiveAt
	}
	return openID, &db.Conversation{
		ConversationID: openID,
		Name:           signalDesktopDisplayName(raw),
		IsGroup:        strings.EqualFold(strings.TrimSpace(raw.Type), "group"),
		Participants:   participantsJSON,
		LastMessageTS:  lastTS,
		UnreadCount:    0,
		SourcePlatform: "signal",
	}, true
}

func (s *SignalDesktop) openMessageMessage(supportDir string, rawConvo signalDesktopConversationRow, row signalDesktopMessageRow, byServiceID, byE164 map[string]signalDesktopIdentity) (*db.Message, error) {
	conversationID := signalDesktopConversationID(rawConvo)
	if conversationID == "" {
		return nil, nil
	}
	timestamp := signalDesktopMessageTimestamp(row)
	if timestamp <= 0 {
		return nil, nil
	}

	isOutgoing := strings.EqualFold(strings.TrimSpace(row.Type), "outgoing")
	body := strings.TrimSpace(row.Body)
	if body == "" {
		body = strings.TrimSpace(row.Caption)
	}
	if body == "" && strings.TrimSpace(row.ContentType) != "" {
		body = signalDesktopAttachmentPlaceholder(row.ContentType)
	}
	if body == "" {
		body = "[Attachment]"
	}

	myName := strings.TrimSpace(s.MyName)
	if myName == "" {
		myName = "Me"
	}
	myAddress := strings.TrimSpace(s.MyAddress)

	senderName := myName
	senderAddress := myAddress
	if !isOutgoing {
		rawSender := firstNonEmpty(strings.TrimSpace(row.Source), strings.TrimSpace(row.SourceServiceID))
		if rawSender == "" && !strings.EqualFold(strings.TrimSpace(rawConvo.Type), "group") {
			rawSender = signalDesktopBestAddress(rawConvo)
		}
		identity := signalDesktopResolveIdentity(rawSender, byServiceID, byE164)
		senderAddress = firstNonEmpty(identity.Address, rawSender)
		senderName = firstNonEmpty(identity.Name, signalDesktopDisplayName(rawConvo), senderAddress)
	}

	replyToID := signalDesktopReplyToID(conversationID, row.QuoteJSON, byServiceID, byE164, myAddress)
	reactionsJSON, err := signalDesktopStoredReactions(row.ReactionsJSON, byServiceID, byE164)
	if err != nil {
		return nil, err
	}

	messageID, sourceID := signalDesktopMessageIDs(conversationID, senderAddress, timestamp, body, isOutgoing)
	msg := &db.Message{
		MessageID:      messageID,
		ConversationID: conversationID,
		SenderName:     senderName,
		SenderNumber:   senderAddress,
		Body:           body,
		TimestampMS:    timestamp,
		Status:         map[bool]string{true: "sent", false: "received"}[isOutgoing],
		IsFromMe:       isOutgoing,
		Reactions:      reactionsJSON,
		ReplyToID:      replyToID,
		SourcePlatform: "signal",
		SourceID:       sourceID,
	}

	if attachmentPath := strings.TrimSpace(row.AttachmentPath); attachmentPath != "" {
		fullPath := filepath.Join(supportDir, "attachments.noindex", filepath.FromSlash(attachmentPath))
		if _, err := os.Stat(fullPath); err == nil {
			msg.MediaID = encodeSignalDesktopLocalAttachment(fullPath)
			msg.MimeType = strings.TrimSpace(row.ContentType)
		}
	}

	return msg, nil
}

func signalDesktopNodePath() (string, error) {
	if resolved, err := signalDesktopLookPath("node"); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved, nil
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/node",
		"/usr/local/bin/node",
		"/opt/local/bin/node",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("Node.js is required for Signal Desktop history import")
}

func signalDesktopAddonPath() (string, error) {
	arch := "darwin-arm64"
	switch runtime.GOARCH {
	case "amd64":
		arch = "darwin-x64"
	}
	candidates := []string{
		filepath.Join("/Applications/Signal.app", "Contents", "Resources", "app.asar.unpacked", "node_modules", "@signalapp", "sqlcipher", "prebuilds", arch, "@signalapp+sqlcipher.node"),
		filepath.Join(os.Getenv("HOME"), "Applications", "Signal.app", "Contents", "Resources", "app.asar.unpacked", "node_modules", "@signalapp", "sqlcipher", "prebuilds", arch, "@signalapp+sqlcipher.node"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("Signal Desktop SQLCipher addon not found")
}

func signalDesktopSQLKey(configPath string) (string, error) {
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read Signal config.json: %w", err)
	}
	var cfg struct {
		EncryptedKey string `json:"encryptedKey"`
	}
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return "", fmt.Errorf("decode Signal config.json: %w", err)
	}
	encryptedKeyHex := strings.TrimSpace(cfg.EncryptedKey)
	if encryptedKeyHex == "" {
		return "", errors.New("Signal config.json is missing encryptedKey")
	}
	encryptedKey, err := hex.DecodeString(encryptedKeyHex)
	if err != nil {
		return "", fmt.Errorf("decode Signal encrypted key: %w", err)
	}
	if len(encryptedKey) <= 3 || string(encryptedKey[:3]) != "v10" {
		return "", errors.New("unsupported Signal encrypted key format")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	safeStorageRaw, err := runSignalSafeStoragePassword(ctx)
	if err != nil {
		return "", fmt.Errorf("read Signal Safe Storage password: %w", err)
	}
	safeStorage := strings.TrimSpace(string(safeStorageRaw))
	if safeStorage == "" {
		return "", errors.New("Signal Safe Storage password is empty")
	}

	key := pbkdf2.Key([]byte(safeStorage), []byte("saltysalt"), 1003, 16, sha1.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}
	ciphertext := encryptedKey[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("invalid Signal encrypted key payload")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte(" "), aes.BlockSize)).CryptBlocks(plaintext, ciphertext)
	plaintext, err = pkcs7Unpad(plaintext, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(plaintext)), nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("invalid PKCS7 data")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid PKCS7 padding bytes")
		}
	}
	return data[:len(data)-padding], nil
}

func writeSignalDesktopHelper() (string, func(), error) {
	content, err := signalDesktopExportFS.ReadFile("signal_desktop_export.cjs")
	if err != nil {
		return "", nil, fmt.Errorf("read embedded Signal helper: %w", err)
	}
	file, err := os.CreateTemp("", "openmessage-signal-desktop-*.cjs")
	if err != nil {
		return "", nil, fmt.Errorf("create Signal helper temp file: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(file.Name())
		return "", nil, fmt.Errorf("write Signal helper temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", nil, fmt.Errorf("close Signal helper temp file: %w", err)
	}
	return file.Name(), func() {
		_ = os.Remove(file.Name())
	}, nil
}

func signalDesktopConversationID(raw signalDesktopConversationRow) string {
	if strings.EqualFold(strings.TrimSpace(raw.Type), "group") {
		groupID := strings.TrimSpace(raw.GroupID)
		if groupID == "" {
			return ""
		}
		return "signal-group:" + groupID
	}
	address := signalDesktopBestAddress(raw)
	if address == "" {
		return ""
	}
	return "signal:" + address
}

func signalDesktopBestAddress(raw signalDesktopConversationRow) string {
	return firstNonEmpty(strings.TrimSpace(raw.E164), strings.TrimSpace(raw.ServiceID), strings.TrimSpace(raw.ID))
}

func signalDesktopDisplayName(raw signalDesktopConversationRow) string {
	if name := strings.TrimSpace(raw.Name); name != "" {
		return name
	}
	if full := strings.TrimSpace(raw.ProfileFullName); full != "" {
		return full
	}
	full := strings.TrimSpace(strings.Join([]string{
		strings.TrimSpace(raw.ProfileName),
		strings.TrimSpace(raw.ProfileFamilyName),
	}, " "))
	if full != "" {
		return full
	}
	return signalDesktopBestAddress(raw)
}

func signalDesktopResolveIdentity(value string, byServiceID, byE164 map[string]signalDesktopIdentity) signalDesktopIdentity {
	value = strings.TrimSpace(value)
	if value == "" {
		return signalDesktopIdentity{}
	}
	if identity, ok := byE164[value]; ok {
		return identity
	}
	if identity, ok := byServiceID[value]; ok {
		return identity
	}
	return signalDesktopIdentity{Address: value, Name: value}
}

func signalDesktopMessageTimestamp(row signalDesktopMessageRow) int64 {
	if row.SentAt > 0 {
		return row.SentAt
	}
	return row.ReceivedAt
}

func signalDesktopReplyToID(conversationID, quoteJSON string, byServiceID, byE164 map[string]signalDesktopIdentity, myAddress string) string {
	quoteJSON = strings.TrimSpace(quoteJSON)
	if quoteJSON == "" || quoteJSON == "null" {
		return ""
	}
	var quote signalDesktopQuote
	if err := json.Unmarshal([]byte(quoteJSON), &quote); err != nil {
		return ""
	}
	if quote.ID == 0 {
		return ""
	}
	identity := signalDesktopResolveIdentity(firstNonEmpty(quote.AuthorAci, quote.Author), byServiceID, byE164)
	author := firstNonEmpty(identity.Address, strings.TrimSpace(quote.AuthorAci), strings.TrimSpace(quote.Author), myAddress)
	if author == "" {
		author = "unknown"
	}
	sum := sha1.Sum([]byte(strings.Join([]string{
		strings.TrimSpace(conversationID),
		strings.TrimSpace(author),
		strconv.FormatInt(quote.ID, 10),
		strings.TrimSpace(quote.Text),
	}, "\x1f")))
	return "signal:" + hex.EncodeToString(sum[:])
}

func signalDesktopStoredReactions(reactionsJSON string, byServiceID, byE164 map[string]signalDesktopIdentity) (string, error) {
	reactionsJSON = strings.TrimSpace(reactionsJSON)
	if reactionsJSON == "" || reactionsJSON == "null" {
		return "", nil
	}
	var items []signalDesktopReaction
	if err := json.Unmarshal([]byte(reactionsJSON), &items); err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	byEmoji := map[string]*signalStoredReaction{}
	for _, item := range items {
		emoji := strings.TrimSpace(item.Emoji)
		if emoji == "" {
			continue
		}
		entry := byEmoji[emoji]
		if entry == nil {
			entry = &signalStoredReaction{Emoji: emoji}
			byEmoji[emoji] = entry
		}
		entry.Count++
		actor := signalDesktopResolveIdentity(item.FromID, byServiceID, byE164).Address
		if actor == "" {
			actor = strings.TrimSpace(item.FromID)
		}
		if actor != "" {
			entry.Actors = appendUnique(entry.Actors, actor)
		}
	}
	if len(byEmoji) == 0 {
		return "", nil
	}
	out := make([]signalStoredReaction, 0, len(byEmoji))
	for _, entry := range byEmoji {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Emoji < out[j].Emoji
	})
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func signalDesktopMessageIDs(conversationID, sender string, timestamp int64, body string, isOutgoing bool) (string, string) {
	body = strings.TrimSpace(body)
	if isOutgoing {
		sum := sha1.Sum([]byte(strings.Join([]string{
			strings.TrimSpace(conversationID),
			"me",
			strconv.FormatInt(timestamp, 10),
			body,
		}, "\x1f")))
		sourceID := "local:" + hex.EncodeToString(sum[:])
		return "signal:" + sourceID, sourceID
	}
	sum := sha1.Sum([]byte(strings.Join([]string{
		strings.TrimSpace(conversationID),
		strings.TrimSpace(sender),
		strconv.FormatInt(timestamp, 10),
		body,
	}, "\x1f")))
	sourceID := hex.EncodeToString(sum[:])
	return "signal:" + sourceID, sourceID
}

func signalDesktopAttachmentPlaceholder(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "[Photo]"
	case strings.HasPrefix(mimeType, "video/"):
		return "[Video]"
	case strings.HasPrefix(mimeType, "audio/"):
		return "[Audio]"
	default:
		return "[Attachment]"
	}
}

func encodeSignalDesktopLocalAttachment(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return signalAttachmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(path))
}

func appendUnique(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, existing := range items {
		if strings.EqualFold(strings.TrimSpace(existing), value) {
			return items
		}
	}
	return append(items, value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
