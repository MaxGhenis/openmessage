package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func noPayloadErr(accountSwitch bool) error {
	return &libgm.ResponsePayloadError{
		Action:        gmproto.ActionType_LIST_CONVERSATIONS,
		Frames:        1,
		AccountSwitch: accountSwitch,
		Waited:        10 * time.Second,
	}
}

func newPullHealthApp(t *testing.T, local int) *App {
	t.Helper()
	a := newTestApp(t, &mockGMClient{})
	a.SetGoogleConversationCounter(func() (int, error) { return local, nil })
	return a
}

func listResp(n int) *gmproto.ListConversationsResponse {
	resp := &gmproto.ListConversationsResponse{}
	for i := 0; i < n; i++ {
		resp.Conversations = append(resp.Conversations, makeConv(fmt.Sprintf("c%d", i), "x"))
	}
	return resp
}

func TestClassifyGooglePull(t *testing.T) {
	cases := []struct {
		count int
		err   error
		want  GooglePullOutcome
	}{
		{3, nil, GooglePullOK},
		{0, nil, GooglePullEmpty},
		{0, noPayloadErr(false), GooglePullNoPayload},
		{0, fmt.Errorf("list conversations: %w", noPayloadErr(true)), GooglePullNoPayload},
		{0, errors.New("dial tcp: timeout"), GooglePullError},
	}
	for _, tc := range cases {
		if got := classifyGooglePull(tc.count, tc.err); got != tc.want {
			t.Errorf("classify(%d, %v) = %s, want %s", tc.count, tc.err, got, tc.want)
		}
	}
}

func TestPullHealthAbsentBeforeFirstPull(t *testing.T) {
	a := newPullHealthApp(t, 500)
	if a.GooglePullHealth() != nil {
		t.Fatal("pull health reported before any pull")
	}
	raw, err := json.Marshal(a.GoogleStatus())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pull_health") {
		t.Fatalf("status carries pull_health before any pull: %s", raw)
	}
}

// The 2026-10-07 shape: INBOX listings come back empty while the store holds
// ~1,000 Google conversations.
func TestEmptyInboxPullWithLocalHistoryRaisesSignal(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, &gmproto.ListConversationsResponse{}, nil)
	h := a.GooglePullHealth()
	if h == nil || !h.EmptyWithLocalHistory || h.LastOutcome != GooglePullEmpty || h.LocalConversations != 1044 || h.ConsecutiveDataless != 1 {
		t.Fatalf("health = %+v", h)
	}
	raw, _ := json.Marshal(a.GoogleStatus())
	if !strings.Contains(string(raw), `"empty_with_local_history":true`) {
		t.Fatalf("status JSON lacks the signal: %s", raw)
	}
}

func TestNoPayloadPullRaisesSignalAndKeepsAccountSwitch(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleListPull("reconcile:listen_recovered", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	h := a.GooglePullHealth()
	if !h.EmptyWithLocalHistory || h.LastOutcome != GooglePullNoPayload || !h.AccountSwitch || h.LastError == "" {
		t.Fatalf("health = %+v", h)
	}
}

func TestEmptyInboxBelowThresholdDoesNotRaiseSignal(t *testing.T) {
	a := newPullHealthApp(t, googlePullEmptyThreshold-1)
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, nil, nil)
	if h := a.GooglePullHealth(); h.EmptyWithLocalHistory || h.ConsecutiveDataless != 1 {
		t.Fatalf("health = %+v", h)
	}
}

func TestOtherFoldersAndLaterPagesDoNotCount(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleListPull("deep", gmproto.ListConversationsRequest_SPAM_BLOCKED, true, nil, nil)
	a.recordGoogleListPull("deep", gmproto.ListConversationsRequest_ARCHIVE, true, nil, nil)
	a.recordGoogleListPull("deep", gmproto.ListConversationsRequest_INBOX, false, nil, nil)
	h := a.GooglePullHealth()
	if h.EmptyWithLocalHistory || h.ConsecutiveDataless != 0 {
		t.Fatalf("legitimately empty pulls counted: %+v", h)
	}
	if h.LastOutcome != GooglePullEmpty || h.LastFolder != "INBOX" {
		t.Fatalf("last pull not recorded: %+v", h)
	}
}

func TestDataClearsSignalAndTransportErrorsLeaveIt(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, nil, errors.New("connection reset"))
	if h := a.GooglePullHealth(); !h.EmptyWithLocalHistory || h.ConsecutiveDataless != 1 || h.LastOutcome != GooglePullError {
		t.Fatalf("a transport error changed the signal: %+v", h)
	}
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, listResp(3), nil)
	h := a.GooglePullHealth()
	if h.EmptyWithLocalHistory || h.ConsecutiveDataless != 0 || h.AccountSwitch || h.LastDataMS == 0 {
		t.Fatalf("data did not clear the signal: %+v", h)
	}
}

func TestLookupWithoutConversationCounts(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleLookupPull("targeted", nil, nil)
	if h := a.GooglePullHealth(); !h.EmptyWithLocalHistory || h.LastOutcome != GooglePullEmpty {
		t.Fatalf("health = %+v", h)
	}
	a.recordGoogleLookupPull("targeted", makeConv("c1", "x"), nil)
	if h := a.GooglePullHealth(); h.EmptyWithLocalHistory || h.LastCount != 1 {
		t.Fatalf("health = %+v", h)
	}
}

func TestPullHealthFallsBackToLegacyStoreCount(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	for i := 0; i < googlePullEmptyThreshold; i++ {
		if err := a.storeConversation(makeConv(fmt.Sprintf("c%d", i), "x")); err != nil {
			t.Fatal(err)
		}
	}
	a.recordGoogleListPull("backfill", gmproto.ListConversationsRequest_INBOX, true, nil, nil)
	if h := a.GooglePullHealth(); h.LocalConversations != googlePullEmptyThreshold || !h.EmptyWithLocalHistory {
		t.Fatalf("health = %+v", h)
	}
}

type pullEvent struct {
	folder    gmproto.ListConversationsRequest_Folder
	firstPage bool
	lookup    bool
	count     int
	err       error
	local     int
}

// Reference model of the signal, written independently of the recorder:
// replay the counted pulls and look at the last data-or-dataless one.
func referenceSignal(events []pullEvent) (signal bool, consecutive int) {
	local := 0
	for _, e := range events {
		counted := e.lookup || (e.firstPage && e.folder == gmproto.ListConversationsRequest_INBOX)
		if !counted {
			continue
		}
		var payloadErr *libgm.ResponsePayloadError
		isNoPayload := errors.As(e.err, &payloadErr)
		switch {
		case e.err == nil && e.count > 0:
			consecutive = 0
			signal = false
		case isNoPayload || (e.err == nil && e.count == 0):
			consecutive++
			local = e.local
			signal = local >= googlePullEmptyThreshold
		}
	}
	return signal, consecutive
}

// Invariants, checked over random pull sequences against the reference model:
//   - empty_with_local_history holds iff the last counted data-or-dataless
//     pull was data-less and the store then held >= threshold conversations;
//   - consecutive_dataless counts data-less counted pulls since the last data;
//   - non-INBOX folders, later pages and transport errors never move either.
func TestPullHealthMatchesReferenceModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20261008))
	folders := []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX,
		gmproto.ListConversationsRequest_ARCHIVE,
		gmproto.ListConversationsRequest_SPAM_BLOCKED,
	}
	errs := []error{nil, nil, nil, noPayloadErr(false), noPayloadErr(true), errors.New("transport")}
	for iter := 0; iter < 400; iter++ {
		a := newTestApp(t, &mockGMClient{})
		local := 0
		a.SetGoogleConversationCounter(func() (int, error) { return local, nil })
		var events []pullEvent
		for n := rng.Intn(12); len(events) <= n; {
			e := pullEvent{
				folder:    folders[rng.Intn(len(folders))],
				firstPage: rng.Intn(4) != 0,
				lookup:    rng.Intn(5) == 0,
				err:       errs[rng.Intn(len(errs))],
				local:     []int{0, googlePullEmptyThreshold - 1, googlePullEmptyThreshold, 1044}[rng.Intn(4)],
			}
			if e.err == nil {
				e.count = rng.Intn(3)
				if e.lookup && e.count > 1 {
					e.count = 1
				}
			}
			events = append(events, e)
			local = e.local
			if e.lookup {
				var conv *gmproto.Conversation
				if e.count > 0 {
					conv = makeConv("c", "x")
				}
				a.recordGoogleLookupPull("targeted", conv, e.err)
			} else {
				a.recordGoogleListPull("deep", e.folder, e.firstPage, listResp(e.count), e.err)
			}
		}
		wantSignal, wantConsecutive := referenceSignal(events)
		h := a.GooglePullHealth()
		if h.EmptyWithLocalHistory != wantSignal || h.ConsecutiveDataless != wantConsecutive {
			t.Fatalf("iter %d: got signal=%t consecutive=%d, want %t/%d\nevents: %+v",
				iter, h.EmptyWithLocalHistory, h.ConsecutiveDataless, wantSignal, wantConsecutive, events)
		}
	}
}

// Through the real call sites: a deep backfill whose INBOX listing fails with
// a payload-less answer reports an error (not errors=0) and raises the signal.
func TestDeepBackfillRecordsPayloadlessInbox(t *testing.T) {
	mock := &mockGMClient{
		listConvErrors: map[gmproto.ListConversationsRequest_Folder]error{
			gmproto.ListConversationsRequest_INBOX: noPayloadErr(true),
		},
	}
	a := newTestApp(t, mock)
	a.SetGoogleConversationCounter(func() (int, error) { return 1044, nil })
	a.DeepBackfill()
	if errs := a.BackfillProgress.snapshot().Errors; errs == 0 {
		t.Fatal("payload-less INBOX listing was not counted as a backfill error")
	}
	if h := a.GooglePullHealth(); h == nil || !h.EmptyWithLocalHistory || !h.AccountSwitch {
		t.Fatalf("health = %+v", h)
	}
}

func TestBackfillAndReconcileRecordPulls(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {{makeConv("c1", "Alice")}},
		},
	}
	a := newTestApp(t, mock)
	a.SetGoogleConversationCounter(func() (int, error) { return 1044, nil })
	if err := a.Backfill(); err != nil {
		t.Fatal(err)
	}
	if h := a.GooglePullHealth(); h.LastTrigger != "backfill" || h.LastOutcome != GooglePullOK || h.LastCount != 1 {
		t.Fatalf("backfill health = %+v", h)
	}

	mock.conversations = nil
	a.reconcileRecentConversations("listen_recovered")
	h := a.GooglePullHealth()
	if h.LastTrigger != "reconcile:listen_recovered" || h.LastOutcome != GooglePullEmpty || !h.EmptyWithLocalHistory {
		t.Fatalf("reconcile health = %+v", h)
	}

	mock.listConvErrors = map[gmproto.ListConversationsRequest_Folder]error{
		gmproto.ListConversationsRequest_INBOX: noPayloadErr(false),
	}
	if err := a.Backfill(); !errors.Is(err, libgm.ErrNoResponsePayload) {
		t.Fatalf("Backfill err = %v, want ErrNoResponsePayload", err)
	}
	if h := a.GooglePullHealth(); h.LastOutcome != GooglePullNoPayload || h.ConsecutiveDataless != 2 {
		t.Fatalf("health = %+v", h)
	}
}

func TestTargetedBackfillRecordsLookup(t *testing.T) {
	mock := &mockGMClient{getOrCreateErrs: map[string]error{"+15550000000": noPayloadErr(true)}}
	a := newTestApp(t, mock)
	a.SetGoogleConversationCounter(func() (int, error) { return 1044, nil })
	if err := a.BackfillConversationByPhone("+15550000000"); !errors.Is(err, libgm.ErrNoResponsePayload) {
		t.Fatalf("err = %v", err)
	}
	if h := a.GooglePullHealth(); h.LastTrigger != "targeted" || !h.EmptyWithLocalHistory || !h.AccountSwitch {
		t.Fatalf("health = %+v", h)
	}
}
