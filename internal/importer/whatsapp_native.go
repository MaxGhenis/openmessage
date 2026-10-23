package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"

	_ "modernc.org/sqlite"
)

// Default path to WhatsApp Desktop's Core Data SQLite database on macOS.
var whatsappDefaultDBPath = filepath.Join(
	os.Getenv("HOME"),
	"Library", "Group Containers",
	"group.net.whatsapp.WhatsApp.shared",
	"ChatStorage.sqlite",
)

// WhatsAppNative imports messages by reading the macOS WhatsApp Desktop database directly.
// This is more robust than the text-export importer because:
//   - No separate bridge process to maintain
//   - WhatsApp Desktop handles its own connection
//   - Always has the latest messages (if the desktop app is synced)
//   - Incremental: only imports messages newer than the last sync
type WhatsAppNative struct {
	// DBPath overrides the default ChatStorage.sqlite location.
	DBPath string
	// MyName is the display name for outgoing messages (default "Me").
	MyName string
	// SinceMS limits import to messages after this Unix millisecond timestamp.
	// When zero, imports everything.
	SinceMS int64

	// lids pairs LIDs with phone numbers from Desktop's own records, so a LID
	// sender is stored by its number when Desktop knows it (address).
	lids *whatsapplid.Evidence
}

type MediaRepairResult struct {
	MessagesRepaired int
	MessagesSkipped  int
}

// ImportFromDB reads the WhatsApp Desktop database and imports all messages.
// When SinceMS is 0, it auto-detects the latest imported WhatsApp timestamp
// from the store and only imports newer messages (incremental sync).
func (w *WhatsAppNative) ImportFromDB(store *db.Store) (*ImportResult, error) {
	dbPath := w.DBPath
	if dbPath == "" {
		dbPath = whatsappDefaultDBPath
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("WhatsApp Desktop database not found at %s — is WhatsApp Desktop installed?", dbPath)
	}

	// Auto-incremental: if no explicit SinceMS, start from last imported message.
	// Negative SinceMS means "force full import" (reset to 0).
	if w.SinceMS < 0 {
		w.SinceMS = 0
	} else if w.SinceMS == 0 {
		if latest, err := store.LatestTimestamp("whatsapp"); err == nil && latest > 0 {
			// Overlap by 5 minutes to catch any messages that might have been missed
			w.SinceMS = latest - 5*60*1000
		}
	}

	waDB, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open WhatsApp db: %w", err)
	}
	defer waDB.Close()
	waDB.SetMaxOpenConns(1)

	result := &ImportResult{}

	// Desktop pairs many LIDs with numbers (chat and member contact
	// identifiers, LID.sqlite). The pairs only refine how a LID is stored, so
	// an unreadable source leaves those LIDs stored as LIDs.
	w.lids = whatsapplid.NewEvidence()
	var skipped []string
	_, _ = whatsapplid.ReadDesktopDB(context.Background(), waDB, w.lids, false, &skipped)
	whatsapplid.ReadDesktopLIDPairs(context.Background(), whatsapplid.DesktopLIDPath(dbPath), w.lids)

	chats, err := w.loadChats(waDB)
	if err != nil {
		return nil, fmt.Errorf("load chats: %w", err)
	}

	for _, chat := range chats {
		convID := "whatsapp:" + chat.jid

		participants, _ := json.Marshal(chat.participants)
		if err := store.UpsertConversation(&db.Conversation{
			ConversationID: convID,
			Name:           chat.name,
			IsGroup:        chat.isGroup,
			Participants:   string(participants),
			LastMessageTS:  chat.lastMessageTS,
			SourcePlatform: "whatsapp",
		}); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("conversation %s: %v", chat.jid, err))
			continue
		}
		result.ConversationsCreated++

		msgs, err := w.loadMessages(waDB, chat.pk, chat.name, chat.isGroup)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("messages for %s: %v", chat.jid, err))
			continue
		}

		for _, m := range msgs {
			msg := &db.Message{
				MessageID:      "whatsapp:" + m.stanzaID,
				ConversationID: convID,
				SenderName:     m.senderName,
				SenderNumber:   m.senderNumber,
				Body:           m.text,
				TimestampMS:    m.timestampMS,
				Status:         "delivered",
				IsFromMe:       m.isFromMe,
				SourcePlatform: "whatsapp",
				SourceID:       m.stanzaID,
			}

			if err := store.UpsertMessage(msg); err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint") {
					result.MessagesDuplicate++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("message %s: %v", m.stanzaID, err))
				}
				continue
			}
			result.MessagesImported++
		}
	}

	return result, nil
}

func (w *WhatsAppNative) RepairLegacyMediaPlaceholders(store *db.Store) (*MediaRepairResult, error) {
	dbPath := w.DBPath
	if dbPath == "" {
		dbPath = whatsappDefaultDBPath
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return &MediaRepairResult{}, nil
	}

	placeholders, err := store.ListLegacyWhatsAppMediaPlaceholders(1000)
	if err != nil {
		return nil, fmt.Errorf("list legacy whatsapp media placeholders: %w", err)
	}
	result := &MediaRepairResult{}
	if len(placeholders) == 0 {
		return result, nil
	}

	waDB, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open WhatsApp db: %w", err)
	}
	defer waDB.Close()
	waDB.SetMaxOpenConns(1)

	root := filepath.Join(filepath.Dir(dbPath), "Message")
	for _, msg := range placeholders {
		sourceID := strings.TrimSpace(msg.SourceID)
		if sourceID == "" {
			result.MessagesSkipped++
			continue
		}
		mediaPath, mimeType, err := w.lookupLegacyMedia(waDB, sourceID, msg.Body)
		if err != nil || mediaPath == "" {
			result.MessagesSkipped++
			continue
		}
		fullPath := filepath.Join(root, filepath.FromSlash(mediaPath))
		if _, err := os.Stat(fullPath); err != nil {
			result.MessagesSkipped++
			continue
		}
		msg.MediaID = whatsappmedia.EncodeLocalMediaRef(mediaPath)
		if msg.MediaID == "" {
			result.MessagesSkipped++
			continue
		}
		if mimeType == "" {
			mimeType = inferWhatsAppMediaMIME(mediaPath, msg.Body)
		}
		msg.MimeType = mimeType
		if err := store.UpsertMessage(msg); err != nil {
			result.MessagesSkipped++
			continue
		}
		result.MessagesRepaired++
	}

	return result, nil
}

type waChat struct {
	pk            int
	jid           string
	name          string
	isGroup       bool
	participants  []map[string]string
	lastMessageTS int64
}

type waNativeMessage struct {
	stanzaID     string
	text         string
	senderName   string
	senderNumber string
	timestampMS  int64
	isFromMe     bool
}

func (w *WhatsAppNative) lookupLegacyMedia(waDB *sql.DB, stanzaID, body string) (string, string, error) {
	row := waDB.QueryRow(`
		SELECT COALESCE(mi.ZMEDIALOCALPATH, '')
		FROM ZWAMESSAGE m
		LEFT JOIN ZWAMEDIAITEM mi ON mi.Z_PK = m.ZMEDIAITEM
		WHERE m.ZSTANZAID = ?
		LIMIT 1
	`, stanzaID)
	var mediaPath string
	if err := row.Scan(&mediaPath); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(mediaPath), inferWhatsAppMediaMIME(mediaPath, body), nil
}

func inferWhatsAppMediaMIME(path, body string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	if ext != "" {
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			return mimeType
		}
	}
	switch strings.TrimSpace(body) {
	case "[Photo]":
		return "image/jpeg"
	case "[Video]":
		return "video/mp4"
	case "[Audio]", "[Voice note]":
		if ext == ".opus" {
			return "audio/ogg"
		}
		return "audio/mpeg"
	default:
		return ""
	}
}

func (w *WhatsAppNative) loadChats(waDB *sql.DB) ([]waChat, error) {
	rows, err := waDB.Query(`
		SELECT cs.Z_PK, cs.ZCONTACTJID, COALESCE(cs.ZPARTNERNAME, ''),
			cs.ZLASTMESSAGEDATE
		FROM ZWACHATSESSION cs
		WHERE cs.ZCONTACTJID IS NOT NULL
			AND cs.ZREMOVED = 0
			AND cs.ZCONTACTJID NOT LIKE '0@%'
			AND cs.ZCONTACTJID NOT LIKE '%@status'
			AND cs.ZCONTACTJID NOT LIKE '%@broadcast'
		ORDER BY cs.ZLASTMESSAGEDATE DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect all rows first (can't nest queries with MaxOpenConns=1)
	var chats []waChat
	for rows.Next() {
		var c waChat
		var lastDate sql.NullFloat64
		if err := rows.Scan(&c.pk, &c.jid, &c.name, &lastDate); err != nil {
			continue
		}

		c.isGroup = strings.Contains(c.jid, "@g.us")
		if lastDate.Valid {
			c.lastMessageTS = coreDataSecsToMS(lastDate.Float64)
		}

		// Skip chats with no name and no messages
		if c.name == "" && c.lastMessageTS == 0 {
			continue
		}

		chats = append(chats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Second pass: load participants (now safe to query)
	for i := range chats {
		c := &chats[i]
		if c.isGroup {
			c.participants = w.loadGroupMembers(waDB, c.pk)
			// If the group has no real name or the stored name is just the raw JID
			// (or its local part, e.g. "16154856400-1585405251"), derive a readable
			// name from members.  We compare against the actual JID so that real
			// group subjects that happen to contain digits and a hyphen (e.g.
			// "2023-2024 Reunion") are never overwritten.
			if c.name == "" || isRawGroupJIDName(c.name, c.jid) {
				if derived := deriveGroupName(c.participants); derived != "" {
					c.name = derived
				} else {
					// Last resort: use the JID itself (covers both empty name and
					// unresolvable raw-JID cases when no participants were found).
					c.name = c.jid
				}
			}
		} else {
			c.participants = []map[string]string{
				{"name": c.name, "number": w.address(c.jid)},
			}
			if c.name == "" {
				c.name = c.jid
			}
		}
	}

	return chats, nil
}

func (w *WhatsAppNative) loadGroupMembers(waDB *sql.DB, chatPK int) []map[string]string {
	rows, err := waDB.Query(`
		SELECT COALESCE(gm.ZMEMBERJID, ''), COALESCE(gm.ZCONTACTNAME, '')
		FROM ZWAGROUPMEMBER gm
		WHERE gm.ZCHATSESSION = ?
	`, chatPK)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var participants []map[string]string
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			continue
		}
		number := w.address(jid)
		if name == "" && strings.HasPrefix(number, "+") {
			// A number reads as a name only when it is one; a LID's JID
			// would pass deriveGroupName's phone-number check as a name.
			name = number
		}
		participants = append(participants, map[string]string{
			"name":   name,
			"number": number,
		})
	}
	return participants
}

func (w *WhatsAppNative) loadMessages(waDB *sql.DB, chatPK int, chatName string, isGroup bool) ([]waNativeMessage, error) {
	// Build query with optional time filter
	query := `
		SELECT m.ZSTANZAID, COALESCE(m.ZTEXT, ''), m.ZMESSAGEDATE,
			m.ZISFROMME, COALESCE(m.ZFROMJID, ''), COALESCE(m.ZPUSHNAME, '')
		FROM ZWAMESSAGE m
		WHERE m.ZCHATSESSION = ?
			AND m.ZSTANZAID IS NOT NULL
			AND m.ZSTANZAID != ''
	`
	args := []any{chatPK}

	if w.SinceMS > 0 {
		// Convert Unix ms to Core Data seconds
		coreDataSecs := float64(w.SinceMS)/1000.0 - float64(coreDataEpoch)
		query += " AND m.ZMESSAGEDATE > ?"
		args = append(args, coreDataSecs)
	}

	query += " ORDER BY m.ZMESSAGEDATE ASC"

	rows, err := waDB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	myName := w.MyName
	if myName == "" {
		myName = "Me"
	}

	var msgs []waNativeMessage
	for rows.Next() {
		var m waNativeMessage
		var date float64
		var fromJID, pushName string
		if err := rows.Scan(&m.stanzaID, &m.text, &date, &m.isFromMe, &fromJID, &pushName); err != nil {
			continue
		}

		// Skip messages with no text content (media-only, system, etc.)
		if m.text == "" {
			continue
		}

		m.timestampMS = coreDataSecsToMS(date)

		if m.isFromMe {
			m.senderName = myName
		} else if !isGroup {
			// 1:1 chat: sender is the chat partner
			m.senderName = chatName
			m.senderNumber = w.address(fromJID)
		} else {
			// Group chat: use fromJID as identifier
			// Push names are encrypted in modern WhatsApp, so use JID-based phone
			m.senderNumber = w.address(fromJID)
			m.senderName = m.senderNumber
			if m.senderName == "" {
				m.senderName = fromJID
			}
		}

		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// coreDataSecsToMS converts a Core Data timestamp (seconds since 2001-01-01) to Unix milliseconds.
func coreDataSecsToMS(secs float64) int64 {
	return int64((secs + float64(coreDataEpoch)) * 1000)
}

// address returns the form a sender or chat peer is stored by: the phone
// number of a phone-number JID ("18312972255@s.whatsapp.net" →
// "+18312972255"), the number Desktop pairs a LID with, or the LID itself
// ("12345@lid"), as the live bridge and v2 ingest store a LID whose number is
// unknown. Only a phone-number JID names a phone number: a LID's digits stored
// as "+<digits>" pass for one (whatsapplid). Other JIDs (groups, broadcast
// lists) give "".
func (w *WhatsAppNative) address(jid string) string {
	return w.lids.Address(jid)
}

// isRawGroupJIDName reports whether name appears to be a raw WhatsApp group JID
// rather than a real group subject.  It matches when name is exactly the full
// JID (e.g. "16154856400-1585405251@g.us") or just its local part before the
// "@" (e.g. "16154856400-1585405251").  Using a direct string comparison
// against the known JID avoids false-positives on real group subjects that
// happen to contain digits and hyphens (e.g. "2023-2024 Reunion").
func isRawGroupJIDName(name, jid string) bool {
	if name == jid {
		return true
	}
	if idx := strings.IndexByte(jid, '@'); idx >= 0 && name == jid[:idx] {
		return true
	}
	return false
}

// deriveGroupName builds a human-readable conversation name from the group's
// participant list. It prefers display names over raw phone numbers, falls
// back to phone numbers when no display names are available, deduplicates,
// sorts alphabetically for a stable result, and caps very large groups at
// 5 names with a "+N more" suffix.
func deriveGroupName(participants []map[string]string) string {
	const maxNames = 5

	// First pass: collect participants whose name looks like a real name
	// (not a phone number), deduplicating as we go.
	seen := map[string]bool{}
	var displayNames []string
	for _, p := range participants {
		name := strings.TrimSpace(p["name"])
		if name != "" && !isPhoneNumber(name) && !seen[name] {
			seen[name] = true
			displayNames = append(displayNames, name)
		}
	}
	if len(displayNames) > 0 {
		sort.Strings(displayNames)
		return joinGroupNames(displayNames, maxNames)
	}

	// Fall back to phone numbers / whatever names are available.
	seen = map[string]bool{}
	var names []string
	for _, p := range participants {
		name := strings.TrimSpace(p["name"])
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return joinGroupNames(names, maxNames)
}

// joinGroupNames joins up to max names from the slice; if there are more, it
// appends a "+N more" suffix so the result stays concise.
func joinGroupNames(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", +%d more", len(names)-max)
}

// isPhoneNumber reports whether s looks like an E.164-style phone number:
// an optional leading '+' followed by 7–15 digits (the ITU-T E.164 range).
// Strings shorter than 7 digits (e.g. single digits or short numeric IDs)
// are not considered phone numbers to avoid misclassifying short numeric
// display names.
func isPhoneNumber(s string) bool {
	if s == "" {
		return false
	}
	check := s
	if strings.HasPrefix(check, "+") {
		check = check[1:]
	}
	// E.164 allows 7–15 digits after the country code.
	if len(check) < 7 || len(check) > 15 {
		return false
	}
	for _, c := range check {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
