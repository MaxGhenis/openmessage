package app

import (
	"sync"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

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
