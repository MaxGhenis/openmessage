package v2wire

import (
	"context"
	"errors"
	"fmt"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// MarkReadV2 records a v2-primary mark-read as the read cursor of the account's
// local installation device. It writes the v2 store only and sends no
// transport read receipt, like MirrorReadCursor on a legacy-primary daemon.
//
// conversationID is the v2 ID the UI sends, or a legacy-form ID resolved with
// the alias fallback v2 reads use (v2read.ResolveConversation), so a stored
// legacy ID marks the thread it reads. An ID that resolves to no conversation
// writes nothing and returns an error wrapping sqlite.ErrNotFound: unlike the
// legacy mirror, this path never creates a conversation, account, or row
// keyed by a legacy ID.
//
// The cursor lands on the device GetLocalInstallationDevice resolves, the one
// ingest advances receipt cursors for, whatever its ID (a migrated store keys
// it by hash). An account with no local installation device gets one under
// v2keys.LocalInstallationDeviceID, the ID the migration would have minted.
//
// The cursor names no message (LastReadMessageID nil), as in the legacy
// mirror: it says the thread was read as of atMS. read_cursors references
// messages without an ON DELETE action, so a cursor that named the newest
// message would pin it. The newest message is the one most likely to be a
// send's echo duplicate, which the outbox deletes on reconcile, and that
// delete would then fail. UpsertReadCursor is monotone in read time, so an
// atMS older than the stored cursor leaves it unchanged.
func MarkReadV2(
	ctx context.Context,
	v2 *sqlite.Store,
	conversationID string,
	atMS int64,
) error {
	if ctx == nil {
		return errors.New("mark read v2: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mark read v2: %w", err)
	}
	if v2 == nil {
		return errors.New("mark read v2: v2 store is nil")
	}
	if atMS <= 0 {
		return errors.New("mark read v2: timestamp must be positive")
	}
	conversation, err := v2read.ResolveConversation(v2, conversationID)
	if err != nil {
		return fmt.Errorf("mark read v2: %w", err)
	}
	device, err := localInstallationDevice(ctx, v2, conversation.AccountID, atMS)
	if err != nil {
		return fmt.Errorf("mark read v2 conversation %q: %w", conversation.ConversationID, err)
	}
	if err := v2.UpsertReadCursor(sqlite.ReadCursor{
		AccountID:         conversation.AccountID,
		DeviceID:          device.DeviceID,
		ConversationID:    conversation.ConversationID,
		LastReadMessageID: nil,
		LastReadAtMS:      atMS,
		UpdatedAtMS:       atMS,
	}); err != nil {
		return fmt.Errorf("mark read v2 conversation %q: %w", conversation.ConversationID, err)
	}
	return nil
}

// localInstallationDevice returns the account's local installation device,
// creating it under the migration's derived ID only when the account has none.
func localInstallationDevice(
	ctx context.Context,
	v2 *sqlite.Store,
	accountID string,
	nowMS int64,
) (sqlite.Device, error) {
	device, err := v2.GetLocalInstallationDevice(ctx, accountID)
	if err == nil {
		return device, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Device{}, err
	}
	device, err = v2.EnsureLocalInstallationDevice(ctx, sqlite.Device{
		DeviceID:    v2keys.LocalInstallationDeviceID(accountID),
		AccountID:   accountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	})
	if err != nil {
		return sqlite.Device{}, fmt.Errorf("ensure local device for account %q: %w", accountID, err)
	}
	return device, nil
}
