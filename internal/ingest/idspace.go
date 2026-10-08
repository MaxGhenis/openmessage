package ingest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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
//
// Participant identity alone cannot tell a re-keyed thread from a second live
// thread with the same roster: two groups with the same members, or two 1:1
// threads with one person, are both live on the phone under their own ids.
// Moving one's binding to the other flips the binding back and forth on every
// event and files both threads' messages into one row. So a roster match may
// take a new id only when its own id is dead (see rekeyable):
//
//   - it was displaced (a collision proved its id now names another thread);
//   - or the phone announced it (a ConversationEvent reached it) only in an
//     earlier device ID space;
//   - or it is a provisional thread (minted from a message frame, never
//     announced) whose id was bound in an earlier ID space.
//
// The account's remote_idspace_epoch counts detected ID spaces. A new epoch
// starts when a wire id the phone announced for one thread in the current
// epoch arrives naming another: that id was re-keyed, so the phone's ID space
// was reset. Group re-keys also require the event's title to match, so a new
// group never inherits another group's history just because it has the same
// members.
//
// A reset is detected only at the first such collision, and a re-paired phone
// can deliver fresh ids for a few threads before any collision arrives (on
// 2026-09-03, 14 threads in the 36 seconds before the first one). Those
// threads look like second live threads at first and get their own rows.
// When the reset is detected, decisions made in the preceding
// preResetRecoveryWindow are revisited, and a row minted that way is merged
// into the old thread it declined (recoverPreResetDeclines).
//
// Two cases stay ambiguous with the evidence frames carry. After a reset, an
// unannounced id from a peer whose thread has only been re-keyed by messages
// keeps moving that thread until the phone announces it. And a second live
// thread that first appears during a reset's opening minutes is merged into
// its twin.

// preResetRecoveryWindow is how far back, in frame time, a detected reset
// revisits rows minted because their roster matched a thread that was still
// current. It spans the opening of a re-pair, not ordinary traffic.
const preResetRecoveryWindow = 15 * time.Minute

// maxRecentDeclines bounds the decision log recoverPreResetDeclines reads.
const maxRecentDeclines = 512

// rosterMatch is the identity a frame claims for its thread: a direct thread's
// sole peer, or a group's exact roster and title.
type rosterMatch struct {
	direct bool
	peers  []sqlite.Identity
	title  string
}

// rekeyDecline records a frame whose wire id got its own row although its
// roster matched an existing thread, because every match was still current.
type rekeyDecline struct {
	accountID string
	remoteID  string
	atMS      int64
	match     rosterMatch
}

type declineLog struct {
	mu      sync.Mutex
	entries []rekeyDecline
}

func (l *declineLog) record(entry rekeyDecline) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := entry.atMS - preResetRecoveryWindow.Milliseconds()
	kept := l.entries[:0]
	for _, existing := range l.entries {
		if existing.atMS >= cutoff && existing.remoteID != entry.remoteID {
			kept = append(kept, existing)
		}
	}
	l.entries = append(kept, entry)
	if overflow := len(l.entries) - maxRecentDeclines; overflow > 0 {
		l.entries = append([]rekeyDecline(nil), l.entries[overflow:]...)
	}
}

// take removes and returns the account's declines made at or after sinceMS.
func (l *declineLog) take(accountID string, sinceMS int64) []rekeyDecline {
	l.mu.Lock()
	defer l.mu.Unlock()
	var taken []rekeyDecline
	kept := l.entries[:0]
	for _, entry := range l.entries {
		if entry.accountID == accountID && entry.atMS >= sinceMS {
			taken = append(taken, entry)
			continue
		}
		kept = append(kept, entry)
	}
	l.entries = kept
	return taken
}

// googleIncomingConversation resolves the conversation for an incoming Google
// message whose sender identity is known. The stored binding is used when its
// direct peer is consistent with the sender. A provisional thread minted in
// the current ID space that hears from a second sender is a group whose
// ConversationEvent has not arrived, and keeps the message. Otherwise a
// contradicting binding is stale and moves to the sender's own thread (created
// if absent). An unbound ID continues the sender's direct thread only when
// that thread's own id is dead; otherwise it is a second live thread.
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
			if conversation.Kind == sqlite.ConversationKindGroup {
				if err := w.recordProvisionalGroupSender(accountID, conversation, sender); err != nil {
					return sqlite.Conversation{}, "", err
				}
			}
			return conversation, remoteID, nil
		}
		peers, peersErr := w.store.ListConversationPeerIdentities(accountID, conversation.ConversationID)
		if peersErr != nil {
			return sqlite.Conversation{}, "", peersErr
		}
		if len(peers) == 0 || identitiesContain(peers, sender.IdentityID) {
			return conversation, remoteID, nil
		}
		epoch, epochErr := w.store.RemoteIDSpaceEpoch(accountID)
		if epochErr != nil {
			return sqlite.Conversation{}, "", epochErr
		}
		binding, bindingErr := w.store.ConversationRemoteBinding(accountID, conversation.ConversationID)
		if bindingErr != nil {
			return sqlite.Conversation{}, "", bindingErr
		}
		if binding.Provisional && binding.BoundEpoch >= epoch {
			group, adoptErr := w.adoptSecondSender(accountID, conversation, sender)
			if adoptErr != nil {
				return sqlite.Conversation{}, "", adoptErr
			}
			return group, remoteID, nil
		}
		epoch, epochErr = w.noteIDSpaceContradiction(accountID, remoteID, conversation, binding, epoch)
		if epochErr != nil {
			return sqlite.Conversation{}, "", epochErr
		}
		target, rerouteErr := w.rerouteGoogleDirect(accountID, remoteID, sender, occurredAtMS, epoch)
		if rerouteErr != nil {
			return sqlite.Conversation{}, "", rerouteErr
		}
		return target, remoteID, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, "", err
	}

	epoch, err := w.store.RemoteIDSpaceEpoch(accountID)
	if err != nil {
		return sqlite.Conversation{}, "", err
	}
	match := rosterMatch{direct: true, peers: []sqlite.Identity{sender}}
	target, findErr := w.findRekeyableThread(accountID, remoteID, match, epoch)
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

// adoptSecondSender keeps a second sender's message in a provisional thread
// minted in the current ID space. One wire id names one thread on one phone,
// so a second sender means the thread is a group whose ConversationEvent has
// not arrived (Google can deliver a group's messages before, or without, its
// snapshot). Rerouting instead would scatter the group across its members'
// rows and move the id on every message. The thread becomes a group with the
// new sender as a member, which also stops sole-peer lookups from treating it
// as the first sender's 1:1.
func (w *Worker) adoptSecondSender(
	accountID string,
	conversation sqlite.Conversation,
	sender sqlite.Identity,
) (sqlite.Conversation, error) {
	nowMS, err := w.nowMS()
	if err != nil {
		return sqlite.Conversation{}, err
	}
	conversation.Kind = sqlite.ConversationKindGroup
	conversation.UpdatedAtMS = max(conversation.UpdatedAtMS, nowMS)
	if err := w.store.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, err
	}
	if err := w.recordProvisionalGroupSender(accountID, conversation, sender); err != nil {
		return sqlite.Conversation{}, err
	}
	w.logger.Info().
		Str("account_id", accountID).
		Str("remote_conversation_id", conversation.RemoteConversationID).
		Str("conversation_id", conversation.ConversationID).
		Msg("ingest: second sender on a provisional Google thread; treating it as a group until its snapshot arrives")
	return w.store.GetConversation(conversation.ConversationID)
}

// recordProvisionalGroupSender adds a sender to a provisional group's members.
// Until the group's ConversationEvent arrives, its senders are the only roster
// evidence; once announced, the event's roster is authoritative and is left
// alone.
func (w *Worker) recordProvisionalGroupSender(
	accountID string,
	conversation sqlite.Conversation,
	sender sqlite.Identity,
) error {
	binding, err := w.store.ConversationRemoteBinding(accountID, conversation.ConversationID)
	if err != nil || !binding.Provisional {
		return err
	}
	_, err = w.store.EnsureConversationParticipant(sqlite.ConversationParticipant{
		AccountID:      accountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     sender.IdentityID,
		Role:           sqlite.ParticipantRoleMember,
		DisplayName:    sender.DisplayName,
		IsActive:       true,
	})
	return err
}

// rerouteGoogleDirect moves a stale direct-thread binding to the sender's own
// thread, minting one when the sender has no direct thread whose id is dead.
func (w *Worker) rerouteGoogleDirect(
	accountID string,
	remoteID string,
	sender sqlite.Identity,
	occurredAtMS int64,
	epoch int64,
) (sqlite.Conversation, error) {
	match := rosterMatch{direct: true, peers: []sqlite.Identity{sender}}
	target, err := w.findRekeyableThread(accountID, remoteID, match, epoch)
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
	if err := w.store.MarkConversationProvisional(accountID, conversation.ConversationID, epoch); err != nil {
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
// consistent with it, or when the row is a provisional thread minted in the
// current ID space (the event is the phone's authoritative description of its
// own id). Otherwise it migrates the binding to the roster-matching thread
// whose own id is dead, and resolves an unbound ID the same way before letting
// the caller mint a new row (ErrNotFound).
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
	match := rosterMatch{direct: direct, peers: eventPeers, title: event.Title}
	if errors.Is(storedErr, sqlite.ErrNotFound) {
		if len(eventPeers) == 0 {
			return sqlite.Conversation{}, storedErr
		}
		epoch, err := w.store.RemoteIDSpaceEpoch(accountID)
		if err != nil {
			return sqlite.Conversation{}, err
		}
		target, findErr := w.findRekeyableThread(accountID, remoteID, match, epoch)
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
	epoch, err := w.store.RemoteIDSpaceEpoch(accountID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	binding, err := w.store.ConversationRemoteBinding(accountID, stored.ConversationID)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	if binding.Provisional && binding.BoundEpoch >= epoch {
		return stored, nil
	}

	// The event's roster names a different thread than the stored binding:
	// the device-local ID space has been re-keyed under this remote ID.
	epoch, err = w.noteIDSpaceContradiction(accountID, remoteID, stored, binding, epoch)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	target, findErr := w.findRekeyableThread(accountID, remoteID, match, epoch)
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

// rosterCandidates lists the existing threads a roster names, most recently
// active first: sole-peer direct threads, or groups whose active peer set
// matches exactly and whose title matches the event's.
func (w *Worker) rosterCandidates(accountID string, match rosterMatch) ([]sqlite.Conversation, error) {
	if match.direct {
		if len(match.peers) != 1 {
			return nil, nil
		}
		return w.store.ListDirectConversationsBySolePeer(accountID, match.peers[0].IdentityID)
	}
	identityIDs := make([]string, 0, len(match.peers))
	for _, peer := range match.peers {
		identityIDs = append(identityIDs, peer.IdentityID)
	}
	groups, err := w.store.ListGroupConversationsByPeerSet(accountID, identityIDs)
	if err != nil {
		return nil, err
	}
	titled := groups[:0]
	for _, group := range groups {
		if sameThreadTitle(group.Title, match.title) {
			titled = append(titled, group)
		}
	}
	return titled, nil
}

func sameThreadTitle(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// findRekeyableThread returns the most recently active thread the roster names
// whose own id is dead (see rekeyable), for remoteID to continue. Matches that
// are still current are other live threads that share the roster; they are
// skipped, and when only such matches exist the result is ErrNotFound so the
// caller gives remoteID its own thread. That decision is logged so a reset
// detected shortly afterwards can revisit it.
func (w *Worker) findRekeyableThread(
	accountID string,
	remoteID string,
	match rosterMatch,
	epoch int64,
) (sqlite.Conversation, error) {
	candidates, err := w.rosterCandidates(accountID, match)
	if err != nil {
		return sqlite.Conversation{}, err
	}
	declined := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.RemoteConversationID == remoteID {
			continue
		}
		ok, err := w.rekeyable(accountID, candidate, epoch)
		if err != nil {
			return sqlite.Conversation{}, err
		}
		if ok {
			return candidate, nil
		}
		declined = append(declined, candidate.RemoteConversationID)
	}
	if len(declined) > 0 {
		nowMS, err := w.nowMS()
		if err != nil {
			return sqlite.Conversation{}, err
		}
		w.recentDeclines.record(rekeyDecline{
			accountID: accountID,
			remoteID:  remoteID,
			atMS:      nowMS,
			match:     match,
		})
		w.counters.account(accountID).rekeysDeclined.Add(1)
		w.logger.Info().
			Str("account_id", accountID).
			Str("remote_conversation_id", remoteID).
			Strs("live_roster_matches", declined).
			Int64("idspace_epoch", epoch).
			Msg("ingest: Google thread shares its roster with live threads; keeping it distinct")
	}
	return sqlite.Conversation{}, fmt.Errorf("re-keyable thread for roster: %w", sqlite.ErrNotFound)
}

// rekeyable reports whether a roster-matched row may take a different wire id
// because its own id is dead: displaced by a collision, announced by the phone
// only in an earlier ID space, or provisional (never announced) with an id
// bound in an earlier ID space. A row announced in the current ID space is
// live under its own id, and a provisional row bound in the current ID space
// is a new thread awaiting its ConversationEvent (possibly a group whose first
// messages beat its snapshot); neither is taken.
func (w *Worker) rekeyable(accountID string, conversation sqlite.Conversation, epoch int64) (bool, error) {
	if isDisplacedConversation(conversation) {
		return true, nil
	}
	binding, err := w.store.ConversationRemoteBinding(accountID, conversation.ConversationID)
	if err != nil {
		return false, err
	}
	if binding.Provisional {
		return binding.BoundEpoch < epoch, nil
	}
	return binding.AnnouncedEpoch < epoch, nil
}

// noteIDSpaceContradiction is called when the row bound to remoteID
// contradicts a frame naming remoteID, and returns the ID-space epoch the
// re-key decision should use. If the phone announced that holder in the
// current ID space, its id has been re-keyed since, so the device ID space
// was reset: the account moves to a new epoch, which makes every thread
// announced only before it re-keyable, and decisions made just before the
// detection are revisited. A holder announced only in an earlier epoch is the
// same reset still being discovered, and a provisional holder proves nothing;
// neither advances the epoch.
func (w *Worker) noteIDSpaceContradiction(
	accountID string,
	remoteID string,
	holder sqlite.Conversation,
	binding sqlite.RemoteBinding,
	epoch int64,
) (int64, error) {
	if isDisplacedConversation(holder) || binding.Provisional || binding.AnnouncedEpoch < epoch {
		return epoch, nil
	}
	next, err := w.store.AdvanceRemoteIDSpaceEpoch(accountID, epoch)
	if err != nil {
		return 0, err
	}
	if next == epoch {
		return epoch, nil
	}
	w.counters.account(accountID).idspaceResets.Add(1)
	w.logger.Warn().
		Str("account_id", accountID).
		Str("remote_conversation_id", remoteID).
		Str("conversation_id", holder.ConversationID).
		Int64("idspace_epoch", next).
		Msg("ingest: Google device ID space was reset (an announced thread id now names another thread)")
	if err := w.recoverPreResetDeclines(accountID, next); err != nil {
		return 0, err
	}
	return next, nil
}

// recoverPreResetDeclines revisits the threads given their own rows in the
// preResetRecoveryWindow before a reset was detected because their roster
// matched a thread that was current then. Under the new epoch that thread's id
// is dead, so the minted row is merged into it and the wire id moves with its
// messages (re-served history that duplicates the old thread is dropped).
func (w *Worker) recoverPreResetDeclines(accountID string, epoch int64) error {
	nowMS, err := w.nowMS()
	if err != nil {
		return err
	}
	for _, decline := range w.recentDeclines.take(accountID, nowMS-preResetRecoveryWindow.Milliseconds()) {
		source, err := w.store.GetConversationByRemote(accountID, decline.remoteID)
		if errors.Is(err, sqlite.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		still, err := w.stillDeclinedShape(accountID, source, decline, epoch)
		if err != nil {
			return err
		}
		if !still {
			continue
		}
		candidates, err := w.rosterCandidates(accountID, decline.match)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if candidate.ConversationID == source.ConversationID {
				continue
			}
			ok, err := w.rekeyable(accountID, candidate, epoch)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			merge, err := w.store.MergeConversationInto(
				context.Background(),
				accountID,
				decline.remoteID,
				source.ConversationID,
				candidate.ConversationID,
				nowMS,
			)
			if err != nil {
				return err
			}
			if err := w.store.MarkConversationBound(accountID, candidate.ConversationID, epoch); err != nil {
				return err
			}
			counters := w.counters.account(accountID)
			counters.remoteRebinds.Add(1)
			counters.rekeysRecovered.Add(1)
			w.logger.Warn().
				Str("account_id", accountID).
				Str("remote_conversation_id", decline.remoteID).
				Str("previous_remote_conversation_id", candidate.RemoteConversationID).
				Str("conversation_id", candidate.ConversationID).
				Str("merged_conversation_id", source.ConversationID).
				Int("moved", merge.Moved).
				Int("duplicates", merge.Duplicates).
				Int("kept", merge.Kept).
				Msg("ingest: Google ID-space reset detected after this thread was kept distinct; merged it into the thread it continues")
			break
		}
	}
	return nil
}

// stillDeclinedShape reports whether the row minted for a declined frame is
// still the thread that decision created: not announced in the new epoch, and
// still the same kind with the same roster. A row that has since become a
// group (more senders) or been announced is left alone.
func (w *Worker) stillDeclinedShape(
	accountID string,
	source sqlite.Conversation,
	decline rekeyDecline,
	epoch int64,
) (bool, error) {
	binding, err := w.store.ConversationRemoteBinding(accountID, source.ConversationID)
	if err != nil {
		return false, err
	}
	if !binding.Provisional && binding.AnnouncedEpoch >= epoch {
		return false, nil
	}
	wantKind := sqlite.ConversationKindGroup
	if decline.match.direct {
		wantKind = sqlite.ConversationKindDirect
	}
	if source.Kind != wantKind {
		return false, nil
	}
	peers, err := w.store.ListConversationPeerIdentities(accountID, source.ConversationID)
	if err != nil {
		return false, err
	}
	if len(peers) == 0 {
		return true, nil
	}
	if len(peers) != len(decline.match.peers) {
		return false, nil
	}
	for _, peer := range decline.match.peers {
		if !identitiesContain(peers, peer.IdentityID) {
			return false, nil
		}
	}
	return true, nil
}

// retireReusedRemoteMessageID frees message's remote ID in its conversation
// when a different message already holds it there. Google message IDs are
// device-local row IDs too, so a thread that continues across an ID-space
// reset can receive a new message under an ID one of its old messages
// carries, and the upsert's natural key would overwrite the old message with
// the new one. The stored row is a different message when it has another
// primary key (it was projected under another wire ID, or is a local outgoing
// placeholder) and differentMessage says so; it then keeps its row under a
// displaced remote ID.
func (w *Worker) retireReusedRemoteMessageID(ctx context.Context, message sqlite.Message) error {
	existing, err := w.messages.GetMessageByRemote(
		ctx,
		message.AccountID,
		message.ConversationID,
		message.RemoteMessageID,
	)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.MessageID == message.MessageID || !differentMessage(existing, message) {
		return nil
	}
	retired := sqlite.DisplacedRemoteIDPrefix + message.RemoteMessageID + ":" + existing.MessageID
	if err := w.messages.RetireRemoteMessageID(ctx, message.AccountID, existing.MessageID, retired); err != nil {
		return err
	}
	w.counters.account(message.AccountID).remoteMessageIDsRetired.Add(1)
	w.logger.Warn().
		Str("account_id", message.AccountID).
		Str("conversation_id", message.ConversationID).
		Str("remote_message_id", message.RemoteMessageID).
		Str("retired_message_id", existing.MessageID).
		Msg("ingest: Google reused a message ID in a thread that spans two device ID spaces; kept the older message under a displaced ID")
	return nil
}

// reusedMessageIDMinGap separates a reused Google message ID from one message
// seen twice. One message's timestamp moves by seconds between deliveries (an
// outgoing echo replacing its local placeholder, or a re-delivery: at most 16
// s across the live inbox replayed 2026-10-08), while IDs reused after that
// inbox's 2026-09-03 phone swap named messages 454 to 553 hours apart.
const reusedMessageIDMinGap = time.Hour

// differentMessage reports whether two messages holding one remote message ID
// in one thread are distinct: opposite directions, or timestamps too far
// apart to be one message.
func differentMessage(stored, incoming sqlite.Message) bool {
	if stored.Direction != incoming.Direction {
		return true
	}
	gap := stored.OccurredAtMS - incoming.OccurredAtMS
	if gap < 0 {
		gap = -gap
	}
	return gap >= reusedMessageIDMinGap.Milliseconds()
}

// markGoogleConversationAnnounced records that a ConversationEvent reached the
// conversation in the account's current ID space.
func (w *Worker) markGoogleConversationAnnounced(accountID, conversationID string) error {
	epoch, err := w.store.RemoteIDSpaceEpoch(accountID)
	if err != nil {
		return err
	}
	return w.store.MarkConversationAnnounced(accountID, conversationID, epoch)
}

func isDisplacedConversation(conversation sqlite.Conversation) bool {
	return strings.HasPrefix(conversation.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix)
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
	epoch, err := w.store.RemoteIDSpaceEpoch(accountID)
	if err != nil {
		return err
	}
	if err := w.store.MarkConversationBound(accountID, target.ConversationID, epoch); err != nil {
		return err
	}
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
