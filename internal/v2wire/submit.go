package v2wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Deps are the transitional stores and v2 services shared by HTTP and later
// Wave-3 callers.
type Deps struct {
	Legacy   *db.Store
	V2       *sqlite.Store
	Service  *messaging.MessageService
	Registry bridge.Registry
}

type TextInput struct {
	ConversationID string
	Body           string
	ReplyToID      string
	IdempotencyKey string
	NotBefore      time.Time
	TTL            time.Duration
	Force          bool
}

type MediaInput struct {
	ConversationID string
	Content        io.Reader
	Filename       string
	MIME           string
	Caption        string
	ReplyToID      string
	IdempotencyKey string
	NotBefore      time.Time
	TTL            time.Duration
}

// SubmitText mirrors only the graph needed by the durable service, rejects
// non-live/unwired accounts before enqueue, and forwards caller-controlled
// idempotency and scheduling unchanged.
func SubmitText(ctx context.Context, deps Deps, input TextInput) (messaging.Submission, error) {
	if err := validateSubmitDeps(ctx, deps); err != nil {
		return messaging.Submission{}, err
	}
	accountID, conversationID, err := MirrorConversation(deps.Legacy, deps.V2, input.ConversationID)
	if err != nil {
		return messaging.Submission{}, err
	}
	if !deps.Registry.Capabilities(accountID).TextSend {
		return messaging.Submission{}, fmt.Errorf(
			"%w: account %q does not support text sends",
			ErrPlatformNotSendable,
			accountID,
		)
	}

	replyToMessageID := ""
	if input.ReplyToID != "" {
		replyToMessageID, err = mirrorSubmitReplyTarget(
			ctx,
			deps,
			input.ReplyToID,
			accountID,
			conversationID,
		)
		if err != nil {
			return messaging.Submission{}, err
		}
	}
	return deps.Service.SendText(ctx, messaging.SendTextCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      accountID,
			ConversationID: conversationID,
			IdempotencyKey: input.IdempotencyKey,
			NotBefore:      input.NotBefore,
			TTL:            input.TTL,
			Force:          input.Force,
		},
		Body:             input.Body,
		ReplyToMessageID: replyToMessageID,
	})
}

// SubmitMedia is the streaming media counterpart to SubmitText.
func SubmitMedia(ctx context.Context, deps Deps, input MediaInput) (messaging.Submission, error) {
	if err := validateSubmitDeps(ctx, deps); err != nil {
		return messaging.Submission{}, err
	}
	accountID, conversationID, err := MirrorConversation(deps.Legacy, deps.V2, input.ConversationID)
	if err != nil {
		return messaging.Submission{}, err
	}
	if !deps.Registry.Capabilities(accountID).MediaSend {
		return messaging.Submission{}, fmt.Errorf(
			"%w: account %q does not support media sends",
			ErrPlatformNotSendable,
			accountID,
		)
	}

	replyToMessageID := ""
	if input.ReplyToID != "" {
		replyToMessageID, err = mirrorSubmitReplyTarget(
			ctx,
			deps,
			input.ReplyToID,
			accountID,
			conversationID,
		)
		if err != nil {
			return messaging.Submission{}, err
		}
	}
	return deps.Service.SendMedia(ctx, messaging.SendMediaCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      accountID,
			ConversationID: conversationID,
			IdempotencyKey: input.IdempotencyKey,
			NotBefore:      input.NotBefore,
			TTL:            input.TTL,
		},
		Content:          input.Content,
		Filename:         input.Filename,
		MIME:             input.MIME,
		Caption:          input.Caption,
		ReplyToMessageID: replyToMessageID,
	})
}

func validateSubmitDeps(ctx context.Context, deps Deps) error {
	if ctx == nil {
		return errors.New("submit message: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("submit message: %w", err)
	}
	if deps.Legacy == nil {
		return errors.New("submit message: legacy store is nil")
	}
	if deps.V2 == nil {
		return errors.New("submit message: v2 store is nil")
	}
	if deps.Service == nil {
		return errors.New("submit message: service is nil")
	}
	if deps.Registry == nil {
		return errors.New("submit message: registry is nil")
	}
	return nil
}

func mirrorSubmitReplyTarget(
	ctx context.Context,
	deps Deps,
	legacyMessageID string,
	accountID string,
	conversationID string,
) (string, error) {
	messageID, err := MirrorReplyTarget(deps.Legacy, deps.V2, legacyMessageID)
	if err != nil {
		return "", err
	}
	repository, err := sqlite.NewMessageRepository(deps.V2, time.Now)
	if err != nil {
		return "", fmt.Errorf("validate mirrored reply target %q: %w", legacyMessageID, err)
	}
	target, err := repository.GetMessage(ctx, messageID)
	if err != nil {
		return "", fmt.Errorf("validate mirrored reply target %q: %w", legacyMessageID, err)
	}
	if target.AccountID != accountID || target.ConversationID != conversationID {
		return "", fmt.Errorf(
			"%w: legacy message %q belongs to conversation %q",
			ErrReplyTargetUnavailable,
			legacyMessageID,
			target.ConversationID,
		)
	}
	return messageID, nil
}
