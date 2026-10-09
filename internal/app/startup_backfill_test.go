package app

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// The startup backfill used to store each inbox conversation's newest 20
// messages. After a stall that left more than 20 new messages in a
// conversation, that moved the conversation's boundary (its newest stored
// message) above the rest, and the recent reconcile, which pages only down to
// the boundary, never fetched them. It now catches each conversation up the way
// the reconcile does, and records what it cannot reach as a history gap.

const (
	gapConv   = "c1"
	gapBaseMS = int64(1_700_000_000_000) // the seeded boundary's time
)

// layoutGM is a phone holding conversation gapConv's messages, newest first.
// A reply starting at index i runs to the next cut after i, at most count
// messages, and carries a cursor to where it ended unless omitCursor[i]. A
// cursor naming a message (one the catch-up synthesized) continues below that
// message; with ignoreIDCursor the phone does not honour it and serves the
// newest page again. Replies from call failFrom on fail.
type layoutGM struct {
	*mockGMClient
	conv           *gmproto.Conversation
	all            []*gmproto.Message
	cuts           map[int]bool
	omitCursor     map[int]bool
	ignoreIDCursor bool
	failFrom       int
	afterFetch     func(call int)

	mu    sync.Mutex
	calls int
}

func newLayoutGM(all []*gmproto.Message) *layoutGM {
	g := &layoutGM{conv: makeConv(gapConv, "Gap"), cuts: map[int]bool{}, omitCursor: map[int]bool{}}
	g.mockGMClient = &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{g.conv}},
		},
	}
	g.setMessages(all)
	return g
}

// setMessages replaces what the phone holds and dates the conversation's last
// message to the newest of them.
func (g *layoutGM) setMessages(all []*gmproto.Message) {
	g.all = all
	g.conv.LastMessageTimestamp = 0
	for _, msg := range all {
		g.conv.LastMessageTimestamp = max(g.conv.LastMessageTimestamp, msg.GetTimestamp())
	}
}

func (g *layoutGM) pageEnd(start, count int) int {
	end := start
	for end < len(g.all) && end-start < count {
		end++
		if g.cuts[end] {
			break
		}
	}
	return end
}

func (g *layoutGM) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	if conversationID != gapConv {
		return &gmproto.ListMessagesResponse{}, nil
	}
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	if g.afterFetch != nil {
		defer g.afterFetch(call)
	}
	if g.failFrom > 0 && call >= g.failFrom {
		return nil, fmt.Errorf("phone did not answer page %d", call)
	}
	start := 0
	if id := cursor.GetLastItemID(); strings.HasPrefix(id, "at:") {
		start, _ = strconv.Atoi(strings.TrimPrefix(id, "at:"))
	} else if id != "" && !g.ignoreIDCursor {
		for index, msg := range g.all {
			if msg.GetMessageID() == id {
				start = index + 1
			}
		}
	}
	start = min(start, len(g.all))
	end := g.pageEnd(start, int(count))
	resp := &gmproto.ListMessagesResponse{Messages: g.all[start:end]}
	if end < len(g.all) && !g.omitCursor[start] {
		resp.Cursor = &gmproto.Cursor{LastItemID: fmt.Sprintf("at:%d", end)}
	}
	return resp, nil
}

func (g *layoutGM) fetchCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *layoutGM) resetCalls() {
	g.mu.Lock()
	g.calls = 0
	g.mu.Unlock()
}

// Outcomes of catchUpOutcome.
const (
	pagingReached = "reached" // a page held the boundary (or an older message)
	pagingEnd     = "end"     // an empty reply: the phone had nothing older
	pagingShort   = "short"   // the page bound, or a re-served page, stopped it first
)

// catchUpOutcome simulates how the startup backfill's or the reconcile's
// paging toward index target ends on this phone: it follows the phone's
// cursors and, where a reply carries none, continues below the page's oldest
// message (the phone re-serves its newest page instead with ignoreIDCursor).
// target < 0 means no message on the phone reaches the boundary.
func (g *layoutGM) catchUpOutcome(target int) string {
	start := 0
	for request := 1; request <= recentReconcileMaxPages; request++ {
		if start >= len(g.all) {
			return pagingEnd
		}
		end := g.pageEnd(start, recentReconcileMessageLimit)
		if target >= 0 && target < end {
			return pagingReached
		}
		if (end >= len(g.all) || g.omitCursor[start]) && g.ignoreIDCursor {
			return pagingShort
		}
		start = end
	}
	return pagingShort
}

func (g *layoutGM) indexOf(id string) int {
	for index, msg := range g.all {
		if msg.GetMessageID() == id {
			return index
		}
	}
	return -1
}

// gapMessages returns n messages newer than the boundary, newest first: a<n>
// down to a001, one second apart (pairs sharing a millisecond with sameMS),
// all after gapBaseMS.
func gapMessages(prefix string, n int, sameMS bool, aboveMS int64) []*gmproto.Message {
	messages := make([]*gmproto.Message, 0, n)
	for index := 0; index < n; index++ {
		number := n - index
		step := int64(number)
		if sameMS {
			step = int64((number + 1) / 2)
		}
		id := fmt.Sprintf("%s%03d", prefix, number)
		messages = append(messages, makeMsg(id, gapConv, id, aboveMS+step*1000))
	}
	return messages
}

func gapBoundaryMessage() *gmproto.Message {
	return makeMsg("b000", gapConv, "boundary", gapBaseMS)
}

func gapOlderMessages(n int) []*gmproto.Message {
	messages := make([]*gmproto.Message, 0, n)
	for index := 1; index <= n; index++ {
		id := fmt.Sprintf("o%03d", index)
		messages = append(messages, makeMsg(id, gapConv, id, gapBaseMS-int64(index)*1000))
	}
	return messages
}

func concatMessages(parts ...[]*gmproto.Message) []*gmproto.Message {
	var all []*gmproto.Message
	for _, part := range parts {
		all = append(all, part...)
	}
	return all
}

// seedGapBoundary stores conversation gapConv with b000 as its newest
// message, as the legacy store holds it before a restart.
func seedGapBoundary(t *testing.T, a *App) {
	t.Helper()
	if err := a.Store.UpsertConversation(&db.Conversation{ConversationID: gapConv, Name: "Gap", LastMessageTS: gapBaseMS}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := a.Store.UpsertMessage(&db.Message{MessageID: "b000", ConversationID: gapConv, Body: "boundary", TimestampMS: gapBaseMS}); err != nil {
		t.Fatalf("seed boundary: %v", err)
	}
}

func gapLegacy(t *testing.T, a *App) map[string]bool {
	t.Helper()
	messages, err := a.Store.GetMessagesByConversation(gapConv, 10_000)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	ids := map[string]bool{}
	for _, message := range messages {
		ids[message.MessageID] = true
	}
	return ids
}

func missingIDs(have map[string]bool, want []*gmproto.Message) []string {
	var missing []string
	for _, msg := range want {
		if !have[msg.GetMessageID()] {
			missing = append(missing, msg.GetMessageID())
		}
	}
	return missing
}

func newGapApp(t *testing.T, g *layoutGM, history GoogleHistoryIngress) *App {
	t.Helper()
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	a := newTestApp(t, g.mockGMClient)
	a.gmClient = g
	a.gmHistory = history
	seedGapBoundary(t, a)
	return a
}

// A restart after 50 messages arrived in a conversation the store last saw at
// b000: the startup backfill pages down to b000 and stores all 50, in both
// stores, leaving nothing for a later catch-up to miss. Before the fix it
// stored a050..a031 and moved the boundary above a030..a001.
func TestStartupBackfillFillsFiftyNewMessagesAboveTheBoundary(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}, gapOlderMessages(5)))
	v2 := hcdNewV2(t)
	a := newGapApp(t, g, &hcdIngress{sink: v2.sink})

	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	v2.drain(t)

	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v after the startup backfill", missing)
	}
	stored := hcdV2Messages(t, v2.inspect)
	for _, msg := range above {
		if stored[msg.GetMessageID()] == nil {
			t.Errorf("v2 lacks %s", msg.GetMessageID())
		}
	}
	if calls := g.fetchCalls(); calls != 2 {
		t.Errorf("pages fetched = %d, want 2 (a050..a021, then a020..b000)", calls)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Errorf("history gaps = %+v, want none", gaps)
	}

	// The next reconcile finds the boundary on its first page.
	g.resetCalls()
	a.reconcileRecentConversations("listen_recovered")
	if calls := g.fetchCalls(); calls != 1 {
		t.Errorf("reconcile pages fetched = %d, want 1", calls)
	}
}

// A conversation that needs only its newest page costs the startup backfill
// one fetch, as before.
func TestStartupBackfillFetchesOnePageWhenTheBoundaryIsOnIt(t *testing.T) {
	g := newLayoutGM(concatMessages(gapMessages("a", 3, false, gapBaseMS), []*gmproto.Message{gapBoundaryMessage()}, gapOlderMessages(40)))
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if calls := g.fetchCalls(); calls != 1 {
		t.Fatalf("pages fetched = %d, want 1", calls)
	}
	if missing := missingIDs(gapLegacy(t, a), g.all[:4]); len(missing) != 0 {
		t.Fatalf("legacy lacks %v", missing)
	}
}

// A message with no timestamp on the newest page says nothing about where the
// page is, so it does not end paging above the boundary. It used to count as
// the page's oldest message, which "reached" b000 on the first page and left
// a020..a001 out with no gap recorded.
func TestStartupBackfillIgnoresAMessageWithoutATimestampWhenFindingTheBoundary(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	above[5].Timestamp = 0
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v", missing)
	}
	if calls := g.fetchCalls(); calls != 2 {
		t.Fatalf("pages fetched = %d, want 2", calls)
	}
}

// A phone that sends no cursors still gets paged down to the boundary: the
// catch-up continues below each page's oldest message.
func TestStartupBackfillContinuesWhenRepliesHaveNoCursor(t *testing.T) {
	above := gapMessages("a", 70, true, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	for index := range g.all {
		g.omitCursor[index] = true
	}
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v", missing)
	}
	if calls := g.fetchCalls(); calls != 3 {
		t.Errorf("pages fetched = %d, want 3", calls)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Errorf("history gaps = %+v, want none", gaps)
	}
}

// A phone that sends no cursor and serves its newest page again for a
// synthesized one: the repeat brings nothing new, which ends paging after two
// fetches, and what lies between is recorded as a gap.
func TestStartupBackfillStopsWhenThePhoneRepeatsAPage(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	g.omitCursor[0] = true
	g.ignoreIDCursor = true
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if calls := g.fetchCalls(); calls != 2 {
		t.Fatalf("pages fetched = %d, want 2", calls)
	}
	gaps := a.GoogleHistoryGaps()
	if gaps == nil || gaps.Count != 1 || gaps.Gaps[0].Reason != googleHistoryGapNoOlderPage || gaps.Gaps[0].Stored != 30 {
		t.Fatalf("history gaps = %+v, want one no_older_page gap above 30 stored messages", gaps)
	}
}

// More new messages than the reconcile's reach: the startup backfill stores
// the newest 120, records the gap below them (in the log, in /api/status, and
// in the data dir, where a restart finds it), and a window backfill from the
// recorded since_ms fills the gap and clears the record.
func TestStartupBackfillRecordsAGapItCannotReachUntilAWindowBackfillFillsIt(t *testing.T) {
	above := gapMessages("a", 150, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}, gapOlderMessages(3)))
	ingress := &hcdIngress{}
	a := newGapApp(t, g, ingress)
	a.DataDir = t.TempDir()

	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if calls := g.fetchCalls(); calls != recentReconcileMaxPages {
		t.Fatalf("pages fetched = %d, want %d", calls, recentReconcileMaxPages)
	}
	legacy := gapLegacy(t, a)
	if missing := missingIDs(legacy, above[:120]); len(missing) != 0 {
		t.Fatalf("legacy lacks %v of the newest 120", missing)
	}
	if missing := missingIDs(legacy, above[120:]); len(missing) != 30 {
		t.Fatalf("legacy lacks %d of a030..a001, want 30 (beyond reach)", len(missing))
	}

	want := GoogleHistoryGap{
		ConversationID: gapConv,
		AfterMS:        gapBaseMS,
		AfterID:        "b000",
		BeforeMS:       above[119].GetTimestamp() / 1000,
		Stored:         120,
		Reason:         googleHistoryGapPageLimit,
		Source:         "startup_backfill",
	}
	status := a.GoogleStatus().HistoryGaps
	if status == nil || status.Count != 1 || status.SinceMS != gapBaseMS || len(status.Gaps) != 1 {
		t.Fatalf("status history_gaps = %+v, want one gap since %d", status, gapBaseMS)
	}
	got := status.Gaps[0]
	if got.DetectedMS == 0 || status.LastDetectedMS != got.DetectedMS {
		t.Errorf("detected_ms = %d, last_detected_ms = %d", got.DetectedMS, status.LastDetectedMS)
	}
	got.DetectedMS = 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gap = %+v, want %+v", got, want)
	}

	// A restart reads it back; the next startup backfill could not have found
	// it, because the boundary is now a150.
	restarted := &App{DataDir: a.DataDir, Logger: zerolog.Nop()}
	if again := restarted.GoogleHistoryGaps(); again == nil || again.Count != 1 || again.Gaps[0].AfterMS != gapBaseMS {
		t.Fatalf("history gaps after a restart = %+v, want the recorded gap", again)
	}
	g.resetCalls()
	a.reconcileRecentConversations("listen_recovered")
	if calls := g.fetchCalls(); calls != 1 {
		t.Fatalf("reconcile pages fetched = %d, want 1 (the boundary is a150)", calls)
	}
	if missing := missingIDs(gapLegacy(t, a), above[120:]); len(missing) != 30 {
		t.Fatalf("the reconcile filled the gap (%d still missing)?", len(missing))
	}

	// A window backfill from a later instant (a120's) neither reaches nor
	// clears it.
	runWindow(t, a, time.UnixMilli(above[30].GetTimestamp()/1000))
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps after a later window = %+v, want the gap kept", gaps)
	}
	if missing := missingIDs(gapLegacy(t, a), above[120:]); len(missing) != 30 {
		t.Fatalf("the later window reached into the gap (%d still missing)", len(missing))
	}
	// One from since_ms does, and clears it.
	runWindow(t, a, time.UnixMilli(status.SinceMS))
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v after the window backfill", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps after the window backfill = %+v, want none", gaps)
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, googleHistoryGapsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gap file after clearing: %v, want removed", err)
	}
}

// A window backfill that did not finish cleanly leaves the gap recorded.
func TestHistoryGapSurvivesAWindowBackfillWithErrors(t *testing.T) {
	above := gapMessages("a", 150, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	g.failFrom = g.fetchCalls() + 2 // the window's second page fails
	runWindow(t, a, time.UnixMilli(gapBaseMS))
	if progress := a.GetBackfillProgress(); progress.Errors == 0 {
		t.Fatalf("window progress = %+v, want an error", progress)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps = %+v, want the gap kept", gaps)
	}
}

// The recent reconcile shares the step, so it records the gap it leaves too.
func TestRecentReconcileRecordsAGapItCannotReach(t *testing.T) {
	above := gapMessages("a", 121, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	a := newGapApp(t, g, &hcdIngress{})
	a.reconcileRecentConversations("listen_recovered")
	gaps := a.GoogleHistoryGaps()
	if gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps = %+v, want one", gaps)
	}
	if gap := gaps.Gaps[0]; gap.Source != "reconcile_listen_recovered" || gap.Reason != googleHistoryGapPageLimit || gap.BeforeMS != above[119].GetTimestamp()/1000 {
		t.Fatalf("gap = %+v", gap)
	}
}

// A later page failing stores the earlier pages and records the gap below
// them; a first page failing stores nothing and records nothing.
func TestStartupBackfillRecordsAGapWhenALaterPageFails(t *testing.T) {
	for _, tc := range []struct {
		failFrom   int
		wantStored int
	}{{failFrom: 2, wantStored: 30}, {failFrom: 1, wantStored: 0}} {
		above := gapMessages("a", 50, false, gapBaseMS)
		g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
		g.failFrom = tc.failFrom
		a := newGapApp(t, g, &hcdIngress{})
		if err := a.Backfill(); err != nil {
			t.Fatalf("failFrom %d: Backfill(): %v", tc.failFrom, err)
		}
		if stored := 50 - len(missingIDs(gapLegacy(t, a), above)); stored != tc.wantStored {
			t.Fatalf("failFrom %d: stored %d, want %d", tc.failFrom, stored, tc.wantStored)
		}
		gaps := a.GoogleHistoryGaps()
		if tc.wantStored == 0 {
			if gaps != nil {
				t.Fatalf("failFrom %d: history gaps = %+v, want none", tc.failFrom, gaps)
			}
			continue
		}
		if gaps == nil || gaps.Gaps[0].Reason != googleHistoryGapFetchError {
			t.Fatalf("failFrom %d: history gaps = %+v, want one fetch_error gap", tc.failFrom, gaps)
		}
	}
}

// A startup backfill whose generation ends partway through storing leaves
// what it did not store above what it stored, and records no gap; the next
// generation's reconcile fetches the rest.
func TestStartupBackfillStoppedByAClosedGenerationLeavesNoHole(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	// Call 1 is the conversation; calls 2..12 hand over b000 (fetched again)
	// and a001..a010, oldest first; call 13 finds the generation ended.
	a := newGapApp(t, g, &hcdIngress{script: func(call int) error {
		if call >= 13 {
			return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
		}
		return nil
	}})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	legacy := gapLegacy(t, a)
	if missing := missingIDs(legacy, above); len(missing) != 40 || missing[len(missing)-1] != "a011" {
		t.Fatalf("legacy lacks %v, want a050..a011 (everything above a010)", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps = %+v, want none", gaps)
	}
	a.gmHistory = &hcdIngress{}
	a.reconcileRecentConversations("listen_recovered")
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v after the next reconcile", missing)
	}
}

// The boundary message is gone from the phone and nothing older is left: the
// empty reply below the oldest fetched message ends the fetch with everything
// above the boundary stored, and no gap.
func TestStartupBackfillTreatsAnEmptyPageAsTheEnd(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	g := newLayoutGM(above)
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if calls := g.fetchCalls(); calls != 3 {
		t.Fatalf("pages fetched = %d, want 3 (two pages, then an empty reply)", calls)
	}
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps = %+v, want none", gaps)
	}
}

// A startup backfill whose client changes between pages stores nothing for
// that conversation and stops; the boundary stays at b000, so the next
// reconcile fetches the whole range.
func TestStartupBackfillInterruptedBetweenPagesStoresNothing(t *testing.T) {
	above := gapMessages("a", 50, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}))
	a := newGapApp(t, g, &hcdIngress{})
	g.afterFetch = func(call int) {
		if call == 1 {
			a.gmClient = &mockGMClient{}
		}
	}
	if err := a.Backfill(); !errors.Is(err, errGoogleCatchUpStopped) {
		t.Fatalf("Backfill() = %v, want errGoogleCatchUpStopped", err)
	}
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 50 {
		t.Fatalf("legacy lacks %d of 50, want all 50 (nothing stored)", len(missing))
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps = %+v, want none", gaps)
	}
	g.afterFetch = nil
	a.gmClient = g
	a.reconcileRecentConversations("listen_recovered")
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v after the next reconcile", missing)
	}
}

// gapAfterStartup is a store whose startup backfill could not reach b000
// below 150 new messages and recorded the gap a030..a001.
func gapAfterStartup(t *testing.T, older int) (*App, *layoutGM, []*gmproto.Message) {
	t.Helper()
	above := gapMessages("a", 150, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}, gapOlderMessages(older)))
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps after the startup backfill = %+v, want one", gaps)
	}
	return a, g, above
}

// A window backfill that cannot page below the gap (the phone re-serves its
// newest page for the request below it) ends without an error, and must
// still leave the gap recorded.
func TestHistoryGapSurvivesAWindowBackfillThatCannotPageBelowIt(t *testing.T) {
	a, g, above := gapAfterStartup(t, 0)
	for index := range g.all {
		g.omitCursor[index] = true
	}
	g.ignoreIDCursor = true
	runWindow(t, a, time.UnixMilli(gapBaseMS))
	if progress := a.GetBackfillProgress(); progress.Errors != 0 {
		t.Fatalf("window progress = %+v, want no errors (the case this guards)", progress)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps = %+v, want the gap kept", gaps)
	}
	if missing := missingIDs(gapLegacy(t, a), above[120:]); len(missing) != 30 {
		t.Fatalf("%d of the gap's 30 messages missing; the window was meant to stop short", len(missing))
	}
}

// A deep backfill pages every message, so it fills and clears the gap too.
func TestDeepBackfillClearsAGapItCovers(t *testing.T) {
	a, _, above := gapAfterStartup(t, 3)
	a.DeepBackfill()
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v after the deep backfill", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps = %+v, want none", gaps)
	}
}

// A deep backfill that gets an empty first page for the gap's conversation
// fetched nothing, so it says nothing about the gap.
func TestHistoryGapSurvivesABackfillThatFetchesNothing(t *testing.T) {
	a, g, _ := gapAfterStartup(t, 0)
	g.setMessages(nil)
	a.DeepBackfill()
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps = %+v, want the gap kept", gaps)
	}
}

// A window backfill on a phone that no longer holds the boundary message, or
// anything older, pages until the phone has nothing more: that fetched the
// whole gap, so it clears it.
func TestWindowBackfillClearsAGapWhenThePhoneHasNothingOlder(t *testing.T) {
	above := gapMessages("a", 150, false, gapBaseMS)
	g := newLayoutGM(above)
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Gaps[0].Reason != googleHistoryGapPageLimit {
		t.Fatalf("history gaps = %+v, want one page_limit gap", gaps)
	}
	runWindow(t, a, time.UnixMilli(gapBaseMS))
	if missing := missingIDs(gapLegacy(t, a), above); len(missing) != 0 {
		t.Fatalf("legacy lacks %v", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("history gaps = %+v, want none", gaps)
	}
}

// A window backfill whose generation ends partway through storing the page
// that reaches below the gap stored that page's oldest messages but refused
// newer ones inside the gap, so the gap stays recorded.
func TestHistoryGapSurvivesAGenerationEndingDuringTheLastPage(t *testing.T) {
	above := gapMessages("a", 150, false, gapBaseMS)
	g := newLayoutGM(concatMessages(above, []*gmproto.Message{gapBoundaryMessage()}, gapOlderMessages(3)))
	// Window pages (50 a page): a150..a101, a100..a051, a050..a041, then
	// a040..a001 with b000 and o001..o003.
	g.cuts[110] = true
	a := newGapApp(t, g, &hcdIngress{})
	if err := a.Backfill(); err != nil {
		t.Fatalf("Backfill(): %v", err)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Gaps[0].BeforeMS != above[109].GetTimestamp()/1000 {
		t.Fatalf("history gaps = %+v, want one below a041", gaps)
	}
	// Call 1 is the conversation; 2..111 the first three pages; the last page
	// stores o003, o002, o001, b000 and a001..a005 (calls 112..120) before
	// call 121 finds the generation ended.
	a.gmHistory = &hcdIngress{script: func(call int) error {
		if call >= 121 {
			return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
		}
		return nil
	}}
	runWindow(t, a, time.UnixMilli(gapBaseMS))
	missing := missingIDs(gapLegacy(t, a), above)
	if len(missing) != 35 {
		t.Fatalf("legacy lacks %v, want a040..a006", missing)
	}
	if gaps := a.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("history gaps = %+v, want the gap kept", gaps)
	}
}

// Gaps recorded for the same conversation merge into one spanning both, from
// the earlier boundary. A gap clears only when a backfill stored a message at
// or before its boundary, or the phone had nothing older.
func TestHistoryGapsMergeAndClearOnlyWhenCovered(t *testing.T) {
	a := &App{DataDir: t.TempDir(), Logger: zerolog.Nop()}
	a.recordGoogleHistoryGap(GoogleHistoryGap{ConversationID: "x", AfterMS: 2000, AfterID: "x2", BeforeMS: 5000, Stored: 3, Reason: googleHistoryGapPageLimit, DetectedMS: 10})
	a.recordGoogleHistoryGap(GoogleHistoryGap{ConversationID: "x", AfterMS: 7000, AfterID: "x7", BeforeMS: 9000, Stored: 4, Reason: googleHistoryGapFetchError, DetectedMS: 20})
	a.recordGoogleHistoryGap(GoogleHistoryGap{ConversationID: "y", AfterMS: 1000, BeforeMS: 1500, Stored: 1, Reason: googleHistoryGapPageLimit, DetectedMS: 15})

	gaps := a.GoogleHistoryGaps()
	if gaps == nil || gaps.Count != 2 || gaps.SinceMS != 1000 || gaps.LastDetectedMS != 20 {
		t.Fatalf("gaps = %+v", gaps)
	}
	if x := gaps.Gaps[1]; x.ConversationID != "x" || x.AfterMS != 2000 || x.AfterID != "x2" || x.BeforeMS != 9000 || x.Stored != 7 || x.Reason != googleHistoryGapFetchError {
		t.Fatalf("merged gap = %+v, want after 2000 (x2), before 9000, fetched 7", x)
	}
	if a.clearGoogleHistoryGapCovered("x", 2001, false) {
		t.Fatal("cleared x though the backfill stopped above its boundary")
	}
	if a.clearGoogleHistoryGapCovered("x", 0, false) {
		t.Fatal("cleared x though the backfill stored nothing with a time")
	}
	if a.clearGoogleHistoryGapCovered("z", 1, true) {
		t.Fatal("cleared a gap that was never recorded")
	}
	if !a.clearGoogleHistoryGapCovered("x", 2000, false) {
		t.Fatal("kept x though the backfill stored a message at its boundary")
	}
	reloaded := &App{DataDir: a.DataDir, Logger: zerolog.Nop()}
	if gaps := reloaded.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 || gaps.Gaps[0].ConversationID != "y" {
		t.Fatalf("reloaded gaps = %+v, want only y", gaps)
	}
	if !a.clearGoogleHistoryGapCovered("y", 99_999, true) {
		t.Fatal("kept y though the phone had nothing older")
	}
	if _, err := os.Stat(filepath.Join(a.DataDir, googleHistoryGapsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gap file with no gaps left: %v, want removed", err)
	}
}

// An unreadable gap file reads as no gaps and is rewritten at the next change.
func TestHistoryGapsUnreadableFileReadsAsNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, googleHistoryGapsFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{DataDir: dir, Logger: zerolog.Nop()}
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		t.Fatalf("gaps = %+v, want none", gaps)
	}
	a.recordGoogleHistoryGap(GoogleHistoryGap{ConversationID: "z", AfterMS: 1, BeforeMS: 2, Stored: 1, Reason: googleHistoryGapPageLimit})
	reloaded := &App{DataDir: dir, Logger: zerolog.Nop()}
	if gaps := reloaded.GoogleHistoryGaps(); gaps == nil || gaps.Count != 1 {
		t.Fatalf("reloaded gaps = %+v, want z", gaps)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("gap file: %v, %v; want mode 0600", info, err)
	}
}

// Status lists at most googleHistoryGapsListed gaps, earliest boundary first,
// and counts all of them.
func TestHistoryGapsStatusListIsBounded(t *testing.T) {
	a := &App{Logger: zerolog.Nop()}
	for index := 0; index < googleHistoryGapsListed+5; index++ {
		a.recordGoogleHistoryGap(GoogleHistoryGap{ConversationID: fmt.Sprintf("c%02d", index), AfterMS: int64(100 - index), BeforeMS: 200, Stored: 1})
	}
	gaps := a.GoogleHistoryGaps()
	if gaps.Count != googleHistoryGapsListed+5 || len(gaps.Gaps) != googleHistoryGapsListed || gaps.SinceMS != int64(100-googleHistoryGapsListed-4) {
		t.Fatalf("gaps = count %d, listed %d, since %d", gaps.Count, len(gaps.Gaps), gaps.SinceMS)
	}
	if !sort.SliceIsSorted(gaps.Gaps, func(i, j int) bool { return gaps.Gaps[i].AfterMS < gaps.Gaps[j].AfterMS }) {
		t.Fatal("gaps are not earliest boundary first")
	}
}

// gapLayout is one restart scenario for conversation gapConv: what the phone
// holds above and below the stored boundary b000, how it pages, how the
// startup backfill is interrupted, and how many messages arrive before the
// following reconcile.
type gapLayout struct {
	Above           int  // messages newer than b000
	Below           int  // messages older than b000 on the phone
	BoundaryOnPhone bool // false: b000 is gone from the phone
	SameMS          bool
	Cuts            []int // indices where a page ends early
	OmitCursor      []int // reply start indices that carry no cursor
	IgnoreIDCursor  bool
	CloseAt         int // the generation closes at this hand-off (1 is the conversation); 0 never
	SwapAfter       int // the client changes after this fetch; 0 never
	Arrivals        int // messages arriving before the following reconcile
}

func (gapLayout) Generate(r *rand.Rand, _ int) reflect.Value {
	l := gapLayout{BoundaryOnPhone: r.Intn(4) != 0, Below: r.Intn(4), SameMS: r.Intn(3) == 0, IgnoreIDCursor: r.Intn(4) == 0}
	switch r.Intn(3) {
	case 0:
		l.Above = 1 + r.Intn(30)
	case 1:
		l.Above = 31 + r.Intn(89)
	default:
		l.Above = 120 + r.Intn(81)
	}
	total := l.Above + l.Below + 1
	if r.Intn(2) == 0 {
		for range 1 + r.Intn(6) {
			l.Cuts = append(l.Cuts, 1+r.Intn(total))
		}
	}
	switch r.Intn(3) {
	case 1:
		for index := 0; index < total; index++ {
			l.OmitCursor = append(l.OmitCursor, index)
		}
	case 2:
		for index := 0; index < total; index++ {
			if r.Intn(3) == 0 {
				l.OmitCursor = append(l.OmitCursor, index)
			}
		}
	}
	switch r.Intn(6) {
	case 0, 1:
		l.CloseAt = 1 + r.Intn(min(l.Above, 120)+2)
	case 2:
		l.SwapAfter = 1 + r.Intn(recentReconcileMaxPages)
	}
	if r.Intn(2) == 0 {
		l.Arrivals = r.Intn(130)
	}
	return reflect.ValueOf(l)
}

func (l gapLayout) gm() *layoutGM {
	var all []*gmproto.Message
	all = append(all, gapMessages("a", l.Above, l.SameMS, gapBaseMS)...)
	if l.BoundaryOnPhone {
		all = append(all, gapBoundaryMessage())
	}
	all = append(all, gapOlderMessages(l.Below)...)
	g := newLayoutGM(all)
	for _, cut := range l.Cuts {
		g.cuts[cut] = true
	}
	for _, start := range l.OmitCursor {
		g.omitCursor[start] = true
	}
	g.ignoreIDCursor = l.IgnoreIDCursor
	return g
}

// boundaryTarget is the index of the first message on the phone a catch-up
// recognises as reaching boundaryID at boundaryMS: the message itself, or the
// first older one.
func boundaryTarget(g *layoutGM, boundaryID string, boundaryMS int64) int {
	for index, msg := range g.all {
		if msg.GetMessageID() == boundaryID || msg.GetTimestamp()/1000 < boundaryMS {
			return index
		}
	}
	return -1
}

// checkNoSilentHole verifies the no-silent-hole invariant over the messages
// above b000 (newest first): any message missing from legacy below a stored
// one lies inside a recorded gap of gapConv. It also checks that each stored
// message was offered to v2.
func checkNoSilentHole(a *App, above []*gmproto.Message, legacy map[string]bool, handed map[string]bool) error {
	var gap *GoogleHistoryGap
	if gaps := a.GoogleHistoryGaps(); gaps != nil {
		for index := range gaps.Gaps {
			if gaps.Gaps[index].ConversationID == gapConv {
				gap = &gaps.Gaps[index]
			}
		}
	}
	storedAbove := false
	for _, msg := range above {
		id := msg.GetMessageID()
		if legacy[id] {
			storedAbove = true
			if !handed[id] {
				return fmt.Errorf("%s is in legacy but was never offered to v2", id)
			}
			continue
		}
		if !storedAbove {
			continue // not yet fetched: above everything stored
		}
		ts := msg.GetTimestamp() / 1000
		if gap == nil || ts < gap.AfterMS || ts > gap.BeforeMS {
			return fmt.Errorf("%s (%d ms) is missing below a stored message and outside any recorded gap (%+v)", id, ts, gap)
		}
	}
	return nil
}

func offeredMessages(ingresses ...*hcdIngress) map[string]bool {
	ids := map[string]bool{}
	for _, ingress := range ingresses {
		_, offers := ingress.snapshot()
		for _, offer := range offers {
			if offer.Kind == "msg" {
				ids[offer.MessageID] = true
			}
		}
	}
	return ids
}

// Over random phones (page sizes, cursor habits, a boundary the phone no
// longer holds, same-millisecond messages), interruptions, and new arrivals:
//
//   - after the startup backfill, nothing is missing below a stored message
//     unless a recorded gap covers it; a gap is recorded exactly when paging
//     ended short of the boundary and something was stored; otherwise every
//     message above the boundary is stored; at most recentReconcileMaxPages
//     pages are fetched;
//   - after the following reconcile, the same holds for the conversation's
//     new boundary, so whatever an interrupted backfill left above what it
//     stored is fetched;
//   - after a window backfill from the recorded since_ms, clearing never hides
//     a hole, and on a phone that pages every gap is filled and cleared.
func TestStartupBackfillPropertyNoSilentHole(t *testing.T) {
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	// coverage counts the cases that exercised each branch, so the property
	// cannot pass vacuously.
	coverage := map[string]int{}
	property := func(l gapLayout) bool {
		fail := func(format string, args ...any) bool {
			t.Logf("layout %+v: "+format, append([]any{l}, args...)...)
			return false
		}
		g := l.gm()
		above := append([]*gmproto.Message(nil), g.all[:l.Above]...)
		first := &hcdIngress{script: func(call int) error {
			if l.CloseAt > 0 && call >= l.CloseAt {
				return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
			}
			return nil
		}}
		a := newTestApp(t, g.mockGMClient)
		a.gmClient = g
		a.gmHistory = first
		seedGapBoundary(t, a)
		swapped := false
		if l.SwapAfter > 0 {
			g.afterFetch = func(call int) {
				if call == l.SwapAfter {
					swapped = true
					a.gmClient = &mockGMClient{}
				}
			}
		}
		outcome := g.catchUpOutcome(boundaryTarget(g, "b000", gapBaseMS))

		err := a.Backfill()
		if err != nil && !swapped {
			return fail("Backfill(): %v", err)
		}
		if calls := g.fetchCalls(); calls > recentReconcileMaxPages {
			return fail("backfill fetched %d pages, over the bound", calls)
		}
		legacy := gapLegacy(t, a)
		if err := checkNoSilentHole(a, above, legacy, offeredMessages(first)); err != nil {
			return fail("after the backfill: %v", err)
		}
		stored := l.Above - len(missingIDs(legacy, above))
		gaps := a.GoogleHistoryGaps()
		if gaps != nil && stored == 0 {
			return fail("gap %+v recorded with nothing stored", gaps)
		}
		handOffs, _ := first.snapshot()
		interrupted := (l.CloseAt > 0 && handOffs >= l.CloseAt) || swapped
		inReach := outcome != pagingShort
		switch {
		case interrupted && stored > 0 && stored < l.Above && gaps == nil:
			coverage["interrupted with a stored prefix"]++
		case !interrupted && inReach:
			coverage["within reach"]++
		case !interrupted:
			coverage["out of reach"]++
		}
		if !interrupted {
			if inReach && (gaps != nil || stored != l.Above) {
				return fail("paging ends %s but stored %d of %d, gaps %+v", outcome, stored, l.Above, gaps)
			}
			if !inReach && gaps == nil {
				return fail("paging ends short but no gap recorded (stored %d)", stored)
			}
		}

		// The next generation's reconcile, after new arrivals.
		g.afterFetch = nil
		g.resetCalls()
		arrivals := gapMessages("n", l.Arrivals, l.SameMS, above0MS(above))
		g.setMessages(concatMessages(arrivals, g.all))
		a.gmClient = g
		second := &hcdIngress{}
		a.gmHistory = second
		hadGap := gaps != nil
		newest, err := a.Store.GetMessagesByConversation(gapConv, 1)
		if err != nil || len(newest) != 1 {
			return fail("read boundary: %v", err)
		}
		outcome = g.catchUpOutcome(boundaryTarget(g, newest[0].MessageID, newest[0].TimestampMS))

		a.reconcileRecentConversations("listen_recovered")
		if calls := g.fetchCalls(); calls > recentReconcileMaxPages {
			return fail("reconcile fetched %d pages, over the bound", calls)
		}
		everything := concatMessages(arrivals, above)
		legacy = gapLegacy(t, a)
		if err := checkNoSilentHole(a, everything, legacy, offeredMessages(first, second)); err != nil {
			return fail("after the reconcile: %v", err)
		}
		if !hadGap && outcome != pagingShort {
			if missing := missingIDs(legacy, everything); len(missing) != 0 {
				return fail("the reconcile's paging ends %s but legacy lacks %v", outcome, missing)
			}
			if gaps := a.GoogleHistoryGaps(); gaps != nil {
				return fail("the reconcile was within reach but recorded %+v", gaps)
			}
			if interrupted && stored < l.Above {
				coverage["reconcile completed an interrupted backfill"]++
			}
		}
		if hadGap {
			coverage["gap carried through the reconcile"]++
		}

		// A window backfill from the earliest recorded boundary (b000's time
		// when none is recorded). Clearing a gap must never hide a hole, and on
		// a phone that honours paging it fills and clears every gap.
		since := gapBaseMS
		if gaps := a.GoogleHistoryGaps(); gaps != nil {
			since = gaps.SinceMS
		}
		third := &hcdIngress{}
		a.gmHistory = third
		gapsBefore := a.GoogleHistoryGaps()
		runWindow(t, a, time.UnixMilli(since))
		legacy = gapLegacy(t, a)
		if err := checkNoSilentHole(a, everything, legacy, offeredMessages(first, second, third)); err != nil {
			return fail("after the window backfill: %v", err)
		}
		gapsAfter := a.GoogleHistoryGaps()
		if !l.IgnoreIDCursor {
			if missing := missingIDs(legacy, everything); len(missing) != 0 || gapsAfter != nil {
				return fail("the window backfill on a phone that pages left %v missing and gaps %+v", missing, gapsAfter)
			}
		}
		switch {
		case gapsBefore != nil && gapsAfter == nil:
			coverage["window cleared a gap"]++
		case gapsBefore != nil:
			coverage["window kept a gap it could not fill"]++
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
	t.Logf("coverage: %v", coverage)
	for _, branch := range []string{
		"interrupted with a stored prefix",
		"within reach",
		"out of reach",
		"reconcile completed an interrupted backfill",
		"gap carried through the reconcile",
		"window cleared a gap",
		"window kept a gap it could not fill",
	} {
		if coverage[branch] < 10 {
			t.Errorf("only %d cases covered %q", coverage[branch], branch)
		}
	}
}

// above0MS is a time above every message in above (newest first), so new
// arrivals land on top.
func above0MS(above []*gmproto.Message) int64 {
	if len(above) == 0 {
		return gapBaseMS
	}
	return above[0].GetTimestamp() / 1000
}

// Differential: for phones whose boundary is within reach, the startup backfill
// and a window backfill from the boundary's time, two separate paging
// implementations, end with the same messages above the boundary: all of them.
func TestStartupBackfillAgreesWithWindowBackfillWithinReach(t *testing.T) {
	t.Setenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC", "0")
	compared := 0
	property := func(l gapLayout) bool {
		// A phone that honours cursors; one that does not can stop either
		// implementation early at different places.
		l.CloseAt, l.SwapAfter, l.BoundaryOnPhone, l.IgnoreIDCursor = 0, 0, true, false
		g := l.gm()
		if g.catchUpOutcome(g.indexOf("b000")) != pagingReached {
			return true
		}
		compared++
		above := g.all[:l.Above]

		startup := newTestApp(t, g.mockGMClient)
		startup.gmClient = g
		startup.gmHistory = &hcdIngress{}
		seedGapBoundary(t, startup)
		if err := startup.Backfill(); err != nil {
			t.Logf("layout %+v: Backfill(): %v", l, err)
			return false
		}

		windowGM := l.gm()
		window := newTestApp(t, windowGM.mockGMClient)
		window.gmClient = windowGM
		window.gmHistory = &hcdIngress{}
		seedGapBoundary(t, window)
		runWindow(t, window, time.UnixMilli(gapBaseMS))

		startupMissing := missingIDs(gapLegacy(t, startup), above)
		windowMissing := missingIDs(gapLegacy(t, window), above)
		if len(startupMissing) != 0 || !reflect.DeepEqual(startupMissing, windowMissing) {
			t.Logf("layout %+v: startup backfill lacks %v, window backfill lacks %v", l, startupMissing, windowMissing)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(20261010))}); err != nil {
		t.Fatal(err)
	}
	if compared < 50 {
		t.Fatalf("only %d layouts were within reach; the comparison is too thin", compared)
	}
}
