// Package reconcile repairs gaps between legacy and v2 local stores.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	signalPlatform  = "signal"
	signalAccountID = "signal-primary"
	signalBridgeKey = "signal_cli"

	skipMissingConversationID = "missing_conversation_id"
	skipMissingMessageKey     = "missing_message_key"
	skipNonPositiveTimestamp  = "non_positive_timestamp"
	skipInvalidSenderIdentity = "invalid_sender_identity"
)

// Options supplies the two local stores and bounds one Signal reconciliation.
type Options struct {
	Legacy  *db.Store
	V2      *sqlite.Store
	SinceMS int64
	DryRun  bool
	Logger  zerolog.Logger
}

// Report is the machine-readable evidence emitted by one reconciliation.
// During a dry run, MessagesImported and ConversationsCreated are planned
// writes; no store mutation has occurred.
type Report struct {
	DryRun                 bool           `json:"dry_run"`
	SinceMS                int64          `json:"since_ms,omitempty"`
	ConversationsScanned   int            `json:"conversations_scanned"`
	ConversationsCreated   int            `json:"conversations_created"`
	MessagesScanned        int            `json:"messages_scanned"`
	MessagesImported       int            `json:"messages_imported"`
	MessagesAlreadyPresent int            `json:"messages_already_present"`
	MediaDeferred          int            `json:"media_deferred"`
	Skipped                int            `json:"skipped"`
	SkipReasons            map[string]int `json:"skip_reasons"`
}

// Signal tops up v2 from legacy Signal history without touching transport,
// credential, or network state. Each write uses the same natural keys as live
// ingest, so interrupted and repeated runs converge.
func Signal(ctx context.Context, opts Options) (Report, error) {
	report := Report{
		DryRun:      opts.DryRun,
		SinceMS:     opts.SinceMS,
		SkipReasons: make(map[string]int),
	}
	if ctx == nil {
		return report, fmt.Errorf("reconcile Signal: context is nil")
	}
	if opts.Legacy == nil {
		return report, fmt.Errorf("reconcile Signal: legacy store is nil")
	}
	if opts.V2 == nil {
		return report, fmt.Errorf("reconcile Signal: v2 store is nil")
	}
	if opts.SinceMS < 0 {
		return report, fmt.Errorf("reconcile Signal: since timestamp %d is negative", opts.SinceMS)
	}
	account, err := opts.V2.GetAccount(signalAccountID)
	if err != nil {
		return report, fmt.Errorf("reconcile Signal: resolve Signal account: %w", err)
	}
	if account.BridgeKey != signalBridgeKey {
		return report, fmt.Errorf(
			"reconcile Signal: account %q has bridge key %q, want %q",
			signalAccountID,
			account.BridgeKey,
			signalBridgeKey,
		)
	}

	messages, err := sqlite.NewMessageRepository(opts.V2, time.Now)
	if err != nil {
		return report, fmt.Errorf("reconcile Signal: create message repository: %w", err)
	}
	legacyConversations, err := opts.Legacy.ListConversationsByPlatform(
		signalPlatform,
		math.MaxInt,
	)
	if err != nil {
		return report, fmt.Errorf("reconcile Signal: list legacy conversations: %w", err)
	}

	nowMS := time.Now().UnixMilli()
	if nowMS <= 0 {
		nowMS = 1
	}
	knownMessages := make(map[string]struct{})

	for _, legacyConversation := range legacyConversations {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("reconcile Signal: %w", err)
		}
		report.ConversationsScanned++

		legacyMessages, err := opts.Legacy.GetMessagesByConversationsRange(
			[]string{legacyConversation.ConversationID},
			opts.SinceMS,
			0,
			math.MaxInt,
		)
		if err != nil {
			return report, fmt.Errorf(
				"reconcile Signal conversation %q: list legacy messages: %w",
				legacyConversation.ConversationID,
				err,
			)
		}

		remoteConversationID := v2keys.NormalizeRemoteConversationID(
			signalPlatform,
			legacyConversation.ConversationID,
		)
		if strings.TrimSpace(remoteConversationID) == "" {
			for _, legacyMessage := range legacyMessages {
				report.MessagesScanned++
				if strings.TrimSpace(legacyMessage.MediaID) != "" {
					report.MediaDeferred++
				}
				report.skip(skipMissingConversationID)
			}
			continue
		}

		conversation, created, err := resolveConversation(
			opts,
			legacyConversation,
			legacyMessages,
			remoteConversationID,
			nowMS,
		)
		if err != nil {
			return report, err
		}
		if created {
			report.ConversationsCreated++
		}

		if signalConversationKind(remoteConversationID) == sqlite.ConversationKindDirect {
			if err := ensureDirectPeer(
				opts,
				conversation,
				remoteConversationID,
				legacyConversation.Name,
				conversation.CreatedAtMS,
			); err != nil {
				return report, fmt.Errorf(
					"reconcile Signal conversation %q: ensure direct peer: %w",
					legacyConversation.ConversationID,
					err,
				)
			}
		}

		remoteByLegacyID := make(map[string]string, len(legacyMessages))
		for _, legacyMessage := range legacyMessages {
			remoteByLegacyID[legacyMessage.MessageID] = deriveRemoteMessageID(legacyMessage)
		}
		for _, legacyMessage := range legacyMessages {
			if err := ctx.Err(); err != nil {
				return report, fmt.Errorf("reconcile Signal: %w", err)
			}
			report.MessagesScanned++
			if strings.TrimSpace(legacyMessage.MediaID) != "" {
				report.MediaDeferred++
			}
			if legacyMessage.TimestampMS <= 0 {
				report.skip(skipNonPositiveTimestamp)
				continue
			}

			remoteMessageID := deriveRemoteMessageID(legacyMessage)
			if strings.TrimSpace(remoteMessageID) == "" {
				report.skip(skipMissingMessageKey)
				continue
			}
			naturalKey := conversation.ConversationID + "\x1f" + remoteMessageID
			alreadyPresent := false
			for _, candidate := range existingRemoteMessageIDs(
				legacyMessage,
				remoteConversationID,
				remoteMessageID,
			) {
				candidateKey := conversation.ConversationID + "\x1f" + candidate
				if _, known := knownMessages[candidateKey]; known {
					alreadyPresent = true
					break
				}
				_, lookupErr := messages.GetMessageByRemote(
					ctx,
					signalAccountID,
					conversation.ConversationID,
					candidate,
				)
				switch {
				case lookupErr == nil:
					knownMessages[candidateKey] = struct{}{}
					alreadyPresent = true
				case !errors.Is(lookupErr, sqlite.ErrNotFound):
					return report, fmt.Errorf(
						"reconcile Signal message %q: check v2 natural key: %w",
						legacyMessage.MessageID,
						lookupErr,
					)
				}
				if alreadyPresent {
					break
				}
			}
			if alreadyPresent {
				knownMessages[naturalKey] = struct{}{}
				report.MessagesAlreadyPresent++
				if !opts.DryRun {
					if err := opts.V2.BumpConversationRecency(
						conversation.ConversationID,
						legacyMessage.TimestampMS,
					); err != nil {
						return report, fmt.Errorf(
							"reconcile Signal message %q: bump conversation recency: %w",
							legacyMessage.MessageID,
							err,
						)
					}
				}
				continue
			}

			senderIdentityID, usable, err := resolveInboundSender(
				opts,
				legacyMessage,
				legacyMessage.TimestampMS,
			)
			if err != nil {
				return report, fmt.Errorf(
					"reconcile Signal message %q: resolve sender: %w",
					legacyMessage.MessageID,
					err,
				)
			}
			if !usable {
				report.skip(skipInvalidSenderIdentity)
				continue
			}
			replyToRemoteID, err := resolveReplyRemoteID(
				opts.Legacy,
				legacyMessage,
				remoteByLegacyID,
			)
			if err != nil {
				return report, fmt.Errorf(
					"reconcile Signal message %q: resolve reply target: %w",
					legacyMessage.MessageID,
					err,
				)
			}

			direction := sqlite.MessageDirectionIncoming
			if legacyMessage.IsFromMe {
				direction = sqlite.MessageDirectionOutgoing
			}
			projection := sqlite.MessageProjection{Message: sqlite.Message{
				MessageID: v2keys.DeriveID(
					"message",
					signalAccountID,
					remoteConversationID+"\x1f"+remoteMessageID,
				),
				ConversationID:   conversation.ConversationID,
				AccountID:        signalAccountID,
				RemoteMessageID:  remoteMessageID,
				SenderIdentityID: senderIdentityID,
				Direction:        direction,
				Body:             legacyMessage.Body,
				ReplyToRemoteID:  replyToRemoteID,
				State:            sqlite.MessageStateActive,
				OccurredAtMS:     legacyMessage.TimestampMS,
			}}
			if !opts.DryRun {
				if err := messages.ImportMessage(ctx, projection); err != nil {
					return report, fmt.Errorf(
						"reconcile Signal message %q: import: %w",
						legacyMessage.MessageID,
						err,
					)
				}
				knownMessages[naturalKey] = struct{}{}
				report.MessagesImported++
				if err := opts.V2.BumpConversationRecency(
					conversation.ConversationID,
					legacyMessage.TimestampMS,
				); err != nil {
					return report, fmt.Errorf(
						"reconcile Signal message %q: bump conversation recency: %w",
						legacyMessage.MessageID,
						err,
					)
				}
			} else {
				knownMessages[naturalKey] = struct{}{}
				report.MessagesImported++
			}
		}
	}

	opts.Logger.Debug().
		Int("conversations_scanned", report.ConversationsScanned).
		Int("messages_scanned", report.MessagesScanned).
		Int("messages_imported", report.MessagesImported).
		Int("messages_already_present", report.MessagesAlreadyPresent).
		Bool("dry_run", opts.DryRun).
		Msg("Signal reconciliation complete")
	return report, nil
}

func (report *Report) skip(reason string) {
	report.Skipped++
	report.SkipReasons[reason]++
}

func resolveConversation(
	opts Options,
	legacyConversation *db.Conversation,
	legacyMessages []*db.Message,
	remoteConversationID string,
	nowMS int64,
) (sqlite.Conversation, bool, error) {
	conversation, err := opts.V2.GetConversationByRemote(
		signalAccountID,
		remoteConversationID,
	)
	if err == nil {
		return conversation, false, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, false, fmt.Errorf(
			"reconcile Signal conversation %q: resolve v2 conversation: %w",
			legacyConversation.ConversationID,
			err,
		)
	}

	createdAtMS, lastMessageAtMS := conversationTimes(
		legacyConversation,
		legacyMessages,
		nowMS,
	)
	conversation = sqlite.Conversation{
		ConversationID: v2keys.DeriveID(
			"conversation",
			signalAccountID,
			remoteConversationID,
		),
		AccountID:            signalAccountID,
		RemoteConversationID: remoteConversationID,
		Kind:                 signalConversationKind(remoteConversationID),
		Title:                legacyConversation.Name,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      lastMessageAtMS,
		MetadataJSON:         "{}",
		CreatedAtMS:          createdAtMS,
		UpdatedAtMS:          maxInt64(createdAtMS, lastMessageAtMS),
	}
	if opts.DryRun {
		return conversation, true, nil
	}
	if err := opts.V2.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, false, fmt.Errorf(
			"reconcile Signal conversation %q: create v2 conversation: %w",
			legacyConversation.ConversationID,
			err,
		)
	}
	// UpsertConversation preserves the first local primary key on a natural-key
	// conflict. Resolve that effective row before importing child messages.
	effective, err := opts.V2.GetConversationByRemote(
		signalAccountID,
		remoteConversationID,
	)
	if err != nil {
		return sqlite.Conversation{}, false, fmt.Errorf(
			"reconcile Signal conversation %q: reload created conversation: %w",
			legacyConversation.ConversationID,
			err,
		)
	}
	return effective, true, nil
}

func signalConversationKind(remoteConversationID string) sqlite.ConversationKind {
	if strings.HasPrefix(remoteConversationID, "signal-group:") {
		return sqlite.ConversationKindGroup
	}
	return sqlite.ConversationKindDirect
}

func ensureDirectPeer(
	opts Options,
	conversation sqlite.Conversation,
	remoteConversationID string,
	displayName string,
	atMS int64,
) error {
	raw := strings.TrimSpace(strings.TrimPrefix(remoteConversationID, "signal:"))
	key, err := v2keys.IdentityKey(signalAccountID, signalPlatform, raw)
	if err != nil {
		return err
	}
	identityID, err := resolveIdentity(
		opts,
		key,
		raw,
		displayName,
		atMS,
	)
	if err != nil {
		return err
	}
	identity, err := opts.V2.GetIdentity(identityID)
	if err == nil && identity.IsSelf {
		return nil
	}
	if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		return err
	}
	if opts.DryRun {
		return nil
	}
	_, err = opts.V2.EnsureConversationParticipant(sqlite.ConversationParticipant{
		AccountID:      signalAccountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     identityID,
		Role:           sqlite.ParticipantRoleMember,
		DisplayName:    strings.TrimSpace(displayName),
		IsActive:       true,
	})
	return err
}

func resolveInboundSender(
	opts Options,
	message *db.Message,
	atMS int64,
) (*string, bool, error) {
	if message.IsFromMe {
		return nil, true, nil
	}
	raw := strings.TrimSpace(message.SenderNumber)
	// Live ingest preserves inbound messages whose transport supplies no usable
	// sender, so historical repair degrades to a null sender in the same way.
	if raw == "" {
		return nil, true, nil
	}
	key, err := v2keys.IdentityKey(signalAccountID, signalPlatform, raw)
	if err != nil {
		return nil, false, nil
	}
	identityID, err := resolveIdentity(
		opts,
		key,
		raw,
		message.SenderName,
		atMS,
	)
	if err != nil {
		return nil, false, err
	}
	return &identityID, true, nil
}

func resolveIdentity(
	opts Options,
	key v2keys.Identity,
	raw string,
	displayName string,
	atMS int64,
) (string, error) {
	identity, err := opts.V2.GetIdentityByCanonical(
		key.AccountID,
		sqlite.IdentityKind(key.Kind),
		key.Canonical,
	)
	if err == nil {
		return identity.IdentityID, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return "", err
	}

	identityID := v2keys.DeriveID(
		"identity",
		key.AccountID,
		key.Kind+"\x1f"+key.Canonical,
	)
	if opts.DryRun {
		return identityID, nil
	}
	if err := opts.V2.UpsertIdentity(sqlite.Identity{
		IdentityID:     identityID,
		AccountID:      key.AccountID,
		Kind:           sqlite.IdentityKind(key.Kind),
		CanonicalValue: key.Canonical,
		RawValue:       strings.TrimSpace(raw),
		DisplayName:    strings.TrimSpace(displayName),
		MetadataJSON:   "{}",
		CreatedAtMS:    atMS,
		UpdatedAtMS:    atMS,
	}); err != nil {
		return "", err
	}
	effective, err := opts.V2.GetIdentityByCanonical(
		key.AccountID,
		sqlite.IdentityKind(key.Kind),
		key.Canonical,
	)
	if err != nil {
		return "", err
	}
	return effective.IdentityID, nil
}

func resolveReplyRemoteID(
	legacy *db.Store,
	message *db.Message,
	remoteByLegacyID map[string]string,
) (*string, error) {
	replyID := strings.TrimSpace(message.ReplyToID)
	if replyID == "" {
		return nil, nil
	}
	remoteID := remoteByLegacyID[replyID]
	if strings.TrimSpace(remoteID) == "" {
		target, err := legacy.GetMessageByID(replyID)
		if err != nil {
			return nil, err
		}
		if target != nil {
			remoteID = deriveRemoteMessageID(target)
		}
	}
	if strings.TrimSpace(remoteID) == "" {
		remoteID = stripPlatformPrefix(replyID)
	}
	if strings.TrimSpace(remoteID) == "" {
		return nil, nil
	}
	return &remoteID, nil
}

func deriveRemoteMessageID(message *db.Message) string {
	if message.SourceID != "" {
		return message.SourceID
	}
	return stripPlatformPrefix(message.MessageID)
}

// existingRemoteMessageIDs keeps the legacy source ID canonical while
// recognizing an older live projection that may have retained Signal's bare
// outgoing timestamp before a local alias existed to converge onto.
func existingRemoteMessageIDs(
	message *db.Message,
	remoteConversationID string,
	canonical string,
) []string {
	keys := []string{canonical}
	if !message.IsFromMe || message.TimestampMS <= 0 {
		return keys
	}
	if canonical != v2keys.SignalLocalAlias(remoteConversationID, message.TimestampMS) {
		return keys
	}
	bareTimestamp := strconv.FormatInt(message.TimestampMS, 10)
	if bareTimestamp != canonical {
		keys = append(keys, bareTimestamp)
	}
	return keys
}

func stripPlatformPrefix(value string) string {
	for _, prefix := range []string{"whatsapp:", "signal:", "gchat:", "imessage:"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return value
}

func conversationTimes(
	conversation *db.Conversation,
	messages []*db.Message,
	fallbackMS int64,
) (int64, int64) {
	createdAtMS := int64(0)
	lastMessageAtMS := conversation.LastMessageTS
	for _, message := range messages {
		if message.TimestampMS <= 0 {
			continue
		}
		if createdAtMS == 0 || message.TimestampMS < createdAtMS {
			createdAtMS = message.TimestampMS
		}
		if message.TimestampMS > lastMessageAtMS {
			lastMessageAtMS = message.TimestampMS
		}
	}
	if createdAtMS == 0 && conversation.LastMessageTS > 0 {
		createdAtMS = conversation.LastMessageTS
	}
	if createdAtMS == 0 {
		createdAtMS = fallbackMS
	}
	if lastMessageAtMS < 0 {
		lastMessageAtMS = 0
	}
	return createdAtMS, lastMessageAtMS
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
