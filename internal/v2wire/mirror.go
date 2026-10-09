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

// MirrorConversation idempotently resolves the v2 account, local device, and
// conversation needed by the outbox, creating only what is missing.
//
// A thread's v2 identity is its natural key (account_id,
// remote_conversation_id), with the legacy conversation ID as the remote ID.
// The dispatcher addresses the transport through remote_conversation_id alone,
// and the legacy visibility projector maps a v2 conversation back to its
// legacy thread through the same column, so the v2 conversation_id itself is
// free to differ from the legacy ID. When no v2 row holds the natural key, the
// mirror creates one keyed by the legacy ID. When a row already holds it under
// another ID (a migrated store keys conversations by v2keys.DeriveID hashes,
// and v2 ingest mints the same hashes), the mirror adopts that row and returns
// its ID.
//
// Rows the mirror did not create are never rewritten. An existing account
// keeps its metadata, the account's existing local installation device is
// reused whatever its ID, and an adopted conversation is returned as stored.
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

// beforeOwnedConversationUpsert runs between the mirror's natural-key lookup
// and its upsert. Tests replace it to take the key from another writer there.
var beforeOwnedConversationUpsert = func() {}

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

	resolved := func(row sqlite.Conversation) mirroredConversation {
		return mirroredConversation{
			accountID:      accountID,
			conversationID: row.ConversationID,
			deviceID:       deviceID,
		}
	}
	// Look before upserting so that adopting a row, the common case on a
	// migrated store, takes no SQLite write lock.
	owner, err := v2.GetConversationByRemote(accountID, legacyConversationID)
	switch {
	case err == nil && owner.ConversationID != legacyConversationID:
		return resolved(owner), nil
	case err != nil && !errors.Is(err, sqlite.ErrNotFound):
		return mirroredConversation{}, fmt.Errorf("load v2 conversation for %q: %w", legacyConversationID, err)
	}
	beforeOwnedConversationUpsert()

	kind := sqlite.ConversationKindDirect
	if conversation.IsGroup {
		kind = sqlite.ConversationKindGroup
	}
	if err := v2.UpsertOwnedConversation(sqlite.Conversation{
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
		// Another writer (v2 ingest) can take the natural key between the
		// lookup and the upsert, which then writes nothing. Adopt its row.
		if errors.Is(err, sqlite.ErrConversationIdentityConflict) {
			if owner, lookupErr := v2.GetConversationByRemote(accountID, legacyConversationID); lookupErr == nil &&
				owner.ConversationID != legacyConversationID {
				return resolved(owner), nil
			}
		}
		return mirroredConversation{}, fmt.Errorf("mirror conversation %q: %w", legacyConversationID, err)
	}

	mirrored, err := v2.GetConversationByRemote(accountID, legacyConversationID)
	if err != nil {
		return mirroredConversation{}, fmt.Errorf("load mirrored conversation %q: %w", legacyConversationID, err)
	}
	return resolved(mirrored), nil
}

// MirrorReplyTarget returns the v2 message a reply submission quotes. It reuses
// the message the conversation already holds under the target's remote ID,
// which on a migrated store or with v2 ingest running is the migrated or
// ingested copy, and otherwise projects the minimum normalized message under
// that same remote ID, so a later ingest or history import of the message
// lands on the same natural key instead of adding a second copy. It
// deliberately does not attempt to synthesize sender identities; Wave-4 ingest
// owns that richer projection.
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
	ids, err := replyTargetRemoteIDs(accountID, target)
	if err != nil {
		return "", err
	}
	remoteMessageID := ids.remote
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
	lookup := func(remoteID string) (sqlite.Message, bool, error) {
		existing, err := repository.GetMessageByRemote(ctx, accountID, conversationID, remoteID)
		if errors.Is(err, sqlite.ErrNotFound) {
			return sqlite.Message{}, false, nil
		}
		if err != nil {
			return sqlite.Message{}, false, fmt.Errorf("load mirrored reply target %q: %w", legacyMessageID, err)
		}
		return existing, true, nil
	}
	for _, candidate := range append([]string{remoteMessageID}, ids.earlier...) {
		existing, found, err := lookup(candidate)
		if err != nil {
			return "", err
		}
		if found {
			return existing.MessageID, nil
		}
	}
	for _, unquotable := range ids.unquotable {
		existing, found, err := lookup(unquotable)
		if err != nil {
			return "", err
		}
		if found {
			return "", fmt.Errorf(
				"%w: v2 holds legacy message %q as %q under remote id %q, which the transport cannot quote",
				ErrReplyTargetUnavailable,
				legacyMessageID,
				existing.MessageID,
				unquotable,
			)
		}
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

// replyTargetIDs are the v2 remote message IDs a legacy reply target can have.
type replyTargetIDs struct {
	// remote is the ID the transport quotes, and the one the migration
	// (deriveRemoteMessageID) and the v2 ingest decoders write for the message,
	// so the mirror finds their copy instead of adding a second one.
	remote string
	// earlier are IDs the mirror stored for the same target before; a copy
	// under one of them is reused.
	earlier []string
	// unquotable are IDs the migration may have stored the message under that
	// the transport cannot quote. A copy under one of them makes the target
	// unavailable rather than adding a second copy under remote.
	unquotable []string
}

func replyTargetRemoteIDs(accountID string, target *db.Message) (replyTargetIDs, error) {
	switch accountID {
	case googleAccountID:
		if remoteID := strings.TrimSpace(target.MessageID); remoteID != "" {
			// The Google transport quotes the legacy message ID, which is
			// Google's own ID and what v2 ingest stores. The migration prefers
			// source_id, which no live Google writer sets.
			ids := replyTargetIDs{remote: remoteID}
			if sourceID := strings.TrimSpace(target.SourceID); sourceID != "" && sourceID != remoteID {
				ids.unquotable = []string{sourceID}
			}
			return ids, nil
		}
	case whatsappAccountID:
		if remoteID := strings.TrimSpace(target.SourceID); remoteID != "" {
			return replyTargetIDs{remote: remoteID}, nil
		}
	case signalAccountID:
		legacyID := strings.TrimSpace(target.MessageID)
		if strings.HasPrefix(legacyID, "signal:local:") {
			break
		}
		if timestamp, ok := strings.CutPrefix(legacyID, "signal:"); ok {
			sourceID := strings.TrimSpace(target.SourceID)
			if parsed, err := strconv.ParseInt(timestamp, 10, 64); err == nil && parsed > 0 &&
				(sourceID == "" || sourceID == timestamp) {
				// v2 keys a Signal message by its legacy ID without the "signal:"
				// prefix: the migration writes the source ID, and for a sent
				// message the sync-message decoder writes the same bare timestamp.
				// The Signal transport quotes a bare ID by restoring the prefix
				// (signallive signalReplyTarget). Before it could, the mirror
				// stored the full legacy ID, which sat beside the migrated or
				// ingested copy as a second message.
				return replyTargetIDs{remote: timestamp, earlier: []string{legacyID}}, nil
			}
		}
	}
	return replyTargetIDs{}, fmt.Errorf(
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

// localDeviceID names the local device the mirror creates for an account that
// has none. It is account-scoped because devices.device_id is a global primary
// key. A constant ID lets mirroring a second account steal the first account's
// device row and invalidates that account's read-cursor foreign key.
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
