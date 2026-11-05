package messaging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// Run dispatches due durable intents in passes until ctx is canceled, and
// returns only then. Each pass recovers expired leases, repairs store-failed
// confirmations and dispatches what is due.
//
// A pass that fails on local storage does not end Run. On 2026-11-03 one
// failed lease UPDATE returned from Run, and nothing left the outbox again
// until the app restarted. Run now records the failure (DispatchHealth and the
// OnPassFailure hook), waits a backoff that starts at the poll delay and
// doubles up to the longest poll delay, and tries again. A lease the failed
// pass left behind expires, and the next pass's recovery makes it dispatchable
// again, or uncertain if its transport call was made.
func (s *MessageService) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("run message service: context is nil")
	}
	var backoff time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.dispatchPass(ctx); err != nil {
			// Cancellation mid-query surfaces as a driver interrupt, not
			// context.Canceled, so a shutdown race would read as a failed
			// pass. As in DispatchDue's lease path, surface the cancellation
			// itself.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			backoff = nextPassBackoff(backoff, s.pollDelay, s.maxPollDelay)
			s.passes.fail(fmt.Errorf("run message service: %w", err), s.clock.Now(), backoff)
			// Only time ends the backoff: a wake from a new submission would
			// turn a storage fault that persists into a tight retry loop.
			timer := s.clock.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C():
			}
			continue
		}
		backoff = 0
		s.passes.succeed()

		timer := s.clock.NewTimer(s.nextPollDelay(ctx))
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

// dispatchPass is one round of Run. It recovers expired leases on every pass,
// not only at startup, so a lease that a failed pass or a crashed process left
// behind is freed once it expires. Recovery never causes a resend: passes run
// one at a time, a pass stops waiting on a call past its bound and marks its
// intent uncertain, and recovery requeues only a lease whose transport call
// never started (MarkTransportCalled commits before every call); a lease whose
// call started becomes uncertain.
//
// An expired lease whose recovery fails on its own row does not hold the
// pass: the store leaves that row as it was, still dispatching and so never
// sent, and recovers the others; the pass records the failure
// (DispatchHealth and the OnRecoveryFailure hook) and goes on to send. The
// next pass tries the row's recovery again.
func (s *MessageService) dispatchPass(ctx context.Context) error {
	if err := s.recoverExpiredLeases(ctx); err != nil {
		return fmt.Errorf("recover expired leases: %w", err)
	}
	if _, err := s.reconcileStoreFailedDue(ctx, s.batchLimit); err != nil {
		return fmt.Errorf("reconcile store-failed deliveries: %w", err)
	}
	if _, err := s.DispatchDue(ctx, s.batchLimit); err != nil {
		return err
	}
	return nil
}

// nextPassBackoff is how long Run waits after a failed pass: the shortest
// poll delay after the first failure, doubling with each failure in a row up
// to the longest poll delay.
func nextPassBackoff(previous, shortest, longest time.Duration) time.Duration {
	if shortest <= 0 {
		shortest = defaultPollDelay
	}
	longest = max(longest, shortest)
	if previous < shortest {
		return shortest
	}
	return min(previous*2, longest)
}

// DispatchDue claims and synchronously processes at most limit due intents.
// Transport failures become durable states; only local state-machine failures
// are returned.
//
// A due intent whose lease fails on its own row does not hold the others:
// LeaseDueBatch sets it aside and leases the rows after it, and DispatchDue
// records it (DispatchHealth and the OnLeaseFailure hook). A row the store
// could not set aside either stays due, so DispatchDue sends the rest and then
// returns an error naming it, and Run backs off before it meets that row again.
func (s *MessageService) DispatchDue(ctx context.Context, limit int) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("dispatch due messages: context is nil")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("dispatch due messages: limit must be positive")
	}
	batch, err := s.outbox.LeaseDueBatch(ctx, sqlite.LeaseRequest{
		Owner:    workerOwner,
		Now:      s.clock.Now(),
		Duration: s.leaseTime,
		Limit:    limit,
	})
	if err != nil {
		// Shutdown can race the lease transaction: database/sql auto-rolls-back
		// a transaction whose context is canceled, so the subsequent commit
		// fails. No lease was handed out, so this is a clean stop rather than a
		// dispatch fault. Surface the cancellation itself.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, fmt.Errorf("lease due messages: %w", err)
	}
	stuck := s.recordFailedLeases(batch.Failed)

	leases := batch.Leases
	processed := 0
	for index, lease := range leases {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return processed, errors.Join(ctxErr, s.releaseUntouched(ctx, leases[index:]), stuck)
		}
		if lease.LeaseExpiresAtMS != nil && *lease.LeaseExpiresAtMS <= s.clock.Now().UnixMilli() {
			mutationCtx, cancel := s.storageMutationContext(ctx)
			recoverErr := s.recoverExpiredLeases(mutationCtx)
			cancel()
			if recoverErr != nil {
				return processed, errors.Join(fmt.Errorf("recover leases expired during dispatch: %w", recoverErr), stuck)
			}
			break
		}
		var dispatchErr error
		switch lease.Kind {
		case sqlite.OutboxKindText:
			dispatchErr = s.dispatchTextLease(ctx, lease)
		case sqlite.OutboxKindMedia:
			dispatchErr = s.dispatchMediaLease(ctx, lease)
		case sqlite.OutboxKindReaction:
			dispatchErr = s.dispatchReactionLease(ctx, lease)
		case sqlite.OutboxKindRead:
			dispatchErr = s.dispatchReadLease(ctx, lease)
		default:
			dispatchErr = s.releaseUnavailable(ctx, lease.OutboxItem, fmt.Errorf(
				"message dispatcher does not handle outbox kind %q",
				lease.Kind,
			))
		}
		if dispatchErr != nil {
			return processed, errors.Join(dispatchErr, s.releaseUntouched(ctx, leases[index+1:]), stuck)
		}
		processed++
	}
	return processed, stuck
}

// recoverExpiredLeases ends every expired lease (RecoverExpiredLeases) and
// records each one whose recovery failed on its own row (DispatchHealth and
// the OnRecoveryFailure hook). The store leaves such a row as it was:
// dispatching, its lease expired. No call can start for it: LeaseDueBatch
// takes only queued or not_dispatched rows, and MarkTransportCalled refuses
// an expired lease. Cancel and RetryNotDispatched refuse it too. It holds
// back no other row, since LeaseDueBatch passes over it, and EarliestDue
// never wakes Run for it. So the row waits, unsent, until a later pass
// recovers it. Only a failure of the store comes back as an error.
func (s *MessageService) recoverExpiredLeases(ctx context.Context) error {
	now := s.clock.Now()
	recovered, err := s.outbox.RecoverExpiredLeases(ctx, now)
	if err != nil {
		return err
	}
	if recovered.NotDispatched > 0 || recovered.Uncertain > 0 {
		s.signalChange()
	}
	for _, failure := range recovered.Failed {
		s.recoveryFailures.add(FailedRecovery{
			OutboxID:        failure.OutboxID,
			AtMS:            now.UnixMilli(),
			Error:           failure.Err.Error(),
			TransportCalled: failure.TransportCalled,
		})
	}
	return nil
}

// recordFailedLeases records each due row whose lease failed on its own row
// and returns an error naming the rows the store could not set aside either.
func (s *MessageService) recordFailedLeases(failed []sqlite.LeaseFailure) error {
	if len(failed) == 0 {
		return nil
	}
	now := s.clock.Now()
	var stuck []error
	setAside := false
	for _, failure := range failed {
		s.leaseFailures.add(newFailedLease(failure, now))
		if failure.SetAsideErr != nil {
			stuck = append(stuck, fmt.Errorf(
				"could neither lease outbox item %q (%w) nor set it aside (%w)",
				failure.OutboxID, failure.Err, failure.SetAsideErr,
			))
			continue
		}
		setAside = true
	}
	if setAside {
		s.signalChange()
	}
	if len(stuck) == 0 {
		return nil
	}
	return fmt.Errorf("lease due messages: %w", errors.Join(stuck...))
}

func (s *MessageService) dispatchTextLease(ctx context.Context, outboxLease sqlite.Lease) error {
	item := outboxLease.OutboxItem
	if item.LeaseToken == nil {
		return fmt.Errorf("dispatch outbox item %q: storage returned no lease token", item.OutboxID)
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
	// An abandoned transport call may still be using the lease, so once the
	// call starts its own goroutine releases it.
	closeLease := true
	defer func() {
		if closeLease {
			dispatchLease.Close()
		}
	}()

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
	replyTo, code, err := s.replyRefForLease(ctx, item, message)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			code,
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}

	conversationRef, err := s.conversationRef(conversation)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_roster",
			err,
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
		AccountID:    item.AccountID,
		Conversation: conversationRef,
		Body:         message.Body,
		RequestID:    item.TransportRequestID,
	}
	request.ReplyTo = replyTo
	closeLease = false
	result, sendErr := callTransport(s, ctx, item, textOperation, s.transportTimeout,
		func(callCtx context.Context) (bridge.SendResult, error) {
			defer dispatchLease.Close()
			return dispatchLease.Text.SendText(callCtx, request)
		})
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if sendErr != nil {
		return s.recordSendError(mutationCtx, item, sendErr, textOperation)
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

func (s *MessageService) dispatchMediaLease(ctx context.Context, outboxLease sqlite.Lease) error {
	item := outboxLease.OutboxItem
	if item.LeaseToken == nil {
		return fmt.Errorf("dispatch outbox item %q: storage returned no lease token", item.OutboxID)
	}

	dispatchLease, err := s.bridges.Acquire(ctx, item.AccountID, bridge.CapabilityMediaSend)
	if err != nil {
		if errors.Is(err, bridge.ErrCapabilityUnavailable) &&
			!s.bridges.Capabilities(item.AccountID).MediaSend {
			return s.rejectPreCall(
				ctx,
				item,
				string(bridge.FailureUnsupported),
				"media_send_unsupported",
				err,
			)
		}
		if releaseErr := s.releaseUnavailable(ctx, item, err); releaseErr != nil {
			return releaseErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	if dispatchLease == nil || dispatchLease.Media == nil {
		if dispatchLease != nil {
			dispatchLease.Close()
		}
		return s.releaseUnavailable(ctx, item, bridge.ErrCapabilityUnavailable)
	}
	// An abandoned transport call may still be using the lease, so once the
	// call starts its own goroutine releases it.
	closeLease := true
	defer func() {
		if closeLease {
			dispatchLease.Close()
		}
	}()

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
	replyTo, code, err := s.replyRefForLease(ctx, item, message)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			code,
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}

	conversationRef, err := s.conversationRef(conversation)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_roster",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}

	attachment, err := s.outbox.GetOutboxAttachment(ctx, item.OutboxID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_media",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	reader, err := s.blobs.Open(blob.BlobRef{Hash: attachment.BlobHash})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.rejectPreCall(
				ctx,
				item,
				string(bridge.FailureMisconfigured),
				"blob_missing",
				err,
			)
		}
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"open_blob",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	// An abandoned media call may still be reading the attachment, so once the
	// call starts its own goroutine closes the reader.
	closeReader := true
	defer func() {
		if closeReader {
			_ = reader.Close()
		}
	}()

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

	request := bridge.MediaRequest{
		AccountID:    item.AccountID,
		Conversation: conversationRef,
		Reader:       reader,
		Size:         attachment.SizeBytes,
		Filename:     attachment.Filename,
		MIME:         attachment.MIME,
		Caption:      message.Body,
		RequestID:    item.TransportRequestID,
	}
	request.ReplyTo = replyTo
	closeReader = false
	closeLease = false
	result, sendErr := callTransport(s, ctx, item, mediaOperation, s.mediaTransportTimeout,
		func(callCtx context.Context) (bridge.SendResult, error) {
			defer dispatchLease.Close()
			defer reader.Close()
			return dispatchLease.Media.SendMedia(callCtx, request)
		})
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if sendErr != nil {
		return s.recordSendError(mutationCtx, item, sendErr, mediaOperation)
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

func (s *MessageService) dispatchReactionLease(ctx context.Context, outboxLease sqlite.Lease) error {
	item := outboxLease.OutboxItem
	if item.LeaseToken == nil {
		return fmt.Errorf("dispatch outbox item %q: storage returned no lease token", item.OutboxID)
	}

	dispatchLease, err := s.bridges.Acquire(ctx, item.AccountID, bridge.CapabilityReactions)
	if err != nil {
		if errors.Is(err, bridge.ErrCapabilityUnavailable) &&
			!s.bridges.Capabilities(item.AccountID).Reactions {
			return s.rejectPreCall(
				ctx,
				item,
				string(bridge.FailureUnsupported),
				"reactions_unsupported",
				err,
			)
		}
		if releaseErr := s.releaseUnavailable(ctx, item, err); releaseErr != nil {
			return releaseErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	if dispatchLease == nil || dispatchLease.Reaction == nil {
		if dispatchLease != nil {
			dispatchLease.Close()
		}
		return s.releaseUnavailable(ctx, item, bridge.ErrCapabilityUnavailable)
	}
	// An abandoned transport call may still be using the lease, so once the
	// call starts its own goroutine releases it.
	closeLease := true
	defer func() {
		if closeLease {
			dispatchLease.Close()
		}
	}()

	reaction, err := s.outbox.GetOutboxReaction(ctx, item.OutboxID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_reaction",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	target, send, code, err := s.targetRefForLease(ctx, item, reaction.TargetMessageID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			code,
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	afterReactionTargetRead()
	switch {
	case send != nil && send.progress == sendUnsent:
		// The target is this account's message whose send has not gone out,
		// so the platform has no message to react to yet. Sending would name
		// a message nobody has (on Google the phone may refuse it, on
		// WhatsApp the reaction would address an ID the send never uses), so
		// the reaction waits, uncalled, until the send is next due.
		if err := s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			sqlite.ReactionTargetUnsentCode,
			fmt.Errorf(
				"the message this reaction targets has not been sent yet (its send %s is %s)",
				send.row.OutboxID,
				send.row.State,
			),
			s.unsentTargetRetryAt(send.row),
		); err != nil {
			return err
		}
		s.wakeIfSendChanged(ctx, send.row)
		return nil
	case send != nil && send.progress == sendNeverSent:
		// The target is this account's message whose send was canceled or
		// rejected: the platform never got it and never will, so there is
		// nothing to react to.
		return s.rejectPreCall(
			ctx,
			item,
			string(bridge.FailureUnsupported),
			reactionTargetNeverSentCode,
			fmt.Errorf(
				"the message this reaction targets was never sent (its send %s is %s)",
				send.row.OutboxID,
				send.row.State,
			),
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
			"load_conversation",
			fmt.Errorf("conversation %q belongs to account %q", item.ConversationID, conversation.AccountID),
			s.clock.Now().Add(s.retryDelay),
		)
	}

	conversationRef, err := s.conversationRef(conversation)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_roster",
			err,
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

	reactionRequest := bridge.ReactionRequest{
		AccountID:    item.AccountID,
		Conversation: conversationRef,
		Target:       target,
		Emoji:        reaction.Emoji,
		Action:       bridge.ReactionAction(reaction.Action),
		RequestID:    item.TransportRequestID,
	}
	closeLease = false
	result, sendErr := callTransport(s, ctx, item, reactionOperation, s.transportTimeout,
		func(callCtx context.Context) (bridge.SendResult, error) {
			defer dispatchLease.Close()
			return dispatchLease.Reaction.SendReaction(callCtx, reactionRequest)
		})
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if sendErr != nil {
		return s.recordSendError(mutationCtx, item, sendErr, reactionOperation)
	}
	// The transport accepted the reaction. Confirming also records it in the
	// read model as this account's own, in one transaction. The adapters store
	// nothing when they send a reaction (the legacy senders wrote the stored
	// reaction themselves after the send), so without this row a reader sees
	// the reaction only if the transport later reports it back. The Google,
	// WhatsApp and Signal adapters return no remote identity for a reaction,
	// so an empty result is a valid confirmation.
	occurredAt := result.AcceptedAt
	if occurredAt.IsZero() {
		occurredAt = s.clock.Now()
	}
	if _, err := s.outbox.ConfirmReaction(mutationCtx, sqlite.ReactionConfirmation{
		OutboxID:       item.OutboxID,
		LeaseToken:     *item.LeaseToken,
		ResultRemoteID: result.RemoteMessageID,
		OccurredAt:     occurredAt,
	}); err != nil {
		// The row stays leased and called, so lease recovery settles it as
		// uncertain: the reaction went out, and nothing here may send it again.
		return fmt.Errorf(
			"dispatch outbox item %q: confirm reaction: %w",
			item.OutboxID,
			err,
		)
	}
	s.signalChange()
	return nil
}

func (s *MessageService) dispatchReadLease(ctx context.Context, outboxLease sqlite.Lease) error {
	item := outboxLease.OutboxItem
	if item.LeaseToken == nil {
		return fmt.Errorf("dispatch outbox item %q: storage returned no lease token", item.OutboxID)
	}

	dispatchLease, err := s.bridges.Acquire(ctx, item.AccountID, bridge.CapabilityReadReceipts)
	if err != nil {
		if errors.Is(err, bridge.ErrCapabilityUnavailable) &&
			!s.bridges.Capabilities(item.AccountID).ReadReceipts {
			return s.rejectPreCall(
				ctx,
				item,
				string(bridge.FailureUnsupported),
				"read_receipts_unsupported",
				err,
			)
		}
		if releaseErr := s.releaseUnavailable(ctx, item, err); releaseErr != nil {
			return releaseErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	if dispatchLease == nil || dispatchLease.ReadReceipt == nil {
		if dispatchLease != nil {
			dispatchLease.Close()
		}
		return s.releaseUnavailable(ctx, item, bridge.ErrCapabilityUnavailable)
	}
	// An abandoned transport call may still be using the lease, so once the
	// call starts its own goroutine releases it.
	closeLease := true
	defer func() {
		if closeLease {
			dispatchLease.Close()
		}
	}()

	receipt, err := s.outbox.GetOutboxReadReceipt(ctx, item.OutboxID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			"load_read_receipt",
			err,
			s.clock.Now().Add(s.retryDelay),
		)
	}
	// A read receipt is not held for its target's send: it names how far the
	// conversation is read, not one message that must exist on the platform.
	target, _, code, err := s.targetRefForLease(ctx, item, receipt.LastReadMessageID)
	if err != nil {
		return s.markPreCallFailure(
			ctx,
			item,
			string(bridge.FailureTransient),
			code,
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
			"load_conversation",
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

	readRequest := bridge.ReadReceiptRequest{
		AccountID: item.AccountID,
		Conversation: bridge.ConversationRef{
			RemoteID: conversation.RemoteConversationID,
		},
		Messages: []bridge.MessageRef{target},
		ReadAt:   time.UnixMilli(receipt.ReadAtMS),
	}
	closeLease = false
	_, sendErr := callTransport(s, ctx, item, readOperation, s.transportTimeout,
		func(callCtx context.Context) (struct{}, error) {
			defer dispatchLease.Close()
			return struct{}{}, dispatchLease.ReadReceipt.MarkRead(callCtx, readRequest)
		})
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if sendErr != nil {
		return s.recordSendError(mutationCtx, item, sendErr, readOperation)
	}
	if err := s.outbox.ConfirmWithoutResult(
		mutationCtx,
		item.OutboxID,
		*item.LeaseToken,
	); err != nil {
		return fmt.Errorf(
			"dispatch outbox item %q: confirm read without result: %w",
			item.OutboxID,
			err,
		)
	}
	s.signalChange()
	return nil
}

// targetRefForLease describes the message a reaction or read receipt names.
// send is the send that named an own target under its request ID, which the
// target still carries (see targetSendProgress), and nil for any other
// target.
func (s *MessageService) targetRefForLease(
	ctx context.Context,
	item sqlite.OutboxItem,
	targetMessageID string,
) (ref bridge.MessageRef, send *ownSend, code string, err error) {
	target, err := s.messages.GetMessage(ctx, targetMessageID)
	if err != nil {
		return bridge.MessageRef{}, nil, "load_target", err
	}
	if target.AccountID != item.AccountID || target.ConversationID != item.ConversationID {
		return bridge.MessageRef{}, nil, "load_target", fmt.Errorf(
			"target message does not match outbox account/conversation",
		)
	}

	authorID, err := s.messageAuthorID(item, target)
	if err != nil {
		return bridge.MessageRef{}, nil, "load_target_author", err
	}
	// Until echo reconciliation, an outgoing target's RemoteID remains its
	// stable transport request ID.
	send, provisional, err := s.targetSendProgress(ctx, target)
	if err != nil {
		return bridge.MessageRef{}, nil, "load_target", err
	}
	return bridge.MessageRef{
		RemoteID:            target.RemoteMessageID,
		AuthorID:            authorID,
		Outgoing:            target.Direction == sqlite.MessageDirectionOutgoing,
		ProvisionalRemoteID: provisional,
		SentAt:              time.UnixMilli(target.OccurredAtMS),
	}, send, "", nil
}

// reactionTargetNeverSentCode is the error code of a reaction rejected before
// the transport call because its target's send was canceled or rejected.
const reactionTargetNeverSentCode = "reaction_target_never_sent"

// provisionalTargetWindow is how long after its send went out an own message
// still named by that send's request ID may yet be renamed by the echo that
// brings the platform's own ID. "Went out" is the send row's updated_at_ms:
// when the dispatcher recorded the transport's answer, or later when lease
// recovery marked the send uncertain or a reconcile confirmed it. It is the
// time echo pairing measures from. It is sqlite.EchoMaxLag (60 s), the
// longest lag at which pairing considers an echo at all. On the live install
// every echo was first seen 0.28-0.79 s after its send, and pairing by
// content takes one only within about 6.2 s (an echo that carries its TmpID
// is correlated whenever it arrives; nothing in the code bounds that). Past
// the window a Google refusal of a reaction to the message is final, instead
// of being retried every 5 s for as long as the message keeps its request
// ID. Content pairing takes text sends only, so with no TmpID on the echo a
// media send keeps its request ID for good. (A text echo stored before its
// send was recorded can still be paired after the window, by the startup
// repair or a later frame for the message; every echo measured on the live
// store arrived after.)
const provisionalTargetWindow = sqlite.EchoMaxLag

// ownSend is the send that named an own message under its request ID, which
// the message still carries, and how far that send has got.
type ownSend struct {
	row      sqlite.OutboxItem
	progress sendProgress
}

type sendProgress int

const (
	// sendUnsent: queued, dispatching or not_dispatched. The platform has no
	// such message yet.
	sendUnsent sendProgress = iota + 1
	// sendNeverSent: canceled or rejected. The platform never got the
	// message and never will.
	sendNeverSent
	// sendWentOut: confirmed and waiting for its echo, uncertain, or
	// store_failed.
	sendWentOut
)

// targetSendProgress finds the send that named an outgoing message under its
// request ID, when the message still carries that ID, and says whether the
// message's remote ID is provisional: the platform may still give the message
// its own ID, because the send is unsent, or went out less than
// provisionalTargetWindow ago. send is nil for any other message: a received
// one, or one already renamed to the platform's ID.
func (s *MessageService) targetSendProgress(
	ctx context.Context,
	message sqlite.Message,
) (send *ownSend, provisional bool, err error) {
	row, found, err := s.ownSendRow(ctx, message.AccountID, message)
	if err != nil || !found {
		return nil, false, err
	}
	switch row.State {
	case sqlite.OutboxQueued, sqlite.OutboxDispatching, sqlite.OutboxNotDispatched:
		return &ownSend{row: row, progress: sendUnsent}, true, nil
	case sqlite.OutboxCanceled, sqlite.OutboxRejected:
		return &ownSend{row: row, progress: sendNeverSent}, false, nil
	case sqlite.OutboxConfirmed, sqlite.OutboxUncertain, sqlite.OutboxStoreFailed:
		wentOut := time.UnixMilli(row.UpdatedAtMS)
		return &ownSend{row: row, progress: sendWentOut},
			s.clock.Now().Before(wentOut.Add(provisionalTargetWindow)), nil
	default:
		return nil, false, nil
	}
}

// afterReactionTargetRead runs between reading a reaction's target (and its
// send) and acting on it. Tests set it to land a change to the send in that
// gap.
var afterReactionTargetRead = func() {}

// wakeIfSendChanged closes the gap between reading a held reaction's target
// send and recording the hold. A cancel or early retry of the send that
// committed in that gap found the reaction still dispatching, so its wake
// (OutboxRepository.Cancel, RetryNotDispatched) passed the reaction by, and
// the hold was timed from the send as it was. So the send is read again, and
// if it has changed the reactions held for it are made due at once, to be
// judged again. Should that fail, the hold stands as recorded: the reaction
// is tried at the send's old due time.
func (s *MessageService) wakeIfSendChanged(ctx context.Context, read sqlite.OutboxItem) {
	current, err := s.outbox.FindByID(ctx, read.OutboxID)
	if err != nil {
		return
	}
	if current.State == read.State && current.UpdatedAtMS == read.UpdatedAtMS &&
		equalOptionalMS(current.NextAttemptAtMS, read.NextAttemptAtMS) {
		return
	}
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if woken, err := s.outbox.WakeReactionsHeldFor(mutationCtx, read.OutboxID); err == nil && woken > 0 {
		s.signalChange()
		s.signalWake()
	}
}

func equalOptionalMS(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// unsentTargetRetryAt is when a reaction held for its target's unsent send is
// next tried: one millisecond after the send is next due (the later of its
// scheduled time and its next attempt, as LeaseDue reads them), and never
// sooner than the usual retry delay. So a reaction to a message scheduled for
// tomorrow waits for tomorrow instead of waking every 5 s, and LeaseDue, which
// hands out due rows in order of when they fell due, gives a dispatcher that
// reaches both the send first, so the reaction finds it sent.
func (s *MessageService) unsentTargetRetryAt(send sqlite.OutboxItem) time.Time {
	dueMS := send.ScheduledForMS
	if send.NextAttemptAtMS != nil {
		dueMS = max(dueMS, *send.NextAttemptAtMS)
	}
	retryAt := s.clock.Now().Add(s.retryDelay)
	if after := time.UnixMilli(dueMS + 1); after.After(retryAt) {
		return after
	}
	return retryAt
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

func (s *MessageService) rejectPreCall(
	ctx context.Context,
	item sqlite.OutboxItem,
	class, code string,
	cause error,
) error {
	if item.LeaseToken == nil {
		return fmt.Errorf("reject outbox item %q: lease token is missing", item.OutboxID)
	}
	mutationCtx, cancel := s.storageMutationContext(ctx)
	defer cancel()
	if err := s.outbox.Reject(
		mutationCtx,
		item.OutboxID,
		*item.LeaseToken,
		class,
		code,
		cause.Error(),
	); err != nil {
		return fmt.Errorf("reject outbox item %q: %w", item.OutboxID, err)
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
	defaultCode string,
) error {
	opErr, classified := asOpError(sendErr)
	class := "unknown"
	code := defaultCode
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

// conversationRef describes the stored conversation an operation goes to: its
// remote ID, kind and roster, plus this account's own identities, so an
// adapter that looks the remote conversation up first can refuse one that now
// names other people.
func (s *MessageService) conversationRef(conversation sqlite.Conversation) (bridge.ConversationRef, error) {
	peers, err := s.roster.ListConversationPeerIdentities(conversation.AccountID, conversation.ConversationID)
	if err != nil {
		return bridge.ConversationRef{}, err
	}
	self, err := s.roster.ListSelfIdentities(conversation.AccountID)
	if err != nil {
		return bridge.ConversationRef{}, err
	}
	return bridge.ConversationRef{
		RemoteID: conversation.RemoteConversationID,
		Kind:     string(conversation.Kind),
		Peers:    identityKeyRefs(peers),
		Self:     identityKeyRefs(self),
	}, nil
}

// identityKeyRefs carries identities' natural keys only: an adapter compares
// them, and has no use for names or raw addresses.
func identityKeyRefs(identities []sqlite.Identity) []bridge.IdentityRef {
	refs := make([]bridge.IdentityRef, 0, len(identities))
	for _, identity := range identities {
		refs = append(refs, bridge.IdentityRef{
			Kind:      string(identity.Kind),
			Canonical: identity.CanonicalValue,
			IsSelf:    identity.IsSelf,
		})
	}
	return refs
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
	case bridge.FailureUnpaired,
		bridge.FailureReauthRequired,
		bridge.FailureUpgradeRequired,
		bridge.FailureMisconfigured,
		bridge.FailureUnsupported,
		bridge.FailureRecipientMismatch:
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
