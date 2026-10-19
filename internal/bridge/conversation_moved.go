package bridge

import "fmt"

// FingerprintConversationMoved identifies an OpError whose Cause is a
// *ConversationMovedError.
const FingerprintConversationMoved = "conversation_moved"

// ConversationMovedError reports that the transport files a conversation
// under a different remote conversation ID than the one the request carried
// (for example after the phone re-keyed its threads). Adapters return it as
// the Cause of a DispatchNotCalled OpError before anything is sent, so the
// dispatcher can rebind the local conversation to ToRemoteID and retry.
// Sending under the new ID before that rebind would route the transport's
// echo of the sent message to a different local conversation.
type ConversationMovedError struct {
	FromRemoteID string
	ToRemoteID   string
}

func (e *ConversationMovedError) Error() string {
	if e == nil {
		return "conversation moved"
	}
	return fmt.Sprintf(
		"conversation moved: transport resolves remote conversation %q as %q",
		e.FromRemoteID,
		e.ToRemoteID,
	)
}
