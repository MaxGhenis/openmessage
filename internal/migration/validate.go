package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/maxghenis/openmessage/internal/storage/blob"

	_ "modernc.org/sqlite"
)

var targetCountTables = []string{
	"accounts", "devices", "identities", "people", "person_identities",
	"conversations", "conversation_participants", "inbox", "messages",
	"message_attachments", "outbox", "outbox_attachments", "read_cursors",
}

func checkpointAndSyncSQLite(ctx context.Context, path string) error {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int64
	if err := database.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(
		&busy, &logFrames, &checkpointed,
	); err != nil {
		_ = database.Close()
		return err
	}
	if busy != 0 || logFrames != checkpointed {
		_ = database.Close()
		return fmt.Errorf(
			"WAL checkpoint incomplete: busy=%d log=%d checkpointed=%d",
			busy, logFrames, checkpointed,
		)
	}
	if err := database.Close(); err != nil {
		return err
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove SQLite sidecar %s: %w", sidecar, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func validateTarget(
	ctx context.Context,
	options Options,
	dataset legacyDataset,
	state *transformState,
	report *Report,
) error {
	database, err := sql.Open("sqlite", readOnlySQLiteDSN(options.TempStorePath))
	if err != nil {
		return fmt.Errorf("%w: open staged database for validation: %v", ErrValidation, err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return fmt.Errorf("%w: make validation connection read-only: %v", ErrValidation, err)
	}

	quick, err := quickCheck(ctx, database)
	if err != nil {
		return fmt.Errorf("%w: quick-check staged database: %v", ErrValidation, err)
	}
	report.Validation.QuickCheck = quick
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&report.Target.SchemaVersion); err != nil {
		return fmt.Errorf("%w: read staged schema version: %v", ErrValidation, err)
	}
	if err := loadMigrationChecksums(ctx, database, &report.Target.MigrationChecksums); err != nil {
		return fmt.Errorf("%w: read staged migration ledger: %v", ErrValidation, err)
	}
	for _, table := range targetCountTables {
		count, err := queryCount(ctx, database, "SELECT COUNT(*) FROM "+table)
		if err != nil {
			return fmt.Errorf("%w: count staged %s: %v", ErrValidation, table, err)
		}
		report.Target.Counts[table] = count
	}

	actualHistoryByPlatform, actualHistory, actualScheduled, err := classifyTargetMessages(ctx, database, state)
	if err != nil {
		return fmt.Errorf("%w: classify staged messages: %v", ErrValidation, err)
	}
	fillTableReconciliations(dataset, state, report, actualHistory)
	fillPlatformReconciliations(dataset, state, report, actualHistoryByPlatform)
	if err := fillIdentityReport(ctx, database, report); err != nil {
		return fmt.Errorf("%w: inspect staged identity graph: %v", ErrValidation, err)
	}

	fkViolations, err := foreignKeyCheck(ctx, database)
	if err != nil {
		return fmt.Errorf("%w: run foreign_key_check: %v", ErrValidation, err)
	}
	report.Validation.ForeignKeyViolations = fkViolations
	orphans, err := detectOrphans(ctx, database)
	if err != nil {
		return fmt.Errorf("%w: detect staged orphans: %v", ErrValidation, err)
	}
	report.Validation.Orphans = orphans

	report.SampledHashes, err = sampledHashChecks(ctx, database, dataset, state)
	if err != nil {
		return fmt.Errorf("%w: sample staged message hashes: %v", ErrValidation, err)
	}
	report.Validation.SampledHashesMatched = true
	for _, sample := range report.SampledHashes {
		if !sample.Matched {
			report.Validation.SampledHashesMatched = false
			break
		}
	}

	report.Validation.BlobReferencesValid, report.Media.ScheduledBlobsVerified =
		validateScheduledBlobs(ctx, database, options.TempBlobPath, state.scheduledBlobs)
	report.Media.PendingAttachments = report.Target.Counts["message_attachments"]
	report.Schedule.PendingMedia = state.expectedPendingMedia

	unchanged, evidenceErr := completeSourceFileEvidence(&report.Source)
	if evidenceErr != nil {
		return fmt.Errorf("%w: complete legacy source evidence: %v", ErrValidation, evidenceErr)
	}
	report.Validation.SourceUnchanged = unchanged

	countsMatched := countsMatch(dataset, state, report, actualHistory, actualScheduled, actualHistoryByPlatform)
	report.Validation.CountsMatched = countsMatched
	report.Validation.Passed = quick == "ok" &&
		report.Target.SchemaVersion == 9 && len(report.Target.MigrationChecksums) == 9 &&
		len(fkViolations) == 0 && orphanTotal(orphans) == 0 &&
		countsMatched && report.Validation.SampledHashesMatched &&
		report.Validation.BlobReferencesValid && report.Validation.SourceUnchanged &&
		len(report.MessageCollisions) == 0

	appendMediaWarnings(report)
	if !report.Validation.Passed {
		return fmt.Errorf("%w: staged v2 store failed one or more integrity gates", ErrValidation)
	}
	return nil
}

func fillTableReconciliations(
	dataset legacyDataset,
	state *transformState,
	report *Report,
	actualHistory int64,
) {
	add := func(name string, legacy, v2, reconciled int64, disposition string) {
		report.TableCounts[name] = TableReconciliation{
			Legacy: legacy, V2: v2, Reconciled: reconciled,
			ReconciliationRatio: reconciliationRatio(reconciled, legacy),
			Disposition:         disposition,
		}
	}
	add("accounts", int64(len(state.accounts)), report.Target.Counts["accounts"], report.Target.Counts["accounts"], "derived")
	add("devices", int64(len(state.accounts)), report.Target.Counts["devices"], report.Target.Counts["devices"], "derived")
	add("identities", int64(len(state.identities)), report.Target.Counts["identities"], report.Target.Counts["identities"], "derived")
	add("people", int64(len(dataset.unified)), report.Target.Counts["people"], report.Target.Counts["people"], "unified_contacts")
	add("person_identities", state.expectedLinks, report.Target.Counts["person_identities"], report.Target.Counts["person_identities"], "explicit")
	add("conversations", int64(len(dataset.conversations)), report.Target.Counts["conversations"], report.Target.Counts["conversations"], "migrated")
	add("conversation_participants", state.expectedParticipants, report.Target.Counts["conversation_participants"], report.Target.Counts["conversation_participants"], "derived")
	add("messages", int64(len(dataset.messages)), actualHistory, actualHistory, "migrated_history")
	add("message_attachments", dataset.mediaRows, report.Target.Counts["message_attachments"], report.Target.Counts["message_attachments"], "pending_remote_ref")
	add("contacts", int64(len(dataset.contacts)), int64(len(state.contactIdentityKeys)), state.contactRowsMapped, "address_book_identities")
	add("unified_contacts", int64(len(dataset.unified)), report.Target.Counts["people"], report.Target.Counts["people"], "people")
	add("scheduled_messages", int64(len(dataset.scheduled)), report.Target.Counts["outbox"], int64(len(dataset.scheduled)), "mapped_or_counted_skip")
	add("outbox", dataset.scheduleByStatus["pending"], report.Target.Counts["outbox"], report.Target.Counts["outbox"], "pending_only")
	add("outbox_attachments", state.expectedPendingMedia, report.Target.Counts["outbox_attachments"], report.Target.Counts["outbox_attachments"], "scheduled_media_blob")
	add("read_cursors", int64(len(dataset.conversations)), report.Target.Counts["read_cursors"], report.Target.Counts["read_cursors"], "lossy_approximation")

	for _, dropped := range []struct {
		name  string
		count int64
	}{
		{"reactions", dataset.dropped.ReactionsBearingMessages},
		{"transcripts", dataset.dropped.TranscriptBearingMessages},
		{"contact_avatars", dataset.dropped.ContactAvatars},
		{"drafts", dataset.dropped.Drafts},
		{"contact_meta", dataset.dropped.ContactMetaCRM},
		{"outgoing_send_keys", dataset.dropped.OutgoingSendKeys},
		{"tabs", dataset.dropped.Tabs},
		{"malformed_messages", dataset.dropped.MalformedMessages},
		{"orphaned_messages", dataset.dropped.OrphanedMessages},
		{"platform_mismatch_messages", dataset.dropped.PlatformMismatchMessages},
		{"non_positive_timestamp_messages", dataset.dropped.NonPositiveTimestampMessages},
		{"unmappable_messages", dataset.dropped.UnmappableMessages},
		{"malformed_participants", dataset.dropped.MalformedParticipants},
		{"unmappable_unified_contacts", dataset.dropped.UnmappableUnifiedContacts},
	} {
		add(dropped.name, dropped.count, 0, dropped.count, "dropped_with_count")
	}
}

func fillPlatformReconciliations(
	dataset legacyDataset,
	state *transformState,
	report *Report,
	actual map[string]int64,
) {
	platforms := map[string]bool{}
	for platform := range dataset.platformMessages {
		platforms[platform] = true
	}
	for platform := range actual {
		platforms[platform] = true
	}
	for platform := range platforms {
		legacy := dataset.platformMessages[platform]
		account := state.accounts[platform]
		report.PlatformCounts[platform] = PlatformReconciliation{
			AccountID: account.AccountID, Legacy: legacy, V2: actual[platform],
			ReconciliationRatio: reconciliationRatio(actual[platform], legacy),
		}
	}
}

func fillIdentityReport(ctx context.Context, database *sql.DB, report *Report) error {
	report.Identities.Created = report.Target.Counts["identities"]
	report.Identities.PeopleCreated = report.Target.Counts["people"]
	report.Identities.LinksByProvenance = map[string]int64{
		"explicit": 0, "address_book": 0, "bridge_alias": 0, "import": 0, "heuristic": 0,
	}
	rows, err := database.QueryContext(ctx, `
		SELECT provenance, COUNT(*)
		FROM person_identities
		GROUP BY provenance
		ORDER BY provenance
	`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var provenance string
		var count int64
		if err := rows.Scan(&provenance, &count); err != nil {
			rows.Close()
			return err
		}
		report.Identities.LinksByProvenance[provenance] = count
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	return database.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM identities AS i
		LEFT JOIN person_identities AS pi ON pi.identity_id = i.identity_id
		WHERE pi.identity_id IS NULL
	`).Scan(&report.Identities.UnunifiedIdentities)
}

func classifyTargetMessages(
	ctx context.Context,
	database *sql.DB,
	state *transformState,
) (map[string]int64, int64, int64, error) {
	accountPlatforms := map[string]string{}
	for platform, account := range state.accounts {
		accountPlatforms[account.AccountID] = platform
	}
	actualByPlatform := map[string]int64{}
	var actualHistory, actualScheduled int64
	rows, err := database.QueryContext(ctx, "SELECT account_id, message_id FROM messages ORDER BY message_id")
	if err != nil {
		return nil, 0, 0, err
	}
	for rows.Next() {
		var accountID, messageID string
		if err := rows.Scan(&accountID, &messageID); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		if _, ok := state.expectedHistory[messageID]; ok {
			actualHistory++
			actualByPlatform[accountPlatforms[accountID]]++
			continue
		}
		if _, ok := state.scheduledMessages[messageID]; ok {
			actualScheduled++
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, 0, 0, err
	}
	return actualByPlatform, actualHistory, actualScheduled, nil
}

func countsMatch(
	dataset legacyDataset,
	state *transformState,
	report *Report,
	actualHistory int64,
	actualScheduled int64,
	actualHistoryByPlatform map[string]int64,
) bool {
	if actualHistory != int64(len(dataset.messages)) ||
		actualScheduled != dataset.scheduleByStatus["pending"]+dataset.scheduleByStatus["sending"] ||
		report.Target.Counts["messages"] != actualHistory+actualScheduled ||
		report.Target.Counts["accounts"] != int64(len(state.accounts)) ||
		report.Target.Counts["devices"] != int64(len(state.accounts)) ||
		report.Target.Counts["identities"] != int64(len(state.identities)) ||
		report.Target.Counts["people"] != int64(len(dataset.unified)) ||
		report.Target.Counts["person_identities"] != state.expectedLinks ||
		report.Target.Counts["conversations"] != int64(len(dataset.conversations)) ||
		report.Target.Counts["conversation_participants"] != state.expectedParticipants ||
		report.Target.Counts["message_attachments"] != dataset.mediaRows ||
		report.Target.Counts["outbox"] != dataset.scheduleByStatus["pending"] ||
		report.Target.Counts["outbox_attachments"] != state.expectedPendingMedia ||
		report.Target.Counts["read_cursors"] != state.expectedCursors ||
		report.Target.Counts["inbox"] != 0 {
		return false
	}
	for platform, legacy := range dataset.platformMessages {
		if actualHistoryByPlatform[platform] != legacy {
			return false
		}
	}
	return true
}

func loadMigrationChecksums(ctx context.Context, database *sql.DB, destination *[]string) error {
	rows, err := database.QueryContext(ctx, `
		SELECT checksum_sha256 FROM schema_migrations ORDER BY version
	`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var checksum string
		if err := rows.Scan(&checksum); err != nil {
			rows.Close()
			return err
		}
		*destination = append(*destination, checksum)
	}
	return closeRows(rows)
}

func foreignKeyCheck(ctx context.Context, database *sql.DB) ([]ForeignKeyViolation, error) {
	rows, err := database.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, err
	}
	violations := []ForeignKeyViolation{}
	for rows.Next() {
		var violation ForeignKeyViolation
		var rowID sql.NullInt64
		if err := rows.Scan(&violation.Table, &rowID, &violation.Parent, &violation.Constraint); err != nil {
			rows.Close()
			return nil, err
		}
		if rowID.Valid {
			value := rowID.Int64
			violation.RowID = &value
		}
		violations = append(violations, violation)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return violations, nil
}

func detectOrphans(ctx context.Context, database *sql.DB) (OrphanReport, error) {
	queries := []struct {
		destination *int64
		query       string
	}{
		{query: `SELECT COUNT(*) FROM messages m LEFT JOIN conversations c ON c.account_id=m.account_id AND c.conversation_id=m.conversation_id WHERE c.conversation_id IS NULL`},
		{query: `SELECT COUNT(*) FROM messages m LEFT JOIN identities i ON i.account_id=m.account_id AND i.identity_id=m.sender_identity_id WHERE m.sender_identity_id IS NOT NULL AND i.identity_id IS NULL`},
		{query: `SELECT COUNT(*) FROM conversation_participants cp LEFT JOIN identities i ON i.account_id=cp.account_id AND i.identity_id=cp.identity_id WHERE i.identity_id IS NULL`},
		{query: `SELECT COUNT(*) FROM message_attachments a LEFT JOIN messages m ON m.message_id=a.message_id WHERE m.message_id IS NULL`},
		{query: `SELECT COUNT(*) FROM outbox o LEFT JOIN accounts a ON a.account_id=o.account_id WHERE a.account_id IS NULL`},
		{query: `SELECT COUNT(*) FROM outbox o LEFT JOIN conversations c ON c.account_id=o.account_id AND c.conversation_id=o.conversation_id WHERE c.conversation_id IS NULL`},
		{query: `SELECT COUNT(*) FROM outbox_attachments a LEFT JOIN outbox o ON o.outbox_id=a.outbox_id WHERE o.outbox_id IS NULL`},
		{query: `SELECT COUNT(*) FROM read_cursors r LEFT JOIN messages m ON m.conversation_id=r.conversation_id AND m.message_id=r.last_read_message_id WHERE r.last_read_message_id IS NOT NULL AND m.message_id IS NULL`},
	}
	var report OrphanReport
	destinations := []*int64{
		&report.MessageConversations, &report.MessageSenders,
		&report.ConversationParticipants, &report.MessageAttachments,
		&report.OutboxAccounts, &report.OutboxConversations,
		&report.OutboxAttachments, &report.ReadCursorMessages,
	}
	for index := range queries {
		queries[index].destination = destinations[index]
		if err := database.QueryRowContext(ctx, queries[index].query).Scan(queries[index].destination); err != nil {
			return OrphanReport{}, err
		}
	}
	return report, nil
}

func orphanTotal(report OrphanReport) int64 {
	return report.MessageConversations + report.MessageSenders +
		report.ConversationParticipants + report.MessageAttachments +
		report.OutboxAccounts + report.OutboxConversations +
		report.OutboxAttachments + report.ReadCursorMessages
}

func sampledHashChecks(
	ctx context.Context,
	database *sql.DB,
	dataset legacyDataset,
	state *transformState,
) ([]SampledHash, error) {
	byPlatform := map[string][]expectedMessage{}
	for _, expected := range state.expectedHistory {
		byPlatform[expected.Platform] = append(byPlatform[expected.Platform], expected)
	}
	platforms := make([]string, 0, len(byPlatform))
	for platform := range byPlatform {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	checks := []SampledHash{}
	for _, platform := range platforms {
		candidates := byPlatform[platform]
		sort.Slice(candidates, func(i, j int) bool {
			left := sha256.Sum256([]byte(platform + "\x1f" + candidates[i].Legacy.ID))
			right := sha256.Sum256([]byte(platform + "\x1f" + candidates[j].Legacy.ID))
			return hex.EncodeToString(left[:]) < hex.EncodeToString(right[:])
		})
		if len(candidates) > SampleMessagesPerPlatform {
			candidates = candidates[:SampleMessagesPerPlatform]
		}
		for _, expected := range candidates {
			expectedHash, err := messageSpotHash(
				expected.Legacy.Body, expected.Legacy.TimestampMS,
				expected.RemoteID, expected.MediaID,
			)
			if err != nil {
				return nil, err
			}
			var body, remoteID, attachmentRemoteID string
			var occurredAt int64
			err = database.QueryRowContext(ctx, `
				SELECT m.body, m.occurred_at_ms, m.remote_message_id,
				       COALESCE(a.remote_id, '')
				FROM messages AS m
				LEFT JOIN message_attachments AS a
				  ON a.message_id = m.message_id AND a.ordinal = 0
				WHERE m.message_id = ?
			`, expected.V2ID).Scan(&body, &occurredAt, &remoteID, &attachmentRemoteID)
			actualHash := "missing"
			if err == nil {
				actualHash, err = messageSpotHash(body, occurredAt, remoteID, attachmentRemoteID)
			}
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			checks = append(checks, SampledHash{
				Platform: platform, LegacyMessageID: expected.Legacy.ID,
				V2MessageID: expected.V2ID, ExpectedSHA256: expectedHash,
				ActualSHA256: actualHash, Matched: expectedHash == actualHash,
			})
		}
	}
	return checks, nil
}

func messageSpotHash(body string, occurredAt int64, remoteID, attachmentRemoteID string) (string, error) {
	payload, err := json.Marshal(struct {
		Body               string `json:"body"`
		OccurredAtMS       int64  `json:"occurred_at_ms"`
		RemoteMessageID    string `json:"remote_message_id"`
		AttachmentRemoteID string `json:"attachment_remote_id"`
	}{body, occurredAt, remoteID, attachmentRemoteID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func validateScheduledBlobs(
	ctx context.Context,
	database *sql.DB,
	root string,
	expected []scheduledBlob,
) (bool, int64) {
	store, err := blob.New(root)
	if err != nil {
		return false, 0
	}
	expectedByOutbox := make(map[string]blob.BlobRef, len(expected))
	for _, item := range expected {
		if item.OutboxID == "" {
			return false, 0
		}
		if _, duplicate := expectedByOutbox[item.OutboxID]; duplicate {
			return false, 0
		}
		expectedByOutbox[item.OutboxID] = item.Ref
	}
	rows, err := database.QueryContext(ctx, `
		SELECT outbox_id, ordinal, blob_hash, size_bytes, mime
		FROM outbox_attachments
		ORDER BY outbox_id, ordinal
	`)
	if err != nil {
		return false, 0
	}
	defer rows.Close()
	var verified int64
	seen := make(map[string]bool, len(expected))
	for rows.Next() {
		var outboxID, hash, mime string
		var ordinal, size int64
		if err := rows.Scan(&outboxID, &ordinal, &hash, &size, &mime); err != nil {
			return false, verified
		}
		want, ok := expectedByOutbox[outboxID]
		if !ok || seen[outboxID] || ordinal != 0 ||
			hash != want.Hash || size != want.Size || mime != want.MIME {
			return false, verified
		}
		seen[outboxID] = true
		persisted := blob.BlobRef{Hash: hash, Size: size, MIME: mime}
		reader, err := store.Open(persisted)
		if err != nil {
			return false, verified
		}
		digest := sha256.New()
		actualSize, copyErr := io.Copy(digest, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil || ctx.Err() != nil ||
			actualSize != persisted.Size || hex.EncodeToString(digest.Sum(nil)) != persisted.Hash {
			return false, verified
		}
		verified++
	}
	if rows.Err() != nil || len(seen) != len(expectedByOutbox) {
		return false, verified
	}
	return true, verified
}

func appendMediaWarnings(report *Report) {
	if count := report.Media.WhatsAppUnresolvable; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d WhatsApp attachments lacked a walocal: reference and remain pending but unresolvable", count))
	}
	if count := report.Media.UnsupportedArchiveAttachments; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d archive-platform attachments have no merged Opaque codec and remain pending but unresolvable", count))
	}
	if count := report.Media.SignalLocalAttachments; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d Signal local attachments were preserved as Opaque v1 local paths without importing bytes", count))
	}
}

func queryCount(ctx context.Context, database *sql.DB, query string) (int64, error) {
	var count int64
	err := database.QueryRowContext(ctx, query).Scan(&count)
	return count, err
}
