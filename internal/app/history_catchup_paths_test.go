package app

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Every catch-up path that lists a conversation hands each fetched message to
// v2 together with that conversation's snapshot (the worker needs it to file a
// message of a thread it has never seen). The pending-media refresh fetches a
// message without listing its conversation, so it hands the message over with
// no snapshot. A path that stopped handing over at all, or dropped the
// snapshot, would silently go back to filling only the legacy store.
func TestHistoryEveryCatchUpPathHandsOverMessagesWithTheirSnapshot(t *testing.T) {
	tests := []struct {
		name         string
		prepare      func(t *testing.T, mock *mockGMClient)
		run          func(t *testing.T, a *App)
		wantMessages []string
		wantSnapshot bool
	}{
		{
			name:         "deep backfill",
			run:          func(_ *testing.T, a *App) { a.DeepBackfill() },
			wantMessages: []string{"t1-a", "t1-b", "t1-c", "t2-a", "t2-b", "t2-c"},
			wantSnapshot: true,
		},
		{
			name: "startup shallow backfill",
			run: func(t *testing.T, a *App) {
				if err := a.Backfill(); err != nil {
					t.Fatalf("Backfill(): %v", err)
				}
			},
			wantMessages: []string{"t1-a", "t1-b", "t1-c", "t2-a", "t2-b", "t2-c"},
			wantSnapshot: true,
		},
		{
			name:         "recent reconcile",
			run:          func(_ *testing.T, a *App) { a.reconcileRecentConversations("listen_recovered") },
			wantMessages: []string{"t1-a", "t1-b", "t1-c", "t2-a", "t2-b", "t2-c"},
			wantSnapshot: true,
		},
		{
			name: "window backfill",
			run: func(t *testing.T, a *App) {
				if !a.beginBackfill() {
					t.Fatal("backfill guard already held")
				}
				a.windowBackfill(time.UnixMilli(1))
			},
			wantMessages: []string{"t1-a", "t1-b", "t1-c", "t2-a", "t2-b", "t2-c"},
			wantSnapshot: true,
		},
		{
			name: "phone backfill",
			prepare: func(_ *testing.T, mock *mockGMClient) {
				mock.getOrCreateResults = map[string]*gmproto.Conversation{"+15550001111": makeConv("t1", "One")}
			},
			run: func(t *testing.T, a *App) {
				if err := a.BackfillConversationByPhone("+15550001111"); err != nil {
					t.Fatalf("BackfillConversationByPhone(): %v", err)
				}
			},
			wantMessages: []string{"t1-a", "t1-b", "t1-c"},
			wantSnapshot: true,
		},
		{
			name: "orphan contact discovery",
			prepare: func(t *testing.T, mock *mockGMClient) {
				t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", "1")
				// The thread is in no folder listing; only its contact finds it.
				mock.conversations = nil
				mock.contacts = []*gmproto.Contact{{Number: &gmproto.ContactNumber{Number: "+15550002222"}}}
				mock.getOrCreateResults = map[string]*gmproto.Conversation{"+15550002222": makeConv("t2", "Two")}
			},
			run:          func(_ *testing.T, a *App) { a.DeepBackfill() },
			wantMessages: []string{"t2-a", "t2-b", "t2-c"},
			wantSnapshot: true,
		},
		{
			name: "pending media refresh",
			run: func(t *testing.T, a *App) {
				if refreshed, _ := a.refreshPendingMediaMessageAttempt("t1", "t1-b"); !refreshed {
					t.Fatal("refreshPendingMediaMessageAttempt() did not find the message")
				}
			},
			wantMessages: []string{"t1-b"},
			wantSnapshot: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := hcdTwoConversationMock()
			if test.prepare != nil {
				test.prepare(t, mock)
			}
			ingress := &hcdIngress{}
			a, _, legacyDB := hcdNewApp(t, mock, ingress)
			test.run(t, a)

			_, offers := ingress.snapshot()
			var handed []string
			for _, offer := range offers {
				if offer.Kind != "msg" {
					continue
				}
				handed = append(handed, offer.MessageID)
				wantSnapshot := ""
				if test.wantSnapshot {
					wantSnapshot = offer.ConversationID
				}
				if offer.SnapshotID != wantSnapshot {
					t.Errorf("message %s handed over with snapshot %q, want %q", offer.MessageID, offer.SnapshotID, wantSnapshot)
				}
			}
			sort.Strings(handed)
			if strings.Join(handed, ",") != strings.Join(test.wantMessages, ",") {
				t.Fatalf("messages handed to v2 = %v, want %v", handed, test.wantMessages)
			}
			// What v2 was offered is exactly what legacy stored.
			legacy := hcdLegacyMessages(t, legacyDB)
			if len(legacy) != len(test.wantMessages) {
				t.Fatalf("legacy holds %d messages, want %d", len(legacy), len(test.wantMessages))
			}
			for _, id := range test.wantMessages {
				if _, ok := legacy[id]; !ok {
					t.Errorf("legacy lacks %s", id)
				}
			}
		})
	}
}

// windowMock is one in-window conversation whose messages come in the given
// pages (newest first), with since at 100 s.
func windowMock(pages ...[]*gmproto.Message) (*mockGMClient, time.Time) {
	conversation := makeConv("w", "Window")
	conversation.LastMessageTimestamp = 500_000 * 1000 // 500 s, in microseconds
	return &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{conversation}},
		},
		messages:   map[string][][]*gmproto.Message{"w": pages},
		fetchCalls: map[string]int{},
	}, time.UnixMilli(100_000)
}

func runWindow(t *testing.T, a *App, since time.Time) {
	t.Helper()
	if !a.beginBackfill() {
		t.Fatal("backfill guard already held")
	}
	a.windowBackfill(since)
}

func storedIDs(t *testing.T, a *App, ingress *hcdIngress) (legacy, handed []string) {
	t.Helper()
	messages, err := a.Store.GetMessagesByConversation("w", 1000)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	for _, message := range messages {
		legacy = append(legacy, message.MessageID)
	}
	_, offers := ingress.snapshot()
	for _, offer := range offers {
		if offer.Kind == "msg" {
			handed = append(handed, offer.MessageID)
		}
	}
	sort.Strings(legacy)
	sort.Strings(handed)
	return legacy, handed
}

// The window boundary is "a message older than since". A message with no
// timestamp says nothing about where a page is, so it must not end paging:
// treating 0 as "before the window" skipped every later in-window page.
func TestHistoryWindowBackfillZeroTimestampDoesNotEndPaging(t *testing.T) {
	mock, since := windowMock(
		[]*gmproto.Message{makeMsg("m1", "w", "newest", 130_000), makeMsg("m-zero", "w", "no timestamp", 0)},
		[]*gmproto.Message{makeMsg("m2", "w", "still in the window", 110_000), makeMsg("m-old", "w", "before the window", 90_000)},
		[]*gmproto.Message{makeMsg("m-older", "w", "never fetched", 80_000)},
	)
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	runWindow(t, a, since)

	if got := mock.fetchCalls["w"]; got != 2 {
		t.Fatalf("pages fetched = %d, want 2 (page 2 crosses the boundary, page 3 is past it)", got)
	}
	legacy, handed := storedIDs(t, a, ingress)
	want := "m-old,m-zero,m1,m2"
	if strings.Join(legacy, ",") != want || strings.Join(handed, ",") != want {
		t.Fatalf("legacy %v, handed to v2 %v; want %s in both", legacy, handed, want)
	}
}

// A message exactly at since is inside the window and does not end paging, so
// another message of the same millisecond on the next page is still fetched.
func TestHistoryWindowBackfillMessageAtSinceIsInsideTheWindow(t *testing.T) {
	mock, since := windowMock(
		[]*gmproto.Message{makeMsg("a", "w", "after", 101_000), makeMsg("b", "w", "exactly at since", 100_000)},
		[]*gmproto.Message{makeMsg("c", "w", "same millisecond", 100_000), makeMsg("d", "w", "before", 99_000)},
		[]*gmproto.Message{makeMsg("e", "w", "never fetched", 98_000)},
	)
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	runWindow(t, a, since)

	if got := mock.fetchCalls["w"]; got != 2 {
		t.Fatalf("pages fetched = %d, want 2", got)
	}
	legacy, handed := storedIDs(t, a, ingress)
	want := "a,b,c,d"
	if strings.Join(legacy, ",") != want || strings.Join(handed, ",") != want {
		t.Fatalf("legacy %v, handed to v2 %v; want %s in both", legacy, handed, want)
	}
}

// timeCursorGM answers FetchMessages the way a phone that sends no cursor
// does: each reply is the next pageSize messages older than the request
// cursor's timestamp (the newest ones when there is no cursor), and never
// carries a cursor of its own.
type timeCursorGM struct {
	*mockGMClient
	pageSize int
	all      []*gmproto.Message // newest first

	mu      sync.Mutex
	cursors []int64 // LastItemTimestamp of each request, 0 for none
}

func (g *timeCursorGM) FetchMessages(_ string, _ int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	g.mu.Lock()
	g.cursors = append(g.cursors, cursor.GetLastItemTimestamp())
	g.mu.Unlock()
	var page []*gmproto.Message
	for _, message := range g.all {
		if cursor != nil && message.GetTimestamp()/1000 >= cursor.GetLastItemTimestamp() {
			continue
		}
		page = append(page, message)
		if len(page) == g.pageSize {
			break
		}
	}
	return &gmproto.ListMessagesResponse{Messages: page}, nil
}

// A reply without a cursor does not mean the conversation is exhausted (the
// libgm bridge synthesizes one from the oldest message for the same reason).
// The window backfill must keep paging below the oldest message it has seen
// until it crosses since; stopping at the first cursor-less page left the rest
// of the window unrecovered.
func TestHistoryWindowBackfillContinuesWhenTheReplyHasNoCursor(t *testing.T) {
	mock, since := windowMock()
	gm := &timeCursorGM{
		mockGMClient: mock,
		pageSize:     2,
		all: []*gmproto.Message{
			makeMsg("n5", "w", "5", 150_000),
			makeMsg("n4", "w", "4", 140_000),
			makeMsg("n3", "w", "3", 130_000),
			makeMsg("n2", "w", "2", 120_000),
			makeMsg("n1", "w", "1", 110_000),
			makeMsg("o1", "w", "before the window", 90_000),
			makeMsg("o2", "w", "never fetched", 80_000),
		},
	}
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	a.gmClient = gm
	runWindow(t, a, since)

	// Pages: [n5 n4], [n3 n2], [n1 o1] (crosses since). Each continues below
	// the oldest message of the page before it.
	if want := []int64{0, 140_000, 120_000}; !equalInt64s(gm.cursors, want) {
		t.Fatalf("request cursors = %v, want %v", gm.cursors, want)
	}
	legacy, handed := storedIDs(t, a, ingress)
	want := "n1,n2,n3,n4,n5,o1"
	if strings.Join(legacy, ",") != want || strings.Join(handed, ",") != want {
		t.Fatalf("legacy %v, handed to v2 %v; want %s in both", legacy, handed, want)
	}
	if progress := a.GetBackfillProgress(); progress.Errors != 0 || progress.MessagesFound != 6 {
		t.Fatalf("progress = %+v, want 6 messages and no errors", progress)
	}
}

// When the conversation really is exhausted inside the window, the extra
// request below the oldest message returns nothing older and paging ends,
// without storing or handing anything twice.
func TestHistoryWindowBackfillStopsWhenNothingOlderComesBack(t *testing.T) {
	for _, honoursCursor := range []bool{true, false} {
		mock, since := windowMock()
		all := []*gmproto.Message{makeMsg("n2", "w", "2", 120_000), makeMsg("n1", "w", "1", 110_000)}
		ingress := &hcdIngress{}
		a, _, _ := hcdNewApp(t, mock, ingress)
		var requests int
		if honoursCursor {
			gm := &timeCursorGM{mockGMClient: mock, pageSize: 5, all: all}
			a.gmClient = gm
			runWindow(t, a, since)
			requests = len(gm.cursors)
		} else {
			// The mock ignores an unknown cursor and serves its first page again.
			mock.messages = map[string][][]*gmproto.Message{"w": {all}}
			runWindow(t, a, since)
			requests = mock.fetchCalls["w"]
		}
		if requests != 2 {
			t.Fatalf("honoursCursor=%v: requests = %d, want the page and one probe below it", honoursCursor, requests)
		}
		calls, _ := ingress.snapshot()
		legacy, handed := storedIDs(t, a, ingress)
		if strings.Join(legacy, ",") != "n1,n2" || strings.Join(handed, ",") != "n1,n2" || calls != 3 {
			t.Fatalf("honoursCursor=%v: legacy %v, handed %v, ingress calls %d; want n1,n2 once each plus the conversation", honoursCursor, legacy, handed, calls)
		}
	}
}

// A window backfill that could not start, or stopped early, says so in the
// progress it reports; it used to look exactly like a completed run.
func TestHistoryWindowBackfillReportsRunsThatDidNotFinish(t *testing.T) {
	t.Run("not connected", func(t *testing.T) {
		a := newTestApp(t, &mockGMClient{})
		a.gmClient = nil
		// Leftovers of an earlier run must not be reported as this one's.
		a.BackfillProgress.add(7, 70, 0, 3)
		runWindow(t, a, time.UnixMilli(100_000))

		progress := a.GetBackfillProgress()
		if progress.Running || progress.Errors != 1 || len(progress.ErrorDetails) != 1 ||
			!strings.Contains(progress.ErrorDetails[0], "not connected") {
			t.Fatalf("progress = %+v, want one 'not connected' error", progress)
		}
		if progress.ConversationsFound != 0 || progress.MessagesFound != 0 || progress.FoldersScanned != 0 {
			t.Fatalf("progress kept an earlier run's counts: %+v", progress)
		}
		if a.IsDeepBackfillRunning() {
			t.Fatal("backfill guard still held after the run")
		}
	})

	t.Run("connection changed mid-run", func(t *testing.T) {
		mock, since := windowMock(
			[]*gmproto.Message{makeMsg("m1", "w", "1", 130_000)},
			[]*gmproto.Message{makeMsg("m2", "w", "2", 120_000)},
		)
		ingress := &hcdIngress{}
		a, _, _ := hcdNewApp(t, mock, ingress)
		mock.afterFetchMessages = func(string, int) { a.gmClient = &mockGMClient{} }
		runWindow(t, a, since)

		progress := a.GetBackfillProgress()
		if progress.Running || progress.Errors != 1 || !strings.Contains(strings.Join(progress.ErrorDetails, " "), "stopped early") {
			t.Fatalf("progress = %+v, want one 'stopped early' error", progress)
		}
		if got := mock.fetchCalls["w"]; got != 1 {
			t.Fatalf("pages fetched = %d, want 1 before the run stopped", got)
		}
	})
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
