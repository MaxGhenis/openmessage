package v2wire

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// ErrReactionTargetUnavailable reports that a reaction names no message this
// store holds, or names it in a conversation the message is not in.
var ErrReactionTargetUnavailable = errors.New("reaction target is unavailable")

// ReactionInput is a reaction addressed the way v2 reads hand IDs out.
type ReactionInput struct {
	// ConversationID is optional: the target message decides the conversation
	// and the account. A supplied value must name that conversation, by its
	// v2 ID or by the remote ID that pre-cutover callers hold.
	ConversationID string
	// MessageID is the target's v2 message ID.
	MessageID string
	Emoji     string
	// Action is "add", "remove" or "switch"; empty means add.
	Action         string
	IdempotencyKey string
}

// SubmitReactionV2 resolves a reaction's account and conversation from its
// target message in the v2 store and submits it to the durable messaging
// service. It reads no legacy state.
func SubmitReactionV2(
	ctx context.Context,
	deps NativeDeps,
	input ReactionInput,
) (messaging.Submission, error) {
	if err := validateNativeSubmitDeps(ctx, deps); err != nil {
		return messaging.Submission{}, err
	}
	messageID := strings.TrimSpace(input.MessageID)
	if messageID == "" {
		return messaging.Submission{}, fmt.Errorf("%w: message ID is empty", messaging.ErrInvalidCommand)
	}
	action, err := reactionAction(input.Action)
	if err != nil {
		return messaging.Submission{}, err
	}

	repository, err := sqlite.NewMessageRepository(deps.V2, time.Now)
	if err != nil {
		return messaging.Submission{}, fmt.Errorf("resolve v2 reaction target %q: %w", messageID, err)
	}
	target, err := repository.GetMessage(ctx, messageID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return messaging.Submission{}, fmt.Errorf(
			"%w: no v2 message %q",
			ErrReactionTargetUnavailable,
			messageID,
		)
	}
	if err != nil {
		return messaging.Submission{}, fmt.Errorf("resolve v2 reaction target %q: %w", messageID, err)
	}
	conversation, err := deps.V2.GetConversation(target.ConversationID)
	if err != nil {
		return messaging.Submission{}, fmt.Errorf(
			"resolve v2 conversation %q of reaction target %q: %w",
			target.ConversationID,
			messageID,
			err,
		)
	}
	if !namesConversation(input.ConversationID, conversation) {
		return messaging.Submission{}, fmt.Errorf(
			"%w: v2 message %q is not in conversation %q",
			ErrReactionTargetUnavailable,
			messageID,
			strings.TrimSpace(input.ConversationID),
		)
	}
	if !deps.Registry.Capabilities(target.AccountID).Reactions {
		return messaging.Submission{}, fmt.Errorf(
			"%w: account %q does not support reactions",
			ErrPlatformNotSendable,
			target.AccountID,
		)
	}

	return deps.Service.SendReaction(ctx, messaging.SendReactionCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      target.AccountID,
			ConversationID: conversation.ConversationID,
			IdempotencyKey: input.IdempotencyKey,
		},
		TargetMessageID: target.MessageID,
		Emoji:           input.Emoji,
		Action:          action,
	})
}

// namesConversation reports whether a caller-supplied conversation key names
// the conversation: empty (the caller left it to the target), its v2 ID, or
// its remote ID, which is the key the legacy store used and v2 reads still
// accept (issue #155).
func namesConversation(supplied string, conversation sqlite.Conversation) bool {
	supplied = strings.TrimSpace(supplied)
	if supplied == "" || supplied == conversation.ConversationID {
		return true
	}
	// Only Signal remote IDs are normalized, and the rule (trim inside the
	// "signal:" prefixes) leaves every other platform's ID as it is.
	return v2keys.NormalizeRemoteConversationID("signal", supplied) == conversation.RemoteConversationID
}

func reactionAction(raw string) (bridge.ReactionAction, error) {
	switch action := bridge.ReactionAction(strings.ToLower(strings.TrimSpace(raw))); action {
	case "":
		return bridge.ReactionAdd, nil
	case bridge.ReactionAdd, bridge.ReactionRemove, bridge.ReactionSwitch:
		return action, nil
	default:
		return "", fmt.Errorf("%w: reaction action %q is invalid", messaging.ErrInvalidCommand, raw)
	}
}
