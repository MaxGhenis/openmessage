// Package whatsappdesktop captures WhatsApp Desktop history without touching
// either the live transport or the OpenMessage projection store.
package whatsappdesktop

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/maxghenis/openmessage/internal/whatsappmedia"
	watypes "go.mau.fi/whatsmeow/types"
	_ "modernc.org/sqlite"
)

const coreDataEpoch = 978307200

type Options struct {
	DBPath        string
	MediaRoot     string
	LIDDBPath     string
	SessionDBPath string
	// SelfJID is the existing WhatsApp account's canonical self identity from
	// v2. An empty value is safe: IsFromMe still gives the outgoing direction
	// and the worker leaves its sender null rather than inventing an identity.
	SelfJID string
}

type Reader struct{ options Options }

func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

func New(options Options) *Reader {
	if options.DBPath == "" {
		options.DBPath = DefaultDBPath()
	}
	if options.MediaRoot == "" {
		options.MediaRoot = filepath.Join(filepath.Dir(options.DBPath), "Message")
	}
	if options.LIDDBPath == "" {
		options.LIDDBPath = filepath.Join(filepath.Dir(options.DBPath), "LID.sqlite")
	}
	return &Reader{options: options}
}

type Participant struct {
	JID  string
	Name string
}

type Chat struct {
	PK           int64
	JID          string
	Title        string
	IsGroup      bool
	Participants []Participant
}

type Message struct {
	PK            int64
	StanzaID      string
	TimestampMS   int64
	IsFromMe      bool
	SenderJID     string
	Type          int
	Text          string
	Caption       string
	FileName      string
	MIMEType      string
	LocalMediaRef string
	VoiceNote     bool
	Size          int64
}

type Chunk struct {
	Chat     Chat
	Messages []Message
}

type ScanOptions struct {
	// AfterPK is the successfully appended scan's high-water mark. Zero scans
	// the whole snapshot; skips also advance the returned high-water mark.
	AfterPK int64
	// SinceMS counts scanned rows since this timestamp for count-only dry runs;
	// it does not filter history or alter the emitted snapshot.
	SinceMS   int64
	ChunkSize int
}

type Stats struct {
	HighWaterPK              int64            `json:"high_water_pk"`
	ChatsResolved            int64            `json:"chats_resolved"`
	ChatsSkippedByReason     map[string]int64 `json:"chats_skipped_by_reason"`
	RowsIncludedByType       map[int]int64    `json:"rows_included_by_type"`
	RowsSkippedByType        map[int]int64    `json:"rows_skipped_by_type"`
	RowsSkippedByReason      map[string]int64 `json:"rows_skipped_by_reason"`
	RowsSinceMS              int64            `json:"rows_since_ms"`
	RowsSinceIncludedByType  map[int]int64    `json:"rows_since_included_by_type"`
	RowsSinceSkippedByReason map[string]int64 `json:"rows_since_skipped_by_reason"`
	UnresolvedLIDChats       int64            `json:"unresolved_lid_chats"`
	MapErrorsBySource        map[string]int64 `json:"map_errors_by_source,omitempty"`
}

func newStats(afterPK int64) Stats {
	return Stats{HighWaterPK: afterPK, ChatsSkippedByReason: map[string]int64{},
		RowsIncludedByType: map[int]int64{}, RowsSkippedByType: map[int]int64{},
		RowsSkippedByReason: map[string]int64{}, MapErrorsBySource: map[string]int64{},
		RowsSinceIncludedByType: map[int]int64{}, RowsSinceSkippedByReason: map[string]int64{}}
}

// Scan uses a single read transaction, so a capture sees a consistent WAL
// snapshot while Desktop continues writing. mode=ro is mandatory; immutable=1
// would hide recent WAL rows. No schema migration or write pragma runs here.
// Payload order is deterministic (chat PK, message PK, participant JID) and a
// chunk holds at most 100 messages, keeping both memory and ingress bounded.
func (r *Reader) Scan(ctx context.Context, options ScanOptions, emit func(Chunk) error) (Stats, error) {
	stats := newStats(options.AfterPK)
	db, err := openReadOnly(ctx, r.options.DBPath)
	if err != nil {
		return stats, fmt.Errorf("open WhatsApp Desktop database read-only: %w", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stats, fmt.Errorf("begin WhatsApp Desktop snapshot: %w", err)
	}
	defer tx.Rollback()

	chats, members, mappings, err := loadRoster(ctx, tx)
	if err != nil {
		return stats, err
	}
	r.loadExternalMappings(ctx, mappings, &stats)
	resolved := make(map[int64]Chat, len(chats))
	skipped := make(map[int64]string)
	for _, raw := range chats {
		jid, reason := canonicalChat(raw.jid, raw.identifier, mappings)
		if raw.removed {
			reason = "removed"
		}
		if reason != "" {
			stats.ChatsSkippedByReason[reason]++
			if reason == "unresolved_lid" {
				stats.UnresolvedLIDChats++
			}
			skipped[raw.pk] = reason
			continue
		}
		chat := Chat{PK: raw.pk, JID: jid, Title: raw.title, IsGroup: strings.HasSuffix(jid, "@g.us")}
		if chat.IsGroup {
			seen := map[string]bool{}
			for _, member := range members {
				if member.chatPK != chat.PK {
					continue
				}
				memberJID := canonicalSender(member.jid, member.identifier, mappings)
				if memberJID == "" || memberJID == r.options.SelfJID || seen[memberJID] {
					continue
				}
				seen[memberJID] = true
				// Desktop contact labels can disagree between chat rosters. Keep
				// identity labels empty so insertion order never chooses a name;
				// conversation titles still preserve the Desktop title.
				chat.Participants = append(chat.Participants, Participant{JID: memberJID})
			}
			sort.Slice(chat.Participants, func(i, j int) bool { return chat.Participants[i].JID < chat.Participants[j].JID })
		} else {
			// A self-chat still carries its peer. The feeder marks this JID
			// IsSelf, so a decoder's missing-roster fallback cannot invent a
			// second non-self identity for the account's own phone number.
			chat.Participants = []Participant{{JID: chat.JID}}
		}
		stats.ChatsResolved++
		resolved[chat.PK] = chat
	}
	memberByPK := make(map[int64]rawMember, len(members))
	for _, member := range members {
		memberByPK[member.pk] = member
	}
	mergeSnapshots(resolved)
	skipAmbiguousRosters(resolved, skipped, r.options.SelfJID, &stats)
	query, err := messageQuery(ctx, tx)
	if err != nil {
		return stats, err
	}
	rows, err := tx.QueryContext(ctx, query, options.AfterPK)
	if err != nil {
		return stats, fmt.Errorf("read WhatsApp Desktop messages: %w", err)
	}
	defer rows.Close()
	limit := options.ChunkSize
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var chunk Chunk
	flush := func() error {
		if len(chunk.Messages) == 0 {
			return nil
		}
		if emit != nil {
			if err := emit(chunk); err != nil {
				return fmt.Errorf("append WhatsApp Desktop capture: %w", err)
			}
		}
		chunk = Chunk{}
		return nil
	}
	for rows.Next() {
		var message Message
		var chatPK, memberPK int64
		var date sql.NullFloat64
		var fromMe int
		var mediaOrigin int
		var fromJID, mediaPath string
		if err := rows.Scan(&message.PK, &chatPK, &message.StanzaID, &message.Type,
			&message.Text, &date, &fromMe, &memberPK, &fromJID, &message.Caption, &mediaPath,
			&message.FileName, &message.MIMEType, &mediaOrigin); err != nil {
			return stats, fmt.Errorf("scan WhatsApp Desktop message: %w", err)
		}
		if message.PK > stats.HighWaterPK {
			stats.HighWaterPK = message.PK
		}
		atMS, validDate := timestampMS(date)
		message.TimestampMS = atMS
		if validDate {
			if options.SinceMS > 0 && message.TimestampMS >= options.SinceMS {
				stats.RowsSinceMS++
			}
		}
		chat, ok := resolved[chatPK]
		reason := ""
		switch {
		case !ok:
			reason = "chat_" + skipped[chatPK]
			if skipped[chatPK] == "ambiguous_roster" {
				reason = "ambiguous_roster"
			}
			if reason == "chat_" {
				reason = "chat_missing"
			}
		default:
			reason = rowEligibility(message.Type, message.StanzaID, validDate, mediaOrigin)
		}
		if reason != "" {
			stats.RowsSkippedByReason[reason]++
			stats.RowsSkippedByType[message.Type]++
			if options.SinceMS > 0 && validDate && message.TimestampMS >= options.SinceMS {
				stats.RowsSinceSkippedByReason[reason]++
			}
			continue
		}
		message.IsFromMe = fromMe == 1
		// Capture-time inference from Desktop's audio rows: origin=1 has
		// audio/ogg plus waveform metadata (and .opus locally); origin=0 has
		// audio/mp4, no waveform and a .m4a file. This distinguishes live PTT
		// placeholders without interpreting unknown enum values as voice notes.
		message.VoiceNote = message.Type == 3 && mediaOrigin == 1
		message.SenderJID, reason = captureSender(chat, message.IsFromMe, memberPK, fromJID, memberByPK, mappings, r.options.SelfJID)
		if reason != "" {
			stats.RowsSkippedByReason[reason]++
			stats.RowsSkippedByType[message.Type]++
			if options.SinceMS > 0 && message.TimestampMS >= options.SinceMS {
				stats.RowsSinceSkippedByReason[reason]++
			}
			continue
		}
		if message.Type != 0 && message.Type != 7 {
			r.captureMedia(&message, mediaPath)
		}
		if len(chunk.Messages) > 0 && chunk.Chat.PK != chat.PK {
			if err := flush(); err != nil {
				return stats, err
			}
		}
		if len(chunk.Messages) == 0 {
			chunk.Chat = chat
			chunk.Messages = make([]Message, 0, limit)
		}
		chunk.Messages = append(chunk.Messages, message)
		stats.RowsIncludedByType[message.Type]++
		if options.SinceMS > 0 && message.TimestampMS >= options.SinceMS {
			stats.RowsSinceIncludedByType[message.Type]++
		}
		if len(chunk.Messages) == limit {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("read WhatsApp Desktop message snapshot: %w", err)
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, tx.Commit()
}

// Only types whose content can be rendered like live WhatsApp are captured.
// Unknown, revoked, system, call, poll and event rows never become bodies.
func includedType(kind int) bool {
	switch kind {
	case 0, 7, 1, 2, 3, 8, 11, 15:
		return true
	default:
		return false
	}
}

func timestampMS(date sql.NullFloat64) (int64, bool) {
	if !date.Valid || math.IsNaN(date.Float64) || math.IsInf(date.Float64, 0) {
		return 0, false
	}
	seconds := math.Round(date.Float64 + coreDataEpoch)
	// Check before converting: a finite malformed Core Data date can still
	// overflow int64 milliseconds and poison every frame in the chat. Strict
	// upper comparison also handles float64 rounding at MaxInt64/1000.
	if seconds <= 0 || seconds >= float64(math.MaxInt64/1000) {
		return 0, false
	}
	return int64(seconds) * 1000, true
}

func rowEligibility(kind int, stanzaID string, validDate bool, mediaOrigin int) string {
	switch {
	case !includedType(kind):
		return "unsupported_type"
	case strings.TrimSpace(stanzaID) == "":
		return "empty_stanza_id"
	case !validDate:
		return "invalid_timestamp"
	case kind == 3 && mediaOrigin != 0 && mediaOrigin != 1:
		return "audio_origin_unknown"
	default:
		return ""
	}
}

func captureSender(chat Chat, fromMe bool, memberPK int64, fromJID string, members map[int64]rawMember, mappings map[string]string, selfJID string) (string, string) {
	if fromMe {
		return selfJID, ""
	}
	if !chat.IsGroup {
		return chat.JID, ""
	}
	member, exists := members[memberPK]
	sender := ""
	if exists && member.chatPK == chat.PK {
		sender = canonicalSender(member.jid, member.identifier, mappings)
	} else {
		sender = canonicalSender(fromJID, "", mappings)
	}
	if sender == "" {
		return "", "missing_group_sender"
	}
	return sender, ""
}

// mergeSnapshots gives every Desktop session that canonicalizes to the same
// chat JID one merged title and roster, so each chunk of that chat carries an
// identical snapshot and a newly minted thread does not depend on chunk order.
func mergeSnapshots(chats map[int64]Chat) {
	canonical := map[string]Chat{}
	pks := make([]int64, 0, len(chats))
	for pk := range chats {
		pks = append(pks, pk)
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i] < pks[j] })
	for _, pk := range pks {
		chat := chats[pk]
		merged, found := canonical[chat.JID]
		if !found {
			canonical[chat.JID] = chat
			continue
		}
		if chat.Title != "" && (merged.Title == "" || chat.Title < merged.Title) {
			merged.Title = chat.Title
		}
		seen := map[string]bool{}
		for _, participant := range merged.Participants {
			seen[participant.JID] = true
		}
		for _, participant := range chat.Participants {
			if !seen[participant.JID] {
				merged.Participants = append(merged.Participants, participant)
				seen[participant.JID] = true
			}
		}
		sort.Slice(merged.Participants, func(i, j int) bool { return merged.Participants[i].JID < merged.Participants[j].JID })
		canonical[chat.JID] = merged
	}
	for pk, chat := range chats {
		merged := canonical[chat.JID]
		merged.PK = pk
		chats[pk] = merged
	}
}

// I5: the worker deliberately refuses an unbound history remote ID whose
// roster already belongs to another thread. If Desktop itself contains two
// distinct remote IDs with the same roster, first-writer placement would make
// frame order choose the surviving thread. Skip every such chat at capture,
// preserving that guard and making the snapshot independent of frame order.
// Duplicate Desktop sessions for the same canonical remote ID are harmless.
func skipAmbiguousRosters(chats map[int64]Chat, skipped map[int64]string, selfJID string, stats *Stats) {
	rosters := map[string]map[string]bool{}
	keys := map[int64]string{}
	for pk, chat := range chats {
		kind := "direct"
		if chat.IsGroup {
			kind = "group"
		}
		peers := []string{}
		for _, participant := range chat.Participants {
			if participant.JID != selfJID {
				peers = append(peers, participant.JID)
			}
		}
		sort.Strings(peers)
		// The worker only compares an unbound snapshot's roster when at
		// least one peer exists. An empty roster is no evidence of equality.
		if len(peers) == 0 {
			continue
		}
		key := kind + "\x00" + strings.Join(peers, "\x00")
		keys[pk] = key
		if rosters[key] == nil {
			rosters[key] = map[string]bool{}
		}
		rosters[key][chat.JID] = true
	}
	for pk := range chats {
		if len(rosters[keys[pk]]) > 1 {
			delete(chats, pk)
			skipped[pk] = "ambiguous_roster"
			stats.ChatsResolved--
			stats.ChatsSkippedByReason["ambiguous_roster"]++
		}
	}
}

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type rawChat struct {
	pk                     int64
	jid, identifier, title string
	removed                bool
}

type rawMember struct {
	pk, chatPK            int64
	jid, identifier, name string
}

func loadRoster(ctx context.Context, tx *sql.Tx) ([]rawChat, []rawMember, map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT Z_PK, COALESCE(ZCONTACTJID,''), COALESCE(ZCONTACTIDENTIFIER,''), COALESCE(ZPARTNERNAME,''), COALESCE(ZREMOVED,0) FROM ZWACHATSESSION ORDER BY Z_PK`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read WhatsApp Desktop chats: %w", err)
	}
	var chats []rawChat
	mappings := map[string]string{}
	for rows.Next() {
		var chat rawChat
		if err := rows.Scan(&chat.pk, &chat.jid, &chat.identifier, &chat.title, &chat.removed); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		chats = append(chats, chat)
		addMapping(mappings, chat.jid, chat.identifier)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT Z_PK, COALESCE(ZCHATSESSION,0), COALESCE(ZMEMBERJID,''), COALESCE(ZCONTACTIDENTIFIER,''), COALESCE(ZCONTACTNAME,'') FROM ZWAGROUPMEMBER ORDER BY Z_PK`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read WhatsApp Desktop group members: %w", err)
	}
	defer rows.Close()
	var members []rawMember
	for rows.Next() {
		var member rawMember
		if err := rows.Scan(&member.pk, &member.chatPK, &member.jid, &member.identifier, &member.name); err != nil {
			return nil, nil, nil, err
		}
		members = append(members, member)
		addMapping(mappings, member.jid, member.identifier)
	}
	return chats, members, mappings, rows.Err()
}

func addMapping(mappings map[string]string, lid, pn string) {
	lid = normalizedJID(lid)
	pn = normalizedPhoneJID(pn)
	if strings.HasSuffix(lid, "@lid") && pn != "" && mappings[lid] == "" {
		mappings[lid] = pn
	}
}

func (r *Reader) loadExternalMappings(ctx context.Context, mappings map[string]string, stats *Stats) {
	for _, source := range []struct{ name, path, query string }{
		{"desktop_lid", r.options.LIDDBPath, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`},
		{"session_lid", r.options.SessionDBPath, `SELECT lid, pn FROM whatsmeow_lid_map`},
	} {
		if source.path == "" {
			continue
		}
		if _, err := os.Stat(source.path); os.IsNotExist(err) {
			continue
		}
		if err := readMappingSource(ctx, source.path, source.query, mappings); err != nil {
			// Maps improve canonicalization but are optional. A bad map never
			// turns an unresolved LID into a new conversation.
			stats.MapErrorsBySource[source.name]++
		}
	}
}

func readMappingSource(ctx context.Context, path, query string, mappings map[string]string) error {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lid, pn sql.NullString
		if err := rows.Scan(&lid, &pn); err != nil {
			return err
		}
		lidValue := strings.TrimSpace(lid.String)
		if !strings.Contains(lidValue, "@") {
			lidValue += "@lid"
		}
		addMapping(mappings, lidValue, pn.String)
	}
	return rows.Err()
}

func canonicalChat(jid, identifier string, mappings map[string]string) (string, string) {
	jid = normalizedJID(jid)
	parsed, _ := watypes.ParseJID(jid)
	switch {
	case jid == "" || parsed.User == "":
		return "", "invalid_jid"
	case parsed.User == "0":
		return "", "system_chat"
	case parsed.IsBot():
		return "", "bot"
	case parsed.Server == watypes.HiddenUserServer:
		if pn := normalizedPhoneJID(identifier); pn != "" {
			return canonicalChat(pn, "", nil)
		}
		if pn := mappings[jid]; pn != "" {
			return canonicalChat(pn, "", nil)
		}
		return "", "unresolved_lid"
	case parsed.Server == watypes.DefaultUserServer:
		if !digits(parsed.User) {
			return "", "invalid_jid"
		}
		return jid, ""
	case parsed.Server == watypes.GroupServer:
		return jid, ""
	case parsed.Server == watypes.BroadcastServer:
		return "", "broadcast"
	case strings.Contains(parsed.Server, "status") || parsed.User == "status":
		return "", "status"
	case parsed.Server == watypes.NewsletterServer:
		return "", "newsletter"
	default:
		return "", "unsupported_jid"
	}
}

func canonicalSender(jid, identifier string, mappings map[string]string) string {
	jid = normalizedJID(jid)
	if strings.HasSuffix(jid, "@lid") {
		if pn := normalizedPhoneJID(identifier); pn != "" {
			return pn
		}
		if pn := mappings[jid]; pn != "" {
			return pn
		}
		return jid
	}
	if pn := normalizedPhoneJID(jid); pn != "" {
		return pn
	}
	return ""
}

func normalizedJID(value string) string {
	value = strings.TrimSpace(value)
	if strings.Count(value, "@") != 1 {
		return ""
	}
	parsed, err := watypes.ParseJID(value)
	if err != nil || parsed.User == "" {
		return ""
	}
	return parsed.ToNonAD().String()
}

func normalizedPhoneJID(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "+")
	if digits(value) && value != "0" {
		return value + "@s.whatsapp.net"
	}
	value = normalizedJID(value)
	parsed, _ := watypes.ParseJID(value)
	if parsed.Server == watypes.DefaultUserServer && digits(parsed.User) && parsed.User != "0" {
		return value
	}
	return ""
}

func digits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var index, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&index, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func messageQuery(ctx context.Context, tx *sql.Tx) (string, error) {
	mediaColumns, err := tableColumns(ctx, tx, "ZWAMEDIAITEM")
	if err != nil {
		return "", err
	}
	// Desktop schema versions vary in optional metadata. Only confirmed
	// columns are used; missing metadata leaves faithful generic placeholders.
	column := func(name, fallback string) string {
		if mediaColumns[name] {
			return "COALESCE(mi." + name + "," + fallback + ")"
		}
		return fallback
	}
	return `SELECT m.Z_PK, COALESCE(m.ZCHATSESSION,0), COALESCE(m.ZSTANZAID,''),
		COALESCE(m.ZMESSAGETYPE,-1), COALESCE(m.ZTEXT,''), m.ZMESSAGEDATE,
		COALESCE(m.ZISFROMME,0), COALESCE(m.ZGROUPMEMBER,0), COALESCE(m.ZFROMJID,''), ` +
		column("ZTITLE", "''") + ", " + column("ZMEDIALOCALPATH", "''") + ", " +
		column("ZFILENAME", "''") + ", " + column("ZVCARDSTRING", "''") + ", " + column("ZMEDIAORIGIN", "-1") + `
		FROM ZWAMESSAGE m LEFT JOIN ZWAMEDIAITEM mi ON mi.Z_PK=m.ZMEDIAITEM
		WHERE m.Z_PK > ? ORDER BY m.ZCHATSESSION, m.Z_PK`, nil
}

func (r *Reader) captureMedia(message *Message, relativePath string) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(message.MIMEType))
	if err != nil || !(strings.HasPrefix(mediaType, "image/") || strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "application/")) {
		message.MIMEType = ""
	} else {
		message.MIMEType = mediaType
	}
	if message.MIMEType == "" {
		message.MIMEType = mime.TypeByExtension(strings.ToLower(filepath.Ext(relativePath)))
	}
	if message.FileName == "" && relativePath != "" {
		message.FileName = filepath.Base(relativePath)
	}
	ref := whatsappmedia.EncodeLocalMediaRef(relativePath)
	if ref == "" {
		return
	}
	path, err := whatsappmedia.ResolveLocalMediaPath(r.options.MediaRoot, ref)
	if err != nil {
		return
	}
	// ResolveLocalMediaPath checks traversal; resolve symlinks as well so a
	// media attachment can never reach a file outside the Message root.
	root, rootErr := filepath.EvalSymlinks(r.options.MediaRoot)
	path, pathErr := filepath.EvalSymlinks(path)
	if rootErr != nil || pathErr != nil {
		return
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	message.LocalMediaRef = ref
	message.Size = info.Size()
}
