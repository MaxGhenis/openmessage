package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// maxTransportAttempts caps the transport attempts one outbox intent may spend
// on failures the transport proved were never dispatched. The intent is
// rejected (class sqlite.RetryExhaustedErrorClass) on the failure that reaches
// it. Failures from an offline account or one whose credentials are being
// repaired are exempt (see budgetExempt): they never count, so an intent
// composed offline still goes out when the account reconnects.
var maxTransportAttempts int64 = 6

const (
	// maxRetryBackoff bounds the wait between budget-consuming attempts.
	maxRetryBackoff = 5 * time.Minute
	// maxBackoffDoublings clamps the exponent before shifting, so no attempt
	// count can overflow the delay into a non-positive retry time (which
	// markNotDispatched refuses, stopping the dispatcher for every account).
	maxBackoffDoublings = 6
)

// e164IdentityKind is the identity kind v2keys.IdentityKey mints for a
// "+"-prefixed phone number. sqlite.IdentityKind is open-ended and declares
// no constant for it.
const e164IdentityKind sqlite.IdentityKind = "e164"

// retryBackoff is the wait after the attempts-th budget-consuming failure:
// base doubled per earlier attempt, capped at maxRetryBackoff. With the 5 s
// default the schedule is 5, 10, 20, 40, 80 s. It is non-decreasing in
// attempts and lies in [base, maxRetryBackoff] for every attempts (a base at
// or above the cap is returned unchanged; a non-positive base falls back to
// defaultRetryDelay).
func retryBackoff(base time.Duration, attempts int64) time.Duration {
	if base <= 0 {
		base = defaultRetryDelay
	}
	if base >= maxRetryBackoff {
		return base
	}
	// Compare before subtracting: attempts-1 wraps for math.MinInt64.
	doublings := int64(0)
	if attempts > 1 {
		doublings = attempts - 1
	}
	if doublings > maxBackoffDoublings {
		doublings = maxBackoffDoublings
	}
	// base < 5 min, so base << 6 stays far below the int64 range.
	delay := base << uint(doublings)
	if delay > maxRetryBackoff {
		return maxRetryBackoff
	}
	return delay
}

// budgetExempt reports a not-dispatched failure that says nothing about the
// intent itself: the account was not connected, or its credentials expired
// and the supervisor is repairing them. Such failures keep the fixed retry
// cadence, give back their attempt and never exhaust the budget.
func budgetExempt(opErr bridge.OpError) bool {
	switch opErr.Class {
	case bridge.FailureCredentialsExpired:
		return true
	case bridge.FailureTransient:
		return strings.HasSuffix(opErr.Fingerprint, "_not_connected")
	default:
		return false
	}
}

// retryExhausted is the one predicate behind Delivery.RetryExhausted and
// PendingDelivery.RetryExhausted.
func retryExhausted(state OutboxState, errorClass string) bool {
	return state == OutboxRejected && errorClass == sqlite.RetryExhaustedErrorClass
}

// fingerprintedDetail prefixes a failure description with the adapter
// fingerprint, which OpError.Error() omits and error_code does not carry.
func fingerprintedDetail(fingerprint, failure string) string {
	if fingerprint == "" {
		return failure
	}
	return "[" + fingerprint + "] " + failure
}

func exhaustedDetail(attempts int64, class, fingerprint, failure string) string {
	label := class
	if fingerprint != "" {
		label += " [" + fingerprint + "]"
	}
	return fmt.Sprintf(
		"retry budget exhausted after %d attempts; last failure %s: %s",
		attempts,
		label,
		failure,
	)
}

// recordCalledNotDispatched files a retryable failure that the transport
// proved never left this device. Exempt failures are refunded and retried at
// exemptRetryAt (the caller's fixed cadence, or the adapter's RetryAt). Every
// other failure consumes one attempt: the attempt that reaches
// maxTransportAttempts rejects the intent as retry_exhausted, and earlier ones
// back off exponentially (never earlier than an adapter-supplied RetryAt). A
// conversation_moved failure first rebinds the local conversation to the
// remote ID the transport now uses, then retries at once under that ID.
func (s *MessageService) recordCalledNotDispatched(
	ctx context.Context,
	item sqlite.OutboxItem,
	opErr bridge.OpError,
	sendErr error,
	class, code string,
	exemptRetryAt time.Time,
) error {
	failure := sendErr.Error()
	if budgetExempt(opErr) {
		if err := s.outbox.MarkCalledNotDispatchedRefundingAttempt(
			ctx,
			item.OutboxID,
			*item.LeaseToken,
			class,
			code,
			fingerprintedDetail(opErr.Fingerprint, failure),
			exemptRetryAt,
		); err != nil {
			return fmt.Errorf("mark called outbox item %q not dispatched: %w", item.OutboxID, err)
		}
		s.signalChange()
		return nil
	}

	now := s.clock.Now()
	// LeaseDue read the row before MarkTransportCalled incremented it, so the
	// leased copy counts the attempts before this one.
	attempts := item.AttemptCount + 1
	retryAt := now.Add(retryBackoff(s.retryDelay, attempts))
	if opErr.RetryAt.After(retryAt) {
		retryAt = opErr.RetryAt
	}

	var moved *bridge.ConversationMovedError
	if opErr.Fingerprint == bridge.FingerprintConversationMoved &&
		errors.As(opErr.Cause, &moved) && moved != nil {
		logger := zerolog.Ctx(ctx).With().
			Str("outbox_id", item.OutboxID).
			Str("account_id", item.AccountID).
			Str("conversation_id", item.ConversationID).
			Str("from_remote_id", moved.FromRemoteID).
			Str("to_remote_id", moved.ToRemoteID).
			Int64("attempt", attempts).
			Logger()
		if rebindErr := s.rebindMovedConversation(item, moved, now); rebindErr != nil {
			logger.Warn().Err(rebindErr).Msg("Outbox could not rebind a moved conversation")
			failure += "; rebind failed: " + rebindErr.Error()
		} else {
			logger.Info().Msg("Outbox rebound a moved conversation before sending")
			failure += fmt.Sprintf("; rebound the conversation to remote ID %q", moved.ToRemoteID)
			// The rebind still spends this attempt, so a phone that keeps
			// flipping a thread's ID cannot loop forever.
			retryAt = now
		}
	}

	if attempts >= maxTransportAttempts {
		if err := s.outbox.Reject(
			ctx,
			item.OutboxID,
			*item.LeaseToken,
			sqlite.RetryExhaustedErrorClass,
			code,
			exhaustedDetail(attempts, class, opErr.Fingerprint, failure),
		); err != nil {
			return fmt.Errorf("reject exhausted outbox item %q: %w", item.OutboxID, err)
		}
		s.signalChange()
		return nil
	}
	if err := s.outbox.MarkCalledNotDispatched(
		ctx,
		item.OutboxID,
		*item.LeaseToken,
		class,
		code,
		fingerprintedDetail(opErr.Fingerprint, failure),
		retryAt,
	); err != nil {
		return fmt.Errorf("mark called outbox item %q not dispatched: %w", item.OutboxID, err)
	}
	s.signalChange()
	return nil
}

// rebindMovedConversation moves the moved-from binding of the intent's
// conversation to the remote ID the transport resolved. It refuses when the
// conversation is no longer bound to FromRemoteID (something else rebound it
// since the attempt started); a binding already at ToRemoteID counts as done.
func (s *MessageService) rebindMovedConversation(
	item sqlite.OutboxItem,
	moved *bridge.ConversationMovedError,
	now time.Time,
) error {
	to := strings.TrimSpace(moved.ToRemoteID)
	if to == "" || to == moved.FromRemoteID {
		return fmt.Errorf("moved-to remote conversation ID %q is not a move", moved.ToRemoteID)
	}
	conversation, err := s.store.GetConversation(item.ConversationID)
	if err != nil {
		return fmt.Errorf("load conversation %q: %w", item.ConversationID, err)
	}
	if conversation.AccountID != item.AccountID {
		return fmt.Errorf("conversation %q belongs to account %q", item.ConversationID, conversation.AccountID)
	}
	switch conversation.RemoteConversationID {
	case to:
		return nil
	case moved.FromRemoteID:
		return s.store.ReassignConversationRemoteID(item.AccountID, to, item.ConversationID, now.UnixMilli())
	default:
		return fmt.Errorf(
			"conversation %q is now bound to remote ID %q, not %q",
			item.ConversationID,
			conversation.RemoteConversationID,
			moved.FromRemoteID,
		)
	}
}

// rejectExhaustedBeforeCall rejects a leased intent whose attempt count is
// already at or over the budget, without acquiring a transport. Steady-state
// exhaustion rejects on the failure that reaches the cap, so this only fires
// for rows that crossed it before the budget existed: they must never fire
// late. It reports whether the lease was consumed.
func (s *MessageService) rejectExhaustedBeforeCall(
	ctx context.Context,
	item sqlite.OutboxItem,
	operation string,
) (bool, error) {
	if item.AttemptCount < maxTransportAttempts {
		return false, nil
	}
	return true, s.rejectPreCall(
		ctx,
		item,
		sqlite.RetryExhaustedErrorClass,
		operation,
		fmt.Errorf(
			"retry budget already exhausted (%d attempts); not attempted again",
			item.AttemptCount,
		),
	)
}

// storedConversationRef describes the stored conversation to an adapter.
func storedConversationRef(conversation sqlite.Conversation) bridge.ConversationRef {
	return bridge.ConversationRef{
		RemoteID: conversation.RemoteConversationID,
		Kind:     string(conversation.Kind),
	}
}

// directConversationRef is storedConversationRef plus, for a direct conversation
// whose only active non-self participant is a canonical E.164 identity, that
// number, so a text or media adapter can re-resolve a thread whose remote ID
// the transport no longer recognizes. A lookup failure leaves the number
// empty and never fails the send.
func (s *MessageService) directConversationRef(conversation sqlite.Conversation) bridge.ConversationRef {
	ref := storedConversationRef(conversation)
	if conversation.Kind != sqlite.ConversationKindDirect {
		return ref
	}
	peers, err := s.store.ListConversationPeerIdentities(
		conversation.AccountID,
		conversation.ConversationID,
	)
	if err != nil || len(peers) != 1 {
		return ref
	}
	if peers[0].Kind != e164IdentityKind || !canonicalE164(peers[0].CanonicalValue) {
		return ref
	}
	ref.DirectPeerNumber = peers[0].CanonicalValue
	return ref
}

// canonicalE164 accepts "+" followed by 1 to 15 digits, the form
// v2keys.IdentityKey stores for phone numbers.
func canonicalE164(value string) bool {
	digits := strings.TrimPrefix(value, "+")
	if len(digits) == len(value) || len(digits) == 0 || len(digits) > 15 {
		return false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
