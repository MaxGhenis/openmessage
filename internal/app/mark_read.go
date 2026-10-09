package app

import (
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

// ReadCursorMarker advances a remote read cursor. *libgm.Client satisfies it,
// which is how the Google Messages side is reached.
//
// It exists so the mark-read logic can be shared by the MCP tool (which holds
// an *App) and the web API (which holds a *db.Store and a *client.Client and
// no *App at all), without either duplicating the rules below.
type ReadCursorMarker interface {
	MarkRead(conversationID, messageID string) error
}

// MarkReadResult reports what a mark-read actually achieved.
//
// Local and remote are reported separately on purpose. Clearing the local
// unread flag is cheap and always attempted; reaching Google can fail for
// reasons that are not the caller's fault (no live connection, expired auth, a
// thread with no incoming message to point the cursor at). A caller that
// promises the user "the badge on your phone is gone" — the MCP tool — must be
// able to tell the difference. The web UI, which only needs its sidebar to
// update, can ignore RemoteErr.
type MarkReadResult struct {
	ConversationID string
	// MessageID is the message the remote cursor was advanced to. Empty when
	// no remote sync was attempted.
	MessageID string
	// RemoteSynced is true only when the platform acknowledged the receipt.
	RemoteSynced bool
	// RemoteSkipped explains why no remote call was made (wrong platform, no
	// incoming message, no connection). Empty when a call was attempted.
	RemoteSkipped string
	// RemoteErr is set when a remote call was attempted and failed.
	RemoteErr error
}

// SyncConversationReadWith clears a conversation's local unread flag and, for
// Google Messages threads, advances the remote read cursor so the unread badge
// clears on the phone too.
//
// messageID is optional: when empty, the cursor is advanced to the newest
// INCOMING message in the thread. Pointing it at one of our own outgoing
// messages would be meaningless, so outgoing messages are skipped when
// deriving it.
//
// remote may be nil (no live connection); the local clear still happens and
// the result says the remote was skipped.
//
// The returned error covers LOCAL failures only. A remote failure is reported
// in the result instead, because the local clear has already succeeded by then
// and rolling it back would be worse than reporting a partial success. This is
// also what keeps the web UI's mark-read from starting to return 500s whenever
// Google is disconnected.
//
// NOTE for RCS threads: a read receipt is visible to the other party. This is
// the function that sends it.
func SyncConversationReadWith(store *db.Store, remote ReadCursorMarker, conversationID, messageID string) (MarkReadResult, error) {
	conversationID = strings.TrimSpace(conversationID)
	result := MarkReadResult{ConversationID: conversationID}
	if conversationID == "" {
		return result, fmt.Errorf("conversation_id is required")
	}
	if store == nil {
		return result, fmt.Errorf("no store available")
	}

	conv, err := store.GetConversation(conversationID)
	if err != nil {
		return result, fmt.Errorf("load conversation: %w", err)
	}
	if conv == nil {
		return result, fmt.Errorf("conversation %s not found", conversationID)
	}

	// Local first. This is the half that must not fail silently; everything
	// after it is best-effort.
	if err := store.MarkConversationRead(conversationID); err != nil {
		return result, fmt.Errorf("mark read: %w", err)
	}

	// Only Google Messages threads have a remote cursor reachable from here.
	// "" is the legacy value for SMS/RCS rows written before source_platform
	// existed.
	switch conv.SourcePlatform {
	case "", "sms":
	default:
		result.RemoteSkipped = fmt.Sprintf(
			"marking read on the source platform is not supported for %s yet; cleared locally only",
			conv.SourcePlatform)
		return result, nil
	}

	if remote == nil {
		result.RemoteSkipped = ErrNotConnected + "; cleared locally only"
		return result, nil
	}

	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		derived, derr := latestIncomingMessageID(store, conversationID)
		if derr != nil {
			result.RemoteSkipped = fmt.Sprintf("could not find a message to mark read: %v", derr)
			return result, nil
		}
		if derived == "" {
			result.RemoteSkipped = "no incoming message in this thread to advance the read cursor to; cleared locally only"
			return result, nil
		}
		messageID = derived
	}
	result.MessageID = messageID

	if err := remote.MarkRead(conversationID, messageID); err != nil {
		result.RemoteErr = err
		return result, nil
	}
	result.RemoteSynced = true
	return result, nil
}

// resolveRemoteReadCursor is the seam tests swap, in the same spirit as
// getGoogleConversationForSend and sendGoogleTextPayload in gm.go: an *App in a
// unit test holds no live client, so without this the tool handler could only
// ever be exercised on its "not connected" path.
var resolveRemoteReadCursor = func(a *App) ReadCursorMarker {
	return RemoteReadCursor(a.GetClient())
}

// SwapReadCursorForTest points the remote read cursor at marker and returns a
// function restoring the previous resolver. Test-only.
func SwapReadCursorForTest(marker ReadCursorMarker) func() {
	previous := resolveRemoteReadCursor
	resolveRemoteReadCursor = func(*App) ReadCursorMarker { return marker }
	return func() { resolveRemoteReadCursor = previous }
}

// SyncConversationRead is SyncConversationReadWith bound to this App, adding
// the auth-expired bookkeeping that only an App can do.
func (a *App) SyncConversationRead(conversationID, messageID string) (MarkReadResult, error) {
	result, err := SyncConversationReadWith(a.Store, resolveRemoteReadCursor(a), conversationID, messageID)
	if result.RemoteErr != nil {
		a.HandleGoogleAuthExpiredError(result.RemoteErr)
	}
	return result, err
}

// RemoteReadCursor adapts a possibly-nil client into a possibly-nil
// ReadCursorMarker. Returning the typed nil directly would produce a non-nil
// interface holding a nil pointer, which would panic on first use instead of
// being reported as "not connected".
func RemoteReadCursor(cli *client.Client) ReadCursorMarker {
	if cli == nil || cli.GM == nil {
		return nil
	}
	return cli.GM
}

// latestIncomingMessageIDScanLimit bounds how far back we look for an incoming
// message. A thread of only outgoing messages is possible (an unanswered
// send), and scanning its whole history to discover that would be wasteful.
const latestIncomingMessageIDScanLimit = 50

func latestIncomingMessageID(store *db.Store, conversationID string) (string, error) {
	// Newest-first, per GetMessagesByConversation's ORDER BY.
	messages, err := store.GetMessagesByConversation(conversationID, latestIncomingMessageIDScanLimit)
	if err != nil {
		return "", err
	}
	return firstIncomingMessageID(messages), nil
}

func firstIncomingMessageID(messages []*db.Message) string {
	for _, m := range messages {
		if m == nil || m.IsFromMe {
			continue
		}
		if id := strings.TrimSpace(m.MessageID); id != "" {
			return id
		}
	}
	return ""
}
