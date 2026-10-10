package v2read

import (
	"errors"
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// resolveConversationID maps a caller-supplied conversation key onto the v2
// primary key with ResolveConversation. Unknown keys, and keys whose lookup
// fails, return unchanged so callers keep their existing
// empty-result/not-found semantics.
func (s *Source) resolveConversationID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return id
	}
	conversation, err := ResolveConversation(s.store, trimmed)
	if err != nil {
		return trimmed
	}
	return conversation.ConversationID
}

// ResolveConversation returns the v2 conversation a caller-supplied key names.
// A v2 conversation ID resolves to itself. Any other value is tried as a
// remote conversation ID — the key the legacy store and every pre-cutover
// consumer used ("signal:+15551234567", "signal-group:…", a Google Messages
// thread id, a WhatsApp JID) — across all accounts. Cutover re-keys every
// conversation to a derived hash, so without this fallback each stored legacy
// ID silently reads as an empty conversation the moment a restart flips the
// app to v2-primary (issue #155).
//
// It is the one alias rule for v2 reads and for writes that take a
// caller-supplied conversation ID (v2wire.MarkReadV2), so a stored legacy ID
// writes to the same thread it reads. A key that matches nothing returns an
// error wrapping sqlite.ErrNotFound.
func ResolveConversation(store *sqlite.Store, id string) (sqlite.Conversation, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return sqlite.Conversation{}, fmt.Errorf("resolve conversation: empty id: %w", sqlite.ErrNotFound)
	}
	conversation, err := store.GetConversation(trimmed)
	if err == nil {
		return conversation, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, fmt.Errorf("resolve conversation %q: %w", trimmed, err)
	}
	accounts, err := store.ListAccounts()
	if err != nil {
		return sqlite.Conversation{}, fmt.Errorf("resolve conversation %q: %w", trimmed, err)
	}
	var best *sqlite.Conversation
	for _, account := range accounts {
		remoteID := v2keys.NormalizeRemoteConversationID(
			platformForBridgeKey(account.BridgeKey),
			trimmed,
		)
		conversation, err := store.GetConversationByRemote(account.AccountID, remoteID)
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
		return *best, nil
	}
	return sqlite.Conversation{}, fmt.Errorf("resolve conversation %q: %w", trimmed, sqlite.ErrNotFound)
}
