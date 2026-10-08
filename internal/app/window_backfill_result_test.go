package app

import (
	"errors"
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
