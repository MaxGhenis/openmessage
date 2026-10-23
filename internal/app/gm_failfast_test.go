package app

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/client"
)

func unanswered(reason error) error {
	return &libgm.UnansweredRequestError{Action: gmproto.ActionType_LIST_MESSAGES, Reason: reason}
}

// scriptedGMClient returns the next scripted error from every method and
// counts the calls that reached it.
type scriptedGMClient struct {
	mu     sync.Mutex
	errs   []error // consumed in order; nil = success
	calls  int
	byName map[string]int
}

func (s *scriptedGMClient) next(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.byName == nil {
		s.byName = map[string]int{}
	}
	s.byName[name]++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *scriptedGMClient) ListConversationsWithCursor(int, gmproto.ListConversationsRequest_Folder, *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	if err := s.next("list"); err != nil {
		return nil, err
	}
	return &gmproto.ListConversationsResponse{}, nil
}

func (s *scriptedGMClient) FetchMessages(string, int64, *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	if err := s.next("fetch"); err != nil {
		return nil, err
	}
	return &gmproto.ListMessagesResponse{}, nil
}

func (s *scriptedGMClient) GetOrCreateConversation(*gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	if err := s.next("get-or-create"); err != nil {
		return nil, err
	}
	return &gmproto.GetOrCreateConversationResponse{}, nil
}

func (s *scriptedGMClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	if err := s.next("contacts"); err != nil {
		return nil, err
	}
	return &gmproto.ListContactsResponse{}, nil
}

func (s *scriptedGMClient) GetParticipantThumbnail(...string) (*gmproto.GetThumbnailResponse, error) {
	if err := s.next("participant-thumb"); err != nil {
		return nil, err
	}
	return &gmproto.GetThumbnailResponse{}, nil
}

func (s *scriptedGMClient) GetContactThumbnail(...string) (*gmproto.GetThumbnailResponse, error) {
	if err := s.next("contact-thumb"); err != nil {
		return nil, err
	}
	return &gmproto.GetThumbnailResponse{}, nil
}

// callAll invokes every GMClient method once and returns their errors.
func callAll(gm GMClient) []error {
	var errs []error
	_, err := gm.ListConversationsWithCursor(10, gmproto.ListConversationsRequest_INBOX, nil)
	errs = append(errs, err)
	_, err = gm.FetchMessages("c", 10, nil)
	errs = append(errs, err)
	_, err = gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{})
	errs = append(errs, err)
	_, err = gm.ListContacts()
	errs = append(errs, err)
	_, err = gm.GetParticipantThumbnail("p")
	errs = append(errs, err)
	_, err = gm.GetContactThumbnail("c")
	errs = append(errs, err)
	return errs
}

func TestFailFastStopsAfterFirstUnansweredRequest(t *testing.T) {
	for _, reason := range []error{libgm.ErrPhoneNotResponding, libgm.ErrConnectionClosed} {
		t.Run(reason.Error(), func(t *testing.T) {
			inner := &scriptedGMClient{errs: []error{nil, unanswered(reason)}}
			gm := newFailFastGMClient(inner)
			if _, err := gm.FetchMessages("a", 10, nil); err != nil {
				t.Fatal(err)
			}
			_, first := gm.FetchMessages("b", 10, nil)
			if !errors.Is(first, reason) || errors.Is(first, ErrGoogleCatchUpStopped) {
				t.Fatalf("first unanswered error = %v", first)
			}
			for i, err := range callAll(gm) {
				if !errors.Is(err, ErrGoogleCatchUpStopped) || !errors.Is(err, reason) {
					t.Fatalf("call %d after the miss: err = %v, want ErrGoogleCatchUpStopped wrapping %v", i, err, reason)
				}
				if !IsGoogleUnansweredError(err) {
					t.Fatalf("call %d: skipped error is not recognised as unanswered", i)
				}
			}
			if inner.calls != 2 {
				t.Fatalf("%d call(s) reached libgm, want 2 (none after the miss)", inner.calls)
			}
			if got := gm.Unanswered(); got != first {
				t.Fatalf("Unanswered() = %v, want the first miss", got)
			}
		})
	}
}

func TestFailFastLetsOtherErrorsThrough(t *testing.T) {
	payloadErr := &libgm.ResponsePayloadError{Action: gmproto.ActionType_LIST_CONVERSATIONS, AccountSwitch: true}
	authErr := errors.New("HTTP 401: invalid authentication credentials")
	inner := &scriptedGMClient{errs: []error{payloadErr, authErr, errors.New("network down")}}
	gm := newFailFastGMClient(inner)
	errs := callAll(gm)
	if !errors.Is(errs[0], libgm.ErrNoResponsePayload) || !IsGoogleAccountSwitchError(errs[0]) {
		t.Fatalf("payload error changed: %v", errs[0])
	}
	if errs[1] != authErr {
		t.Fatalf("auth error changed: %v", errs[1])
	}
	for i, err := range errs {
		if errors.Is(err, ErrGoogleCatchUpStopped) {
			t.Fatalf("call %d was skipped although no request went unanswered: %v", i, err)
		}
	}
	if inner.calls != len(errs) || gm.Unanswered() != nil {
		t.Fatalf("calls = %d, unanswered = %v; want every call through and no miss", inner.calls, gm.Unanswered())
	}
}

// Invariant, over random scripts of libgm results: calls reach libgm up to and
// including the first unanswered request and never after it; every call after
// it fails with ErrGoogleCatchUpStopped wrapping that first error; before it,
// results pass through unchanged.
func TestFailFastPropertyOverRandomScripts(t *testing.T) {
	rng := rand.New(rand.NewSource(20261009))
	outcomes := []func() error{
		func() error { return nil },
		func() error { return errors.New("transient") },
		func() error { return &libgm.ResponsePayloadError{Action: gmproto.ActionType_LIST_MESSAGES} },
		func() error { return unanswered(libgm.ErrPhoneNotResponding) },
		func() error { return unanswered(libgm.ErrConnectionClosed) },
		// Wrapped, as a caller might wrap it before it reaches the client.
		func() error { return fmt.Errorf("fetch: %w", unanswered(libgm.ErrPhoneNotResponding)) },
	}
	for iter := 0; iter < 3000; iter++ {
		n := 1 + rng.Intn(12)
		script := make([]error, n)
		firstMiss := -1
		for i := range script {
			script[i] = outcomes[rng.Intn(len(outcomes))]()
			if firstMiss < 0 && IsGoogleUnansweredError(script[i]) {
				firstMiss = i
			}
		}
		inner := &scriptedGMClient{errs: append([]error(nil), script...)}
		gm := newFailFastGMClient(inner)
		for i := 0; i < n; i++ {
			var err error
			switch rng.Intn(3) {
			case 0:
				_, err = gm.FetchMessages("c", 10, nil)
			case 1:
				_, err = gm.ListConversationsWithCursor(10, gmproto.ListConversationsRequest_INBOX, nil)
			default:
				_, err = gm.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{})
			}
			switch {
			case firstMiss < 0 || i <= firstMiss:
				if err != script[i] {
					t.Fatalf("iter %d call %d: err = %v, want the libgm result %v unchanged", iter, i, err, script[i])
				}
			default:
				if !errors.Is(err, ErrGoogleCatchUpStopped) || !errors.Is(err, script[firstMiss]) {
					t.Fatalf("iter %d call %d: err = %v, want ErrGoogleCatchUpStopped wrapping %v", iter, i, err, script[firstMiss])
				}
			}
		}
		wantCalls := n
		if firstMiss >= 0 {
			wantCalls = firstMiss + 1
		}
		if inner.calls != wantCalls {
			t.Fatalf("iter %d script %v: %d call(s) reached libgm, want %d", iter, script, inner.calls, wantCalls)
		}
	}
}

// Concurrent calls (thumbnail and contact fetches run on their own
// goroutines) are race-free, and once a miss is recorded no call reaches
// libgm. Run under -race.
func TestFailFastConcurrentCalls(t *testing.T) {
	inner := &scriptedGMClient{errs: []error{unanswered(libgm.ErrPhoneNotResponding)}}
	gm := newFailFastGMClient(inner)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callAll(gm)
		}()
	}
	wg.Wait()
	if gm.Unanswered() == nil {
		t.Fatal("the miss was not recorded")
	}
	before := inner.calls
	callAll(gm)
	if inner.calls != before {
		t.Fatal("a call reached libgm after the miss was recorded")
	}
}

// Every catch-up, whichever client it uses, fetches through the fail-fast
// wrapper.
func TestEveryCatchUpFailsFast(t *testing.T) {
	a := newTestApp(t, &mockGMClient{})
	catchUp := a.beginGoogleCatchUp("test")
	if _, ok := catchUp.gm.(*failFastGMClient); !ok {
		t.Fatalf("injected-client catch-up uses %T, want *failFastGMClient", catchUp.gm)
	}
	catchUp.finish()

	a.gmClient = nil
	a.Client = &client.Client{GM: libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop())}
	catchUp = a.beginGoogleCatchUp("test")
	if catchUp == nil {
		t.Fatal("no catch-up for a connected libgm client")
	}
	defer catchUp.finish()
	ff, ok := catchUp.gm.(*failFastGMClient)
	if !ok {
		t.Fatalf("libgm catch-up uses %T, want *failFastGMClient", catchUp.gm)
	}
	if _, ok := ff.inner.(*realGMClient); !ok {
		t.Fatalf("libgm catch-up wraps %T, want *realGMClient", ff.inner)
	}
}

// countingGMClient counts FetchMessages calls that reach the mock, failed or
// not.
type countingGMClient struct {
	*mockGMClient
	mu      sync.Mutex
	fetches int
}

func (c *countingGMClient) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	c.mu.Lock()
	c.fetches++
	c.mu.Unlock()
	return c.mockGMClient.FetchMessages(conversationID, count, cursor)
}

func silentPhoneMock(n int) *mockGMClient {
	var convs []*gmproto.Conversation
	errs := map[string]error{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		convs = append(convs, makeConv(id, id))
		errs[id] = unanswered(libgm.ErrPhoneNotResponding)
	}
	return &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {convs},
		},
		fetchMsgErrors: errs,
	}
}

// The startup backfill against a phone that stops answering after the
// listing waits for one request, not one per conversation, and releases the
// backfill guard.
func TestBackfillStopsAfterFirstUnansweredFetch(t *testing.T) {
	counting := &countingGMClient{mockGMClient: silentPhoneMock(8)}
	a := newTestApp(t, counting.mockGMClient)
	a.gmClient = counting
	if err := a.Backfill(); err != nil {
		t.Fatal(err)
	}
	if counting.fetches != 1 {
		t.Fatalf("%d fetch(es) reached libgm, want 1", counting.fetches)
	}
	if !a.beginBackfill() {
		t.Fatal("the backfill guard is still held")
	}
	a.endBackfill()
}

// A deep backfill likewise stops fetching after the first unanswered request
// and reports every skipped conversation as an error.
func TestDeepBackfillStopsAfterFirstUnansweredFetch(t *testing.T) {
	counting := &countingGMClient{mockGMClient: silentPhoneMock(6)}
	a := newTestApp(t, counting.mockGMClient)
	a.gmClient = counting
	a.DeepBackfill()
	if counting.fetches != 1 {
		t.Fatalf("%d fetch(es) reached libgm, want 1", counting.fetches)
	}
	progress := a.GetBackfillProgress()
	if progress.Running {
		t.Fatal("deep backfill still reports running")
	}
	if progress.Errors < 6 {
		t.Fatalf("errors = %d, want one per conversation (6)", progress.Errors)
	}
	skipped := 0
	for _, detail := range progress.ErrorDetails {
		if strings.Contains(detail, ErrGoogleCatchUpStopped.Error()) {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("no error detail says the fetches were skipped: %v", progress.ErrorDetails)
	}
}

// gatedGMClient blocks each FetchMessages until the test releases it with the
// error that call should return.
type gatedGMClient struct {
	scriptedGMClient
	entered chan struct{}
	release chan error
}

func (g *gatedGMClient) FetchMessages(string, int64, *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	g.entered <- struct{}{}
	return nil, <-g.release
}

// Two requests already in flight can both go unanswered; the catch-up keeps
// the first miss as its cause, not whichever finished last.
func TestFailFastKeepsTheFirstMissOfConcurrentRequests(t *testing.T) {
	gated := &gatedGMClient{entered: make(chan struct{}), release: make(chan error)}
	gm := newFailFastGMClient(gated)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := gm.FetchMessages("c", 10, nil)
			done <- err
		}()
	}
	<-gated.entered
	<-gated.entered // both calls are past the fail-fast check
	first := unanswered(libgm.ErrPhoneNotResponding)
	gated.release <- first
	<-done
	gated.release <- unanswered(libgm.ErrConnectionClosed)
	<-done
	if got := gm.Unanswered(); got != first {
		t.Fatalf("Unanswered() = %v, want the first miss %v", got, first)
	}
	_, err := gm.FetchMessages("c", 10, nil)
	if !errors.Is(err, libgm.ErrPhoneNotResponding) || errors.Is(err, libgm.ErrConnectionClosed) {
		t.Fatalf("skipped call wraps %v, want the first miss", err)
	}
}
