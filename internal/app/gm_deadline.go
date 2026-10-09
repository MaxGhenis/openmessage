package app

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// googleRecoveryCallDeadline bounds each request an automatic silence
// recovery makes. libgm reports a phone that is slow to answer after 5 s but
// then waits for the reply with no hard timeout. Tests shorten it.
var googleRecoveryCallDeadline = 2 * time.Minute

// ErrGoogleCallDeadline reports a Google request that got no reply within its
// deadline, or one refused because an earlier request of the same catch-up
// already timed out.
var ErrGoogleCallDeadline = errors.New("google request got no reply before its deadline")

// deadlineGMClient gives each call a deadline. A call that misses it returns
// ErrGoogleCallDeadline; its goroutine stays blocked inside libgm until the
// reply arrives or the process exits, since libgm cannot cancel a request.
// After the first miss every later call fails at once, so one catch-up leaves
// at most one such goroutine behind and finishes quickly.
type deadlineGMClient struct {
	inner    GMClient
	deadline time.Duration

	mu     sync.Mutex
	missed error
}

func newDeadlineGMClient(inner GMClient, deadline time.Duration) GMClient {
	return &deadlineGMClient{inner: inner, deadline: deadline}
}

func callWithDeadline[T any](c *deadlineGMClient, name string, call func() (T, error)) (T, error) {
	var zero T
	c.mu.Lock()
	missed := c.missed
	c.mu.Unlock()
	if missed != nil {
		return zero, missed
	}
	type reply struct {
		value    T
		err      error
		panicked any
	}
	done := make(chan reply, 1)
	go func() {
		// A panic inside libgm must not take the daemon down from a goroutine
		// nobody recovers; hand it to the caller, whose own recovery handles
		// it as before the deadline existed. Once the caller has given up on
		// the call, the panic is dropped with the reply.
		defer func() {
			if p := recover(); p != nil {
				done <- reply{panicked: p}
			}
		}()
		value, err := call()
		done <- reply{value: value, err: err}
	}()
	timer := time.NewTimer(c.deadline)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.panicked != nil {
			panic(r.panicked)
		}
		return r.value, r.err
	case <-timer.C:
		err := fmt.Errorf("%s: %w (%s)", name, ErrGoogleCallDeadline, c.deadline)
		c.mu.Lock()
		if c.missed == nil {
			c.missed = err
		}
		c.mu.Unlock()
		return zero, err
	}
}

func (c *deadlineGMClient) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	return callWithDeadline(c, "list conversations", func() (*gmproto.ListConversationsResponse, error) {
		return c.inner.ListConversationsWithCursor(count, folder, cursor)
	})
}

func (c *deadlineGMClient) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	return callWithDeadline(c, "fetch messages", func() (*gmproto.ListMessagesResponse, error) {
		return c.inner.FetchMessages(conversationID, count, cursor)
	})
}

func (c *deadlineGMClient) GetOrCreateConversation(req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	return callWithDeadline(c, "get or create conversation", func() (*gmproto.GetOrCreateConversationResponse, error) {
		return c.inner.GetOrCreateConversation(req)
	})
}

func (c *deadlineGMClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	return callWithDeadline(c, "list contacts", c.inner.ListContacts)
}

func (c *deadlineGMClient) GetParticipantThumbnail(participantIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return callWithDeadline(c, "participant thumbnail", func() (*gmproto.GetThumbnailResponse, error) {
		return c.inner.GetParticipantThumbnail(participantIDs...)
	})
}

func (c *deadlineGMClient) GetContactThumbnail(contactIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return callWithDeadline(c, "contact thumbnail", func() (*gmproto.GetThumbnailResponse, error) {
		return c.inner.GetContactThumbnail(contactIDs...)
	})
}

// inboxPullRecorder records how the catch-up's first INBOX listing went,
// classified the way pull health classifies it (classifyGooglePull). That
// listing is the counted pull #193's pull health records, but read off this
// run alone: the shared pull health snapshot can be stale, or overwritten by
// an unrelated pull while the run is going.
type inboxPullRecorder struct {
	GMClient

	mu       sync.Mutex
	recorded bool
	outcome  GooglePullOutcome
}

func (r *inboxPullRecorder) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	resp, err := r.GMClient.ListConversationsWithCursor(count, folder, cursor)
	if folder == gmproto.ListConversationsRequest_INBOX && cursor == nil {
		r.mu.Lock()
		if !r.recorded {
			r.recorded = true
			r.outcome = classifyGooglePull(len(resp.GetConversations()), err)
		}
		r.mu.Unlock()
	}
	return resp, err
}

// Outcome returns the first INBOX listing's outcome, or "" before one.
func (r *inboxPullRecorder) Outcome() GooglePullOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}
