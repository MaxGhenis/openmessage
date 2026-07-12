package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Run recovers expired work once at startup and then periodically dispatches
// due durable intents until ctx is canceled.
func (s *MessageService) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("run message service: context is nil")
	}
	recovered, err := s.outbox.RecoverExpiredLeases(ctx, s.clock.Now())
	if err != nil {
		return fmt.Errorf("run message service: recover expired leases: %w", err)
	}
	if recovered.NotDispatched > 0 || recovered.Uncertain > 0 {
		s.signalChange()
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.DispatchDue(ctx, s.batchLimit); err != nil {
			return fmt.Errorf("run message service: %w", err)
		}

		timer := s.clock.NewTimer(s.pollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-s.wake:
			timer.Stop()
		case <-timer.C():
		}
	}
}

// DispatchDue claims and synchronously processes at most limit due intents.
// Transport failures become durable states; only local state-machine failures
// are returned.
func (s *MessageService) DispatchDue(ctx context.Context, limit int) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("dispatch due messages: context is nil")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("dispatch due messages: limit must be positive")
	}
	leases, err := s.outbox.LeaseDue(ctx, sqlite.LeaseRequest{
		Owner:    workerOwner,
		Now:      s.clock.Now(),
		Duration: s.leaseTime,
		Limit:    limit,
	})
	if err != nil {
		return 0, fmt.Errorf("lease due messages: %w", err)
	}

	processed := 0
	for index, lease := range leases {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return processed, errors.Join(ctxErr, s.releaseUntouched(ctx, leases[index:]))
		}
		if lease.LeaseExpiresAtMS != nil && *lease.LeaseExpiresAtMS <= s.clock.Now().UnixMilli() {
			mutationCtx, cancel := s.storageMutationContext(ctx)
			recovered, recoverErr := s.outbox.RecoverExpiredLeases(mutationCtx, s.clock.Now())
			cancel()
			if recovered.NotDispatched > 0 || recovered.Uncertain > 0 {
				s.signalChange()
			}
			if recoverErr != nil {
				return processed, fmt.Errorf("recover leases expired during dispatch: %w", recoverErr)
			}
			break
		}
		if err := s.dispatchTextLease(ctx, lease); err != nil {
			return processed, errors.Join(err, s.releaseUntouched(ctx, leases[index+1:]))
		}
		processed++
	}
	return processed, nil
}

func (s *MessageService) dispatchTextLease(ctx context.Context, outboxLease sqlite.Lease) error {
	item := outboxLease.OutboxItem
	if item.LeaseToken == nil {
		return fmt.Errorf("dispatch outbox item %q: storage returned no lease token", item.OutboxID)
	}
	if item.Kind != sqlite.OutboxKindText {
		return s.releaseUnavailable(ctx, item, fmt.Errorf(
			"text dispatcher does not handle outbox kind %q",
			item.Kind,
		))
	}

	dispatchLease, err := s.bridges.Acquire(ctx, item.AccountID, bridge.CapabilityTextSend)
	if err != nil {
		if releaseErr := s.releaseUnavailable(ctx, item, err); releaseErr != nil {
			return releaseErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	if dispatchLease == nil || dispatchLease.Text == nil {
		if dispatchLease != nil {
			dispatchLease.Close()
		}
		return s.releaseUnavailable(ctx, item, bridge.ErrCapabilityUnavailable)
	}
	defer dispatchLease.Close()

	message, err := s.messageForLease(ctx, item)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_message",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	conversation, err := s.store.GetConversation(item.ConversationID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_conversation",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	if conversation.AccountID != item.AccountID {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"cross_account_conversation",
			fmt.Errorf("conversation %q belongs to account %q", item.ConversationID, conversation.AccountID),
			s.clock.Now().Add(s.retryDelay),
		)
	}

	if err := s.outbox.MarkTransportCalled(ctx, sqlite.Attempt{
		OutboxID:             item.OutboxID,
		LeaseToken:           *item.LeaseToken,
		AttemptToken:         item.OutboxID + ":" + *item.LeaseToken,
		ConnectionGeneration: uint64(dispatchLease.Generation),
		StartedAt:            s.clock.Now(),
	}); err != nil {
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), s.releaseUnavailable(ctx, item, err))
		}
		return fmt.Errorf("dispatch outbox item %q: mark transport called: %w", item.OutboxID, err)
	}
	s.signalChange()

	request := bridge.TextRequest{
		AccountID: item.AccountID,
		Conversation: bridge.ConversationRef{
			RemoteID: conversation.RemoteConversationID,
		},
		Body:      message.Body,
		RequestID: item.TransportRequestID,
	}
	if message.ReplyToRemoteID != nil {
		request.ReplyTo = &bridge.MessageRef{RemoteID: *message.ReplyToRemoteID}
	}
	result, sendErr := dispatchLease.Text.SendText(ctx, request)
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if sendErr != nil {
		return s.recordSendError(mutationCtx, item, sendErr)
	}
	if strings.TrimSpace(result.RemoteMessageID) == "" {
		if err := s.outbox.MarkUncertain(
			mutationCtx,
			item.OutboxID,
			*item.LeaseToken,
			"unknown",
			"missing_remote_message_id",
			"transport returned success without a remote message ID",
		); err != nil {
			return fmt.Errorf("dispatch outbox item %q: record invalid transport result: %w", item.OutboxID, err)
		}
		s.signalChange()
		return nil
	}

	confirmation := sqlite.Confirmation{
		OutboxID:       item.OutboxID,
		LeaseToken:     *item.LeaseToken,
		ResultRemoteID: result.RemoteMessageID,
	}
	if err := s.outbox.Confirm(mutationCtx, confirmation); err != nil {
		storeErr := s.outbox.MarkStoreFailed(
			mutationCtx,
			item.OutboxID,
			*item.LeaseToken,
			result.RemoteMessageID,
			err.Error(),
		)
		if storeErr != nil {
			return fmt.Errorf(
				"dispatch outbox item %q: confirm: %w",
				item.OutboxID,
				errors.Join(err, storeErr),
			)
		}
	}
	s.signalChange()
	return nil
}

func (s *MessageService) messageForLease(
	ctx context.Context,
	item sqlite.OutboxItem,
) (sqlite.Message, error) {
	if item.LocalMessageID == nil {
		return sqlite.Message{}, fmt.Errorf("outbox item has no local message ID")
	}
	message, err := s.messages.GetMessage(ctx, *item.LocalMessageID)
	if err != nil {
		return sqlite.Message{}, err
	}
	if message.AccountID != item.AccountID || message.ConversationID != item.ConversationID {
		return sqlite.Message{}, fmt.Errorf("local message does not match outbox account/conversation")
	}
	return message, nil
}

func (s *MessageService) releaseUnavailable(
	ctx context.Context,
	item sqlite.OutboxItem,
	cause error,
) error {
	if item.LeaseToken == nil {
		return fmt.Errorf("release unavailable outbox item %q: lease token is missing", item.OutboxID)
	}
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if err := s.outbox.ReleaseUnavailable(mutationCtx, item.OutboxID, *item.LeaseToken); err != nil {
		return fmt.Errorf(
			"release unavailable outbox item %q after %v: %w",
			item.OutboxID,
			cause,
			err,
		)
	}
	s.signalChange()
	return nil
}

func (s *MessageService) markPreCallFailure(
	ctx context.Context,
	item sqlite.OutboxItem,
	class, code string,
	cause error,
	retryAt time.Time,
) error {
	if item.LeaseToken == nil {
		return fmt.Errorf("mark outbox item %q not dispatched: lease token is missing", item.OutboxID)
	}
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if err := s.outbox.MarkNotDispatched(
		mutationCtx,
		item.OutboxID,
		*item.LeaseToken,
		class,
		code,
		cause.Error(),
		retryAt,
	); err != nil {
		return fmt.Errorf("mark outbox item %q not dispatched: %w", item.OutboxID, err)
	}
	s.signalChange()
	return nil
}

func (s *MessageService) releaseUntouched(ctx context.Context, leases []sqlite.Lease) error {
	if len(leases) == 0 {
		return nil
	}
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()

	var releaseErrors []error
	released := false
	for _, lease := range leases {
		if lease.LeaseToken == nil {
			releaseErrors = append(releaseErrors, fmt.Errorf(
				"release untouched outbox item %q: lease token is missing",
				lease.OutboxID,
			))
			continue
		}
		if err := s.outbox.ReleaseUnavailable(
			mutationCtx,
			lease.OutboxID,
			*lease.LeaseToken,
		); err != nil {
			releaseErrors = append(releaseErrors, fmt.Errorf(
				"release untouched outbox item %q: %w",
				lease.OutboxID,
				err,
			))
			continue
		}
		released = true
	}
	if released {
		s.signalChange()
	}
	return errors.Join(releaseErrors...)
}

func (s *MessageService) storageMutationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), defaultFinalizeTime)
}

func (s *MessageService) recordSendError(
	ctx context.Context,
	item sqlite.OutboxItem,
	sendErr error,
) error {
	opErr, classified := asOpError(sendErr)
	class := "unknown"
	code := "send_text"
	retryAt := s.clock.Now().Add(s.retryDelay)
	if classified {
		if opErr.Class != "" {
			class = string(opErr.Class)
		}
		if opErr.Operation != "" {
			code = opErr.Operation
		}
		if !opErr.RetryAt.IsZero() {
			retryAt = opErr.RetryAt
		}

		// An explicitly ambiguous result can never be converted into a safe or
		// terminal outcome based only on its failure class.
		if opErr.Dispatch == bridge.DispatchUncertain {
			return s.markSendUncertain(ctx, item, class, code, sendErr)
		}

		if terminalFailure(opErr.Class) {
			if err := s.outbox.Reject(
				ctx,
				item.OutboxID,
				*item.LeaseToken,
				class,
				code,
				sendErr.Error(),
			); err != nil {
				return fmt.Errorf("reject outbox item %q: %w", item.OutboxID, err)
			}
			s.signalChange()
			return nil
		}

		if retryableFailure(opErr.Class) && opErr.Dispatch == bridge.DispatchNotCalled {
			if err := s.outbox.MarkCalledNotDispatched(
				ctx,
				item.OutboxID,
				*item.LeaseToken,
				class,
				code,
				sendErr.Error(),
				retryAt,
			); err != nil {
				return fmt.Errorf(
					"mark called outbox item %q not dispatched: %w",
					item.OutboxID,
					err,
				)
			}
			s.signalChange()
			return nil
		}
	}
	if contextError(sendErr) {
		code = "timeout"
	}
	return s.markSendUncertain(ctx, item, class, code, sendErr)
}

func (s *MessageService) markSendUncertain(
	ctx context.Context,
	item sqlite.OutboxItem,
	class, code string,
	sendErr error,
) error {
	if err := s.outbox.MarkUncertain(
		ctx,
		item.OutboxID,
		*item.LeaseToken,
		class,
		code,
		sendErr.Error(),
	); err != nil {
		return fmt.Errorf("mark outbox item %q uncertain: %w", item.OutboxID, err)
	}
	s.signalChange()
	return nil
}

func retryableFailure(class bridge.FailureClass) bool {
	switch class {
	case bridge.FailureTransient,
		bridge.FailureRateLimited,
		bridge.FailureCredentialsExpired:
		return true
	default:
		return false
	}
}

func terminalFailure(class bridge.FailureClass) bool {
	switch class {
	case bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired,
		bridge.FailureMisconfigured,
		bridge.FailureUnsupported:
		return true
	default:
		return false
	}
}

func asOpError(err error) (bridge.OpError, bool) {
	var pointer *bridge.OpError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	var value bridge.OpError
	if errors.As(err, &value) {
		return value, true
	}
	return bridge.OpError{}, false
}
