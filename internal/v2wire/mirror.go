package v2wire

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	googleAccountID   = "google-primary"
	whatsappAccountID = "whatsapp-primary"
	signalAccountID   = "signal-primary"
)

var (
	// ErrPlatformNotSendable marks conversations whose legacy platform is not
	// backed by one of the live Wave-3 dispatch adapters.
	ErrPlatformNotSendable = errors.New("conversation platform is not sendable")

	// ErrReplyTargetUnavailable marks legacy reply targets that cannot be
	// represented with a stable transport remote ID.
	ErrReplyTargetUnavailable = errors.New("reply target is unavailable")
)

// AccountForConversation applies the same routing predicates as the legacy
// HTTP send path. Prefix routing intentionally wins over stored metadata.
func AccountForConversation(legacy *db.Store, legacyConversationID string) (string, error) {
	if legacy == nil {
		return "", errors.New("route conversation: legacy store is nil")
	}
	legacyConversationID = strings.TrimSpace(legacyConversationID)
	if legacyConversationID == "" {
		return "", errors.New("route conversation: conversation id is empty")
	}
	if strings.HasPrefix(legacyConversationID, "whatsapp:") {
		return whatsappAccountID, nil
	}
	if strings.HasPrefix(legacyConversationID, "signal:") ||
		strings.HasPrefix(legacyConversationID, "signal-group:") {
		return signalAccountID, nil
	}

	conversation, err := legacy.GetConversation(legacyConversationID)
	if err != nil {
		return "", fmt.Errorf("route conversation %q: %w", legacyConversationID, err)
	}
	switch conversation.SourcePlatform {
	case "whatsapp":
		return whatsappAccountID, nil
	case "signal":
		return signalAccountID, nil
	case "", "sms":
		return googleAccountID, nil
	default:
		return "", fmt.Errorf(
			"%w: conversation %q uses platform %q",
			ErrPlatformNotSendable,
			legacyConversationID,
			conversation.SourcePlatform,
		)
	}
}

// MirrorConversation idempotently creates the v2 account, local device, and
// conversation needed by the outbox. Conversation identity deliberately stays
// byte-for-byte equal to the legacy ID consumed by the live adapters.
func MirrorConversation(
	legacy *db.Store,
	v2 *sqlite.Store,
	legacyConversationID string,
) (accountID string, conversationID string, err error) {
	if v2 == nil {
		return "", "", errors.New("mirror conversation: v2 store is nil")
	}
	accountID, err = AccountForConversation(legacy, legacyConversationID)
	if err != nil {
		return "", "", err
	}
	legacyConversationID = strings.TrimSpace(legacyConversationID)
	conversation, err := legacy.GetConversation(legacyConversationID)
	if err != nil {
		return "", "", fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}

	bridgeKey := accountBridgeKey(accountID)
	nowMS := time.Now().UnixMilli()
	if err := v2.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   bridgeKey,
		DisplayName: accountID,
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		return "", "", fmt.Errorf("mirror account %q: %w", accountID, err)
	}
	if err := ensureLocalDevice(v2, accountID, nowMS); err != nil {
		return "", "", err
	}

	kind := sqlite.ConversationKindDirect
	if conversation.IsGroup {
		kind = sqlite.ConversationKindGroup
	}
	if err := v2.UpsertConversation(sqlite.Conversation{
		ConversationID:       legacyConversationID,
		AccountID:            accountID,
		RemoteConversationID: legacyConversationID,
		Kind:                 kind,
		Title:                conversation.Name,
		NotificationMode:     sqlite.NotificationModeAll,
		IsFavorite:           conversation.IsFavorite,
		LastMessageAtMS:      max(conversation.LastMessageTS, 0),
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		return "", "", fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}

	mirrored, err := v2.GetConversationByRemote(accountID, legacyConversationID)
	if err != nil {
		return "", "", fmt.Errorf("load mirrored conversation %q: %w", legacyConversationID, err)
	}
	if mirrored.ConversationID != legacyConversationID {
		return "", "", fmt.Errorf(
			"mirror conversation %q: natural key belongs to v2 conversation %q",
			legacyConversationID,
			mirrored.ConversationID,
		)
	}
	return accountID, mirrored.ConversationID, nil
}

// MirrorReplyTarget creates the minimum normalized v2 message needed for a
// reply submission. It deliberately does not attempt to synthesize sender
// identities; Wave-4 ingest owns that richer projection.
func MirrorReplyTarget(
	legacy *db.Store,
	v2 *sqlite.Store,
	legacyMessageID string,
) (string, error) {
	if legacy == nil {
		return "", errors.New("mirror reply target: legacy store is nil")
	}
	if v2 == nil {
		return "", errors.New("mirror reply target: v2 store is nil")
	}
	legacyMessageID = strings.TrimSpace(legacyMessageID)
	if legacyMessageID == "" {
		return "", fmt.Errorf("%w: message id is empty", ErrReplyTargetUnavailable)
	}
	target, err := legacy.GetMessageByID(legacyMessageID)
	if err != nil {
		return "", fmt.Errorf("load legacy reply target %q: %w", legacyMessageID, err)
	}
	if target == nil {
		return "", fmt.Errorf("%w: legacy message %q does not exist", ErrReplyTargetUnavailable, legacyMessageID)
	}

	accountID, conversationID, err := MirrorConversation(legacy, v2, target.ConversationID)
	if err != nil {
		return "", err
	}
	remoteMessageID, err := replyRemoteID(accountID, target)
	if err != nil {
		return "", err
	}
	if target.TimestampMS <= 0 {
		return "", fmt.Errorf(
			"%w: legacy message %q has no timestamp",
			ErrReplyTargetUnavailable,
			legacyMessageID,
		)
	}

	ctx := context.Background()
	repository, err := sqlite.NewMessageRepository(v2, time.Now)
	if err != nil {
		return "", fmt.Errorf("mirror reply target %q: %w", legacyMessageID, err)
	}
	if existing, err := repository.GetMessageByRemote(
		ctx,
		accountID,
		conversationID,
		remoteMessageID,
	); err == nil {
		return existing.MessageID, nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return "", fmt.Errorf("load mirrored reply target %q: %w", legacyMessageID, err)
	}

	digest := sha256.Sum256([]byte(accountID + "\x00" + conversationID + "\x00" + legacyMessageID))
	key := fmt.Sprintf("%x", digest[:])
	messageID := "legacy-reply:" + key
	inboxID := "legacy-reply-inbox:" + key
	effectiveInboxID, err := repository.AppendInbox(ctx, sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    accountID,
		Generation:   0,
		DedupeKey:    inboxID,
		Codec:        "legacy.reply",
		CodecVersion: 1,
		Payload:      []byte("{}"),
	})
	if err != nil {
		return "", fmt.Errorf("append mirrored reply target %q: %w", legacyMessageID, err)
	}
	direction := sqlite.MessageDirectionIncoming
	if target.IsFromMe {
		direction = sqlite.MessageDirectionOutgoing
	}
	if err := repository.ProjectMessage(ctx, sqlite.MessageProjection{
		InboxID: effectiveInboxID,
		Message: sqlite.Message{
			MessageID:       messageID,
			ConversationID:  conversationID,
			AccountID:       accountID,
			RemoteMessageID: remoteMessageID,
			Direction:       direction,
			Body:            target.Body,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    target.TimestampMS,
		},
	}); err != nil {
		return "", fmt.Errorf("project mirrored reply target %q: %w", legacyMessageID, err)
	}
	mirrored, err := repository.GetMessageByRemote(ctx, accountID, conversationID, remoteMessageID)
	if err != nil {
		return "", fmt.Errorf("load projected reply target %q: %w", legacyMessageID, err)
	}
	return mirrored.MessageID, nil
}

// MirrorReadCursor dual-writes legacy mark-read state into the v2 canonical
// store without broadcasting a transport read receipt.
func MirrorReadCursor(
	ctx context.Context,
	legacy *db.Store,
	v2 *sqlite.Store,
	legacyConversationID string,
	atMS int64,
) error {
	if ctx == nil {
		return errors.New("mirror read cursor: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mirror read cursor: %w", err)
	}
	if atMS <= 0 {
		return errors.New("mirror read cursor: timestamp must be positive")
	}
	accountID, conversationID, err := MirrorConversation(legacy, v2, legacyConversationID)
	if err != nil {
		return err
	}
	if err := v2.UpsertReadCursor(sqlite.ReadCursor{
		AccountID:         accountID,
		DeviceID:          localDeviceID(accountID),
		ConversationID:    conversationID,
		LastReadMessageID: nil,
		LastReadAtMS:      atMS,
		UpdatedAtMS:       atMS,
	}); err != nil {
		return fmt.Errorf("mirror read cursor for conversation %q: %w", legacyConversationID, err)
	}
	return nil
}

func replyRemoteID(accountID string, target *db.Message) (string, error) {
	switch accountID {
	case googleAccountID:
		if remoteID := strings.TrimSpace(target.MessageID); remoteID != "" {
			return remoteID, nil
		}
	case whatsappAccountID:
		if remoteID := strings.TrimSpace(target.SourceID); remoteID != "" {
			return remoteID, nil
		}
	case signalAccountID:
		legacyID := strings.TrimSpace(target.MessageID)
		if strings.HasPrefix(legacyID, "signal:local:") {
			break
		}
		if strings.HasPrefix(legacyID, "signal:") {
			timestamp := strings.TrimPrefix(legacyID, "signal:")
			if parsed, err := strconv.ParseInt(timestamp, 10, 64); err == nil && parsed > 0 {
				// The design document says the v2 remote ID should be the bare
				// timestamp. The merged Signal adapter, however, forwards this ID
				// to legacy signalQuoteArgs, which resolves it with GetMessageByID.
				// Retaining the full legacy ID is therefore required until that
				// forbidden transport seam is changed in a later wave.
				return legacyID, nil
			}
		}
	}
	return "", fmt.Errorf(
		"%w: legacy message %q has no stable remote id",
		ErrReplyTargetUnavailable,
		target.MessageID,
	)
}

func accountBridgeKey(accountID string) string {
	switch accountID {
	case whatsappAccountID:
		return "whatsapp"
	case signalAccountID:
		return "signal"
	default:
		return "google"
	}
}

// AccountIDForPlatform maps a send platform key ("sms", "whatsapp",
// "signal") to the primary account ID the daemon registers adapters under.
// Empty for unknown platforms.
func AccountIDForPlatform(platform string) string {
	switch platform {
	case "sms", "rcs":
		return googleAccountID
	case "whatsapp":
		return whatsappAccountID
	case "signal":
		return signalAccountID
	default:
		return ""
	}
}

// localDeviceID is account-scoped because devices.device_id is a global
// primary key. A constant ID lets mirroring a second account steal the first
// account's device row and invalidates that account's read-cursor foreign key.
func localDeviceID(accountID string) string {
	return "local-primary:" + accountID
}

func ensureLocalDevice(v2 *sqlite.Store, accountID string, nowMS int64) error {
	if err := v2.UpsertDevice(sqlite.Device{
		DeviceID:    localDeviceID(accountID),
		AccountID:   accountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		return fmt.Errorf("ensure local device for account %q: %w", accountID, err)
	}
	return nil
}
