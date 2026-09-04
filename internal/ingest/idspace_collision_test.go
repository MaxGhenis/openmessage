package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// These tests replay the 2026-09-03 incident: a re-pair onto a replaced phone
// delivered Google frames whose device-local conversation and message ids had
// been re-keyed. The new phone's id for Shoshana's thread (2873) collided with
// the old phone's id for a PayPal shortcode thread, so her messages were filed
// under "72975" and a re-served history message duplicated; a campaign text
// from a new number (conv 2916) landed in Karl's thread.

const (
	idsSelfNumber    = "+16506303657"
	idsShoshana      = "+15169021075"
	idsPayPal        = "72975"
	idsKarl          = "+12026022529"
	idsCampaignSpam  = "+16516898007"
	idsAnimalsBody   = "random animals 🤣"
	idsAnimalsTimeMS = int64(1787049077326)
)

type idsScript struct {
	events map[string][]bridge.Event
}

func (s *idsScript) Decode(_ context.Context, record bridge.RawIngressRecord) ([]bridge.Event, error) {
	events, ok := s.events[string(record.Payload)]
	if !ok {
		return nil, errors.New("unscripted payload " + string(record.Payload))
	}
	return events, nil
}

func (s *idsScript) add(name string, events ...bridge.Event) {
	if s.events == nil {
		s.events = make(map[string][]bridge.Event)
	}
	s.events[name] = events
}

func idsConversationEvent(remoteID, kind, title string, peers ...string) bridge.Event {
	participants := []bridge.Participant{{
		Identity: bridge.IdentityRef{Raw: idsSelfNumber, Name: "Max Ghenis", IsSelf: true},
		Role:     "member",
		Active:   true,
	}}
	for _, peer := range peers {
		participants = append(participants, bridge.Participant{
			Identity: bridge.IdentityRef{Raw: peer},
			Role:     "member",
			Active:   true,
		})
	}
	return bridge.Event{
		Kind: bridge.EventConversation,
		Conversation: &bridge.ConversationEvent{
			RemoteConversationID: remoteID,
			Kind:                 kind,
			Title:                title,
			Participants:         participants,
		},
	}
}

func idsIncoming(remoteConversationID, remoteMessageID, sender, body string, occurredMS int64) bridge.Event {
	return bridge.Event{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: remoteConversationID,
			RemoteMessageID:      remoteMessageID,
			Sender:               bridge.IdentityRef{Raw: sender},
			Direction:            "incoming",
			Body:                 body,
			OccurredAt:           time.UnixMilli(occurredMS),
		},
	}
}

func idsRun(t *testing.T, harness *i01Harness, name string) {
	t.Helper()
	i01MustAppend(t, harness.sink, i01IngressRecord("ids:"+name, []byte(name)))
	harness.worker.drain(context.Background())
	if got := harness.counters.Snapshot(i01AccountID).Quarantined; got != 0 {
		t.Fatalf("after %s: quarantined = %d, want 0", name, got)
	}
	i01AssertNoPending(t, harness.messages)
}

func idsMessageCount(t *testing.T, harness *i01Harness, conversationID string) int64 {
	t.Helper()
	return i01QueryInt64(
		t,
		harness.path,
		`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`,
		conversationID,
	)
}

func idsPeerNumbers(t *testing.T, harness *i01Harness, conversationID string) []string {
	t.Helper()
	peers, err := harness.store.ListConversationPeerIdentities(i01AccountID, conversationID)
	if err != nil {
		t.Fatalf("ListConversationPeerIdentities(%q): %v", conversationID, err)
	}
	numbers := make([]string, 0, len(peers))
	for _, peer := range peers {
		numbers = append(numbers, peer.CanonicalValue)
	}
	return numbers
}

// seedOldPhone stores the pre-swap threads exactly as the old device keyed them.
func seedOldPhone(t *testing.T, script *idsScript, harness *i01Harness) (paypal, shoshana, karl sqlite.Conversation) {
	t.Helper()
	script.add("old-paypal-conv", idsConversationEvent("2873", "direct", "72975", idsPayPal))
	script.add("old-paypal-msg", idsIncoming("2873", "500", idsPayPal, "Your PayPal code is 123456", idsAnimalsTimeMS-86_400_000))
	script.add("old-shoshana-conv", idsConversationEvent("3093", "direct", "Shoshana Weissmann", idsShoshana))
	script.add("old-shoshana-msg", idsIncoming("3093", "86133", idsShoshana, idsAnimalsBody, idsAnimalsTimeMS))
	script.add("old-karl-conv", idsConversationEvent("2916", "direct", "(202) 602-2529", idsKarl))
	script.add("old-karl-msg", idsIncoming("2916", "700", idsKarl, "Vote Racine", idsAnimalsTimeMS-3_600_000))
	for _, name := range []string{
		"old-paypal-conv", "old-paypal-msg",
		"old-shoshana-conv", "old-shoshana-msg",
		"old-karl-conv", "old-karl-msg",
	} {
		idsRun(t, harness, name)
	}
	var err error
	if paypal, err = harness.store.GetConversationByRemote(i01AccountID, "2873"); err != nil {
		t.Fatalf("seed paypal: %v", err)
	}
	if shoshana, err = harness.store.GetConversationByRemote(i01AccountID, "3093"); err != nil {
		t.Fatalf("seed shoshana: %v", err)
	}
	if karl, err = harness.store.GetConversationByRemote(i01AccountID, "2916"); err != nil {
		t.Fatalf("seed karl: %v", err)
	}
	if got := harness.counters.Snapshot(i01AccountID).RemoteRebinds; got != 0 {
		t.Fatalf("seeding rebinds = %d, want 0 (consistent bindings must not rebind)", got)
	}
	return paypal, shoshana, karl
}

func TestGoogleIDSpaceResetRoutesIncomingBySenderNotByCollidingID(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	paypal, shoshana, karl := seedOldPhone(t, script, harness)

	// New phone: Shoshana's thread is now id 2873 and the phone re-serves her
	// stored message as message 59 (same content, same millisecond).
	script.add("new-shoshana-replay", idsIncoming("2873", "59", idsShoshana, idsAnimalsBody, idsAnimalsTimeMS))
	idsRun(t, harness, "new-shoshana-replay")

	if got := idsMessageCount(t, harness, paypal.ConversationID); got != 1 {
		t.Fatalf("PayPal thread messages = %d, want 1 (Shoshana's text must not file there)", got)
	}
	if got := idsMessageCount(t, harness, shoshana.ConversationID); got != 1 {
		t.Fatalf("Shoshana thread messages = %d, want 1 (re-delivery must dedupe)", got)
	}
	if _, err := harness.messages.GetMessageByRemote(context.Background(), i01AccountID, shoshana.ConversationID, "86133"); err != nil {
		t.Fatalf("original Shoshana message missing after replay: %v", err)
	}
	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.RemoteRebinds != 1 || snapshot.ContentDupesSkipped != 1 {
		t.Fatalf("counters after replay = rebinds %d dupes %d, want 1/1", snapshot.RemoteRebinds, snapshot.ContentDupesSkipped)
	}

	// The binding followed the sender: 2873 now names Shoshana's thread and the
	// PayPal thread keeps its history under a displaced marker.
	bound, err := harness.store.GetConversationByRemote(i01AccountID, "2873")
	if err != nil {
		t.Fatalf("GetConversationByRemote(2873): %v", err)
	}
	if bound.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 bound to %q, want Shoshana's thread %q", bound.ConversationID, shoshana.ConversationID)
	}
	if bound.Title != "Shoshana Weissmann" {
		t.Fatalf("rebound thread title = %q, want Shoshana Weissmann", bound.Title)
	}
	displaced, err := harness.store.GetConversation(paypal.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation(paypal): %v", err)
	}
	if !strings.HasPrefix(displaced.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix+"2873:") {
		t.Fatalf("PayPal remote id = %q, want displaced marker", displaced.RemoteConversationID)
	}
	if displaced.Title != "72975" {
		t.Fatalf("PayPal title = %q, want unchanged 72975", displaced.Title)
	}
	if peers := idsPeerNumbers(t, harness, paypal.ConversationID); len(peers) != 1 || peers[0] != idsPayPal {
		t.Fatalf("PayPal peers = %v, want [%s]", peers, idsPayPal)
	}

	// A genuinely new message under the new id continues Shoshana's thread.
	script.add("new-shoshana-live", idsIncoming("2873", "85793", idsShoshana, "see you at 12", idsAnimalsTimeMS+1_000_000))
	idsRun(t, harness, "new-shoshana-live")
	if got := idsMessageCount(t, harness, shoshana.ConversationID); got != 2 {
		t.Fatalf("Shoshana thread messages = %d, want 2", got)
	}
	if got := idsMessageCount(t, harness, paypal.ConversationID); got != 1 {
		t.Fatalf("PayPal thread messages = %d, want 1", got)
	}
	if got := harness.counters.Snapshot(i01AccountID).RemoteRebinds; got != 1 {
		t.Fatalf("rebinds after consistent live message = %d, want still 1", got)
	}

	// A first-ever text from an unknown number whose new-phone id collides
	// with Karl's old thread: Karl's thread is untouched and a fresh thread is
	// minted for the new sender under id 2916, with a distinct primary key.
	script.add("new-spam", idsIncoming("2916", "85624", idsCampaignSpam, "Donate before midnight", idsAnimalsTimeMS+2_000_000))
	idsRun(t, harness, "new-spam")
	if got := idsMessageCount(t, harness, karl.ConversationID); got != 1 {
		t.Fatalf("Karl thread messages = %d, want 1 (spam must not file there)", got)
	}
	spamThread, err := harness.store.GetConversationByRemote(i01AccountID, "2916")
	if err != nil {
		t.Fatalf("GetConversationByRemote(2916): %v", err)
	}
	if spamThread.ConversationID == karl.ConversationID {
		t.Fatal("2916 still bound to Karl's thread")
	}
	if spamThread.ConversationID == v2keys.DeriveID("conversation", i01AccountID, "2916") {
		t.Fatal("minted thread reused the displaced holder's derived primary key")
	}
	if got := idsMessageCount(t, harness, spamThread.ConversationID); got != 1 {
		t.Fatalf("spam thread messages = %d, want 1", got)
	}
	if peers := idsPeerNumbers(t, harness, spamThread.ConversationID); len(peers) != 1 || peers[0] != idsCampaignSpam {
		t.Fatalf("spam thread peers = %v, want [%s]", peers, idsCampaignSpam)
	}
	karlNow, err := harness.store.GetConversation(karl.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation(karl): %v", err)
	}
	if !strings.HasPrefix(karlNow.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix+"2916:") {
		t.Fatalf("Karl remote id = %q, want displaced marker", karlNow.RemoteConversationID)
	}

	// An unbound new id from a known sender continues their thread instead of
	// splitting it, and the binding moves with them.
	script.add("new-shoshana-newer-id", idsIncoming("4001", "85900", idsShoshana, "moved threads", idsAnimalsTimeMS+3_000_000))
	idsRun(t, harness, "new-shoshana-newer-id")
	if got := idsMessageCount(t, harness, shoshana.ConversationID); got != 3 {
		t.Fatalf("Shoshana thread messages = %d, want 3", got)
	}
	bound, err = harness.store.GetConversationByRemote(i01AccountID, "4001")
	if err != nil || bound.ConversationID != shoshana.ConversationID {
		t.Fatalf("4001 bound to %q (err %v), want Shoshana's thread", bound.ConversationID, err)
	}
	if _, err := harness.store.GetConversationByRemote(i01AccountID, "2873"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("2873 should be unbound after Shoshana moved to 4001, got err %v", err)
	}
}

func TestGoogleIDSpaceResetConversationEventRebindsByRoster(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	paypal, shoshana, _ := seedOldPhone(t, script, harness)

	// The new phone announces id 2873 as Shoshana's direct thread. The stored
	// holder (PayPal) must not be retitled or re-rostered; Shoshana's existing
	// thread takes the binding.
	script.add("new-shoshana-conv", idsConversationEvent("2873", "direct", "Shoshana W.", idsShoshana))
	idsRun(t, harness, "new-shoshana-conv")

	bound, err := harness.store.GetConversationByRemote(i01AccountID, "2873")
	if err != nil {
		t.Fatalf("GetConversationByRemote(2873): %v", err)
	}
	if bound.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 bound to %q, want Shoshana's thread", bound.ConversationID)
	}
	if bound.Title != "Shoshana W." {
		t.Fatalf("rebound title = %q, want the event title", bound.Title)
	}
	paypalNow, err := harness.store.GetConversation(paypal.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation(paypal): %v", err)
	}
	if paypalNow.Title != "72975" || !strings.HasPrefix(paypalNow.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix) {
		t.Fatalf("PayPal row after event = %+v, want title intact and displaced remote id", paypalNow)
	}
	if peers := idsPeerNumbers(t, harness, paypal.ConversationID); len(peers) != 1 || peers[0] != idsPayPal {
		t.Fatalf("PayPal peers = %v, want [%s]", peers, idsPayPal)
	}

	// A conversation event for a thread whose roster matches the binding is a
	// plain refresh: rename only, no rebind.
	before := harness.counters.Snapshot(i01AccountID).RemoteRebinds
	script.add("rename-shoshana", idsConversationEvent("2873", "direct", "Shoshana Weissmann", idsShoshana))
	idsRun(t, harness, "rename-shoshana")
	if got := harness.counters.Snapshot(i01AccountID).RemoteRebinds; got != before {
		t.Fatalf("consistent refresh rebinds = %d, want %d", got, before)
	}
	renamed, _ := harness.store.GetConversation(shoshana.ConversationID)
	if renamed.Title != "Shoshana Weissmann" {
		t.Fatalf("title after rename = %q", renamed.Title)
	}
}

func TestGoogleIDSpaceResetGroupRosterDisjointMintsNewGroup(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)

	script.add("old-group", idsConversationEvent("1544", "group", "Giorgio, Mindy, Rohan", "+15550000001", "+15550000002", "+15550000003"))
	script.add("old-group-msg", idsIncoming("1544", "900", "+15550000001", "dinner?", idsAnimalsTimeMS))
	idsRun(t, harness, "old-group")
	idsRun(t, harness, "old-group-msg")
	oldGroup, err := harness.store.GetConversationByRemote(i01AccountID, "1544")
	if err != nil {
		t.Fatalf("seed group: %v", err)
	}

	// Membership churn shares members: same thread, refreshed roster.
	script.add("group-churn", idsConversationEvent("1544", "group", "Giorgio, Rohan, Sam", "+15550000001", "+15550000003", "+15550000004"))
	idsRun(t, harness, "group-churn")
	same, err := harness.store.GetConversationByRemote(i01AccountID, "1544")
	if err != nil || same.ConversationID != oldGroup.ConversationID {
		t.Fatalf("overlapping roster rebound the group: %q (err %v)", same.ConversationID, err)
	}
	if got := harness.counters.Snapshot(i01AccountID).RemoteRebinds; got != 0 {
		t.Fatalf("rebinds after overlapping roster = %d, want 0", got)
	}

	// A fully disjoint roster under the same id is a different group on a
	// re-keyed device: the old group keeps its history, a new row takes the id.
	script.add("group-collision", idsConversationEvent("1544", "group", "Book club", "+15550000007", "+15550000008"))
	idsRun(t, harness, "group-collision")
	newGroup, err := harness.store.GetConversationByRemote(i01AccountID, "1544")
	if err != nil {
		t.Fatalf("GetConversationByRemote(1544) after collision: %v", err)
	}
	if newGroup.ConversationID == oldGroup.ConversationID {
		t.Fatal("disjoint roster did not displace the old group binding")
	}
	if newGroup.Kind != sqlite.ConversationKindGroup || newGroup.Title != "Book club" {
		t.Fatalf("new group row = %+v", newGroup)
	}
	oldNow, _ := harness.store.GetConversation(oldGroup.ConversationID)
	if oldNow.Title != "Giorgio, Rohan, Sam" || !strings.HasPrefix(oldNow.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix) {
		t.Fatalf("old group after collision = %+v", oldNow)
	}
	if got := idsMessageCount(t, harness, oldGroup.ConversationID); got != 1 {
		t.Fatalf("old group messages = %d, want 1", got)
	}

	// The displaced group is found again by exact roster when its members'
	// thread shows up under a fresh id.
	script.add("group-return", idsConversationEvent("2200", "group", "Giorgio, Rohan, Sam", "+15550000001", "+15550000003", "+15550000004"))
	idsRun(t, harness, "group-return")
	returned, err := harness.store.GetConversationByRemote(i01AccountID, "2200")
	if err != nil || returned.ConversationID != oldGroup.ConversationID {
		t.Fatalf("2200 bound to %q (err %v), want the displaced group %q", returned.ConversationID, err, oldGroup.ConversationID)
	}
}
