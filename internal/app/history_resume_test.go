package app

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/quick"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// Second-round review of PR #200: what a catch-up leaves behind when it stops
// partway must be something a later catch-up fills, and the window backfill's
// paging must hold up against phones that page by ID or never stop handing
// out cursors.

// reconcileMock is conversation c1 with seven messages in three pages, newest
// first, of which legacy already holds m1 (the reconcile's boundary).
func reconcileMock(t *testing.T) (*mockGMClient, func(a *App)) {
	t.Helper()
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{makeConv("c1", "Alice")}},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m7", "c1", "7", 700), makeMsg("m6", "c1", "6", 600)},
				{makeMsg("m5", "c1", "5", 500), makeMsg("m4", "c1", "4", 400)},
				{makeMsg("m3", "c1", "3", 300), makeMsg("m2", "c1", "2", 200), makeMsg("m1", "c1", "1", 100)},
			},
		},
		fetchCalls: map[string]int{},
	}
	seed := func(a *App) {
		t.Helper()
		if err := a.Store.UpsertConversation(&db.Conversation{ConversationID: "c1", Name: "Alice", LastMessageTS: 100}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		if err := a.Store.UpsertMessage(&db.Message{MessageID: "m1", ConversationID: "c1", Body: "1", TimestampMS: 100}); err != nil {
			t.Fatalf("seed boundary message: %v", err)
		}
	}
	return mock, seed
}

func legacyIDs(t *testing.T, a *App, conversationID string) string {
	t.Helper()
	messages, err := a.Store.GetMessagesByConversation(conversationID, 1000)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(%q): %v", conversationID, err)
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.MessageID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// A reconcile whose generation ends partway through storing must leave what
// it did not store above what it stored, so the next generation's reconcile,
// which pages down only to the newest stored message, fetches all of it. It
// used to store page by page, newest first: a close on the second page left
// that page's rest and the third page below the stored first page, where no
// reconcile looks again.
func TestHistoryReconcileStoppedByAClosedGenerationLeavesNoHole(t *testing.T) {
	v2 := hcdNewV2(t)
	mock, seed := reconcileMock(t)
	// Call 1 is the conversation; calls 2-5 store m1 (the boundary, fetched
	// again), m2, m3 and m4, oldest first; call 6, m5, finds the generation
	// ended.
	first := &hcdIngress{sink: v2.sink, script: func(call int) error {
		if call >= 6 {
			return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
		}
		return nil
	}}
	a, _, _ := hcdNewApp(t, mock, first)
	seed(a)

	a.reconcileRecentConversations("listen_recovered")
	if got := legacyIDs(t, a, "c1"); got != "m1,m2,m3,m4" {
		t.Fatalf("legacy after the closed reconcile = %s, want m1,m2,m3,m4 (the oldest, contiguous with the boundary)", got)
	}

	// The next generation's reconcile.
	a.gmHistory = &hcdIngress{sink: v2.sink}
	a.reconcileRecentConversations("listen_recovered")
	v2.drain(t)

	if got := legacyIDs(t, a, "c1"); got != "m1,m2,m3,m4,m5,m6,m7" {
		t.Fatalf("legacy after the next reconcile = %s, want every message", got)
	}
	stored := hcdV2Messages(t, v2.inspect)
	for _, id := range []string{"m2", "m3", "m4", "m5", "m6", "m7"} {
		if _, ok := stored[id]; !ok {
			t.Errorf("v2 lacks %s after the next reconcile", id)
		}
	}
}

// A reconcile interrupted between pages (the client changed) stores nothing
// for that conversation, so its boundary stays where it was and the next
// reconcile fetches the whole range. Storing the pages already fetched would
// move the boundary above the ones not yet fetched.
func TestHistoryReconcileInterruptedBetweenPagesStoresNothing(t *testing.T) {
	mock, seed := reconcileMock(t)
	ingress := &hcdIngress{}
	a, gm, _ := hcdNewApp(t, mock, ingress)
	seed(a)
	mock.afterFetchMessages = func(_ string, page int) {
		if page == 0 {
			a.gmClient = &mockGMClient{}
		}
	}

	a.reconcileRecentConversations("listen_recovered")
	if got := legacyIDs(t, a, "c1"); got != "m1" {
		t.Fatalf("legacy after the interrupted reconcile = %s, want only the boundary m1", got)
	}
	if calls, offers := ingress.snapshot(); calls != 1 || offers[0].Kind != "conv" {
		t.Fatalf("ingress offers = %+v, want only the conversation", offers)
	}

	mock.afterFetchMessages = nil
	a.gmClient = gm
	a.reconcileRecentConversations("listen_recovered")
	if got := legacyIDs(t, a, "c1"); got != "m1,m2,m3,m4,m5,m6,m7" {
		t.Fatalf("legacy after the next reconcile = %s, want every message", got)
	}
}

// reconcileCase is one conversation whose messages (newest first, the
// oldest already in legacy as the reconcile's boundary) come in up to
// recentReconcileMaxPages pages, and one way the first reconcile stops: the
// generation closes at hand-off CloseAt (1 is the conversation), or, when
// CloseAt is 0, the client changes after page SwapAfter is fetched.
type reconcileCase struct {
	Newer     int   // messages above the boundary
	Pages     []int // page sizes, newest page first; they sum to Newer+1
	CloseAt   int
	SwapAfter int
	// Tied[i] puts message i+1 (newest first) in the same millisecond as
	// message i, so ties can straddle pages and the boundary.
	Tied []bool
}

func (reconcileCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := reconcileCase{Newer: 1 + r.Intn(9)}
	total := c.Newer + 1
	pages := 1 + r.Intn(min(recentReconcileMaxPages, total))
	// Split total into pages non-empty runs.
	cuts := r.Perm(total - 1)[:pages-1]
	sort.Ints(cuts)
	previous := 0
	for _, cut := range cuts {
		c.Pages = append(c.Pages, cut+1-previous)
		previous = cut + 1
	}
	c.Pages = append(c.Pages, total-previous)
	for index := 1; index < total; index++ {
		c.Tied = append(c.Tied, r.Intn(3) == 0)
	}
	if r.Intn(2) == 0 {
		c.CloseAt = 1 + r.Intn(total+1)
	} else {
		c.SwapAfter = r.Intn(pages)
	}
	return reflect.ValueOf(c)
}

// Whatever point a recent reconcile stops at, what it stored is every fetched
// message from the boundary up to some point (nothing above a hole), and the
// next reconcile leaves legacy holding all of them. Messages may share a
// millisecond, across pages and with the boundary: storing them oldest first
// then relies on reversing the page order before the stable sort.
func TestHistoryReconcilePropertyStopLeavesNoHole(t *testing.T) {
	property := func(c reconcileCase) bool {
		total := c.Newer + 1
		all := make([]*gmproto.Message, 0, total) // newest first
		ts := int64(1000 * (total + 1))
		for index := 0; index < total; index++ {
			if index > 0 && !c.Tied[index-1] {
				ts -= 1000
			}
			id := fmt.Sprintf("r%02d", total-index)
			all = append(all, makeMsg(id, "c1", id, ts))
		}
		var pages [][]*gmproto.Message
		start := 0
		for _, size := range c.Pages {
			pages = append(pages, all[start:start+size])
			start += size
		}
		mock := &mockGMClient{
			conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
				gmproto.ListConversationsRequest_INBOX: {{makeConv("c1", "Alice")}},
			},
			messages: map[string][][]*gmproto.Message{"c1": pages},
		}
		ingress := &hcdIngress{script: func(call int) error {
			if c.CloseAt > 0 && call >= c.CloseAt {
				return fmt.Errorf("google generation 1 ended: %w", ErrGoogleHistoryClosed)
			}
			return nil
		}}
		a, gm, _ := hcdNewApp(t, mock, ingress)
		boundary := all[total-1]
		boundaryMS := boundary.GetTimestamp() / 1000
		if err := a.Store.UpsertConversation(&db.Conversation{ConversationID: "c1", Name: "Alice", LastMessageTS: boundaryMS}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		if err := a.Store.UpsertMessage(&db.Message{MessageID: boundary.GetMessageID(), ConversationID: "c1", Body: "boundary", TimestampMS: boundaryMS}); err != nil {
			t.Fatalf("seed boundary: %v", err)
		}
		if c.CloseAt == 0 {
			mock.afterFetchMessages = func(_ string, page int) {
				if page == c.SwapAfter {
					a.gmClient = &mockGMClient{}
				}
			}
		}

		a.reconcileRecentConversations("listen_recovered")
		stored := map[string]bool{}
		messages, err := a.Store.GetMessagesByConversation("c1", 1000)
		if err != nil {
			t.Fatalf("GetMessagesByConversation(): %v", err)
		}
		for _, message := range messages {
			stored[message.MessageID] = true
		}
		// Oldest first: once a message is missing, nothing newer may be stored.
		hole := ""
		for index := total - 1; index >= 0; index-- {
			id := all[index].GetMessageID()
			if !stored[id] && hole == "" {
				hole = id
			}
			if stored[id] && hole != "" {
				t.Logf("case %+v: %s stored above the missing %s", c, id, hole)
				return false
			}
		}

		mock.afterFetchMessages = nil
		a.gmClient = gm
		a.gmHistory = &hcdIngress{}
		a.reconcileRecentConversations("listen_recovered")
		var want []string
		for _, message := range all {
			want = append(want, message.GetMessageID())
		}
		sort.Strings(want)
		if got := legacyIDs(t, a, "c1"); got != strings.Join(want, ",") {
			t.Logf("case %+v: legacy after the next reconcile = %s, want %s", c, got, strings.Join(want, ","))
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(20261009))}); err != nil {
		t.Fatal(err)
	}
}

// failingPageGM fails every fetch after the first page of a conversation.
type failingPageGM struct{ *hcdRecordingGM }

func (g failingPageGM) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	if cursor != nil {
		return nil, fmt.Errorf("phone could not load page %s", cursor.GetLastItemID())
	}
	return g.hcdRecordingGM.FetchMessages(conversationID, count, cursor)
}

// A reconcile whose later page fails still stores the pages it fetched, as
// before: a page the phone keeps failing must not keep every newer message out.
func TestHistoryReconcileStoresFetchedPagesWhenALaterPageFails(t *testing.T) {
	mock, seed := reconcileMock(t)
	a, gm, _ := hcdNewApp(t, mock, &hcdIngress{})
	seed(a)
	a.gmClient = failingPageGM{gm}

	a.reconcileRecentConversations("listen_recovered")
	if got := legacyIDs(t, a, "c1"); got != "m1,m6,m7" {
		t.Fatalf("legacy = %s, want the first page stored above the boundary", got)
	}
}

// idCursorGM answers FetchMessages the way a phone that pages by message ID
// and sends no cursor does: each reply is the next pageSize messages after the
// request cursor's LastItemID in its newest-first order (the newest ones when
// there is no cursor, or the ID is unknown).
type idCursorGM struct {
	*mockGMClient
	pageSize int
	all      []*gmproto.Message // newest first

	mu      sync.Mutex
	cursors []string
}

func (g *idCursorGM) FetchMessages(_ string, _ int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	g.mu.Lock()
	g.cursors = append(g.cursors, cursor.GetLastItemID())
	g.mu.Unlock()
	start := 0
	for index, message := range g.all {
		if message.GetMessageID() == cursor.GetLastItemID() {
			start = index + 1
		}
	}
	end := min(start+g.pageSize, len(g.all))
	return &gmproto.ListMessagesResponse{Messages: g.all[start:end]}, nil
}

// Paging below a page by the ID of its oldest message must not lose a message
// that shares that oldest millisecond: the first revision dropped any page
// with nothing older by timestamp, so n1 (same millisecond as n2, the
// conversation's first message) was in neither store.
func TestHistoryWindowBackfillPagesByIDWithoutLosingSameMillisecondMessages(t *testing.T) {
	mock, since := windowMock()
	gm := &idCursorGM{
		mockGMClient: mock,
		pageSize:     2,
		all: []*gmproto.Message{
			makeMsg("n3", "w", "3", 130_000),
			makeMsg("n2", "w", "2", 110_000),
			makeMsg("n1", "w", "1", 110_000),
		},
	}
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	a.gmClient = gm
	runWindow(t, a, since)

	// [n3 n2], then below n2 (the later of the two at 110 s on the page):
	// [n1], then below n1: nothing, which ends paging.
	if want := []string{"", "n2", "n1"}; strings.Join(gm.cursors, ",") != strings.Join(want, ",") {
		t.Fatalf("request cursors = %q, want %q", gm.cursors, want)
	}
	legacy, handed := storedIDs(t, a, ingress)
	if strings.Join(legacy, ",") != "n1,n2,n3" || strings.Join(handed, ",") != "n1,n2,n3" {
		t.Fatalf("legacy %v, handed to v2 %v; want n1,n2,n3 in both", legacy, handed)
	}
	if progress := a.GetBackfillProgress(); progress.Errors != 0 || progress.MessagesFound != 3 {
		t.Fatalf("progress = %+v, want 3 messages and no errors", progress)
	}
}

// A conversation whose last message is exactly at since is in the window.
func TestHistoryWindowBackfillScansAConversationLastActiveAtSince(t *testing.T) {
	mock, since := windowMock([]*gmproto.Message{makeMsg("s1", "w", "at since", 100_000), makeMsg("s0", "w", "before", 90_000)})
	conversation := mock.conversations[gmproto.ListConversationsRequest_INBOX][0][0]
	conversation.LastMessageTimestamp = since.UnixMilli() * 1000
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	runWindow(t, a, since)

	if got := mock.fetchCalls["w"]; got != 1 {
		t.Fatalf("pages fetched = %d, want the conversation scanned", got)
	}
	legacy, handed := storedIDs(t, a, ingress)
	if strings.Join(legacy, ",") != "s0,s1" || strings.Join(handed, ",") != "s0,s1" {
		t.Fatalf("legacy %v, handed to v2 %v; want s0,s1 in both", legacy, handed)
	}
}

// endlessGM always has one more in-window page and a cursor to it, as a phone
// that keeps re-serving a cursor might.
type endlessGM struct {
	*mockGMClient
	mu    sync.Mutex
	pages int
}

func (g *endlessGM) FetchMessages(_ string, _ int64, _ *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pages++
	if g.pages > 2*windowBackfillMaxPages {
		// A backstop for the test itself, should the limit stop working.
		return &gmproto.ListMessagesResponse{}, nil
	}
	id := fmt.Sprintf("e%d", g.pages)
	return &gmproto.ListMessagesResponse{
		Messages: []*gmproto.Message{makeMsg(id, "w", id, 400_000-int64(g.pages))},
		Cursor:   &gmproto.Cursor{LastItemID: id},
	}, nil
}

// A window backfill stops paging a conversation at windowBackfillMaxPages and
// reports it, rather than paging forever.
func TestHistoryWindowBackfillStopsAtThePageLimit(t *testing.T) {
	mock, since := windowMock()
	gm := &endlessGM{mockGMClient: mock}
	a, _, _ := hcdNewApp(t, mock, &hcdIngress{})
	a.gmClient = gm
	runWindow(t, a, since)

	if gm.pages != windowBackfillMaxPages {
		t.Fatalf("pages fetched = %d, want %d", gm.pages, windowBackfillMaxPages)
	}
	progress := a.GetBackfillProgress()
	if progress.Errors != 1 || !strings.Contains(strings.Join(progress.ErrorDetails, " "), "page limit") {
		t.Fatalf("progress = %+v, want one page-limit error", progress)
	}
	if progress.MessagesFound != windowBackfillMaxPages {
		t.Fatalf("messages found = %d, want %d", progress.MessagesFound, windowBackfillMaxPages)
	}
}

// A phone backfill is a one-shot user request: it finishes even when the
// client reconnects partway.
func TestHistoryPhoneBackfillFinishesAcrossAClientChange(t *testing.T) {
	mock := hcdTwoConversationMock()
	mock.messages["t1"] = [][]*gmproto.Message{
		{makeMsg("t1-a", "t1", "a", 3000)},
		{makeMsg("t1-b", "t1", "b", 2000)},
		{makeMsg("t1-c", "t1", "c", 1000)},
	}
	mock.getOrCreateResults = map[string]*gmproto.Conversation{"+15550001111": makeConv("t1", "One")}
	ingress := &hcdIngress{}
	a, _, _ := hcdNewApp(t, mock, ingress)
	mock.afterFetchMessages = func(_ string, page int) {
		if page == 0 {
			a.gmClient = &mockGMClient{}
		}
	}
	if err := a.BackfillConversationByPhone("+15550001111"); err != nil {
		t.Fatalf("BackfillConversationByPhone(): %v", err)
	}
	if got := legacyIDs(t, a, "t1"); got != "t1-a,t1-b,t1-c" {
		t.Fatalf("legacy = %s, want every page despite the client change", got)
	}
}
