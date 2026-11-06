package v2wire

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// ErrUnknownConversation reports a send that names no v2 conversation. Sends
// take v2 conversation IDs only, the IDs v2 reads return. Reads also accept a
// conversation's remote ID (issue #155), but a send must not: a Google
// Messages thread number is device-local, and a phone change re-numbers
// threads (issue #177), so an old number can name another person's thread
// now. Showing the wrong thread is recoverable; texting it is not.
var ErrUnknownConversation = errors.New("unknown conversation")

// NativeDeps are the v2 store and services needed to submit using v2-native
// conversation and message IDs. Unlike Deps, this path has no legacy store.
type NativeDeps struct {
	V2       *sqlite.Store
	Service  *messaging.MessageService
	Registry bridge.Registry
}

// SubmitTextV2 resolves the account from an existing v2 conversation and
// submits directly to the durable messaging service without mirroring legacy
// state.
func SubmitTextV2(
	ctx context.Context,
	deps NativeDeps,
	input TextInput,
) (messaging.Submission, error) {
	if err := validateNativeSubmitDeps(ctx, deps); err != nil {
		return messaging.Submission{}, err
	}
	conversation, err := deps.V2.GetConversation(input.ConversationID)
	if err != nil {
		return messaging.Submission{}, conversationLookupError(input.ConversationID, err)
	}
	if !deps.Registry.Capabilities(conversation.AccountID).TextSend {
		return messaging.Submission{}, fmt.Errorf(
			"%w: account %q does not support text sends",
			ErrPlatformNotSendable,
			conversation.AccountID,
		)
	}

	replyToMessageID, err := validateNativeReplyTarget(
		ctx,
		deps.V2,
		input.ReplyToID,
		conversation.AccountID,
		conversation.ConversationID,
	)
	if err != nil {
		return messaging.Submission{}, err
	}
	return deps.Service.SendText(ctx, messaging.SendTextCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      conversation.AccountID,
			ConversationID: conversation.ConversationID,
			IdempotencyKey: input.IdempotencyKey,
			NotBefore:      input.NotBefore,
		},
		Body:             input.Body,
		ReplyToMessageID: replyToMessageID,
	})
}

// conversationLookupError wraps a failed lookup of a send's conversation. A
// missing row becomes ErrUnknownConversation, still wrapping the store's
// not-found error, so callers that map not-found keep working.
func conversationLookupError(conversationID string, err error) error {
	if errors.Is(err, sqlite.ErrNotFound) {
		return fmt.Errorf(
			"%w %q (sends take the conversation ID that reads return, not a platform's thread ID): %w",
			ErrUnknownConversation,
			conversationID,
			err,
		)
	}
	return fmt.Errorf("resolve v2 conversation %q: %w", conversationID, err)
}

// SubmitMediaV2 is the streaming media counterpart to SubmitTextV2.
func SubmitMediaV2(
	ctx context.Context,
	deps NativeDeps,
	input MediaInput,
) (messaging.Submission, error) {
	if err := validateNativeSubmitDeps(ctx, deps); err != nil {
		return messaging.Submission{}, err
	}
	conversation, err := deps.V2.GetConversation(input.ConversationID)
	if err != nil {
		return messaging.Submission{}, conversationLookupError(input.ConversationID, err)
	}
	if !deps.Registry.Capabilities(conversation.AccountID).MediaSend {
		return messaging.Submission{}, fmt.Errorf(
			"%w: account %q does not support media sends",
			ErrPlatformNotSendable,
			conversation.AccountID,
		)
	}

	replyToMessageID, err := validateNativeReplyTarget(
		ctx,
		deps.V2,
		input.ReplyToID,
		conversation.AccountID,
		conversation.ConversationID,
	)
	if err != nil {
		return messaging.Submission{}, err
	}
	return deps.Service.SendMedia(ctx, messaging.SendMediaCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      conversation.AccountID,
			ConversationID: conversation.ConversationID,
			IdempotencyKey: input.IdempotencyKey,
			NotBefore:      input.NotBefore,
		},
		Content:          input.Content,
		Filename:         input.Filename,
		MIME:             input.MIME,
		Caption:          input.Caption,
		ReplyToMessageID: replyToMessageID,
	})
}

func validateNativeSubmitDeps(ctx context.Context, deps NativeDeps) error {
	if ctx == nil {
		return errors.New("submit v2 message: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("submit v2 message: %w", err)
	}
	if deps.V2 == nil {
		return errors.New("submit v2 message: v2 store is nil")
	}
	if deps.Service == nil {
		return errors.New("submit v2 message: service is nil")
	}
	if deps.Registry == nil {
		return errors.New("submit v2 message: registry is nil")
	}
	return nil
}

func validateNativeReplyTarget(
	ctx context.Context,
	store *sqlite.Store,
	messageID string,
	accountID string,
	conversationID string,
) (string, error) {
	if messageID == "" {
		return "", nil
	}
	repository, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		return "", fmt.Errorf("validate v2 reply target %q: %w", messageID, err)
	}
	target, err := repository.GetMessage(ctx, messageID)
	if err != nil {
		return "", fmt.Errorf(
			"%w: load v2 message %q: %v",
			ErrReplyTargetUnavailable,
			messageID,
			err,
		)
	}
	if target.AccountID != accountID || target.ConversationID != conversationID {
		return "", fmt.Errorf(
			"%w: v2 message %q belongs to conversation %q",
			ErrReplyTargetUnavailable,
			messageID,
			target.ConversationID,
		)
	}
	return target.MessageID, nil
}
