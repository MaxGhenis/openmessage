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
	"github.com/maxghenis/openmessage/internal/v2keys"
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
//
// The only v2 conversation row the mirror writes is the one whose ID equals
// the legacy ID; it inserts or refreshes that row. An existing account keeps
// its metadata, and the account's existing local installation device is
// reused whatever its ID. When the legacy thread already maps to a v2 row
// under another ID (see mirrorNaturalKey), the call fails with
// sqlite.ErrConversationIdentityConflict and leaves that row unchanged. The
// check runs before the account and device bootstraps, so such a refusal
// writes nothing unless a concurrent writer creates the conflicting row
// mid-call; the conversation upsert is guarded either way.
func MirrorConversation(
	legacy *db.Store,
	v2 *sqlite.Store,
	legacyConversationID string,
) (accountID string, conversationID string, err error) {
	mirrored, err := mirrorConversation(legacy, v2, legacyConversationID)
	if err != nil {
		return "", "", err
	}
	return mirrored.accountID, mirrored.conversationID, nil
}

type mirroredConversation struct {
	accountID      string
	conversationID string
	deviceID       string
}

func mirrorConversation(
	legacy *db.Store,
	v2 *sqlite.Store,
	legacyConversationID string,
) (mirroredConversation, error) {
	if v2 == nil {
		return mirroredConversation{}, errors.New("mirror conversation: v2 store is nil")
	}
	accountID, err := AccountForConversation(legacy, legacyConversationID)
	if err != nil {
		return mirroredConversation{}, err
	}
	legacyConversationID = strings.TrimSpace(legacyConversationID)
	conversation, err := legacy.GetConversation(legacyConversationID)
	if err != nil {
		return mirroredConversation{}, fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}
	remoteID, err := mirrorNaturalKey(v2, accountID, legacyConversationID)
	if err != nil {
		return mirroredConversation{}, err
	}

	bridgeKey, displayName := accountBootstrap(accountID)
	nowMS := time.Now().UnixMilli()
	if _, err := v2.EnsureAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   bridgeKey,
		DisplayName: displayName,
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		return mirroredConversation{}, fmt.Errorf("mirror account %q: %w", accountID, err)
	}
	deviceID, err := ensureLocalDevice(v2, accountID, nowMS)
	if err != nil {
		return mirroredConversation{}, err
	}

	kind := sqlite.ConversationKindDirect
	if conversation.IsGroup {
		kind = sqlite.ConversationKindGroup
	}
	if err := v2.UpsertOwnedConversation(sqlite.Conversation{
		ConversationID:       legacyConversationID,
		AccountID:            accountID,
		RemoteConversationID: remoteID,
		Kind:                 kind,
		Title:                conversation.Name,
		NotificationMode:     sqlite.NotificationModeAll,
		IsFavorite:           conversation.IsFavorite,
		LastMessageAtMS:      max(conversation.LastMessageTS, 0),
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}); err != nil {
		if errors.Is(err, sqlite.ErrConversationIdentityConflict) {
			if owner, lookupErr := v2.GetConversationByRemote(accountID, remoteID); lookupErr == nil {
				return mirroredConversation{}, naturalKeyOwnedElsewhere(legacyConversationID, owner.ConversationID, err)
			}
		}
		return mirroredConversation{}, fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}

	mirrored, err := v2.GetConversationByRemote(accountID, remoteID)
	if err != nil {
		return mirroredConversation{}, fmt.Errorf("load mirrored conversation %q: %w", legacyConversationID, err)
	}
	if mirrored.ConversationID != legacyConversationID {
		return mirroredConversation{}, naturalKeyOwnedElsewhere(
			legacyConversationID,
			mirrored.ConversationID,
			sqlite.ErrConversationIdentityConflict,
		)
	}
	return mirroredConversation{
		accountID:      accountID,
		conversationID: mirrored.ConversationID,
		deviceID:       deviceID,
	}, nil
}

// mirrorNaturalKey returns the remote ID the mirror writes the conversation
// under. It fails with sqlite.ErrConversationIdentityConflict when the legacy
// thread already maps to a v2 row under another ID.
//
// That check looks the legacy ID up the way v2 reads resolve one
// (internal/v2read resolveConversationID): under every account, as the remote
// ID normalized for that account's platform. The migration routes a thread by
// its stored platform while the mirror routes by ID prefix, so a "signal:"
// thread stored as sms was migrated under the Google account. Creating a row
// under the legacy ID beside it would shadow that history for legacy-ID reads.
//
// The key is the migration's normalized form, so "signal:  +1650…" meets a
// migrated "signal:+1650…". The one exception is a row an earlier mirror wrote
// under this legacy ID and account with the raw legacy ID as its key: once
// nothing else holds the normalized key, that row keeps its key. A row under
// the legacy ID with any other key or account is refused, not adopted (ingest
// rewrites a stale Google binding's key to "displaced:…").
func mirrorNaturalKey(v2 *sqlite.Store, accountID, legacyConversationID string) (string, error) {
	bridgeKey, _ := accountBootstrap(accountID)
	normalized := v2keys.NormalizeRemoteConversationID(platformForBridgeKey(bridgeKey), legacyConversationID)

	accounts, err := v2.ListAccounts()
	if err != nil {
		return "", fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}
	type naturalKey struct{ accountID, remoteID string }
	keys := []naturalKey{{accountID, normalized}}
	seen := map[naturalKey]bool{keys[0]: true}
	for _, account := range accounts {
		key := naturalKey{
			account.AccountID,
			v2keys.NormalizeRemoteConversationID(platformForBridgeKey(account.BridgeKey), legacyConversationID),
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		owner, err := v2.GetConversationByRemote(key.accountID, key.remoteID)
		if errors.Is(err, sqlite.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
		}
		if owner.ConversationID != legacyConversationID {
			return "", naturalKeyOwnedElsewhere(
				legacyConversationID,
				owner.ConversationID,
				sqlite.ErrConversationIdentityConflict,
			)
		}
	}

	existing, err := v2.GetConversation(legacyConversationID)
	switch {
	case errors.Is(err, sqlite.ErrNotFound):
		return normalized, nil
	case err != nil:
		return "", fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	case existing.AccountID == accountID && existing.RemoteConversationID == normalized:
		return normalized, nil
	case existing.AccountID == accountID && existing.RemoteConversationID == legacyConversationID:
		return legacyConversationID, nil
	default:
		return "", fmt.Errorf(
			"mirror conversation %q: v2 conversation %q is keyed by (%q, %q): %w",
			legacyConversationID,
			existing.ConversationID,
			existing.AccountID,
			existing.RemoteConversationID,
			sqlite.ErrConversationIdentityConflict,
		)
	}
}

func naturalKeyOwnedElsewhere(legacyConversationID, ownerConversationID string, cause error) error {
	return fmt.Errorf(
		"mirror conversation %q: natural key belongs to v2 conversation %q: %w",
		legacyConversationID,
		ownerConversationID,
		cause,
	)
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
	mirrored, err := mirrorConversation(legacy, v2, legacyConversationID)
	if err != nil {
		return err
	}
	if err := v2.UpsertReadCursor(sqlite.ReadCursor{
		AccountID:         mirrored.accountID,
		DeviceID:          mirrored.deviceID,
		ConversationID:    mirrored.conversationID,
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

// accountBootstrap returns the bridge key and display name an account gets
// when the mirror is the first to create it. They match the live adapter
// bootstrap (cmd/v2stack.go liveAccountSpec) and the migration's account
// table, because v2 reads derive each conversation's platform from the bridge
// key (internal/v2read platformForBridgeKey maps "google_messages" to sms and
// passes unknown keys through, so "google" would read as platform "google").
func accountBootstrap(accountID string) (bridgeKey string, displayName string) {
	switch accountID {
	case whatsappAccountID:
		return "whatsmeow", "WhatsApp"
	case signalAccountID:
		return "signal_cli", "Signal"
	default:
		return "google_messages", "Google Messages"
	}
}

// platformForBridgeKey maps an account's bridge key to the migration's platform
// vocabulary, which v2keys.NormalizeRemoteConversationID keys on. It is the
// mapping internal/v2read and internal/ingest apply to stored bridge keys.
func platformForBridgeKey(bridgeKey string) string {
	switch bridgeKey = strings.TrimSpace(bridgeKey); bridgeKey {
	case "google_messages":
		return "sms"
	case "whatsmeow":
		return "whatsapp"
	case "signal_cli":
		return "signal"
	default:
		return bridgeKey
	}
}

// localDeviceID names the local device the mirror creates for an account that
// has none. It is account-scoped because devices.device_id is a global primary
// key, so one shared ID would collide across accounts. Before UpsertDevice
// refused cross-account writes, a shared ID let mirroring a second account take
// over the first account's device row whenever that row had no read cursors
// (with cursors, the read_cursors foreign key failed the write).
func localDeviceID(accountID string) string {
	return "local-primary:" + accountID
}

// ensureLocalDevice returns the ID of the account's local installation device,
// creating one only when the account has none. A migrated store already holds
// the account's current local device under a derived ID, and
// devices_current_local_uq rejects a second current one, so the mirror must
// not assume localDeviceID.
func ensureLocalDevice(v2 *sqlite.Store, accountID string, nowMS int64) (string, error) {
	device, err := v2.EnsureLocalInstallationDevice(context.Background(), sqlite.Device{
		DeviceID:    localDeviceID(accountID),
		AccountID:   accountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	})
	if err != nil {
		return "", fmt.Errorf("ensure local device for account %q: %w", accountID, err)
	}
	return device.DeviceID, nil
}
