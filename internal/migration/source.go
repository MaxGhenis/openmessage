package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

type legacyConversation struct {
	ID               string
	Name             string
	IsGroup          bool
	ParticipantsJSON string
	LastMessageMS    int64
	UnreadCount      int64
	Platform         string
	DisplayProtocol  string
	NotificationMode string
	IsFavorite       bool
	Tab              string
}

type legacyMessage struct {
	ID              string
	ConversationID  string
	SenderName      string
	SenderNumber    string
	Body            string
	TimestampMS     int64
	Status          string
	IsFromMe        bool
	MentionsMe      bool
	MediaID         string
	MIME            string
	DecryptionKey   string
	Reactions       string
	ReplyToID       string
	Platform        string
	SourceID        string
	Transcript      string
	TranscribedAtMS int64
	TranscriptModel string
}

type legacyContact struct {
	ID     string
	Name   string
	Number string
}

type legacyUnifiedContact struct {
	ID              string
	DisplayName     string
	IdentifiersJSON string
}

type legacyScheduledMessage struct {
	ID             string
	ConversationID string
	Body           string
	ReplyToID      string
	SendAtMS       int64
	Status         string
	Attempts       int64
	LastError      string
	CreatedAtMS    int64
	SentMessageID  string
	MediaData      []byte
	MediaFilename  string
	MediaMIME      string
}

type legacyDataset struct {
	conversations []legacyConversation
	messages      []legacyMessage
	contacts      []legacyContact
	unified       []legacyUnifiedContact
	scheduled     []legacyScheduledMessage

	tableCounts      map[string]int64
	scheduleByStatus map[string]int64
	platformMessages map[string]int64
	dropped          DroppedDimensions
	signalLocalRows  int64
	unreadRows       int64
	mediaRows        int64
}

var requiredLegacyColumns = map[string][]string{
	"conversations": {
		"conversation_id", "name", "is_group", "participants", "last_message_ts",
		"unread_count", "source_platform", "display_protocol", "notification_mode",
		"is_favorite", "tab",
	},
	"messages": {
		"message_id", "conversation_id", "sender_name", "sender_number", "body",
		"timestamp_ms", "status", "is_from_me", "mentions_me", "media_id",
		"mime_type", "decryption_key", "reactions", "reply_to_id", "source_platform",
		"source_id", "transcript", "transcribed_at", "transcript_model",
	},
	"contacts":           {"contact_id", "name", "number"},
	"contact_avatars":    {"avatar_id"},
	"drafts":             {"draft_id"},
	"outgoing_send_keys": {"idempotency_key"},
	"unified_contacts":   {"unified_id", "display_name", "identifiers"},
	"scheduled_messages": {
		"id", "conversation_id", "body", "reply_to_id", "send_at", "status",
		"attempts", "last_error", "created_at", "sent_message_id", "media_data",
		"media_filename", "media_mime",
	},
	"contact_meta": {"person_key"},
	"tabs":         {"tab_id"},
}

func loadLegacySource(
	ctx context.Context,
	sourcePath string,
	snapshotDirectory string,
) (datasetResult legacyDataset, reportResult SourceReport, returnErr error) {
	files, err := captureSourceFileEvidence(sourcePath)
	if err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: inspect legacy database family: %v", ErrSource, err)
	}
	before := files[0].SHA256Before
	snapshotPath, cleanupSnapshot, err := stageLegacySourceSnapshot(snapshotDirectory, files)
	if err != nil {
		cleanupErr := cleanupSnapshot()
		return legacyDataset{}, SourceReport{}, errors.Join(
			fmt.Errorf("%w: stage immutable legacy snapshot: %v", ErrSource, err),
			wrapSnapshotCleanupError(cleanupErr),
		)
	}
	defer func() {
		if cleanupErr := cleanupSnapshot(); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf(
				"%w: remove staged legacy snapshot: %v", ErrSource, cleanupErr,
			))
		}
	}()
	database, err := sql.Open("sqlite", readOnlySQLiteDSN(snapshotPath))
	if err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: open legacy database: %v", ErrSource, err)
	}
	database.SetMaxOpenConns(1)
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf(
				"%w: close staged legacy snapshot: %v", ErrSource, closeErr,
			))
		}
	}()
	if err := database.PingContext(ctx); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: connect to legacy database: %v", ErrSource, err)
	}
	if _, err := database.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: make legacy database connection read-only: %v", ErrSource, err)
	}

	quick, err := quickCheck(ctx, database)
	if err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: quick-check legacy database: %v", ErrSource, err)
	}
	if quick != "ok" {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: legacy database PRAGMA quick_check: %s", ErrSource, quick)
	}
	if err := validateLegacySchema(ctx, database); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: %v", ErrSource, err)
	}
	fingerprint, err := databaseSchemaFingerprint(ctx, database)
	if err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: fingerprint legacy schema: %v", ErrSource, err)
	}
	var userVersion int64
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: read legacy user_version: %v", ErrSource, err)
	}

	dataset := legacyDataset{
		tableCounts: map[string]int64{},
		scheduleByStatus: map[string]int64{
			"pending": 0, "sending": 0, "sent": 0, "failed": 0, "canceled": 0,
		},
		platformMessages: map[string]int64{},
	}
	if err := loadLegacyRows(ctx, database, &dataset); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: %v", ErrSource, err)
	}
	if err := validateLegacyRelationships(dataset); err != nil {
		return legacyDataset{}, SourceReport{}, fmt.Errorf("%w: %v", ErrSource, err)
	}

	report := SourceReport{
		Path:              filepath.Dir(sourcePath),
		DatabasePath:      sourcePath,
		SchemaFingerprint: fingerprint,
		SchemaUserVersion: userVersion,
		QuickCheck:        quick,
		SHA256Before:      before,
		Files:             files,
	}
	return dataset, report, nil
}

func loadLegacyRows(ctx context.Context, database *sql.DB, dataset *legacyDataset) error {
	rows, err := database.QueryContext(ctx, `
		SELECT conversation_id, name, is_group, participants, last_message_ts,
		       unread_count, source_platform, display_protocol, notification_mode,
		       is_favorite, tab
		FROM conversations
		ORDER BY conversation_id
	`)
	if err != nil {
		return fmt.Errorf("read conversations: %w", err)
	}
	for rows.Next() {
		var row legacyConversation
		if err := rows.Scan(
			&row.ID, &row.Name, &row.IsGroup, &row.ParticipantsJSON,
			&row.LastMessageMS, &row.UnreadCount, &row.Platform,
			&row.DisplayProtocol, &row.NotificationMode, &row.IsFavorite, &row.Tab,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan conversation: %w", err)
		}
		dataset.conversations = append(dataset.conversations, row)
		if row.UnreadCount > 0 {
			dataset.unreadRows++
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("read conversations: %w", err)
	}

	rows, err = database.QueryContext(ctx, `
		SELECT message_id, conversation_id, sender_name, sender_number, body,
		       timestamp_ms, status, is_from_me, mentions_me, media_id, mime_type,
		       decryption_key, reactions, reply_to_id, source_platform, source_id,
		       transcript, transcribed_at, transcript_model
		FROM messages
		ORDER BY source_platform, conversation_id, timestamp_ms, message_id
	`)
	if err != nil {
		return fmt.Errorf("read messages: %w", err)
	}
	for rows.Next() {
		var row legacyMessage
		if err := rows.Scan(
			&row.ID, &row.ConversationID, &row.SenderName, &row.SenderNumber,
			&row.Body, &row.TimestampMS, &row.Status, &row.IsFromMe, &row.MentionsMe,
			&row.MediaID, &row.MIME, &row.DecryptionKey, &row.Reactions, &row.ReplyToID,
			&row.Platform, &row.SourceID, &row.Transcript, &row.TranscribedAtMS,
			&row.TranscriptModel,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan message: %w", err)
		}
		dataset.messages = append(dataset.messages, row)
		platform := normalizeLegacyPlatform(row.Platform)
		dataset.platformMessages[platform]++
		if strings.TrimSpace(row.MediaID) != "" {
			dataset.mediaRows++
		}
		if strings.TrimSpace(row.Reactions) != "" &&
			strings.TrimSpace(row.Reactions) != "null" &&
			strings.TrimSpace(row.Reactions) != "[]" {
			dataset.dropped.ReactionsBearingMessages++
		}
		if strings.TrimSpace(row.Transcript) != "" || row.TranscribedAtMS != 0 ||
			strings.TrimSpace(row.TranscriptModel) != "" {
			dataset.dropped.TranscriptBearingMessages++
		}
		if strings.HasPrefix(row.ID, "signal:local:") {
			dataset.signalLocalRows++
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("read messages: %w", err)
	}

	rows, err = database.QueryContext(ctx, `
		SELECT contact_id, name, number FROM contacts ORDER BY contact_id
	`)
	if err != nil {
		return fmt.Errorf("read contacts: %w", err)
	}
	for rows.Next() {
		var row legacyContact
		if err := rows.Scan(&row.ID, &row.Name, &row.Number); err != nil {
			rows.Close()
			return fmt.Errorf("scan contact: %w", err)
		}
		dataset.contacts = append(dataset.contacts, row)
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("read contacts: %w", err)
	}

	rows, err = database.QueryContext(ctx, `
		SELECT unified_id, display_name, identifiers
		FROM unified_contacts ORDER BY unified_id
	`)
	if err != nil {
		return fmt.Errorf("read unified contacts: %w", err)
	}
	for rows.Next() {
		var row legacyUnifiedContact
		if err := rows.Scan(&row.ID, &row.DisplayName, &row.IdentifiersJSON); err != nil {
			rows.Close()
			return fmt.Errorf("scan unified contact: %w", err)
		}
		dataset.unified = append(dataset.unified, row)
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("read unified contacts: %w", err)
	}

	rows, err = database.QueryContext(ctx, `
		SELECT id, conversation_id, body, reply_to_id, send_at, status, attempts,
		       last_error, created_at, sent_message_id, media_data, media_filename,
		       media_mime
		FROM scheduled_messages
		ORDER BY id
	`)
	if err != nil {
		return fmt.Errorf("read scheduled messages: %w", err)
	}
	for rows.Next() {
		var row legacyScheduledMessage
		if err := rows.Scan(
			&row.ID, &row.ConversationID, &row.Body, &row.ReplyToID, &row.SendAtMS,
			&row.Status, &row.Attempts, &row.LastError, &row.CreatedAtMS,
			&row.SentMessageID, &row.MediaData, &row.MediaFilename, &row.MediaMIME,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan scheduled message: %w", err)
		}
		row.Status = strings.ToLower(strings.TrimSpace(row.Status))
		dataset.scheduled = append(dataset.scheduled, row)
		dataset.scheduleByStatus[row.Status]++
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("read scheduled messages: %w", err)
	}

	for _, table := range []string{
		"conversations", "messages", "contacts", "contact_avatars", "drafts",
		"outgoing_send_keys", "unified_contacts", "scheduled_messages", "contact_meta", "tabs",
	} {
		var count int64
		if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			return fmt.Errorf("count %s: %w", table, err)
		}
		dataset.tableCounts[table] = count
	}
	dataset.dropped.ContactAvatars = dataset.tableCounts["contact_avatars"]
	dataset.dropped.Drafts = dataset.tableCounts["drafts"]
	dataset.dropped.ContactMetaCRM = dataset.tableCounts["contact_meta"]
	dataset.dropped.OutgoingSendKeys = dataset.tableCounts["outgoing_send_keys"]
	dataset.dropped.Tabs = dataset.tableCounts["tabs"]
	return nil
}

func validateLegacyRelationships(dataset legacyDataset) error {
	conversations := make(map[string]string, len(dataset.conversations))
	for _, conversation := range dataset.conversations {
		if strings.TrimSpace(conversation.ID) == "" {
			return fmt.Errorf("conversation has an empty primary key")
		}
		platform := normalizeLegacyPlatform(conversation.Platform)
		if _, err := accountForPlatform(platform); err != nil {
			return err
		}
		conversations[conversation.ID] = platform
	}
	for _, message := range dataset.messages {
		if strings.TrimSpace(message.ID) == "" {
			return fmt.Errorf("message has an empty primary key")
		}
		platform := normalizeLegacyPlatform(message.Platform)
		if _, err := accountForPlatform(platform); err != nil {
			return err
		}
		conversationPlatform, ok := conversations[message.ConversationID]
		if !ok {
			return fmt.Errorf("message %q references missing conversation %q", message.ID, message.ConversationID)
		}
		if platform != conversationPlatform {
			return fmt.Errorf(
				"message %q platform %q does not match conversation %q platform %q",
				message.ID, platform, message.ConversationID, conversationPlatform,
			)
		}
		if message.TimestampMS <= 0 {
			return fmt.Errorf("message %q has non-positive timestamp_ms %d", message.ID, message.TimestampMS)
		}
		if strings.TrimSpace(deriveRemoteMessageID(message)) == "" {
			return fmt.Errorf("message %q maps to an empty remote_message_id", message.ID)
		}
	}
	for _, scheduled := range dataset.scheduled {
		if strings.TrimSpace(scheduled.ID) == "" {
			return fmt.Errorf("scheduled message has an empty primary key")
		}
		if _, ok := conversations[scheduled.ConversationID]; !ok {
			return fmt.Errorf("scheduled message %q references missing conversation %q", scheduled.ID, scheduled.ConversationID)
		}
		switch scheduled.Status {
		case "pending", "sending", "sent", "failed", "canceled":
		default:
			return fmt.Errorf("scheduled message %q has unsupported status %q", scheduled.ID, scheduled.Status)
		}
		if (scheduled.Status == "pending" || scheduled.Status == "sending") && scheduled.SendAtMS <= 0 {
			return fmt.Errorf("scheduled message %q has non-positive send_at %d", scheduled.ID, scheduled.SendAtMS)
		}
	}
	return nil
}

func validateLegacySchema(ctx context.Context, database *sql.DB) error {
	tables := make([]string, 0, len(requiredLegacyColumns))
	for table := range requiredLegacyColumns {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		rows, err := database.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return fmt.Errorf("inspect legacy table %s: %w", table, err)
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int64
			var name, dataType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return fmt.Errorf("inspect legacy table %s: %w", table, err)
			}
			columns[name] = true
		}
		if err := closeRows(rows); err != nil {
			return fmt.Errorf("inspect legacy table %s: %w", table, err)
		}
		if len(columns) == 0 {
			return fmt.Errorf("unsupported legacy schema: table %s is missing", table)
		}
		for _, column := range requiredLegacyColumns[table] {
			if !columns[column] {
				return fmt.Errorf("unsupported legacy schema: column %s.%s is missing", table, column)
			}
		}
	}
	return nil
}

func quickCheck(ctx context.Context, database *sql.DB) (string, error) {
	rows, err := database.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return "", err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", fmt.Errorf("PRAGMA quick_check returned no rows")
	}
	return strings.Join(results, "; "), nil
}

func databaseSchemaFingerprint(ctx context.Context, database *sql.DB) (string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT type, name, tbl_name, IFNULL(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name, tbl_name
	`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return "", err
		}
		for _, field := range []string{kind, name, table, statement} {
			_, _ = io.WriteString(hash, field)
			_, _ = hash.Write([]byte{0x1f})
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readOnlySQLiteDSN(path string) string {
	return (&url.URL{
		Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro",
	}).String()
}

func stageLegacySourceSnapshot(
	directory string,
	evidence []SourceFileEvidence,
) (string, func() error, error) {
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", func() error { return nil }, err
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	snapshotPath := filepath.Join(directory, "messages.db")
	for _, file := range evidence {
		if !file.ExistsBefore || (file.Role != "database" && file.Role != "wal") {
			continue
		}
		destination := snapshotPath
		if file.Role == "wal" {
			destination += "-wal"
		}
		if err := copySourceSnapshotFile(file.Path, destination); err != nil {
			return snapshotStageFailure(cleanup, fmt.Errorf("copy %s: %w", file.Role, err))
		}
		copied, err := inspectSourceFile(destination)
		if err != nil {
			return snapshotStageFailure(cleanup, fmt.Errorf("verify copied %s: %w", file.Role, err))
		}
		if copied.size != file.SizeBefore || copied.sha256 != file.SHA256Before {
			return snapshotStageFailure(cleanup, fmt.Errorf("%s changed while its snapshot was copied", file.Role))
		}
	}
	return snapshotPath, cleanup, nil
}

func snapshotStageFailure(
	cleanup func() error,
	stageErr error,
) (string, func() error, error) {
	cleanupErr := cleanup()
	return "", cleanup, errors.Join(stageErr, wrapSnapshotCleanupError(cleanupErr))
}

func wrapSnapshotCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remove partial staged legacy snapshot: %w", err)
}

func copySourceSnapshotFile(sourcePath, destinationPath string) (returnErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := destination.Close(); returnErr == nil {
			returnErr = closeErr
		}
	}()
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	return destination.Sync()
}

func captureSourceFileEvidence(databasePath string) ([]SourceFileEvidence, error) {
	files := []SourceFileEvidence{
		{Role: "database", Path: databasePath},
		{Role: "wal", Path: databasePath + "-wal"},
		{Role: "shm", Path: databasePath + "-shm"},
	}
	for index := range files {
		state, err := inspectSourceFile(files[index].Path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s %s: %w", files[index].Role, files[index].Path, err)
		}
		files[index].ExistsBefore = state.exists
		files[index].SizeBefore = state.size
		files[index].SHA256Before = state.sha256
		files[index].ModeBefore = state.mode
		files[index].ModifiedAtNSBefore = state.modifiedAtNS
	}
	if !files[0].ExistsBefore {
		return nil, fmt.Errorf("database does not exist: %s", databasePath)
	}
	return files, nil
}

func completeSourceFileEvidence(report *SourceReport) (bool, error) {
	if report == nil || len(report.Files) != 3 {
		return false, fmt.Errorf("source database family evidence is incomplete")
	}
	allUnchanged := true
	for index := range report.Files {
		file := &report.Files[index]
		state, err := inspectSourceFile(file.Path)
		if err != nil {
			return false, fmt.Errorf("inspect %s %s after transform: %w", file.Role, file.Path, err)
		}
		file.ExistsAfter = state.exists
		file.SizeAfter = state.size
		file.SHA256After = state.sha256
		file.ModeAfter = state.mode
		file.ModifiedAtNSAfter = state.modifiedAtNS
		file.Unchanged = file.ExistsBefore == file.ExistsAfter &&
			file.SizeBefore == file.SizeAfter &&
			file.SHA256Before == file.SHA256After &&
			file.ModeBefore == file.ModeAfter &&
			file.ModifiedAtNSBefore == file.ModifiedAtNSAfter
		allUnchanged = allUnchanged && file.Unchanged
		if file.Role == "database" {
			report.SHA256After = file.SHA256After
		}
	}
	report.Unchanged = allUnchanged
	return allUnchanged, nil
}

type sourceFileState struct {
	exists       bool
	size         int64
	sha256       string
	mode         string
	modifiedAtNS int64
}

func inspectSourceFile(path string) (sourceFileState, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return sourceFileState{}, nil
	}
	if err != nil {
		return sourceFileState{}, err
	}
	if !info.Mode().IsRegular() {
		return sourceFileState{}, fmt.Errorf("not a regular file")
	}
	hash, err := fileSHA256(path)
	if err != nil {
		return sourceFileState{}, err
	}
	return sourceFileState{
		exists: true, size: info.Size(), sha256: hash,
		mode: fmt.Sprintf("%04o", info.Mode().Perm()), modifiedAtNS: info.ModTime().UnixNano(),
	}, nil
}

func closeRows(rows *sql.Rows) error {
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	return closeErr
}
