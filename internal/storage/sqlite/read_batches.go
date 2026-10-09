package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Batched point reads for the v2 read mapping (internal/v2read). Each one
// answers, for many keys in one statement, what a single-row read already
// answers for one key: mapping a page of messages or conversations used to
// issue those single-row reads once per message, participant or conversation,
// so a 100-message thread page cost ~250 statements and a 200-conversation
// list ~1,300. Every statement here seeks a primary key or an existing index
// once per key, so the rows read are the same; only the round trips go.

// readBatchSize bounds the keys bound into one IN list or VALUES list (SQLite
// allows 32,766 host parameters; this matches ReactionsForMessages' batches).
const readBatchSize = 500

// forEachKeyBatch calls visit with successive batches of the distinct keys, in
// first-seen order, and the same keys as statement arguments.
func forEachKeyBatch(keys []string, visit func(batch []string, args []any) error) error {
	unique := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	for start := 0; start < len(unique); start += readBatchSize {
		batch := unique[start:min(start+readBatchSize, len(unique))]
		args := make([]any, len(batch))
		for index, key := range batch {
			args[index] = key
		}
		if err := visit(batch, args); err != nil {
			return err
		}
	}
	return nil
}

// placeholderList returns n comma-separated host parameters.
func placeholderList(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// queryKeyBatches runs statement once per key batch (the statement text is
// built from the batch's placeholder list) and hands every row to scan.
func queryKeyBatches(
	ctx context.Context,
	db *sql.DB,
	operation string,
	keys []string,
	statement func(placeholders string) string,
	leadingArgs []any,
	scan func(rowScanner) error,
) error {
	return forEachKeyBatch(keys, func(batch []string, args []any) error {
		rows, err := db.QueryContext(
			ctx,
			statement(placeholderList(len(batch))),
			append(append([]any{}, leadingArgs...), args...)...,
		)
		if err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return fmt.Errorf("%s: scan: %w", operation, err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
		return nil
	})
}

// ConversationsByID returns the conversations with the given IDs, keyed by
// conversation ID: GetConversation for many IDs. IDs with no row are absent.
func (s *Store) ConversationsByID(conversationIDs []string) (map[string]Conversation, error) {
	result := make(map[string]Conversation, len(conversationIDs))
	err := queryKeyBatches(
		context.Background(), s.db, "list conversations by ID", conversationIDs,
		func(placeholders string) string {
			return "SELECT " + conversationColumns + `
			 FROM conversations
			 WHERE conversation_id IN (` + placeholders + `)`
		},
		nil,
		func(row rowScanner) error {
			conversation, err := scanConversation(row)
			if err != nil {
				return err
			}
			result[conversation.ConversationID] = conversation
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// IdentitiesByID returns the identities with the given IDs, keyed by identity
// ID: GetIdentity for many IDs. IDs with no row are absent.
func (s *Store) IdentitiesByID(identityIDs []string) (map[string]Identity, error) {
	result := make(map[string]Identity, len(identityIDs))
	err := queryKeyBatches(
		context.Background(), s.db, "list identities by ID", identityIDs,
		func(placeholders string) string {
			return "SELECT " + identityColumns + `
			 FROM identities
			 WHERE identity_id IN (` + placeholders + `)`
		},
		nil,
		func(row rowScanner) error {
			identity, err := scanIdentity(row)
			if err != nil {
				return err
			}
			result[identity.IdentityID] = identity
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ParticipantIdentity is one conversation participant row with the identity
// fields a roster renders: what ListParticipants and then GetIdentity on each
// participant read.
type ParticipantIdentity struct {
	ConversationID string
	IdentityID     string
	// ParticipantDisplayName is the participant row's own display name.
	ParticipantDisplayName string
	// IdentityFound is false when no identities row has IdentityID. The
	// composite foreign key forbids that; it is reported so a caller can keep
	// GetIdentity's not-found error rather than render an empty identity.
	IdentityFound       bool
	CanonicalValue      string
	IdentityDisplayName string
	IsSelf              bool
}

// ListParticipantIdentities returns every participant row (active or not) of
// each requested conversation joined to its identity, keyed by conversation
// ID and ordered by identity ID within a conversation, as ListParticipants
// orders them. Conversations with no participant rows are absent.
func (s *Store) ListParticipantIdentities(
	conversationIDs []string,
) (map[string][]ParticipantIdentity, error) {
	result := make(map[string][]ParticipantIdentity, len(conversationIDs))
	err := queryKeyBatches(
		context.Background(), s.db, "list participant identities", conversationIDs,
		func(placeholders string) string {
			return `
			SELECT
				cp.conversation_id,
				cp.identity_id,
				cp.display_name,
				i.identity_id IS NOT NULL,
				COALESCE(i.canonical_value, ''),
				COALESCE(i.display_name, ''),
				COALESCE(i.is_self, 0)
			FROM conversation_participants AS cp
			LEFT JOIN identities AS i ON i.identity_id = cp.identity_id
			WHERE cp.conversation_id IN (` + placeholders + `)
			ORDER BY cp.conversation_id, cp.identity_id`
		},
		nil,
		func(row rowScanner) error {
			var participant ParticipantIdentity
			if err := row.Scan(
				&participant.ConversationID,
				&participant.IdentityID,
				&participant.ParticipantDisplayName,
				&participant.IdentityFound,
				&participant.CanonicalValue,
				&participant.IdentityDisplayName,
				&participant.IsSelf,
			); err != nil {
				return err
			}
			result[participant.ConversationID] = append(result[participant.ConversationID], participant)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// LatestMessagesForConversations returns each requested conversation's newest
// message, keyed by conversation ID: ListMessagesByConversation(id, 0, "", 1)
// for many conversations. Each conversation's row is chosen by the same
// ORDER BY (occurred_at_ms DESC, message_id DESC) over
// messages_conversation_time_idx. Conversations without messages are absent.
func (r *MessageRepository) LatestMessagesForConversations(
	ctx context.Context,
	conversationIDs []string,
) (map[string]Message, error) {
	result := make(map[string]Message, len(conversationIDs))
	err := queryKeyBatches(
		ctx, r.store.db, "list latest messages for conversations", conversationIDs,
		func(placeholders string) string {
			values := strings.ReplaceAll(placeholders, "?", "(?)")
			return `
			WITH requested(conversation_id) AS (VALUES ` + values + `)
			SELECT ` + prefixedMessageColumns("m") + `
			FROM requested
			JOIN messages AS m ON m.message_id = (
				SELECT latest.message_id
				FROM messages AS latest
				WHERE latest.conversation_id = requested.conversation_id
				ORDER BY latest.occurred_at_ms DESC, latest.message_id DESC
				LIMIT 1
			)`
		},
		nil,
		func(row rowScanner) error {
			message, err := scanMessage(row)
			if err != nil {
				return err
			}
			result[message.ConversationID] = message
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListForDownload is GetForDownload for many messages at one ordinal: each
// message's attachment row joined to its owning account, keyed by message ID.
// Messages without an attachment at that ordinal are absent.
func (r *MessageAttachmentRepository) ListForDownload(
	ctx context.Context,
	messageIDs []string,
	ordinal int64,
) (map[string]MessageAttachment, error) {
	result := make(map[string]MessageAttachment, len(messageIDs))
	err := queryKeyBatches(
		ctx, r.store.db, "list message attachments for download", messageIDs,
		func(placeholders string) string {
			return `
			SELECT ` + downloadAttachmentColumns + `
			FROM message_attachments AS ma
			JOIN messages AS m ON m.message_id = ma.message_id
			WHERE ma.ordinal = ? AND ma.message_id IN (` + placeholders + `)`
		},
		[]any{ordinal},
		func(row rowScanner) error {
			attachment, err := scanDownloadAttachment(row)
			if err != nil {
				return err
			}
			result[attachment.MessageID] = attachment
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// LatestStatesForLocalMessages is LatestStateForLocalMessage for many local
// messages of one account: the state of each message's newest outbox row
// (created_at_ms, then outbox_id, descending), keyed by local message ID.
// Messages with no outbox row are absent.
func (r *OutboxRepository) LatestStatesForLocalMessages(
	ctx context.Context,
	accountID string,
	localMessageIDs []string,
) (map[string]OutboxState, error) {
	result := make(map[string]OutboxState, len(localMessageIDs))
	err := queryKeyBatches(
		ctx, r.store.db, fmt.Sprintf("outbox states for local messages (account %q)", accountID), localMessageIDs,
		func(placeholders string) string {
			return `
			SELECT local_message_id, state
			FROM outbox
			WHERE account_id = ? AND local_message_id IN (` + placeholders + `)
			ORDER BY local_message_id, created_at_ms DESC, outbox_id DESC`
		},
		[]any{accountID},
		func(row rowScanner) error {
			var localMessageID, state string
			if err := row.Scan(&localMessageID, &state); err != nil {
				return err
			}
			// Rows arrive newest first within each message: keep the first.
			if _, ok := result[localMessageID]; !ok {
				result[localMessageID] = OutboxState(state)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}
