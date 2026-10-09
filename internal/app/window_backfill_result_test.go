package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// convAt is a listed conversation whose last message is at lastMS.
func convAt(id string, lastMS int64) *gmproto.Conversation {
	return &gmproto.Conversation{ConversationID: id, Name: id, LastMessageTimestamp: lastMS * 1000}
}

func TestRunGoogleWindowBackfillReportsTheRun(t *testing.T) {
	since := time.UnixMilli(1_000_000)
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{
				convAt("in-window", 1_500_000),
				convAt("old", 500_000),
				convAt("unknown-time", 0),
			}},
			gmproto.ListConversationsRequest_ARCHIVE: {{convAt("archived-in-window", 1_200_000)}},
		},
		messages: map[string][][]*gmproto.Message{
			"in-window": {{
				makeMsg("m2", "in-window", "newest", 1_500_000),
				makeMsg("m1", "in-window", "older than since", 900_000),
			}},
			"unknown-time": {{makeMsg("u1", "unknown-time", "hi", 1_100_000)}},
			// "archived-in-window" says its last message is inside the window
			// but its fetch returns nothing: the empty-response defect.
		},
	}
	a := newTestApp(t, mock)

	result, started := a.RunGoogleWindowBackfill(since, BackfillTriggerSilenceRecovery)
	if !started {
		t.Fatal("RunGoogleWindowBackfill did not start with the guard free")
	}
	if !result.Connected || result.Aborted {
		t.Fatalf("connected/aborted = %v/%v, want true/false", result.Connected, result.Aborted)
	}
	if result.Listed != 4 {
		t.Errorf("Listed = %d, want 4 (every distinct listed conversation)", result.Listed)
	}
	if result.Conversations != 3 {
		t.Errorf("Conversations = %d, want 3 (in-window, unknown-time, archived-in-window)", result.Conversations)
	}
	if result.Messages != 3 {
		t.Errorf("Messages = %d, want 3", result.Messages)
	}
	if result.EmptyConversations != 1 {
		t.Errorf("EmptyConversations = %d, want 1 (archived-in-window fetched nothing)", result.EmptyConversations)
	}
	if !result.Since.Equal(since) || result.Trigger != BackfillTriggerSilenceRecovery {
		t.Errorf("since/trigger = %v/%q", result.Since, result.Trigger)
	}
	if result.FinishedAt.Before(result.StartedAt) {
		t.Errorf("finished %v before started %v", result.FinishedAt, result.StartedAt)
	}
	snap := a.GetBackfillProgress()
	if snap.Running {
		t.Error("backfill still reported running after a synchronous run")
	}
	if snap.Trigger != BackfillTriggerSilenceRecovery || snap.SinceMS != since.UnixMilli() {
		t.Errorf("snapshot trigger/since = %q/%d, want %q/%d", snap.Trigger, snap.SinceMS, BackfillTriggerSilenceRecovery, since.UnixMilli())
	}
	if a.IsDeepBackfillRunning() {
		t.Error("the backfill guard was not released")
	}
}

func TestRunGoogleWindowBackfillRespectsTheGuard(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	if !a.beginBackfill() {
		t.Fatal("guard unexpectedly held")
	}
	if _, started := a.RunGoogleWindowBackfill(time.UnixMilli(1), BackfillTriggerSilenceRecovery); started {
		t.Fatal("RunGoogleWindowBackfill ran while another backfill held the guard")
	}
	if !a.IsDeepBackfillRunning() {
		t.Fatal("a refused run released someone else's guard")
	}
	a.endBackfill()
}

func TestRunGoogleWindowBackfillWithoutClient(t *testing.T) {
	a := newTestApp(t, nil)
	a.gmClient = nil
	result, started := a.RunGoogleWindowBackfill(time.UnixMilli(1), BackfillTriggerSilenceRecovery)
	if !started {
		t.Fatal("the guard was free, so the run should start")
	}
	if result.Connected {
		t.Fatal("Connected = true with no client")
	}
	if a.IsDeepBackfillRunning() {
		t.Fatal("the guard was not released")
	}
}

func TestRunGoogleWindowBackfillReportsAnAuthAbort(t *testing.T) {
	mock := &mockGMClient{
		listConvErrors: map[gmproto.ListConversationsRequest_Folder]error{
			gmproto.ListConversationsRequest_INBOX: errors.New("http 401: session_cookie_invalid"),
		},
	}
	a := newTestApp(t, mock)
	result, started := a.RunGoogleWindowBackfill(time.UnixMilli(1), BackfillTriggerSilenceRecovery)
	if !started || !result.Connected {
		t.Fatalf("started/connected = %v/%v", started, result.Connected)
	}
	if !result.Aborted {
		t.Fatal("an auth failure did not abort the run")
	}
	if result.Errors == 0 {
		t.Error("the failed listing was not counted")
	}
}

func TestStartGoogleWindowBackfillRecordsWindowTrigger(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{convAt("c", 2_000)}},
		},
		messages: map[string][][]*gmproto.Message{"c": {{makeMsg("m", "c", "x", 2_000)}}},
	}
	a := newTestApp(t, mock)
	if !a.StartGoogleWindowBackfill(time.UnixMilli(1_000)) {
		t.Fatal("StartGoogleWindowBackfill refused with the guard free")
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.IsDeepBackfillRunning() {
		if time.Now().After(deadline) {
			t.Fatal("window backfill did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap := a.GetBackfillProgress(); snap.Trigger != BackfillTriggerWindow || snap.SinceMS != 1_000 {
		t.Fatalf("trigger/since = %q/%d, want %q/1000", snap.Trigger, snap.SinceMS, BackfillTriggerWindow)
	}
}

// countingIngress is a GoogleHistoryIngress that accepts everything except
// the message ids in refuse.
type countingIngress struct {
	mu            sync.Mutex
	conversations int
	messages      int
	refuse        map[string]bool
}

func (c *countingIngress) AppendHistoryConversation(context.Context, *gmproto.Conversation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conversations++
	return nil
}

func (c *countingIngress) AppendHistoryMessage(_ context.Context, _ string, _ *gmproto.Conversation, message *gmproto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse[message.GetMessageID()] {
		return errors.New("v2 refused it")
	}
	c.messages++
	return nil
}

func TestWindowBackfillResultCountsV2HandOffs(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{convAt("a", 2_000), convAt("b", 3_000)}},
		},
		messages: map[string][][]*gmproto.Message{
			"a": {{makeMsg("a1", "a", "x", 2_000)}},
			"b": {{makeMsg("b2", "b", "y", 3_000), makeMsg("b1", "b", "z", 2_500)}},
		},
	}
	a := newTestApp(t, mock)
	ingress := &countingIngress{refuse: map[string]bool{"b1": true}}
	a.gmHistory = ingress
	result, started := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	if !started {
		t.Fatal("did not start")
	}
	// Two conversations and two messages reached v2; one message was refused.
	if result.HistoryTeed != 4 || result.HistoryTeeFailed != 1 {
		t.Fatalf("teed/failed = %d/%d, want 4/1", result.HistoryTeed, result.HistoryTeeFailed)
	}
	if ingress.conversations != 2 || ingress.messages != 2 {
		t.Fatalf("ingress saw %d conversations, %d messages", ingress.conversations, ingress.messages)
	}
}

func TestWindowBackfillCountsListedOnceAndErrorsApartFromEmpties(t *testing.T) {
	old := convAt("old", 500)
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			// "old" is listed on two INBOX pages and in ARCHIVE.
			gmproto.ListConversationsRequest_INBOX: {
				{old, convAt("fails", 2_000)},
				{old, convAt("empty", 2_000)},
			},
			gmproto.ListConversationsRequest_ARCHIVE: {{old}},
		},
		fetchMsgErrors: map[string]error{"fails": errors.New("phone hung up")},
	}
	a := newTestApp(t, mock)
	result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	if result.Listed != 3 {
		t.Errorf("Listed = %d, want 3 distinct conversations", result.Listed)
	}
	if result.EmptyConversations != 1 {
		t.Errorf("EmptyConversations = %d, want 1: a failed fetch is an error, not an empty answer", result.EmptyConversations)
	}
	if result.Errors != 1 {
		t.Errorf("Errors = %d, want 1", result.Errors)
	}
}

func TestWindowBackfillCountsLegacyStoreFailures(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{convAt("c", 2_000)}},
		},
		messages: map[string][][]*gmproto.Message{"c": {{makeMsg("m", "c", "x", 2_000)}}},
	}
	a := newTestApp(t, mock)
	// The legacy store goes away: every write fails.
	a.Store.Close()
	result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	if result.Messages != 1 {
		t.Fatalf("Messages = %d", result.Messages)
	}
	if result.Errors < 2 {
		t.Fatalf("Errors = %d, want the failed conversation and message writes counted", result.Errors)
	}
}

// blockingGM answers listings only when release is closed.
type blockingGM struct {
	mockGMClient
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingGM) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	b.calls.Add(1)
	<-b.release
	return b.mockGMClient.ListConversationsWithCursor(count, folder, cursor)
}

func TestRecoveryRunGivesUpOnARequestThePhoneNeverAnswers(t *testing.T) {
	saved := googleRecoveryCallDeadline
	googleRecoveryCallDeadline = 50 * time.Millisecond
	t.Cleanup(func() { googleRecoveryCallDeadline = saved })

	gm := &blockingGM{release: make(chan struct{})}
	t.Cleanup(func() { close(gm.release) })
	a := newTestApp(t, nil)
	a.gmClient = gm

	done := make(chan GoogleWindowBackfillResult, 1)
	go func() {
		result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
		done <- result
	}()
	var result GoogleWindowBackfillResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the recovery run hung on an unanswered request")
	}
	if result.Errors == 0 {
		t.Fatal("the missed deadline was not counted as an error")
	}
	if result.InboxOutcome != GooglePullError {
		t.Fatalf("InboxOutcome = %q, want error for a missed INBOX deadline", result.InboxOutcome)
	}
	// After the first miss, later listings fail at once instead of each
	// waiting out its own deadline.
	if calls := gm.calls.Load(); calls != 1 {
		t.Fatalf("%d listings reached the phone, want 1", calls)
	}
	if a.IsDeepBackfillRunning() {
		t.Fatal("the backfill guard is still held")
	}

}

func TestDeadlineGMClientPassesRepliesThrough(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{convAt("c", 2_000)}},
		},
		fetchMsgErrors: map[string]error{"c": errors.New("boom")},
	}
	client := newDeadlineGMClient(mock, time.Second)
	resp, err := client.ListConversationsWithCursor(10, gmproto.ListConversationsRequest_INBOX, nil)
	if err != nil || len(resp.GetConversations()) != 1 {
		t.Fatalf("list = %v, %v", resp, err)
	}
	if _, err := client.FetchMessages("c", 10, nil); err == nil || errors.Is(err, ErrGoogleCallDeadline) {
		t.Fatalf("fetch error = %v, want the client's own error", err)
	}
	// An ordinary error does not poison later calls.
	if _, err := client.ListConversationsWithCursor(10, gmproto.ListConversationsRequest_INBOX, nil); err != nil {
		t.Fatalf("later call failed: %v", err)
	}
}

func TestUnknownTimeConversationThatFetchesNothingIsNotEmpty(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			// No last-message time and no messages: nothing says it should
			// have any, so it is not evidence of a broken fetch.
			gmproto.ListConversationsRequest_INBOX: {{convAt("unknown", 0), convAt("c", 2_000)}},
		},
		messages: map[string][][]*gmproto.Message{"c": {{makeMsg("m", "c", "x", 2_000)}}},
	}
	a := newTestApp(t, mock)
	result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	if result.Conversations != 2 || result.EmptyConversations != 0 {
		t.Fatalf("conversations/empty = %d/%d, want 2/0", result.Conversations, result.EmptyConversations)
	}
}

func TestWindowBackfillRecordsItsOwnInboxListing(t *testing.T) {
	cases := []struct {
		name string
		mock *mockGMClient
		want GooglePullOutcome
	}{
		{"inbox with data", &mockGMClient{conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{convAt("c", 2_000)}},
		}}, GooglePullOK},
		{"inbox empty, archive has data", &mockGMClient{conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_ARCHIVE: {{convAt("a", 2_000)}},
		}}, GooglePullEmpty},
		{"inbox fails", &mockGMClient{listConvErrors: map[gmproto.ListConversationsRequest_Folder]error{
			gmproto.ListConversationsRequest_INBOX: errors.New("transport closed"),
		}}, GooglePullError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, tc.mock)
			result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
			if result.InboxOutcome != tc.want {
				t.Fatalf("InboxOutcome = %q, want %q", result.InboxOutcome, tc.want)
			}
		})
	}
}

func TestInboxOutcomeIgnoresOtherPulls(t *testing.T) {
	// Pull health is shared: an unrelated pull that returns data must not
	// change what this run's own INBOX listing recorded.
	mock := &mockGMClient{conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
		gmproto.ListConversationsRequest_ARCHIVE: {{convAt("a", 2_000)}},
	}}
	a := newTestApp(t, mock)
	a.recordGooglePull(a.gmClient, "targeted", "", 1, nil, true) // another catch-up's lookup succeeds
	result, _ := a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	a.recordGooglePull(a.gmClient, "targeted", "", 1, nil, true)
	if result.InboxOutcome != GooglePullEmpty {
		t.Fatalf("InboxOutcome = %q, want empty", result.InboxOutcome)
	}
}

// panickingGM panics inside a listing, as a libgm bug would.
type panickingGM struct{ mockGMClient }

func (*panickingGM) ListConversationsWithCursor(int, gmproto.ListConversationsRequest_Folder, *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	panic("libgm blew up")
}

func TestRecoveryRunPanicReachesTheCaller(t *testing.T) {
	a := newTestApp(t, nil)
	a.gmClient = &panickingGM{}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		a.RunGoogleWindowBackfill(time.UnixMilli(1_000), BackfillTriggerSilenceRecovery)
	}()
	// Before the deadline wrapper ran calls on their own goroutine, a libgm
	// panic reached the caller, whose tick recovers it. It still must, or it
	// takes the whole daemon down.
	if recovered != "libgm blew up" {
		t.Fatalf("recovered %v, want the libgm panic on the caller's goroutine", recovered)
	}
	if a.IsDeepBackfillRunning() {
		t.Fatal("the backfill guard is still held after the panic")
	}
}
