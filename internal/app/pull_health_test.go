package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
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
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, &gmproto.ListConversationsResponse{}, nil)
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
	a.recordGoogleListPull(nil, "reconcile:listen_recovered", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	h := a.GooglePullHealth()
	if !h.EmptyWithLocalHistory || h.LastOutcome != GooglePullNoPayload || !h.AccountSwitch || h.LastError == "" {
		t.Fatalf("health = %+v", h)
	}
}

func TestEmptyInboxBelowThresholdDoesNotRaiseSignal(t *testing.T) {
	a := newPullHealthApp(t, googlePullEmptyThreshold-1)
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, nil, nil)
	if h := a.GooglePullHealth(); h.EmptyWithLocalHistory || h.ConsecutiveDataless != 1 {
		t.Fatalf("health = %+v", h)
	}
}

func TestOtherFoldersAndLaterPagesDoNotCount(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleListPull(nil, "deep", gmproto.ListConversationsRequest_SPAM_BLOCKED, true, nil, nil)
	a.recordGoogleListPull(nil, "deep", gmproto.ListConversationsRequest_ARCHIVE, true, nil, nil)
	a.recordGoogleListPull(nil, "deep", gmproto.ListConversationsRequest_INBOX, false, nil, nil)
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
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, nil, errors.New("connection reset"))
	if h := a.GooglePullHealth(); !h.EmptyWithLocalHistory || h.ConsecutiveDataless != 1 || h.LastOutcome != GooglePullError {
		t.Fatalf("a transport error changed the signal: %+v", h)
	}
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, listResp(3), nil)
	h := a.GooglePullHealth()
	if h.EmptyWithLocalHistory || h.ConsecutiveDataless != 0 || h.AccountSwitch || h.LastDataMS == 0 {
		t.Fatalf("data did not clear the signal: %+v", h)
	}
}

func TestLookupWithoutConversationCounts(t *testing.T) {
	a := newPullHealthApp(t, 1044)
	a.recordGoogleLookupPull(nil, "targeted", nil, nil)
	if h := a.GooglePullHealth(); !h.EmptyWithLocalHistory || h.LastOutcome != GooglePullEmpty {
		t.Fatalf("health = %+v", h)
	}
	a.recordGoogleLookupPull(nil, "targeted", makeConv("c1", "x"), nil)
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
	a.recordGoogleListPull(nil, "backfill", gmproto.ListConversationsRequest_INBOX, true, nil, nil)
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
				a.recordGoogleLookupPull(nil, "targeted", conv, e.err)
			} else {
				a.recordGoogleListPull(nil, "deep", e.folder, e.firstPage, listResp(e.count), e.err)
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

// Review regression (PR 193 finding 2): an older data-less pull that is still
// counting local conversations must not overwrite a later recovery.
func TestOlderPullCannotOverwriteLaterRecovery(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	a.SetGoogleConversationCounter(func() (int, error) {
		close(entered)
		<-release
		return 1044, nil
	})
	go func() {
		defer close(done)
		a.recordGoogleListPull(nil, "older-empty", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	}()
	<-entered
	a.recordGoogleListPull(nil, "later-recovery", gmproto.ListConversationsRequest_INBOX, true, listResp(1), nil)
	close(release)
	<-done
	h := a.GooglePullHealth()
	if h.EmptyWithLocalHistory || h.LastTrigger != "later-recovery" || h.ConsecutiveDataless != 0 || h.AccountSwitch || h.LastDataMS == 0 {
		t.Fatalf("older pull overwrote the recovery: %+v", h)
	}
}

// Two data-less pulls recorded concurrently both count toward the streak,
// whichever finishes its store count first.
func TestConcurrentDatalessPullsBothCount(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	first := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	a.SetGoogleConversationCounter(func() (int, error) {
		calls++
		if calls == 1 {
			close(first)
			<-release
		}
		return 1044, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.recordGoogleListPull(nil, "older", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(false))
	}()
	<-first
	a.recordGoogleListPull(nil, "newer", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	close(release)
	<-done
	h := a.GooglePullHealth()
	if h.ConsecutiveDataless != 2 || !h.EmptyWithLocalHistory || h.LastTrigger != "newer" || !h.AccountSwitch {
		t.Fatalf("health = %+v", h)
	}
}

// Invariant: the health state depends only on the pulls and their arrival
// sequence, never on the order in which their recorders finish. Checked by
// applying random record sets in sequence order and in a random permutation
// and comparing the full snapshots.
func TestPullHealthApplyOrderInvariance(t *testing.T) {
	rng := rand.New(rand.NewSource(1008))
	outcomes := []GooglePullOutcome{GooglePullOK, GooglePullEmpty, GooglePullNoPayload, GooglePullError}
	saved := googlePullDatalessCap
	googlePullDatalessCap = 3 // so random sets also exercise saturation
	t.Cleanup(func() { googlePullDatalessCap = saved })
	for iter := 0; iter < 500; iter++ {
		n := 1 + rng.Intn(10)
		recs := make([]googlePullRecord, n)
		for i := range recs {
			o := outcomes[rng.Intn(len(outcomes))]
			rec := googlePullRecord{
				seq:     uint64(i + 1),
				atMS:    int64(1000 + i),
				trigger: fmt.Sprintf("t%d", i),
				outcome: o,
				counted: rng.Intn(4) != 0,
				local:   []int{-1, 0, googlePullEmptyThreshold - 1, googlePullEmptyThreshold, 1044}[rng.Intn(5)],
			}
			if o == GooglePullOK {
				rec.count = 1 + rng.Intn(3)
			}
			if o == GooglePullNoPayload {
				rec.accountSwitch = rng.Intn(2) == 0
				rec.err = noPayloadErr(rec.accountSwitch)
			}
			if o == GooglePullError {
				rec.err = errors.New("transport")
			}
			recs[i] = rec
		}
		var inOrder, shuffled googlePullHealth
		for _, r := range recs {
			inOrder.apply(r)
		}
		for _, i := range rng.Perm(n) {
			shuffled.apply(recs[i])
		}
		if inOrder.snap != shuffled.snap {
			t.Fatalf("iter %d: apply order changed the result\nin order: %+v\nshuffled: %+v\nrecords: %+v", iter, inOrder.snap, shuffled.snap, recs)
		}
	}
}

// A retired client's late answer (for example a payload-less expiry that
// fires after a re-pair) must not touch the current session's health.
func TestPullFromRetiredClientIsIgnored(t *testing.T) {
	current := &mockGMClient{}
	a := newTestApp(t, current)
	a.SetGoogleConversationCounter(func() (int, error) { return 1044, nil })
	a.recordGoogleListPull(current, "backfill", gmproto.ListConversationsRequest_INBOX, true, listResp(2), nil)
	retired := &mockGMClient{}
	a.recordGoogleListPull(retired, "reconcile:old", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	h := a.GooglePullHealth()
	if h.EmptyWithLocalHistory || h.LastTrigger != "backfill" || h.ConsecutiveDataless != 0 {
		t.Fatalf("retired client's pull was recorded: %+v", h)
	}
}

func TestDatalessStreakSaturates(t *testing.T) {
	var h googlePullHealth
	for i := 1; i <= googlePullDatalessCap+50; i++ {
		h.apply(googlePullRecord{seq: uint64(i), outcome: GooglePullEmpty, counted: true, local: 1044})
	}
	if h.snap.ConsecutiveDataless != googlePullDatalessCap || !h.snap.EmptyWithLocalHistory {
		t.Fatalf("snap = %+v", h.snap)
	}
	h.apply(googlePullRecord{seq: uint64(googlePullDatalessCap + 51), outcome: GooglePullOK, counted: true, count: 1, local: -1})
	if h.snap.ConsecutiveDataless != 0 || h.snap.EmptyWithLocalHistory {
		t.Fatalf("data did not clear a saturated streak: %+v", h.snap)
	}
}

// Round-3 review: an account-switch answer must not count toward
// needs_repair. Three of them would park the transport, and in that state
// push is the only delivery still working.
func TestAccountSwitchSendErrorsDoNotParkTheTransport(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	a.SessionPath = t.TempDir() + "/session.json"
	if err := os.WriteFile(a.SessionPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < googleRepairThreshold+2; i++ {
		a.RecordGoogleSendError(fmt.Errorf("get or create conversation: %w", noPayloadErr(true)))
	}
	if a.GoogleStatus().NeedsRepair {
		t.Fatal("account-switch errors raised needs_repair, which parks the transport")
	}
}

func TestIsGoogleAccountSwitchError(t *testing.T) {
	if !IsGoogleAccountSwitchError(fmt.Errorf("wrapped: %w", noPayloadErr(true))) {
		t.Fatal("wrapped account-switch error not recognised")
	}
	if IsGoogleAccountSwitchError(noPayloadErr(false)) || IsGoogleAccountSwitchError(errors.New("x")) || IsGoogleAccountSwitchError(nil) {
		t.Fatal("false positive")
	}
}

// Round-2 review: a client replaced while a pull's local count runs must not
// have that pull applied; the check and the apply hold the client lock.
func TestPullFromClientRetiredDuringCountIsIgnored(t *testing.T) {
	oldClient := &mockGMClient{}
	a := newTestApp(t, oldClient)
	entered := make(chan struct{})
	release := make(chan struct{})
	a.SetGoogleConversationCounter(func() (int, error) {
		close(entered)
		<-release
		return 1044, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.recordGoogleListPull(oldClient, "reconcile:old", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	}()
	<-entered
	a.gmClient = &mockGMClient{} // replacement lands mid-count
	close(release)
	<-done
	if h := a.GooglePullHealth(); h != nil {
		t.Fatalf("pull from the retired client was applied: %+v", h)
	}
}

// Round-2 review: at saturation, a newer data-less pull applied before a
// delayed older recovery must survive it (the streak is 1, not 0).
func TestSaturatedStreakKeepsNewestAcrossDelayedRecovery(t *testing.T) {
	var h googlePullHealth
	n := uint64(googlePullDatalessCap)
	for i := uint64(1); i <= n; i++ {
		h.apply(googlePullRecord{seq: i, outcome: GooglePullEmpty, counted: true, local: 1044})
	}
	h.apply(googlePullRecord{seq: n + 2, outcome: GooglePullNoPayload, counted: true, local: 1044, accountSwitch: true})
	h.apply(googlePullRecord{seq: n + 1, outcome: GooglePullOK, counted: true, count: 3, local: -1})
	if h.snap.ConsecutiveDataless != 1 || !h.snap.EmptyWithLocalHistory || !h.snap.AccountSwitch {
		t.Fatalf("snap = %+v", h.snap)
	}
}

// Round-3 review: the retirement check on the production path (a real client
// generation, not the mock), with the replacement landing mid-count.
func TestPullFromGenerationRetiredDuringCountIsIgnored(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := &App{Store: store, Logger: zerolog.Nop()}
	oldCli := &client.Client{GM: libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop())}
	a.BeginGoogleGeneration(oldCli)
	entered := make(chan struct{})
	release := make(chan struct{})
	a.SetGoogleConversationCounter(func() (int, error) {
		close(entered)
		<-release
		return 1044, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.recordGoogleListPull(oldCli.GM, "reconcile:old", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	}()
	<-entered
	a.BeginGoogleGeneration(&client.Client{GM: libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop())})
	close(release)
	<-done
	if h := a.GooglePullHealth(); h != nil {
		t.Fatalf("pull from the retired generation was applied: %+v", h)
	}

	// A pull from the current generation is applied.
	a.SetGoogleConversationCounter(func() (int, error) { return 1044, nil })
	a.recordGoogleListPull(a.GetClient().GM, "reconcile:new", gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
	if h := a.GooglePullHealth(); h == nil || h.LastTrigger != "reconcile:new" || !h.EmptyWithLocalHistory {
		t.Fatalf("current-generation pull not applied: %+v", h)
	}
}

// Round-4 review: the other production retirement shapes. Release and
// SessionInvalid clear a.Client mid-count, so the pull is dropped; a client
// that stays current is applied. (Adapted from the round-3 reviewer's probe.)
func TestPullRetirementShapesOnProductionPath(t *testing.T) {
	cases := []struct {
		name    string
		retire  func(g *GoogleGeneration)
		applied bool
	}{
		{"release", func(g *GoogleGeneration) { g.Release() }, false},
		{"session-invalid", func(g *GoogleGeneration) { g.SessionInvalid() }, false},
		{"still-current", func(g *GoogleGeneration) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{Logger: zerolog.Nop(), SessionPath: t.TempDir() + "/session.json"}
			cli := &client.Client{GM: &libgm.Client{}}
			g := a.BeginGoogleGeneration(cli)
			counting, release := make(chan struct{}), make(chan struct{})
			a.SetGoogleConversationCounter(func() (int, error) {
				close(counting)
				<-release
				return 1044, nil
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.recordGoogleListPull(cli.GM, tc.name, gmproto.ListConversationsRequest_INBOX, true, nil, noPayloadErr(true))
			}()
			<-counting
			tc.retire(g)
			close(release)
			<-done
			got := a.GooglePullHealth()
			if (got != nil) != tc.applied {
				t.Fatalf("applied=%t, want %t: %+v", got != nil, tc.applied, got)
			}
			if tc.applied && (!got.EmptyWithLocalHistory || !got.AccountSwitch || got.LocalConversations != 1044) {
				t.Fatalf("current client's pull mis-recorded: %+v", got)
			}
		})
	}
}
