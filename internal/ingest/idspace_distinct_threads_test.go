package ingest

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Two Google threads can share a roster while both are live on the phone: two
// groups with the same members ("Family" and "Book club"), or two 1:1 threads
// with one person. Neither may take the other's v2 row. The #176 roster re-key
// exists for a different case — a device id-space reset, where the old id is
// dead — and the reset tests below pin that it still happens.

func idsBound(t *testing.T, harness *i01Harness, remoteID string) sqlite.Conversation {
	t.Helper()
	conversation, err := harness.store.GetConversationByRemote(i01AccountID, remoteID)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%s): %v", remoteID, err)
	}
	return conversation
}

func idsRebinds(harness *i01Harness) uint64 {
	return harness.counters.Snapshot(i01AccountID).RemoteRebinds
}

func idsDisplacedCount(t *testing.T, harness *i01Harness) int64 {
	t.Helper()
	return i01QueryInt64(
		t,
		harness.path,
		`SELECT COUNT(*) FROM conversations WHERE remote_conversation_id LIKE ?`,
		sqlite.DisplacedRemoteIDPrefix+"%",
	)
}

// idsMessageHome returns the conversation that holds the message with this
// remote message ID (unique across a test's frames).
func idsMessageHome(t *testing.T, harness *i01Harness, remoteMessageID string) string {
	t.Helper()
	database := i01OpenInspector(t, harness.path)
	defer database.Close()
	var conversationID string
	if err := database.QueryRow(
		`SELECT conversation_id FROM messages WHERE remote_message_id = ?`,
		remoteMessageID,
	).Scan(&conversationID); err != nil {
		t.Fatalf("locate message %s: %v", remoteMessageID, err)
	}
	return conversationID
}

func TestGoogleEqualRosterGroupsStayDistinct(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	at := idsAnimalsTimeMS

	script.add("family-conv", idsConversationEvent("5001", "group", "Family", idsKarl, idsShoshana))
	script.add("family-msg", idsIncoming("5001", "1", idsKarl, "family msg", at))
	script.add("book-conv", idsConversationEvent("5002", "group", "Book club", idsKarl, idsShoshana))
	script.add("book-msg", idsIncoming("5002", "2", idsShoshana, "book club msg", at+1000))
	script.add("family-conv-again", idsConversationEvent("5001", "group", "Family", idsKarl, idsShoshana))
	script.add("book-conv-again", idsConversationEvent("5002", "group", "Book club", idsKarl, idsShoshana))
	script.add("family-msg-2", idsIncoming("5001", "3", idsShoshana, "family again", at+2000))
	for _, name := range []string{
		"family-conv", "family-msg", "book-conv", "book-msg",
		"family-conv-again", "book-conv-again", "family-msg-2",
	} {
		idsRun(t, harness, name)
	}

	family := idsBound(t, harness, "5001")
	book := idsBound(t, harness, "5002")
	if family.ConversationID == book.ConversationID {
		t.Fatalf("5001 and 5002 share row %q; equal rosters must not merge distinct groups", family.ConversationID)
	}
	if family.Title != "Family" || book.Title != "Book club" {
		t.Fatalf("titles = %q / %q, want Family / Book club", family.Title, book.Title)
	}
	if got := idsMessageCount(t, harness, family.ConversationID); got != 2 {
		t.Fatalf("Family messages = %d, want 2", got)
	}
	if got := idsMessageCount(t, harness, book.ConversationID); got != 1 {
		t.Fatalf("Book club messages = %d, want 1", got)
	}
	if got := idsRebinds(harness); got != 0 {
		t.Fatalf("remote_rebinds = %d, want 0", got)
	}
	if got := idsDisplacedCount(t, harness); got != 0 {
		t.Fatalf("displaced rows = %d, want 0", got)
	}
}

func TestGoogleSameSolePeerDirectThreadsStayDistinct(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	at := idsAnimalsTimeMS

	script.add("sms-conv", idsConversationEvent("6001", "direct", "Karl SMS", idsKarl))
	script.add("sms-msg", idsIncoming("6001", "11", idsKarl, "sms hello", at))
	script.add("rcs-conv", idsConversationEvent("6002", "direct", "Karl RCS", idsKarl))
	script.add("rcs-msg", idsIncoming("6002", "12", idsKarl, "rcs hello", at+1000))
	script.add("sms-conv-again", idsConversationEvent("6001", "direct", "Karl SMS", idsKarl))
	script.add("rcs-conv-again", idsConversationEvent("6002", "direct", "Karl RCS", idsKarl))
	script.add("sms-msg-2", idsIncoming("6001", "13", idsKarl, "sms again", at+2000))
	for _, name := range []string{
		"sms-conv", "sms-msg", "rcs-conv", "rcs-msg",
		"sms-conv-again", "rcs-conv-again", "sms-msg-2",
	} {
		idsRun(t, harness, name)
	}

	sms := idsBound(t, harness, "6001")
	rcs := idsBound(t, harness, "6002")
	if sms.ConversationID == rcs.ConversationID {
		t.Fatalf("6001 and 6002 share row %q; one peer's two live threads must stay distinct", sms.ConversationID)
	}
	if sms.Title != "Karl SMS" || rcs.Title != "Karl RCS" {
		t.Fatalf("titles = %q / %q, want Karl SMS / Karl RCS", sms.Title, rcs.Title)
	}
	if got := idsMessageCount(t, harness, sms.ConversationID); got != 2 {
		t.Fatalf("6001 messages = %d, want 2", got)
	}
	if got := idsMessageCount(t, harness, rcs.ConversationID); got != 1 {
		t.Fatalf("6002 messages = %d, want 1", got)
	}
	for _, conversation := range []sqlite.Conversation{sms, rcs} {
		if peers := idsPeerNumbers(t, harness, conversation.ConversationID); len(peers) != 1 || peers[0] != idsKarl {
			t.Fatalf("%s peers = %v, want [%s]", conversation.RemoteConversationID, peers, idsKarl)
		}
	}
	if got := idsRebinds(harness); got != 0 {
		t.Fatalf("remote_rebinds = %d, want 0", got)
	}
	if got := idsDisplacedCount(t, harness); got != 0 {
		t.Fatalf("displaced rows = %d, want 0", got)
	}
}

func TestGoogleUnboundMessageDoesNotTakeThePeersAnnouncedThread(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	at := idsAnimalsTimeMS

	script.add("sms-conv", idsConversationEvent("6001", "direct", "Karl SMS", idsKarl))
	script.add("sms-msg", idsIncoming("6001", "11", idsKarl, "sms hello", at))
	idsRun(t, harness, "sms-conv")
	idsRun(t, harness, "sms-msg")
	sms := idsBound(t, harness, "6001")

	// A message under a second live id reaches v2 before that thread's
	// ConversationEvent. Karl's announced 6001 thread keeps its id.
	script.add("rcs-msg", idsIncoming("6002", "12", idsKarl, "rcs first", at+1000))
	idsRun(t, harness, "rcs-msg")
	if got := idsBound(t, harness, "6001"); got.ConversationID != sms.ConversationID {
		t.Fatalf("6001 moved to %q, want it to stay on %q", got.ConversationID, sms.ConversationID)
	}
	rcs := idsBound(t, harness, "6002")
	if rcs.ConversationID == sms.ConversationID {
		t.Fatal("the 6002 message took Karl's announced 6001 thread")
	}

	script.add("rcs-conv", idsConversationEvent("6002", "direct", "Karl RCS", idsKarl))
	script.add("sms-msg-2", idsIncoming("6001", "13", idsKarl, "sms again", at+2000))
	idsRun(t, harness, "rcs-conv")
	idsRun(t, harness, "sms-msg-2")
	if got := idsBound(t, harness, "6002"); got.ConversationID != rcs.ConversationID || got.Title != "Karl RCS" {
		t.Fatalf("6002 after its event = %+v, want row %q titled Karl RCS", got, rcs.ConversationID)
	}
	if got := idsMessageCount(t, harness, sms.ConversationID); got != 2 {
		t.Fatalf("6001 messages = %d, want 2", got)
	}
	if got := idsMessageCount(t, harness, rcs.ConversationID); got != 1 {
		t.Fatalf("6002 messages = %d, want 1", got)
	}
	if got := idsRebinds(harness); got != 0 {
		t.Fatalf("remote_rebinds = %d, want 0", got)
	}
}

func TestGoogleGroupMessageAheadOfItsConversationLeavesOneToOnesIntact(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	at := idsAnimalsTimeMS

	script.add("karl-conv", idsConversationEvent("2916", "direct", "Karl", idsKarl))
	script.add("karl-msg", idsIncoming("2916", "700", idsKarl, "Vote Racine", at))
	script.add("shoshana-conv", idsConversationEvent("3093", "direct", "Shoshana Weissmann", idsShoshana))
	script.add("shoshana-msg", idsIncoming("3093", "86133", idsShoshana, idsAnimalsBody, at))
	for _, name := range []string{"karl-conv", "karl-msg", "shoshana-conv", "shoshana-msg"} {
		idsRun(t, harness, name)
	}
	karl := idsBound(t, harness, "2916")
	shoshana := idsBound(t, harness, "3093")

	// A new group's messages arrive before its ConversationEvent.
	script.add("group-msg-karl", idsIncoming("4000", "901", idsKarl, "weekend?", at+1000))
	script.add("group-msg-shoshana", idsIncoming("4000", "902", idsShoshana, "saturday works", at+2000))
	script.add("group-conv", idsConversationEvent("4000", "group", "Weekend Group", idsKarl, idsShoshana))
	for _, name := range []string{"group-msg-karl", "group-msg-shoshana", "group-conv"} {
		idsRun(t, harness, name)
	}

	for _, want := range []struct {
		row    sqlite.Conversation
		remote string
		title  string
		peer   string
	}{
		{karl, "2916", "Karl", idsKarl},
		{shoshana, "3093", "Shoshana Weissmann", idsShoshana},
	} {
		now, err := harness.store.GetConversation(want.row.ConversationID)
		if err != nil {
			t.Fatalf("GetConversation(%s): %v", want.remote, err)
		}
		if now.RemoteConversationID != want.remote || now.Kind != sqlite.ConversationKindDirect || now.Title != want.title {
			t.Fatalf("1:1 %s after the group's frames = %+v, want it untouched", want.remote, now)
		}
		if peers := idsPeerNumbers(t, harness, now.ConversationID); len(peers) != 1 || peers[0] != want.peer {
			t.Fatalf("1:1 %s peers = %v, want [%s]", want.remote, peers, want.peer)
		}
		if got := idsMessageCount(t, harness, now.ConversationID); got != 1 {
			t.Fatalf("1:1 %s messages = %d, want 1", want.remote, got)
		}
	}
	group := idsBound(t, harness, "4000")
	if group.Kind != sqlite.ConversationKindGroup || group.Title != "Weekend Group" {
		t.Fatalf("4000 = %+v, want the Weekend Group", group)
	}
	for _, id := range []string{"901", "902"} {
		if home := idsMessageHome(t, harness, id); home != group.ConversationID {
			t.Fatalf("message %s filed in %q, want the group %q", id, home, group.ConversationID)
		}
	}
	if got := idsRebinds(harness); got != 0 {
		t.Fatalf("remote_rebinds = %d, want 0", got)
	}
}

func TestGoogleIDSpaceResetRekeysStaleThreadsButNotNewOnes(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	_, shoshana, karl := seedOldPhone(t, script, harness)
	const g1, g2, g3 = "+15550000001", "+15550000002", "+15550000003"
	script.add("old-group", idsConversationEvent("1544", "group", "Giorgio, Mindy, Rohan", g1, g2, g3))
	script.add("old-group-msg", idsIncoming("1544", "900", g1, "dinner?", idsAnimalsTimeMS))
	idsRun(t, harness, "old-group")
	idsRun(t, harness, "old-group-msg")
	oldGroup := idsBound(t, harness, "1544")

	// New phone. Its id for Shoshana's thread collides with PayPal's old id:
	// the reset is detected here.
	script.add("new-shoshana", idsConversationEvent("2873", "direct", "Shoshana W.", idsShoshana))
	idsRun(t, harness, "new-shoshana")
	if got := idsBound(t, harness, "2873"); got.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 bound to %q, want Shoshana's thread", got.ConversationID)
	}

	// Ids that collide with nothing. Karl's thread and the old group were
	// announced only by the old phone, so their new ids continue them.
	script.add("new-karl", idsConversationEvent("4100", "direct", "Karl", idsKarl))
	script.add("new-group", idsConversationEvent("7000", "group", "Giorgio, Mindy, Rohan", g1, g2, g3))
	idsRun(t, harness, "new-karl")
	idsRun(t, harness, "new-group")
	if got := idsBound(t, harness, "4100"); got.ConversationID != karl.ConversationID {
		t.Fatalf("4100 bound to %q, want Karl's thread %q (re-key after reset)", got.ConversationID, karl.ConversationID)
	}
	if _, err := harness.store.GetConversationByRemote(i01AccountID, "2916"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("old id 2916 should be free after the re-key, got err %v", err)
	}
	if got := idsBound(t, harness, "7000"); got.ConversationID != oldGroup.ConversationID {
		t.Fatalf("7000 bound to %q, want the old group %q (re-key after reset)", got.ConversationID, oldGroup.ConversationID)
	}
	rebindsAfterRekey := idsRebinds(harness)
	if rebindsAfterRekey != 3 {
		t.Fatalf("remote_rebinds after the re-keys = %d, want 3", rebindsAfterRekey)
	}

	// The new phone also has a second group with the same members and a second
	// thread with Karl. Their twins were already announced by the new phone, so
	// these are different threads.
	script.add("new-group-2", idsConversationEvent("7001", "group", "Dinner plans", g1, g2, g3))
	script.add("new-karl-2", idsConversationEvent("4101", "direct", "Karl (work)", idsKarl))
	script.add("new-group-again", idsConversationEvent("7000", "group", "Giorgio, Mindy, Rohan", g1, g2, g3))
	script.add("new-karl-again", idsConversationEvent("4100", "direct", "Karl", idsKarl))
	for _, name := range []string{"new-group-2", "new-karl-2", "new-group-again", "new-karl-again"} {
		idsRun(t, harness, name)
	}
	group2 := idsBound(t, harness, "7001")
	if group2.ConversationID == oldGroup.ConversationID {
		t.Fatal("7001 took the group the new phone announced as 7000")
	}
	if got := idsBound(t, harness, "7000"); got.ConversationID != oldGroup.ConversationID || got.Title != "Giorgio, Mindy, Rohan" {
		t.Fatalf("7000 after its twin appeared = %+v, want the old group, title unchanged", got)
	}
	if group2.Title != "Dinner plans" {
		t.Fatalf("7001 title = %q, want Dinner plans", group2.Title)
	}
	karl2 := idsBound(t, harness, "4101")
	if karl2.ConversationID == karl.ConversationID {
		t.Fatal("4101 took the Karl thread the new phone announced as 4100")
	}
	if got := idsBound(t, harness, "4100"); got.ConversationID != karl.ConversationID {
		t.Fatalf("4100 moved to %q, want Karl's thread", got.ConversationID)
	}
	if got := idsRebinds(harness); got != rebindsAfterRekey {
		t.Fatalf("remote_rebinds = %d, want still %d", got, rebindsAfterRekey)
	}
	if strings.HasPrefix(group2.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix) {
		t.Fatalf("7001 row displaced: %+v", group2)
	}
}

// The id-space state lives in the store: a worker restarted after a reset
// still treats old-phone threads as re-keyable and new-phone threads as live.
func TestGoogleIDSpaceStateSurvivesWorkerRestart(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	_, shoshana, karl := seedOldPhone(t, script, harness)
	script.add("new-shoshana", idsConversationEvent("2873", "direct", "Shoshana W.", idsShoshana))
	idsRun(t, harness, "new-shoshana")

	counters := &Counters{}
	restarted := &i01Harness{
		path:     harness.path,
		store:    harness.store,
		messages: harness.messages,
		counters: counters,
	}
	restarted.worker = i01NewWorker(t, harness.store, harness.messages, counters, script, nil)
	restarted.sink = i01NewSink(t, harness.messages, restarted.worker, counters, "inbox-restart")

	script.add("new-karl", idsConversationEvent("4100", "direct", "Karl", idsKarl))
	script.add("new-shoshana-2", idsConversationEvent("4200", "direct", "Shoshana (2)", idsShoshana))
	idsRun(t, restarted, "new-karl")
	idsRun(t, restarted, "new-shoshana-2")
	if got := idsBound(t, restarted, "4100"); got.ConversationID != karl.ConversationID {
		t.Fatalf("after restart, 4100 bound to %q, want Karl's old-phone thread re-keyed", got.ConversationID)
	}
	if got := idsBound(t, restarted, "4200"); got.ConversationID == shoshana.ConversationID {
		t.Fatal("after restart, 4200 took Shoshana's thread the new phone announced as 2873")
	}
	if got := idsBound(t, restarted, "2873"); got.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 moved to %q", got.ConversationID)
	}
}

// A row minted from a message frame takes the message's own timestamp as its
// creation time. When the phone's clock runs ahead of this one, the thread's
// ConversationEvent used to fail the updated_at_ms >= created_at_ms check and
// was quarantined, leaving the thread untitled and roster-less.
func TestGoogleConversationEventAfterFutureDatedMessageIsApplied(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	ahead := i01TestTime.UnixMilli() + 5_000
	script.add("msg", idsIncoming("9100", "1", idsKarl, "from a fast phone clock", ahead))
	script.add("conv", idsConversationEvent("9100", "direct", "Karl", idsKarl))
	idsRun(t, harness, "msg")
	idsRun(t, harness, "conv")
	got := idsBound(t, harness, "9100")
	if got.Title != "Karl" {
		t.Fatalf("9100 title = %q, want the ConversationEvent's Karl", got.Title)
	}
	if got.UpdatedAtMS < got.CreatedAtMS {
		t.Fatalf("9100 updated_at %d < created_at %d", got.UpdatedAtMS, got.CreatedAtMS)
	}
}

// Replays the 2026-09-03 re-pair: the new phone delivered messages under
// fresh ids for 14 threads in the 36 seconds before the first id collision
// revealed the reset. Each looked like a second live thread with its peer and
// got its own row; once the reset is detected those rows fold back into the
// threads they continue, and re-served history is not kept twice.
func TestGoogleResetDetectedLateMergesThreadsKeptDistinctBeforeIt(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	_, shoshana, karl := seedOldPhone(t, script, harness)

	script.add("new-karl-reserved", idsIncoming("4400", "40", idsKarl, "Vote Racine", idsAnimalsTimeMS-3_600_000))
	script.add("new-karl-live", idsIncoming("4400", "41", idsKarl, "polls close at 8", idsAnimalsTimeMS+60_000))
	idsRun(t, harness, "new-karl-reserved")
	idsRun(t, harness, "new-karl-live")
	early := idsBound(t, harness, "4400")
	if early.ConversationID == karl.ConversationID {
		t.Fatal("before any reset evidence, 4400 took Karl's announced thread")
	}

	// The first collision: the new phone's id for Shoshana is PayPal's old id.
	script.add("new-shoshana-replay", idsIncoming("2873", "59", idsShoshana, idsAnimalsBody, idsAnimalsTimeMS))
	idsRun(t, harness, "new-shoshana-replay")

	if got := idsBound(t, harness, "4400"); got.ConversationID != karl.ConversationID {
		t.Fatalf("after the reset was detected, 4400 bound to %q, want Karl's thread %q", got.ConversationID, karl.ConversationID)
	}
	if got := idsMessageCount(t, harness, karl.ConversationID); got != 2 {
		t.Fatalf("Karl thread messages = %d, want 2 (original + new; the re-served copy dropped)", got)
	}
	if home := idsMessageHome(t, harness, "41"); home != karl.ConversationID {
		t.Fatalf("new message 41 in %q, want Karl's thread", home)
	}
	if _, err := harness.store.GetConversation(early.ConversationID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("emptied interim row still present: %v", err)
	}
	if got := idsBound(t, harness, "2873"); got.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 bound to %q, want Shoshana's thread", got.ConversationID)
	}
	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.IDSpaceResets != 1 || snapshot.RekeysRecovered != 1 {
		t.Fatalf("idspace_resets %d rekeys_recovered %d, want 1/1", snapshot.IDSpaceResets, snapshot.RekeysRecovered)
	}

	script.add("new-karl-later", idsIncoming("4400", "42", idsKarl, "thanks", idsAnimalsTimeMS+120_000))
	idsRun(t, harness, "new-karl-later")
	if home := idsMessageHome(t, harness, "42"); home != karl.ConversationID {
		t.Fatalf("later 4400 message in %q, want Karl's thread", home)
	}
}

// Replays wire 3097 in the live inbox: a group's messages from several
// members arrive with no ConversationEvent. Rerouting each message by sender
// scattered the group across per-sender fragments and moved the id on every
// message (15 times in 5 seconds on main).
func TestGoogleGroupWithoutItsConversationEventStaysOneThread(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	at := idsAnimalsTimeMS
	const u = "+15550000001"
	for _, seed := range []struct{ remote, title, peer string }{
		{"2916", "Karl", idsKarl},
		{"3093", "Shoshana Weissmann", idsShoshana},
		{"3200", "U", u},
	} {
		script.add("conv-"+seed.remote, idsConversationEvent(seed.remote, "direct", seed.title, seed.peer))
		idsRun(t, harness, "conv-"+seed.remote)
	}
	senders := []string{idsKarl, idsShoshana, u, idsShoshana, idsKarl, u, idsKarl}
	for index, sender := range senders {
		name := "group-msg-" + strconv.Itoa(index)
		script.add(name, idsIncoming("3097", strconv.Itoa(900+index), sender, "group line "+strconv.Itoa(index), at+int64(index)*1000))
		idsRun(t, harness, name)
	}
	group := idsBound(t, harness, "3097")
	if got := idsMessageCount(t, harness, group.ConversationID); got != int64(len(senders)) {
		t.Fatalf("3097 thread messages = %d, want all %d", got, len(senders))
	}
	if group.Kind != sqlite.ConversationKindGroup {
		t.Fatalf("3097 kind = %q, want group once a second sender appeared", group.Kind)
	}
	if peers := idsPeerNumbers(t, harness, group.ConversationID); len(peers) != 3 {
		t.Fatalf("3097 members = %v, want all three senders", peers)
	}
	if got := idsRebinds(harness); got != 0 {
		t.Fatalf("remote_rebinds = %d, want 0", got)
	}
	if got := idsDisplacedCount(t, harness); got != 0 {
		t.Fatalf("displaced rows = %d, want 0", got)
	}
	for _, remote := range []string{"2916", "3093", "3200"} {
		if got := idsMessageCount(t, harness, idsBound(t, harness, remote).ConversationID); got != 0 {
			t.Fatalf("1:1 %s took %d group messages", remote, got)
		}
	}

	script.add("group-conv", idsConversationEvent("3097", "group", "Weekend", idsKarl, idsShoshana, u))
	idsRun(t, harness, "group-conv")
	if got := idsBound(t, harness, "3097"); got.ConversationID != group.ConversationID || got.Title != "Weekend" {
		t.Fatalf("3097 after its event = %+v, want the same row titled Weekend", got)
	}
}

// After a reset, groups with one roster re-key by title: each continues its
// own history, and a new group with that roster continues neither.
func TestGoogleResetRekeysSameRosterGroupsByTitle(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	const q = "+15550000004"
	at := idsAnimalsTimeMS
	script.add("family", idsConversationEvent("5001", "group", "Family", idsKarl, q))
	script.add("family-msg", idsIncoming("5001", "930", idsKarl, "Family old", at+100))
	script.add("book", idsConversationEvent("5002", "group", "Book club", idsKarl, q))
	script.add("book-msg", idsIncoming("5002", "931", idsKarl, "Book old", at+200))
	for _, name := range []string{"family", "family-msg", "book", "book-msg"} {
		idsRun(t, harness, name)
	}
	family := idsBound(t, harness, "5001")
	book := idsBound(t, harness, "5002")
	_, _, _ = seedOldPhone(t, script, harness)
	script.add("reset", idsConversationEvent("2873", "direct", "Shoshana W.", idsShoshana))
	idsRun(t, harness, "reset")

	script.add("new-family", idsConversationEvent("7001", "group", "Family", idsKarl, q))
	script.add("new-book", idsConversationEvent("7002", "group", "Book club", idsKarl, q))
	script.add("new-club", idsConversationEvent("7003", "group", "New club", idsKarl, q))
	for _, name := range []string{"new-family", "new-book", "new-club"} {
		idsRun(t, harness, name)
	}
	if got := idsBound(t, harness, "7001"); got.ConversationID != family.ConversationID {
		t.Fatalf("7001 Family bound to %q, want the Family thread %q", got.ConversationID, family.ConversationID)
	}
	if got := idsBound(t, harness, "7002"); got.ConversationID != book.ConversationID {
		t.Fatalf("7002 Book club bound to %q, want the Book club thread %q", got.ConversationID, book.ConversationID)
	}
	club := idsBound(t, harness, "7003")
	if club.ConversationID == family.ConversationID || club.ConversationID == book.ConversationID {
		t.Fatal("a new group with the same members took an old group's history")
	}
	if got := idsMessageCount(t, harness, club.ConversationID); got != 0 {
		t.Fatalf("new club inherited %d messages", got)
	}
}

// A thread that never got a ConversationEvent (minted from its messages) on
// the old phone still re-keys after a reset, and its re-served history dedupes.
func TestGoogleResetRekeysOldPhoneProvisionalThreads(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	script.add("paypal", idsConversationEvent("2873", "direct", "72975", idsPayPal))
	script.add("shoshana-msg", idsIncoming("3093", "86133", idsShoshana, idsAnimalsBody, idsAnimalsTimeMS))
	idsRun(t, harness, "paypal")
	idsRun(t, harness, "shoshana-msg")
	shoshana := idsBound(t, harness, "3093")

	script.add("reset", idsIncoming("2873", "59", idsShoshana, idsAnimalsBody, idsAnimalsTimeMS))
	idsRun(t, harness, "reset")
	if got := idsBound(t, harness, "2873"); got.ConversationID != shoshana.ConversationID {
		t.Fatalf("2873 bound to %q, want Shoshana's message-minted thread %q", got.ConversationID, shoshana.ConversationID)
	}
	if got := idsMessageCount(t, harness, shoshana.ConversationID); got != 1 {
		t.Fatalf("Shoshana thread messages = %d, want 1 (re-served copy deduped)", got)
	}
}

// Google message ids are device-local too. A thread that continues across a
// reset can receive a new message under an id one of its old messages holds;
// the old message must survive.
func TestGoogleReusedMessageIDInAContinuedThreadKeepsBothMessages(t *testing.T) {
	script := &idsScript{}
	harness := i01NewHarness(t, script, nil)
	_, _, karl := seedOldPhone(t, script, harness)
	script.add("reset", idsConversationEvent("2873", "direct", "Shoshana W.", idsShoshana))
	script.add("new-karl", idsConversationEvent("4100", "direct", "Karl", idsKarl))
	idsRun(t, harness, "reset")
	idsRun(t, harness, "new-karl")
	if got := idsBound(t, harness, "4100"); got.ConversationID != karl.ConversationID {
		t.Fatalf("4100 bound to %q, want Karl's thread", got.ConversationID)
	}

	later := idsAnimalsTimeMS + 30*24*3_600_000
	script.add("reused-id", idsIncoming("4100", "700", idsKarl, "a different message", later))
	idsRun(t, harness, "reused-id")
	if got := idsMessageCount(t, harness, karl.ConversationID); got != 2 {
		t.Fatalf("Karl thread messages = %d, want both the old and the new message 700", got)
	}
	var oldBody string
	database := i01OpenInspector(t, harness.path)
	defer database.Close()
	if err := database.QueryRow(
		`SELECT body FROM messages WHERE conversation_id = ? AND remote_message_id LIKE ?`,
		karl.ConversationID, sqlite.DisplacedRemoteIDPrefix+"700:%",
	).Scan(&oldBody); err != nil || oldBody != "Vote Racine" {
		t.Fatalf("old message 700 = %q (err %v), want it kept under a displaced id", oldBody, err)
	}
	if got := harness.counters.Snapshot(i01AccountID).RemoteMessageIDsRetired; got != 1 {
		t.Fatalf("remote_message_ids_retired = %d, want 1", got)
	}

	// The same new message re-delivered seconds later is one message.
	script.add("reused-id-again", idsIncoming("4100", "700", idsKarl, "a different message", later+5_000))
	idsRun(t, harness, "reused-id-again")
	if got := idsMessageCount(t, harness, karl.ConversationID); got != 2 {
		t.Fatalf("after a re-delivery, Karl thread messages = %d, want 2", got)
	}
	if got := harness.counters.Snapshot(i01AccountID).RemoteMessageIDsRetired; got != 1 {
		t.Fatalf("re-delivery retired another id: %d", got)
	}
}
