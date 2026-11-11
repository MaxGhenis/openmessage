package google

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	gmproto "go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// Fictional numbers in the 555-01xx range reserved for examples. The stored
// thread is with Vic; after a phone swap the phone uses the same thread number
// for its thread with Lee.
const (
	targetSelf = "+15550100000"
	targetVic  = "+15550100001"
	targetLee  = "+15550100002"
)

func storedThreadRef(kind string, peers ...string) bridge.ConversationRef {
	ref := bridge.ConversationRef{
		RemoteID: "remote-conversation",
		Kind:     kind,
		Self:     []bridge.IdentityRef{{Kind: "e164", Canonical: targetSelf}},
	}
	for _, peer := range peers {
		ref.Peers = append(ref.Peers, bridge.IdentityRef{Kind: "e164", Canonical: peer})
	}
	return ref
}

// phoneThread is the phone's answer to a conversation lookup. It lists its own
// number with IsMe set, as the phone normally does.
func phoneThread(group bool, peers ...string) *gmproto.Conversation {
	conversation := &gmproto.Conversation{
		ConversationID: "remote-conversation",
		IsGroupChat:    group,
		Participants: []*gmproto.Participant{{
			ID:         &gmproto.SmallInfo{Number: targetSelf},
			IsMe:       true,
			SimPayload: &gmproto.SIMPayload{Two: 7, SIMNumber: 2},
		}},
	}
	for _, peer := range peers {
		conversation.Participants = append(conversation.Participants, &gmproto.Participant{
			ID: &gmproto.SmallInfo{Number: peer},
		})
	}
	return conversation
}

func requireMovedThreadRefusal(t *testing.T, failure bridge.OpError, operation string) {
	t.Helper()
	if failure.Class != bridge.FailureRecipientMismatch || failure.Operation != operation ||
		failure.Dispatch != bridge.DispatchNotCalled || failure.Fingerprint != "google_conversation_roster_mismatch" {
		t.Fatalf("failure = %+v, want recipient_mismatch %s, not dispatched", failure, operation)
	}
	// The refusal lands in error_detail and the UI: counts only, no numbers.
	if text := failure.Error(); strings.Contains(text, "555") {
		t.Fatalf("refusal names a number: %q", text)
	}
}

func TestSendTextRefusesAThreadThatNowNamesOtherPeople(t *testing.T) {
	fake := &fakeTextSendClient{
		conversationResult: phoneThread(false, targetLee),
		sendResult:         &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
	}
	a := newTextSendTestAdapter(t, fake)

	_, err := a.SendText(context.Background(), bridge.TextRequest{
		AccountID:    "google-primary",
		Conversation: storedThreadRef("direct", targetVic),
		Body:         "for Vic",
		RequestID:    "request-id",
	})
	requireMovedThreadRefusal(t, requireTextOpError(t, err), "send_text")
	if fake.sendCalls != 0 {
		t.Fatalf("SendMessage called %d times after the refusal", fake.sendCalls)
	}
	if !strings.Contains(err.Error(), "(this conversation: direct, 1 person; the phone's thread: direct, 1 person; 0 in common)") {
		t.Fatalf("refusal = %q, want the shapes of both rosters", err.Error())
	}
}

func TestSendTextSendsWhenThePhoneThreadStillNamesTheSamePeople(t *testing.T) {
	tests := []struct {
		name         string
		ref          bridge.ConversationRef
		conversation *gmproto.Conversation
	}{
		{"same peer", storedThreadRef("direct", targetVic), phoneThread(false, targetVic)},
		{"own number also listed without the self flag", storedThreadRef("direct", targetVic), phoneThread(false, targetSelf, targetVic)},
		{"group that gained a member", storedThreadRef("group", targetVic, targetLee), phoneThread(true, targetLee, "+15550100003")},
		{"no stored roster to compare", bridge.ConversationRef{RemoteID: "remote-conversation"}, phoneThread(false, targetLee)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeTextSendClient{
				conversationResult: test.conversation,
				sendResult:         &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
			}
			a := newTextSendTestAdapter(t, fake)
			if _, err := a.SendText(context.Background(), bridge.TextRequest{
				AccountID:    "google-primary",
				Conversation: test.ref,
				Body:         "hello",
				RequestID:    "request-id",
			}); err != nil {
				t.Fatalf("SendText() error = %v", err)
			}
			if fake.sendCalls != 1 {
				t.Fatalf("SendMessage called %d times, want 1", fake.sendCalls)
			}
		})
	}
}

func TestSendMediaRefusesAMovedThreadBeforeUploading(t *testing.T) {
	fake := &fakeMediaSendClient{
		uploadResult:       &gmproto.MediaContent{MediaID: "uploaded"},
		conversationResult: phoneThread(false, targetLee),
		sendResults:        []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}},
	}
	a := newMediaSendTestAdapter(t, fake)

	_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID:    "google-primary",
		Conversation: storedThreadRef("direct", targetVic),
		Reader:       bytes.NewBufferString("data"),
		Size:         4,
		Filename:     "photo.png",
		MIME:         "image/png",
		Caption:      "for Vic",
		RequestID:    "request-id",
	})
	requireMovedThreadRefusal(t, requireMediaOpError(t, err), "send_media")
	if fake.uploadCalls != 0 || len(fake.sent) != 0 {
		t.Fatalf("refused media still reached the phone: %d uploads, %d sends", fake.uploadCalls, len(fake.sent))
	}
}

func TestSendReactionRefusesAThreadThatNowNamesOtherPeople(t *testing.T) {
	fake := &fakeReactionSendClient{
		conversationResult: phoneThread(true, targetLee),
		sendResult:         &gmproto.SendReactionResponse{Success: true},
	}
	a := newReactionSendTestAdapter(t, fake)

	_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
		AccountID:    "google-primary",
		Conversation: storedThreadRef("group", targetVic, "+15550100003"),
		Target:       bridge.MessageRef{RemoteID: "target-id"},
		Emoji:        "👍",
		Action:       bridge.ReactionAdd,
		RequestID:    "request-id",
	})
	requireMovedThreadRefusal(t, requireReactionOpError(t, err), "send_reaction")
	if fake.sendCalls != 0 {
		t.Fatalf("SendReaction called %d times after the refusal", fake.sendCalls)
	}
}

// The phone answered the lookup, so a refusal says nothing about the
// connection: the receive generation stays up and needs_repair stays off.
func TestMovedThreadRefusalLeavesTheReceiveLifecycleAlone(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	run, err := a.Start(context.Background(), bridge.StartRequest{AccountID: "google-primary", Generation: 1}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeTextSendClient{conversationResult: phoneThread(false, targetLee)}
	installTextSendClient(t, host.GetClient(), fake)
	for i := 0; i < 5; i++ {
		_, err = a.SendText(context.Background(), bridge.TextRequest{
			AccountID:    "google-primary",
			Conversation: storedThreadRef("direct", targetVic),
			Body:         "hello",
			RequestID:    "request-id",
		})
		requireMovedThreadRefusal(t, requireTextOpError(t, err), "send_text")
	}
	if !host.Connected.Load() {
		t.Fatal("a refused send marked the connected receive generation lost")
	}
	select {
	case terminal := <-run.Done():
		t.Fatalf("a refused send retired the receive generation with %v", terminal)
	default:
	}
	if host.GoogleStatus().NeedsRepair {
		t.Fatal("a refused send parked the Google lifecycle")
	}
}

// The 2026-11 incident shape, end to end through the real dispatcher and v2
// store with only libgm faked. v2 still binds a thread with Vic to a Google
// thread number that, since a phone swap, names the phone's thread with Lee.
// Before this check, a text in Vic's thread went to Lee. Now the dispatcher
// rejects a text and a reaction there as recipient_mismatch before either
// reaches the phone, and once the number names Vic again, sending works.
func TestDispatchRefusesAStaleGoogleBindingEndToEnd(t *testing.T) {
	f := newReactionDispatchFixture(t)
	nowMS := f.clock.Now().UnixMilli()
	identity := func(number string, isSelf bool) string {
		id := v2keys.DeriveID("identity", reactionDispatchAccountID, "e164\x1f"+number)
		if err := f.store.UpsertIdentity(sqlite.Identity{
			IdentityID: id, AccountID: reactionDispatchAccountID, Kind: "e164", CanonicalValue: number,
			RawValue: number, IsSelf: isSelf, MetadataJSON: `{}`, CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
		}); err != nil {
			t.Fatalf("UpsertIdentity(): %v", err)
		}
		return id
	}
	self := identity(targetSelf, true)
	vic := identity(targetVic, false)
	if err := f.store.ReplaceConversationParticipants(reactionDispatchConversationID, []sqlite.ConversationParticipant{
		{AccountID: reactionDispatchAccountID, ConversationID: reactionDispatchConversationID, IdentityID: self, Role: sqlite.ParticipantRoleMember, IsActive: true},
		{AccountID: reactionDispatchAccountID, ConversationID: reactionDispatchConversationID, IdentityID: vic, Role: sqlite.ParticipantRoleMember, IsActive: true},
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}
	f.text.conversationResult = phoneThread(false, targetLee)
	f.reaction.conversationResult = phoneThread(false, targetLee)
	f.reaction.sendResult = &gmproto.SendReactionResponse{Success: true}

	text, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "for Vic"})
	if err != nil {
		t.Fatal(err)
	}
	reaction := f.react("v2-target")
	f.dispatch(2)
	for _, id := range []string{text.OutboxID, reaction.OutboxID} {
		delivery := f.delivery(id)
		if delivery.State != messaging.OutboxRejected || delivery.ErrorClass != string(bridge.FailureRecipientMismatch) {
			t.Fatalf("delivery %s = %+v, want rejected recipient_mismatch", id, delivery)
		}
	}
	if f.text.sendCalls != 0 || f.reaction.sendCalls != 0 {
		t.Fatalf("refused intents reached the phone: text=%d reaction=%d", f.text.sendCalls, f.reaction.sendCalls)
	}
	// Rejected is final: the dispatcher never tries it again.
	f.clock.Advance(time.Hour)
	f.dispatch(0)

	// The phone's thread names Vic again (live traffic rebound the thread):
	// sending works.
	f.text.conversationResult = phoneThread(false, targetVic)
	again, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "for Vic"})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch(1)
	if delivery := f.delivery(again.OutboxID); delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("delivery once the thread names Vic again = %+v, want confirmed", delivery)
	}
	if f.text.sendCalls != 1 {
		t.Fatalf("SendMessage called %d times, want 1", f.text.sendCalls)
	}
}
