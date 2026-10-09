package ingest

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

// The live path used to key every roster entry of a Google conversation
// snapshot and quarantine the whole frame when one had no number (libgm
// delivers these) or one IdentityKey rejects. The snapshot's kind, title and
// roster were then lost. A group v2 first saw through an incoming message
// stayed a direct thread with the sender as its peer, and history (#200)
// skipped every fetched message of it as contradicting that thread. Third
// review of #200, finding F1.

const lrLee = "Lee (no number)"

// lrNumberless is a roster entry the phone sent without a number: libgm
// fills the participant ID and the name, not the number.
func lrNumberless(name string, isMe bool, participantID string) *gmproto.Participant {
	return &gmproto.Participant{
		FullName: name,
		IsMe:     isMe,
		ID:       &gmproto.SmallInfo{ParticipantID: participantID},
	}
}

// lrUnkeyable is a roster entry whose number v2keys rejects (a "+" with no
// digits).
func lrUnkeyable(number string, isMe bool) *gmproto.Participant {
	return &gmproto.Participant{
		FullName: "Unknown",
		IsMe:     isMe,
		ID:       &gmproto.SmallInfo{Number: number},
	}
}

// lrWith returns a copy of conversation with extra roster entries appended.
func lrWith(conversation *gmproto.Conversation, extra ...*gmproto.Participant) *gmproto.Conversation {
	copied := proto.Clone(conversation).(*gmproto.Conversation)
	copied.Participants = append(copied.Participants, extra...)
	return copied
}

// lrParticipantRows renders every participant row, keyed by conversation and
// then identity.
func lrParticipantRows(t *testing.T, h *hwtHarness) map[string]map[string]string {
	t.Helper()
	database := i01OpenInspector(t, h.path)
	defer database.Close()
	rows, err := database.Query(`
		SELECT conversation_id, identity_id, role, display_name, is_active,
		       coalesce(joined_at_ms, -1), coalesce(left_at_ms, -1)
		FROM conversation_participants`)
	if err != nil {
		t.Fatalf("query participants: %v", err)
	}
	defer rows.Close()
	byConversation := make(map[string]map[string]string)
	for rows.Next() {
		var conversationID, identityID, role, name string
		var active bool
		var joined, left int64
		if err := rows.Scan(&conversationID, &identityID, &role, &name, &active, &joined, &left); err != nil {
			t.Fatalf("scan participant: %v", err)
		}
		if byConversation[conversationID] == nil {
			byConversation[conversationID] = make(map[string]string)
		}
		byConversation[conversationID][identityID] = fmt.Sprintf(
			"role=%s name=%q active=%v joined=%d left=%d", role, name, active, joined, left,
		)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("participant rows: %v", err)
	}
	return byConversation
}

func lrIdentityID(number string) string {
	key, err := v2keys.IdentityKey(i01AccountID, "google", number)
	if err != nil {
		panic(err)
	}
	return v2keys.DeriveID("identity", i01AccountID, key.Kind+"\x1f"+key.Canonical)
}

// The reviewer's scenario. Karl's message is the first v2 sees of the group,
// so v2 stores a direct thread with Karl as its peer. The group's live
// snapshot lists a member with no number. It must correct that thread to
// the group, with every member that has a number, instead of being
// quarantined. History for the thread then imports: the frame fetched
// before the correction once it is fetched again, and new frames at once.
func TestLiveGroupSnapshotWithANumberlessMemberCorrectsAMessageMintedThread(t *testing.T) {
	h := newHWTHarness(t, false)
	h.liveMessage(t, hwtMessage{id: "lr-g-0", conversation: "lr-g", body: "first", from: hwtKarl, at: hwtStart.Add(-3 * time.Hour)}.proto())
	h.pump(t)
	minted := h.conversation(t, "lr-g")
	if minted.Kind != "direct" {
		t.Fatalf("fixture: message-minted thread kind = %q, want direct", minted.Kind)
	}
	if got := h.peers(t, minted.ConversationID); !reflect.DeepEqual(got, []string{hwtKarl}) {
		t.Fatalf("fixture: message-minted thread peers = %v, want Karl", got)
	}

	group := lrWith(hwtConversation("lr-g", "Weekend", true, hwtKarl, hwtShoshana), lrNumberless(lrLee, false, "7"))

	// Fetched before the live snapshot lands: the thread is still direct, so
	// history skips the frame (#200's stale-binding rule).
	early := h.historyMessage(t, group, hwtMessage{id: "lr-g-1", conversation: "lr-g", body: "fetched early", from: hwtShoshana, at: hwtStart.Add(-2 * time.Hour)}.proto())
	h.pump(t)
	if counts := h.counts(); counts.HistoryImported != 0 || counts.HistorySkipped != 2 {
		t.Fatalf("counters before the correction = %+v, want the frame skipped", counts)
	}

	h.liveConversation(t, group)
	h.pump(t)
	counts := h.counts()
	if counts.Quarantined != 0 {
		t.Fatalf("quarantined = %d, want the snapshot applied", counts.Quarantined)
	}
	if counts.RemoteRebinds != 0 {
		t.Fatalf("remote_rebinds = %d, want the thread corrected in place", counts.RemoteRebinds)
	}
	corrected := h.conversation(t, "lr-g")
	if corrected.ConversationID != minted.ConversationID {
		t.Fatalf("lr-g moved from %s to %s, want the same thread corrected", minted.ConversationID, corrected.ConversationID)
	}
	if corrected.Kind != "group" || corrected.Title != "Weekend" {
		t.Fatalf("thread = kind %q title %q, want group Weekend", corrected.Kind, corrected.Title)
	}
	if got := h.peers(t, corrected.ConversationID); !reflect.DeepEqual(got, []string{hwtKarl, hwtShoshana}) {
		t.Fatalf("peers = %v, want Karl and Shoshana", got)
	}
	if got := h.message(t, "lr-g", "lr-g-0").Body; got != "first" {
		t.Fatalf("Karl's message body = %q, want it still in the thread", got)
	}

	// The early frame, fetched again, now imports; so does a new one.
	early.ReceivedAt = h.tick()
	h.appendHistory(t, early)
	h.historyMessage(t, group, hwtMessage{id: "lr-g-2", conversation: "lr-g", body: "fetched later", at: hwtStart.Add(-time.Hour)}.proto())
	h.pump(t)
	counts = h.counts()
	if counts.HistoryImported != 2 {
		t.Fatalf("history_imported = %d, want both fetched messages (counters %+v)", counts.HistoryImported, counts)
	}
	for id, body := range map[string]string{"lr-g-1": "fetched early", "lr-g-2": "fetched later"} {
		message := h.message(t, "lr-g", id)
		if message.Body != body || message.ConversationID != corrected.ConversationID {
			t.Fatalf("%s = %q in %s, want %q in the group", id, message.Body, message.ConversationID, body)
		}
	}
	if counts.Quarantined != 0 {
		t.Fatalf("quarantined = %d after history", counts.Quarantined)
	}
}

// The shape seen on the live inbox (4 of 3,810 snapshots, 2026-08-08 to
// 2026-10-09): a 1:1 thread whose snapshot lists the account itself five
// times, four of them without a number. It must apply exactly as the same
// snapshot without those four entries does.
func TestLiveSnapshotListingSelfWithoutANumberAppliesLikeTheCleanOne(t *testing.T) {
	clean := hwtConversation("lr-obs", "Ada Lovelace", false, hwtAda)
	observed := &gmproto.Conversation{
		ConversationID: "lr-obs",
		Name:           "Ada Lovelace",
		Participants: []*gmproto.Participant{
			lrNumberless(hwtNames[hwtSelf], true, "1234"),
			lrNumberless(hwtNames[hwtSelf], true, "1"),
			lrNumberless(hwtNames[hwtSelf], true, "5678"),
			hwtParticipant(hwtSelf),
			lrNumberless(hwtNames[hwtSelf], true, "1"),
			hwtParticipant(hwtAda),
		},
	}
	run := func(snapshot *gmproto.Conversation) *hwtHarness {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-obs", "Ada", false, hwtAda))
		h.liveMessage(t, hwtMessage{id: "lr-obs-0", conversation: "lr-obs", body: "hi", from: hwtAda, at: hwtStart.Add(-time.Hour)}.proto())
		h.liveConversation(t, snapshot)
		h.pump(t)
		return h
	}
	want, got := run(clean), run(observed)
	if counts := got.counts(); counts.Quarantined != 0 {
		t.Fatalf("quarantined = %d, want the observed snapshot applied", counts.Quarantined)
	}
	if title := got.conversation(t, "lr-obs").Title; title != "Ada Lovelace" {
		t.Fatalf("title = %q, want the snapshot's", title)
	}
	for _, table := range []string{"conversations", "conversation_participants", "identities", "messages"} {
		order := map[string]string{
			"conversations":             "conversation_id",
			"conversation_participants": "conversation_id, identity_id",
			"identities":                "identity_id",
			"messages":                  "message_id",
		}[table]
		query := "SELECT * FROM " + table + " ORDER BY " + order
		if a, b := want.rows(t, query), got.rows(t, query); !sameRows(a, b) {
			t.Fatalf("%s differs from the clean snapshot's:\n clean    %v\n observed %v", table, a, b)
		}
	}
}

// A stored member the snapshot doesn't name stays when it may be one of the
// entries the snapshot left out: any member when a left-out entry isn't
// flagged as the account, only the account's own rows when every left-out
// entry is. A complete snapshot still replaces the roster.
func TestLiveSnapshotWithUnaddressableEntriesKeepsTheMembersTheyMayBe(t *testing.T) {
	t.Run("1:1 whose peer has no number keeps its peer", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-d", "Ada", false, hwtAda))
		h.pump(t)
		thread := h.conversation(t, "lr-d")

		h.liveConversation(t, &gmproto.Conversation{
			ConversationID: "lr-d",
			Name:           "Ada L.",
			Participants:   []*gmproto.Participant{hwtParticipant(hwtSelf), lrNumberless("Ada L.", false, "9")},
		})
		h.pump(t)
		if counts := h.counts(); counts.Quarantined != 0 || counts.RemoteRebinds != 0 {
			t.Fatalf("counters = %+v, want the snapshot applied in place", counts)
		}
		after := h.conversation(t, "lr-d")
		if after.ConversationID != thread.ConversationID || after.Title != "Ada L." {
			t.Fatalf("thread = %s %q, want %s retitled", after.ConversationID, after.Title, thread.ConversationID)
		}
		if got := h.peers(t, after.ConversationID); !reflect.DeepEqual(got, []string{hwtAda}) {
			t.Fatalf("peers = %v, want Ada kept", got)
		}
	})

	t.Run("group keeps an unnamed member until a complete snapshot", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-fam", "Family", true, hwtKarl, hwtShoshana, hwtBea))
		h.pump(t)
		family := h.conversation(t, "lr-fam").ConversationID

		renamed := hwtConversation("lr-fam", "Family", true, hwtKarl, hwtShoshana)
		renamed.Participants[1].FullName = "Karl K."
		h.liveConversation(t, lrWith(renamed, lrUnkeyable("+", false), lrNumberless(lrLee, false, "7")))
		h.pump(t)
		if got := h.peers(t, family); !reflect.DeepEqual(got, []string{hwtKarl, hwtBea, hwtShoshana}) {
			t.Fatalf("peers after an incomplete snapshot = %v, want Bea kept", got)
		}
		rows := lrParticipantRows(t, h)[family]
		if row := rows[lrIdentityID(hwtKarl)]; !strings.Contains(row, `name="Karl K."`) {
			t.Fatalf("Karl's row = %s, want the snapshot's name", row)
		}

		h.liveConversation(t, hwtConversation("lr-fam", "Family", true, hwtKarl, hwtShoshana))
		h.pump(t)
		if got := h.peers(t, family); !reflect.DeepEqual(got, []string{hwtKarl, hwtShoshana}) {
			t.Fatalf("peers after a complete snapshot = %v, want Bea removed", got)
		}
		if counts := h.counts(); counts.Quarantined != 0 {
			t.Fatalf("quarantined = %d", counts.Quarantined)
		}
	})

	t.Run("left-out entries flagged as the account say nothing about peers", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-club", "Club", true, hwtKarl, hwtShoshana, hwtBea))
		h.pump(t)
		club := h.conversation(t, "lr-club").ConversationID

		h.liveConversation(t, lrWith(
			hwtConversation("lr-club", "Club", true, hwtKarl, hwtShoshana),
			lrNumberless(hwtNames[hwtSelf], true, "1234"),
			lrNumberless(hwtNames[hwtSelf], true, "1"),
		))
		h.pump(t)
		if got := h.peers(t, club); !reflect.DeepEqual(got, []string{hwtKarl, hwtShoshana}) {
			t.Fatalf("peers = %v, want Bea removed: only the account's entries were left out", got)
		}
		if _, ok := lrParticipantRows(t, h)[club][lrIdentityID(hwtSelf)]; !ok {
			t.Fatalf("the account's own row is gone, want it kept")
		}
	})

	t.Run("the account's own row stays when its only entry has no number", func(t *testing.T) {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-s", "Ada", false, hwtAda))
		h.pump(t)
		thread := h.conversation(t, "lr-s").ConversationID

		h.liveConversation(t, &gmproto.Conversation{
			ConversationID: "lr-s",
			Name:           "Ada",
			Participants:   []*gmproto.Participant{lrNumberless(hwtNames[hwtSelf], true, "1"), hwtParticipant(hwtAda)},
		})
		h.pump(t)
		rows := lrParticipantRows(t, h)[thread]
		if _, ok := rows[lrIdentityID(hwtSelf)]; !ok || len(rows) != 2 {
			t.Fatalf("roster = %v, want the account and Ada", rows)
		}
	})
}

// #176's rebinding (googleConversationEventTarget) must judge the same
// entries the roster write keeps. zz-7 is bound to Ada's thread; on the phone
// it is now Bea's. Bea's snapshot also lists a "+" entry and a number-less
// one. It must move the binding exactly as Bea's clean snapshot does, instead
// of being quarantined and leaving the stale binding in place.
func TestLiveRebindJudgesTheAddressableRoster(t *testing.T) {
	run := func(snapshot *gmproto.Conversation) *hwtHarness {
		h := newHWTHarness(t, false)
		h.liveConversation(t, hwtConversation("lr-7", "Ada", false, hwtAda))
		h.liveConversation(t, hwtConversation("lr-bea", "Bea", false, hwtBea))
		h.liveMessage(t, hwtMessage{id: "lr-7-0", conversation: "lr-7", body: "from Ada", from: hwtAda, at: hwtStart.Add(-time.Hour)}.proto())
		h.liveConversation(t, snapshot)
		h.pump(t)
		return h
	}
	clean := hwtConversation("lr-7", "Bea", false, hwtBea)
	want := run(clean)
	got := run(lrWith(clean, lrUnkeyable("+", false), lrNumberless("Bea", false, "4")))

	if counts := got.counts(); counts.Quarantined != 0 {
		t.Fatalf("quarantined = %d, want the snapshot applied", counts.Quarantined)
	}
	if a, b := want.counts().RemoteRebinds, got.counts().RemoteRebinds; a != b {
		t.Fatalf("remote_rebinds = %d, want %d (the clean snapshot's)", b, a)
	}
	if peers := got.peers(t, got.conversation(t, "lr-7").ConversationID); !reflect.DeepEqual(peers, []string{hwtBea}) {
		t.Fatalf("lr-7 now resolves to a thread with peers %v, want Bea's", peers)
	}
	for _, query := range []string{
		"SELECT * FROM conversations ORDER BY conversation_id",
		"SELECT * FROM conversation_participants ORDER BY conversation_id, identity_id",
		"SELECT * FROM messages ORDER BY message_id",
	} {
		if a, b := want.rows(t, query), got.rows(t, query); !sameRows(a, b) {
			t.Fatalf("%q differs from the clean snapshot's:\n clean %v\n got   %v", query, a, b)
		}
	}
}

// ---------------------------------------------------------------------------
// Property: a live snapshot with unaddressable entries applies as the same
// snapshot without them, except that it removes no stored member.

var lrPool = []string{hwtKarl, hwtShoshana, hwtAda, hwtBea}

// lrEntry is one roster entry of a generated snapshot.
type lrEntry struct {
	// Peer indexes lrPool; -1 is the account itself.
	Peer int
	// Bad, when not lrAddressable, makes the entry unaddressable.
	Bad int
	// Formatted sends the number only as the formatted number (addressable;
	// the decoder falls back to it).
	Formatted bool
	Renamed   bool
	// Unflagged lists the account's own number without the self flag (seen
	// live on 2026-08-01, PR #160).
	Unflagged bool
}

const (
	lrAddressable = iota
	lrBadNoNumber
	lrBadPlusOnly
	lrBadBlank
	lrBadKinds
)

type lrCase struct {
	// Setup is how v2 first knew wire lr-w: 0 unknown, 1 an incoming live
	// message from SetupPeers[0], 2 a clean 1:1 snapshot with
	// SetupPeers[0], 3 a clean group snapshot with SetupPeers, 4 an
	// outgoing live message.
	Setup      int
	SetupPeers []int
	// Others seeds a 1:1 thread for each pool peer under its own wire, so a
	// snapshot can rebind lr-w to one of them.
	Others  bool
	Group   bool
	Title   int
	Entries []lrEntry
}

func (lrCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := lrCase{
		// Threads v2 already knows with a roster (setups 2 and 3) are where
		// the keep rule matters, so they get half the cases.
		Setup:  []int{0, 1, 2, 2, 3, 3, 3, 4}[r.Intn(8)],
		Others: r.Intn(2) == 0,
		Group:  r.Intn(2) == 0,
		Title:  r.Intn(3),
	}
	peers := r.Perm(len(lrPool))
	c.SetupPeers = peers[:2+r.Intn(len(lrPool)-1)]
	if c.Setup != 3 {
		c.SetupPeers = c.SetupPeers[:1]
	}
	if r.Intn(3) != 0 {
		c.Entries = append(c.Entries, lrEntry{Peer: -1})
	}
	named := 1 + r.Intn(3)
	if !c.Group {
		named = r.Intn(2) + r.Intn(2)*r.Intn(2) // mostly 0 or 1, sometimes 2
	}
	order := r.Perm(len(lrPool))
	for _, index := range order[:min(named, len(order))] {
		c.Entries = append(c.Entries, lrEntry{Peer: index, Formatted: r.Intn(5) == 0, Renamed: r.Intn(2) == 0})
	}
	// Consistent duplicates: an entry listed twice, the account's number
	// listed again without the self flag.
	if len(c.Entries) > 0 && r.Intn(3) == 0 {
		c.Entries = append(c.Entries, c.Entries[r.Intn(len(c.Entries))])
	}
	if r.Intn(4) == 0 {
		c.Entries = append(c.Entries, lrEntry{Peer: -1, Unflagged: true})
	}
	// Left-out entries: in a third of the cases all flagged as the account
	// (the only shape seen live), otherwise each one is the account or a
	// peer with equal odds.
	selfOnly := r.Intn(3) == 0
	for bad := r.Intn(4); bad > 0; bad-- {
		entry := lrEntry{Peer: -1, Bad: 1 + r.Intn(lrBadKinds-1)}
		if !selfOnly && r.Intn(2) == 0 {
			entry.Peer = r.Intn(len(lrPool))
		}
		c.Entries = append(c.Entries, entry)
	}
	r.Shuffle(len(c.Entries), func(i, j int) { c.Entries[i], c.Entries[j] = c.Entries[j], c.Entries[i] })
	return reflect.ValueOf(c)
}

func (c lrCase) participant(entry lrEntry) *gmproto.Participant {
	number, isMe := hwtSelf, !entry.Unflagged
	if entry.Peer >= 0 {
		number, isMe = lrPool[entry.Peer], false
	}
	name := hwtNames[number]
	if entry.Renamed {
		name += " (renamed)"
	}
	participant := &gmproto.Participant{FullName: name, IsMe: isMe, ID: &gmproto.SmallInfo{ParticipantID: fmt.Sprint(entry.Peer + 2)}}
	switch entry.Bad {
	case lrBadNoNumber:
	case lrBadPlusOnly:
		participant.ID.Number = "+"
	case lrBadBlank:
		participant.ID.Number = "   "
	default:
		if entry.Formatted {
			participant.FormattedNumber = number
		} else {
			participant.ID.Number = number
		}
	}
	return participant
}

// snapshot builds the generated snapshot, with or without its unaddressable
// entries.
func (c lrCase) snapshot(withBad bool) *gmproto.Conversation {
	conversation := &gmproto.Conversation{
		ConversationID: "lr-w",
		Name:           []string{"Weekend", "Ada", ""}[c.Title],
		IsGroupChat:    c.Group,
	}
	for _, entry := range c.Entries {
		if entry.Bad != lrAddressable && !withBad {
			continue
		}
		conversation.Participants = append(conversation.Participants, c.participant(entry))
	}
	return conversation
}

// badSelf counts the unaddressable entries flagged as the account.
func (c lrCase) badSelf() int {
	count := 0
	for _, entry := range c.Entries {
		if entry.Bad != lrAddressable && entry.Peer < 0 && !entry.Unflagged {
			count++
		}
	}
	return count
}

func (c lrCase) bad() int {
	count := 0
	for _, entry := range c.Entries {
		if entry.Bad != lrAddressable {
			count++
		}
	}
	return count
}

// setUp gives a harness the state the case starts from.
func (c lrCase) setUp(t *testing.T, h *hwtHarness) {
	t.Helper()
	if c.Others {
		for index, number := range lrPool {
			h.liveConversation(t, hwtConversation(fmt.Sprintf("lr-other-%d", index), hwtNames[number], false, number))
		}
	}
	first := lrPool[c.SetupPeers[0]]
	switch c.Setup {
	case 1:
		h.liveMessage(t, hwtMessage{id: "lr-w-0", conversation: "lr-w", body: "first", from: first, at: hwtStart.Add(-3 * time.Hour)}.proto())
	case 2:
		h.liveConversation(t, hwtConversation("lr-w", hwtNames[first], false, first))
	case 3:
		numbers := make([]string, 0, len(c.SetupPeers))
		for _, index := range c.SetupPeers {
			numbers = append(numbers, lrPool[index])
		}
		h.liveConversation(t, hwtConversation("lr-w", "Old group", true, numbers...))
	case 4:
		h.liveMessage(t, hwtMessage{id: "lr-w-0", conversation: "lr-w", body: "first", at: hwtStart.Add(-3 * time.Hour)}.proto())
	}
	h.pump(t)
}

// For random rosters mixing addressable entries (some only as a formatted
// number, some renamed, some listed twice) with entries that have no
// number, a "+" with no digits or a blank number, applied to a wire v2
// didn't know, knew from an incoming or outgoing message, or knew as a 1:1
// or a group (with or without other 1:1 threads it could rebind to):
//
//   - P1: nothing is quarantined.
//   - P2: the binding, every conversation row, every identity, every message
//     and the rebind count equal those of the same snapshot without the
//     unaddressable entries.
//   - P3: the roster of the thread the snapshot lands on is that snapshot's
//     roster plus each member stored before that it doesn't name and that a
//     left-out entry may be (any member when a left-out entry isn't flagged
//     as the account, the account's own row otherwise); every other
//     thread's roster is unchanged.
//   - P4: a complete snapshot's roster is exactly its addressable entries.
//   - P5: a history frame fetched afterwards with the snapshot imports its
//     message.
func TestLiveRosterPropertyUnaddressableEntriesChangeNothingElse(t *testing.T) {
	covered := make(map[string]int)
	property := func(c lrCase) bool {
		if err := lrCheck(t, c, covered); err != nil {
			t.Logf("counterexample %+v:\n%v", c, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 120, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
	// The generator must keep reaching the cases the keep rule is about.
	for _, class := range []string{
		"self-only left out, unnamed stored peer removed",
		"peer left out, unnamed stored member kept",
		"left out, unnamed stored self row kept",
		"complete, unnamed stored member removed",
		"rebound",
		"direct corrected to group",
	} {
		if covered[class] == 0 {
			t.Errorf("no generated case covered %q (coverage %v)", class, covered)
		}
	}
	t.Logf("coverage: %v", covered)
}

func lrCheck(t *testing.T, c lrCase, covered map[string]int) error {
	full := openHWTHarness(t, t.TempDir(), false)
	defer full.stop()
	clean := openHWTHarness(t, t.TempDir(), false)
	defer clean.stop()
	c.setUp(t, full)
	c.setUp(t, clean)
	before := lrParticipantRows(t, full)
	prior, priorErr := full.store.GetConversationByRemote(i01AccountID, "lr-w")

	full.liveConversation(t, c.snapshot(true))
	clean.liveConversation(t, c.snapshot(false))
	full.pump(t)
	clean.pump(t)

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// P1
	if q := full.counts().Quarantined; q != 0 {
		fail("P1: quarantined = %d", q)
	}
	if q := clean.counts().Quarantined; q != 0 {
		fail("P1: the clean snapshot was quarantined (%d)", q)
	}
	// P2
	if a, b := clean.counts().RemoteRebinds, full.counts().RemoteRebinds; a != b {
		fail("P2: remote_rebinds = %d, clean %d", b, a)
	}
	for _, query := range []string{
		"SELECT * FROM conversations ORDER BY conversation_id",
		"SELECT * FROM identities ORDER BY identity_id",
		"SELECT * FROM messages ORDER BY message_id",
	} {
		if a, b := clean.rows(t, query), full.rows(t, query); !sameRows(a, b) {
			fail("P2: %q differs:\n  clean %v\n  full  %v", query, a, b)
		}
	}
	target, err := full.store.GetConversationByRemote(i01AccountID, "lr-w")
	if err != nil {
		fail("P2: lr-w is unbound after its snapshot: %v", err)
		return errors.New(strings.Join(problems, "\n"))
	}
	if full.counts().RemoteRebinds > 0 {
		covered["rebound"]++
	}
	if priorErr == nil && prior.Kind == "direct" && target.ConversationID == prior.ConversationID && target.Kind == "group" {
		covered["direct corrected to group"]++
	}
	// P3
	cleanRows, fullRows := lrParticipantRows(t, clean), lrParticipantRows(t, full)
	conversations := make(map[string]struct{})
	for id := range cleanRows {
		conversations[id] = struct{}{}
	}
	for id := range fullRows {
		conversations[id] = struct{}{}
	}
	for id := range conversations {
		want := make(map[string]string, len(cleanRows[id]))
		for identity, row := range cleanRows[id] {
			want[identity] = row
		}
		if id == target.ConversationID {
			peerLeftOut := c.bad() > c.badSelf()
			for identity, row := range before[id] {
				if _, named := want[identity]; named {
					continue
				}
				self := identity == lrIdentityID(hwtSelf)
				switch {
				case c.bad() == 0:
					covered["complete, unnamed stored member removed"]++
				case peerLeftOut:
					covered["peer left out, unnamed stored member kept"]++
					want[identity] = row
				case self:
					covered["left out, unnamed stored self row kept"]++
					want[identity] = row
				default:
					covered["self-only left out, unnamed stored peer removed"]++
				}
			}
		}
		if !reflect.DeepEqual(want, fullRows[id]) && !(len(want) == 0 && len(fullRows[id]) == 0) {
			fail("P3: roster of %s (target %v) = %v, want %v", id, id == target.ConversationID, fullRows[id], want)
		}
	}
	// P4
	wantIDs := make([]string, 0)
	seen := make(map[string]bool)
	for _, entry := range c.Entries {
		if entry.Bad != lrAddressable {
			continue
		}
		number := hwtSelf
		if entry.Peer >= 0 {
			number = lrPool[entry.Peer]
		}
		if id := lrIdentityID(number); !seen[id] {
			seen[id] = true
			wantIDs = append(wantIDs, id)
		}
	}
	gotIDs := make([]string, 0, len(cleanRows[target.ConversationID]))
	for identity := range cleanRows[target.ConversationID] {
		gotIDs = append(gotIDs, identity)
	}
	sort.Strings(wantIDs)
	sort.Strings(gotIDs)
	if !reflect.DeepEqual(wantIDs, gotIDs) && !(len(wantIDs) == 0 && len(gotIDs) == 0) {
		fail("P4: the complete snapshot's roster = %v, want exactly %v", gotIDs, wantIDs)
	}
	// P5
	from := ""
	for _, entry := range c.Entries {
		if entry.Bad == lrAddressable && entry.Peer >= 0 {
			from = lrPool[entry.Peer]
			break
		}
	}
	fetched := hwtMessage{id: "lr-w-h", conversation: "lr-w", body: "fetched", from: from, at: hwtStart.Add(-time.Hour)}.proto()
	full.historyMessage(t, c.snapshot(true), fetched)
	clean.historyMessage(t, c.snapshot(false), fetched)
	full.pump(t)
	clean.pump(t)
	if a, b := clean.counts().HistoryImported, full.counts().HistoryImported; a != 1 || b != 1 {
		fail("P5: history_imported = %d, clean %d, want 1 each", b, a)
	}
	if a, b := clean.rows(t, "SELECT * FROM messages ORDER BY message_id"), full.rows(t, "SELECT * FROM messages ORDER BY message_id"); !sameRows(a, b) {
		fail("P5: messages differ after history:\n  clean %v\n  full  %v", a, b)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}
