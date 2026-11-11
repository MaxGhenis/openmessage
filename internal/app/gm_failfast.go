package app

import (
	"errors"
	"fmt"
	"sync"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// ErrGoogleCatchUpStopped marks a Google request a catch-up skipped because an
// earlier request of the same catch-up went unanswered. The skipped call's
// error also wraps that first error, so errors.Is still matches
// libgm.ErrPhoneNotResponding or libgm.ErrConnectionClosed.
var ErrGoogleCatchUpStopped = errors.New("skipped: an earlier request in this Google catch-up got no answer")

// IsGoogleUnansweredError reports whether err is a Google request that ended
// without an answer: the phone did not respond within libgm's request timeout,
// or the client disconnected while the request waited. The server may still
// have accepted it, so a send that fails this way may yet be delivered.
func IsGoogleUnansweredError(err error) bool {
	return errors.Is(err, libgm.ErrPhoneNotResponding) || errors.Is(err, libgm.ErrConnectionClosed)
}

// failFastGMClient stops a catch-up after its first unanswered request. libgm
// fails a request the phone doesn't answer within its request timeout, and
// every request still waiting when the client disconnects. A catch-up moves on
// to the next conversation after a failed fetch, so without this a run against
// a silent phone would wait out the timeout once per conversation while it
// holds the backfill guard. After the first unanswered request every later
// call fails at once with ErrGoogleCatchUpStopped, wrapping that request's
// error. Other errors (auth, payload-less answers) pass through untouched.
type failFastGMClient struct {
	inner GMClient

	mu         sync.Mutex
	unanswered error
}

func newFailFastGMClient(inner GMClient) *failFastGMClient {
	return &failFastGMClient{inner: inner}
}

// Unanswered returns the first unanswered request's error, or nil.
func (c *failFastGMClient) Unanswered() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unanswered
}

func failFast[T any](c *failFastGMClient, name string, call func() (T, error)) (T, error) {
	if first := c.Unanswered(); first != nil {
		var zero T
		return zero, fmt.Errorf("%s %w: %w", name, ErrGoogleCatchUpStopped, first)
	}
	value, err := call()
	if IsGoogleUnansweredError(err) {
		c.mu.Lock()
		if c.unanswered == nil {
			c.unanswered = err
		}
		c.mu.Unlock()
	}
	return value, err
}

func (c *failFastGMClient) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	return failFast(c, "list conversations", func() (*gmproto.ListConversationsResponse, error) {
		return c.inner.ListConversationsWithCursor(count, folder, cursor)
	})
}

func (c *failFastGMClient) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	return failFast(c, "fetch messages", func() (*gmproto.ListMessagesResponse, error) {
		return c.inner.FetchMessages(conversationID, count, cursor)
	})
}

func (c *failFastGMClient) GetOrCreateConversation(req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	return failFast(c, "get or create conversation", func() (*gmproto.GetOrCreateConversationResponse, error) {
		return c.inner.GetOrCreateConversation(req)
	})
}

func (c *failFastGMClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	return failFast(c, "list contacts", c.inner.ListContacts)
}

func (c *failFastGMClient) GetParticipantThumbnail(participantIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return failFast(c, "participant thumbnail", func() (*gmproto.GetThumbnailResponse, error) {
		return c.inner.GetParticipantThumbnail(participantIDs...)
	})
}

func (c *failFastGMClient) GetContactThumbnail(contactIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return failFast(c, "contact thumbnail", func() (*gmproto.GetThumbnailResponse, error) {
		return c.inner.GetContactThumbnail(contactIDs...)
	})
}
