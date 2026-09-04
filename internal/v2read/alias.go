package v2read

import (
	"errors"
	"strings"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// resolveConversationID maps a caller-supplied conversation key onto the v2
// primary key. v2 conversation IDs pass through untouched. Any other value is
// tried as a remote conversation ID — the key the legacy store and every
// pre-cutover consumer used ("signal:+15551234567", "signal-group:…", a
// Google Messages thread id, a WhatsApp JID) — across all accounts. Cutover
// re-keys every conversation to a derived hash, so without this fallback each
// stored legacy ID silently reads as an empty conversation the moment a
// restart flips the app to v2-primary (issue #155).
//
// Unknown keys return unchanged so callers keep their existing
// empty-result/not-found semantics.
func (s *Source) resolveConversationID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return id
	}
	if _, err := s.store.GetConversation(trimmed); err == nil {
		return trimmed
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return trimmed
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return trimmed
	}
	var best *sqlite.Conversation
	for _, account := range accounts {
		remoteID := v2keys.NormalizeRemoteConversationID(
			platformForBridgeKey(account.BridgeKey),
			trimmed,
		)
		conversation, err := s.store.GetConversationByRemote(account.AccountID, remoteID)
		if err != nil {
			continue
		}
		// The same remote ID can exist under more than one account (a phone
		// number threads on both SMS and Signal only with platform prefixes,
		// but Google thread ids are opaque); prefer the most recently active
		// match deterministically.
		if best == nil ||
			conversation.LastMessageAtMS > best.LastMessageAtMS ||
			(conversation.LastMessageAtMS == best.LastMessageAtMS &&
				conversation.ConversationID < best.ConversationID) {
			match := conversation
			best = &match
		}
	}
	if best != nil {
		return best.ConversationID
	}
	return trimmed
}
