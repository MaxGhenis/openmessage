package ingest

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Google Messages remote conversation and message IDs are device-local row
// IDs, not account-level identifiers. A phone swap, factory reset, or backup
// restore re-keys every thread: the same person's conversation arrives under a
// fresh numeric ID, and that ID can collide with the ID an unrelated thread
// had on the previous device. Binding purely on (account, remote ID) then
// appends new messages into the unrelated thread, and re-delivered history
// duplicates instead of deduping (2026-09-03 incident: a re-pair onto a
// replaced phone filed spam into named contact threads and duplicated
// re-served messages).
//
// The guards here treat participant identity — not the numeric ID — as the
// authority on which thread a Google frame belongs to, and migrate the ID
// binding whenever the two disagree.

// googleIncomingConversation resolves the conversation for an incoming Google
// message whose sender identity is known. The stored binding is used only when
// its direct peer is consistent with the sender; otherwise the binding is
// stale and moves to the sender's own thread (created if absent). An unbound
// ID whose sender already has a direct thread continues that thread rather
// than minting a parallel one.
func (w *Worker) googleIncomingConversation(
	accountID string,
	platform bridge.Platform,
	remoteConversationID string,
	sender sqlite.Identity,
	occurredAtMS int64,
) (sqlite.Conversation, string, error) {
	conversation, remoteID, err := w.existingConversation(accountID, platform, remoteConversationID)
	if err == nil {
		if conversation.Kind != sqlite.ConversationKindDirect {
			return conversation, remoteID, nil
		}
		peers, peersErr := w.store.ListConversationPeerIdentities(accountID, conversation.ConversationID)
		if peersErr != nil {
			return sqlite.Conversation{}, "", peersErr
		}
		if len(peers) == 0 || identitiesContain(peers, sender.IdentityID) {
			return conversation, remoteID, nil
		}
		target, rerouteErr := w.rerouteGoogleDirect(accountID, remoteID, sender, occurredAtMS)
		if rerouteErr != nil {
			return sqlite.Conversation{}, "", rerouteErr
		}
		return target, remoteID, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, "", err
	}

	target, findErr := w.store.FindDirectConversationBySolePeer(accountID, sender.IdentityID)
	if findErr == nil {
		if bindErr := w.bindRemoteConversationID(accountID, remoteID, &target); bindErr != nil {
			return sqlite.Conversation{}, "", bindErr
		}
		return target, remoteID, nil
	}
	if !errors.Is(findErr, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, "", findErr
	}
	conversation, remoteID, err = w.ensureMessageConversation(
		accountID,
		platform,
		remoteConversationID,
		occurredAtMS,
	)
	return conversation, remoteID, err
}

// rerouteGoogleDirect moves a stale direct-thread binding to the sender's own
// thread, minting one when the sender has no direct thread yet.
func (w *Worker) rerouteGoogleDirect(
	accountID string,
	remoteID string,
	sender sqlite.Identity,
	occurredAtMS int64,
) (sqlite.Conversation, error) {
	target, err := w.store.FindDirectConversationBySolePeer(accountID, sender.IdentityID)
	if err == nil {
		if bindErr := w.bindRemoteConversationID(accountID, remoteID, &target); bindErr != nil {
			return sqlite.Conversation{}, bindErr
		}
		return target, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, err
	}

	nowMS, err := w.nowMS()
	if err != nil {
		return sqlite.Conversation{}, err
	}
	if _, err := w.store.DisplaceConversationRemoteID(accountID, remoteID, nowMS); err != nil {
		return sqlite.Conversation{}, err
	}
	conversationID, err := w.mintConversationID(accountID, remoteID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	conversation := sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: remoteID,
		Kind:                 sqlite.ConversationKindDirect,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      occurredAtMS,
		MetadataJSON:         "{}",
		CreatedAtMS:          occurredAtMS,
		UpdatedAtMS:          occurredAtMS,
	}
	if err := w.store.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, err
	}
	conversation, err = w.store.GetConversationByRemote(accountID, remoteID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	w.counters.account(accountID).remoteRebinds.Add(1)
	w.logger.Warn().
		Str("account_id", accountID).
		Str("remote_conversation_id", remoteID).
		Str("conversation_id", conversation.ConversationID).
		Str("sender", sender.CanonicalValue).
		Msg("ingest: displaced stale Google thread binding; minted fresh thread for sender")
	return conversation, nil
}

// googleConversationEventTarget picks the row a Google ConversationEvent
// should refresh. It returns the stored binding when the event's roster is
// consistent with it, migrates the binding to the roster-matching thread when
// it is not, and resolves an unbound ID to the thread its roster names before
// letting the caller mint a new row (ErrNotFound).
func (w *Worker) googleConversationEventTarget(
	accountID string,
	platform bridge.Platform,
	event bridge.ConversationEvent,
	remoteID string,
	stored sqlite.Conversation,
	storedErr error,
) (sqlite.Conversation, error) {
	if storedErr != nil && !errors.Is(storedErr, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, storedErr
	}
	eventPeers, err := w.resolveEventPeers(accountID, platform, event)
	if err != nil {
		return sqlite.Conversation{}, err
	}

	direct := event.Kind != string(sqlite.ConversationKindGroup)
	if errors.Is(storedErr, sqlite.ErrNotFound) {
		if len(eventPeers) == 0 {
			return sqlite.Conversation{}, storedErr
		}
		target, findErr := w.findThreadByRoster(accountID, direct, eventPeers)
		if errors.Is(findErr, sqlite.ErrNotFound) {
			return sqlite.Conversation{}, storedErr
		}
		if findErr != nil {
			return sqlite.Conversation{}, findErr
		}
		if bindErr := w.bindRemoteConversationID(accountID, remoteID, &target); bindErr != nil {
			return sqlite.Conversation{}, bindErr
		}
		return target, nil
	}

	if len(eventPeers) == 0 {
		return stored, nil
	}
	storedPeers, err := w.store.ListConversationPeerIdentities(accountID, stored.ConversationID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	if len(storedPeers) == 0 || rostersConsistent(direct, eventPeers, storedPeers) {
		return stored, nil
	}

	// The event's roster names a different thread than the stored binding:
	// the device-local ID space has been re-keyed under this remote ID.
	target, findErr := w.findThreadByRoster(accountID, direct, eventPeers)
	if findErr == nil {
		if bindErr := w.bindRemoteConversationID(accountID, remoteID, &target); bindErr != nil {
			return sqlite.Conversation{}, bindErr
		}
		return target, nil
	}
	if !errors.Is(findErr, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, findErr
	}
	nowMS, err := w.nowMS()
	if err != nil {
		return sqlite.Conversation{}, err
	}
	if _, err := w.store.DisplaceConversationRemoteID(accountID, remoteID, nowMS); err != nil {
		return sqlite.Conversation{}, err
	}
	conversationID, err := w.mintConversationID(accountID, remoteID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	kind := sqlite.ConversationKindDirect
	if !direct {
		kind = sqlite.ConversationKindGroup
	}
	conversation := sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: remoteID,
		Kind:                 kind,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          nowMS,
		UpdatedAtMS:          nowMS,
	}
	if err := w.store.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, err
	}
	conversation, err = w.store.GetConversationByRemote(accountID, remoteID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	w.counters.account(accountID).remoteRebinds.Add(1)
	w.logger.Warn().
		Str("account_id", accountID).
		Str("remote_conversation_id", remoteID).
		Str("conversation_id", conversation.ConversationID).
		Str("title", event.Title).
		Msg("ingest: displaced stale Google thread binding; minted fresh thread for roster")
	return conversation, nil
}

// resolveEventPeers resolves a conversation event's non-self participants to
// identity rows. Participants without a usable address are skipped: they can
// neither prove nor disprove a binding.
func (w *Worker) resolveEventPeers(
	accountID string,
	platform bridge.Platform,
	event bridge.ConversationEvent,
) ([]sqlite.Identity, error) {
	peers := make([]sqlite.Identity, 0, len(event.Participants))
	seen := make(map[string]struct{}, len(event.Participants))
	for _, participant := range event.Participants {
		if participant.Identity.IsSelf || identityRaw(participant.Identity) == "" {
			continue
		}
		identity, err := w.resolveIdentity(accountID, platform, participant.Identity)
		if err != nil {
			return nil, err
		}
		if identity.IsSelf {
			continue
		}
		if _, duplicate := seen[identity.IdentityID]; duplicate {
			continue
		}
		seen[identity.IdentityID] = struct{}{}
		peers = append(peers, identity)
	}
	return peers, nil
}

// rostersConsistent reports whether an event roster and a stored roster can
// name the same thread. A direct thread has exactly one peer, so any set
// difference is a different thread; group membership evolves legitimately, so
// only fully disjoint rosters prove a different thread.
func rostersConsistent(direct bool, eventPeers, storedPeers []sqlite.Identity) bool {
	stored := make(map[string]struct{}, len(storedPeers))
	for _, peer := range storedPeers {
		stored[peer.IdentityID] = struct{}{}
	}
	if direct {
		if len(eventPeers) != len(storedPeers) {
			return false
		}
		for _, peer := range eventPeers {
			if _, ok := stored[peer.IdentityID]; !ok {
				return false
			}
		}
		return true
	}
	for _, peer := range eventPeers {
		if _, ok := stored[peer.IdentityID]; ok {
			return true
		}
	}
	return false
}

// findThreadByRoster finds the existing thread a roster names: the sole-peer
// direct thread, or the group whose active peer set matches exactly.
func (w *Worker) findThreadByRoster(
	accountID string,
	direct bool,
	eventPeers []sqlite.Identity,
) (sqlite.Conversation, error) {
	if direct {
		if len(eventPeers) != 1 {
			return sqlite.Conversation{}, fmt.Errorf(
				"direct thread roster has %d peers: %w",
				len(eventPeers),
				sqlite.ErrNotFound,
			)
		}
		return w.store.FindDirectConversationBySolePeer(accountID, eventPeers[0].IdentityID)
	}
	identityIDs := make([]string, 0, len(eventPeers))
	for _, peer := range eventPeers {
		identityIDs = append(identityIDs, peer.IdentityID)
	}
	return w.store.FindGroupConversationByPeerSet(accountID, identityIDs)
}

// bindRemoteConversationID points an account-scoped remote conversation ID at
// the target thread, displacing any different current holder, and updates the
// caller's copy of the row.
func (w *Worker) bindRemoteConversationID(
	accountID string,
	remoteID string,
	target *sqlite.Conversation,
) error {
	if target.RemoteConversationID == remoteID {
		return nil
	}
	nowMS, err := w.nowMS()
	if err != nil {
		return err
	}
	previous := target.RemoteConversationID
	if err := w.store.ReassignConversationRemoteID(
		accountID,
		remoteID,
		target.ConversationID,
		nowMS,
	); err != nil {
		return err
	}
	target.RemoteConversationID = remoteID
	w.counters.account(accountID).remoteRebinds.Add(1)
	w.logger.Warn().
		Str("account_id", accountID).
		Str("remote_conversation_id", remoteID).
		Str("previous_remote_conversation_id", previous).
		Str("conversation_id", target.ConversationID).
		Msg("ingest: re-bound Google remote conversation ID to its participant-matched thread")
	return nil
}

// mintConversationID derives a new conversation primary key for a remote ID,
// stepping past keys still owned by displaced former holders of the same ID.
func (w *Worker) mintConversationID(accountID, remoteID string) (string, error) {
	candidate := v2keys.DeriveID("conversation", accountID, remoteID)
	for salt := 1; ; salt++ {
		_, err := w.store.GetConversation(candidate)
		if errors.Is(err, sqlite.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = v2keys.DeriveID(
			"conversation",
			accountID,
			remoteID+"\x1frebind\x1f"+strconv.Itoa(salt),
		)
	}
}

func identitiesContain(identities []sqlite.Identity, identityID string) bool {
	for _, identity := range identities {
		if identity.IdentityID == identityID {
			return true
		}
	}
	return false
}
