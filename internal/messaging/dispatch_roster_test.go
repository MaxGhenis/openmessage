package messaging

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// seedRosterForDispatch gives the test conversation a peer, a departed member
// and this account's own row, and the account a second self identity. Every
// raw value differs from its canonical value, so a ref built from raw values
// fails the comparison.
func seedRosterForDispatch(t *testing.T, store *sqlite.Store) {
	t.Helper()
	nowMS := messagingTestTime.UnixMilli()
	identities := []sqlite.Identity{
		{IdentityID: "identity-self", Kind: "e164", CanonicalValue: "+15550100000", RawValue: "+1 (555) 010-0000", IsSelf: true},
		{IdentityID: "identity-self-legacy", Kind: "legacy_participant", CanonicalValue: "legacy-participant:me", RawValue: "me", IsSelf: true},
		{IdentityID: "identity-peer", Kind: "e164", CanonicalValue: "+15550100001", RawValue: "+1 (555) 010-0001"},
		{IdentityID: "identity-departed", Kind: "e164", CanonicalValue: "+15550100002", RawValue: "+1 (555) 010-0002"},
	}
	for _, identity := range identities {
		identity.AccountID = "account-1"
		identity.DisplayName = "Someone"
		identity.MetadataJSON = `{}`
		identity.CreatedAtMS, identity.UpdatedAtMS = nowMS, nowMS
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.IdentityID, err)
		}
	}
	participant := func(id string, active bool) sqlite.ConversationParticipant {
		return sqlite.ConversationParticipant{
			AccountID: "account-1", ConversationID: "conversation-1", IdentityID: id,
			Role: sqlite.ParticipantRoleMember, IsActive: active,
		}
	}
	if err := store.ReplaceConversationParticipants("conversation-1", []sqlite.ConversationParticipant{
		participant("identity-self", true),
		participant("identity-peer", true),
		participant("identity-departed", false),
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}
}

// wantDispatchRosterRef is what the adapter must receive for the seeded
// conversation: its kind, its active peer, and every self identity, as
// natural keys only (no names or raw values).
var wantDispatchRosterRef = bridge.ConversationRef{
	RemoteID: "remote-conversation",
	Kind:     "direct",
	Peers:    []bridge.IdentityRef{{Kind: "e164", Canonical: "+15550100001"}},
	Self: []bridge.IdentityRef{
		{Kind: "e164", Canonical: "+15550100000", IsSelf: true},
		{Kind: "legacy_participant", Canonical: "legacy-participant:me", IsSelf: true},
	},
}

// rosterDispatchKinds are the intents whose transport request carries a
// ConversationRef.
var rosterDispatchKinds = []string{"text", "media", "reaction"}

// rosterDispatch is one queued intent in the seeded conversation, with a
// reader of the refs its transport has received.
type rosterDispatch struct {
	service    *MessageService
	store      *sqlite.Store
	clock      *manualClock
	submission Submission
	refs       func() []bridge.ConversationRef
}

// queueRosterDispatch seeds the roster and queues one intent of the given
// kind. Its transport succeeds once.
func queueRosterDispatch(t *testing.T, kind string) rosterDispatch {
	t.Helper()
	clock := newManualClock(messagingTestTime)
	store := openMessagingTestStore(t, clock.Now())
	seedRosterForDispatch(t, store)
	text := &scriptedTextSender{}
	registry := newScriptedRegistry(bridge.Platform("roster-"+kind), text)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)
	queued := rosterDispatch{service: service, store: store, clock: clock}
	switch kind {
	case "text":
		queued.submission = mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand("roster-text"),
			Body:          "hello",
		})
		queued.refs = func() []bridge.ConversationRef {
			var refs []bridge.ConversationRef
			for _, request := range text.snapshotRequests() {
				refs = append(refs, request.Conversation)
			}
			return refs
		}
	case "media":
		media := &scriptedMediaSender{steps: []sendStep{{result: bridge.SendResult{RemoteMessageID: "remote-media"}}}}
		registry.setMediaSender(media)
		queued.submission = mustSendDispatchMedia(t, service, SendMediaCommand{
			CommonCommand: testCommonCommand("roster-media"),
			Content:       bytes.NewReader([]byte("image")),
			Filename:      "photo.png",
			MIME:          "image/png",
		})
		queued.refs = func() []bridge.ConversationRef {
			var refs []bridge.ConversationRef
			for _, request := range media.snapshotRequests() {
				refs = append(refs, request.Conversation)
			}
			return refs
		}
	case "reaction":
		reaction := &scriptedReactionSender{steps: []sendStep{{}}}
		registry.setReactionSender(reaction)
		target := mustSentDispatchTarget(t, store, clock, "roster-reaction-target")
		queued.submission = mustSendDispatchReaction(t, service, SendReactionCommand{
			CommonCommand:   testCommonCommand("roster-reaction"),
			TargetMessageID: target.MessageID,
			Emoji:           "👍",
		})
		queued.refs = func() []bridge.ConversationRef {
			var refs []bridge.ConversationRef
			for _, request := range reaction.snapshotRequests() {
				refs = append(refs, request.Conversation)
			}
			return refs
		}
	default:
		t.Fatalf("unknown intent kind %q", kind)
	}
	return queued
}

func TestDispatchPassesTheStoredRoster(t *testing.T) {
	for _, kind := range rosterDispatchKinds {
		t.Run(kind, func(t *testing.T) {
			queued := queueRosterDispatch(t, kind)
			if processed, err := queued.service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v", processed, err)
			}
			if refs := queued.refs(); len(refs) != 1 || !reflect.DeepEqual(refs[0], wantDispatchRosterRef) {
				t.Fatalf("%s refs = %+v, want one equal to %+v", kind, refs, wantDispatchRosterRef)
			}
		})
	}
}

// failingRosterRepository fails the peer load, or the self load when
// failSelf is true, and passes the other through.
type failingRosterRepository struct {
	rosterRepository
	failSelf bool
}

func (r failingRosterRepository) ListConversationPeerIdentities(accountID, conversationID string) ([]sqlite.Identity, error) {
	if !r.failSelf {
		return nil, errors.New("scripted peer load failure")
	}
	return r.rosterRepository.ListConversationPeerIdentities(accountID, conversationID)
}

func (r failingRosterRepository) ListSelfIdentities(accountID string) ([]sqlite.Identity, error) {
	if r.failSelf {
		return nil, errors.New("scripted self load failure")
	}
	return r.rosterRepository.ListSelfIdentities(accountID)
}

// A roster that fails to load stops the intent before the transport call: the
// row records a transient load_roster failure with no transport call, and
// the next attempt sends with the full roster.
func TestRosterLoadFailureRetriesBeforeTheTransportCall(t *testing.T) {
	for _, kind := range rosterDispatchKinds {
		for _, failSelf := range []bool{false, true} {
			name := kind + "/peers"
			if failSelf {
				name = kind + "/self"
			}
			t.Run(name, func(t *testing.T) {
				queued := queueRosterDispatch(t, kind)
				queued.service.roster = failingRosterRepository{rosterRepository: queued.store, failSelf: failSelf}
				if processed, err := queued.service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
					t.Fatalf("DispatchDue(failing roster) = %d, %v; want 1, nil", processed, err)
				}
				row := mustOutboxItem(t, queued.service, queued.submission.OutboxID)
				if row.State != sqlite.OutboxNotDispatched ||
					row.ErrorClass == nil || *row.ErrorClass != string(bridge.FailureTransient) ||
					row.ErrorCode == nil || *row.ErrorCode != "load_roster" ||
					row.TransportCalledAtMS != nil {
					t.Fatalf("outbox row after a roster load failure = %+v, want not_dispatched/transient/load_roster before the transport call", row)
				}
				if refs := queued.refs(); len(refs) != 0 {
					t.Fatalf("transport calls after a roster load failure = %d, want 0", len(refs))
				}

				queued.service.roster = queued.store
				queued.clock.Advance(defaultRetryDelay)
				if processed, err := queued.service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
					t.Fatalf("DispatchDue(retry) = %d, %v; want 1, nil", processed, err)
				}
				if refs := queued.refs(); len(refs) != 1 || !reflect.DeepEqual(refs[0], wantDispatchRosterRef) {
					t.Fatalf("%s refs after the retry = %+v, want one equal to %+v", kind, refs, wantDispatchRosterRef)
				}
			})
		}
	}
}
