// Package messaging owns durable outbound message submission and delivery.
package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// OutboxState is the platform-independent state of a durable outbound intent.
type OutboxState = sqlite.OutboxState

const (
	OutboxQueued        = sqlite.OutboxQueued
	OutboxDispatching   = sqlite.OutboxDispatching
	OutboxNotDispatched = sqlite.OutboxNotDispatched
	OutboxUncertain     = sqlite.OutboxUncertain
	OutboxConfirmed     = sqlite.OutboxConfirmed
	OutboxStoreFailed   = sqlite.OutboxStoreFailed
	OutboxRejected      = sqlite.OutboxRejected
	OutboxCanceled      = sqlite.OutboxCanceled
)

// EchoOutcome is what observing a transport echo did to durable send state.
type EchoOutcome = sqlite.ReconcileOutcome

const (
	EchoReconciled = sqlite.ReconcileOutcomeReconciled
	EchoEnriched   = sqlite.ReconcileOutcomeEnriched
	EchoNoop       = sqlite.ReconcileOutcomeNoop
	EchoNotFound   = sqlite.ReconcileOutcomeNotFound
)

var (
	// ErrIdempotencyConflict means the account-scoped key already names a
	// different outbound intent.
	ErrIdempotencyConflict = sqlite.ErrIdempotencyConflict

	// ErrInvalidCommand means a submission is incomplete or inconsistent.
	ErrInvalidCommand = errors.New("messaging: invalid command")

	// ErrTooLarge means an attachment exceeded the configured hard byte cap.
	ErrTooLarge = errors.New("messaging: attachment exceeds size limit")

	// ErrInvalidState means the requested delivery transition is unsafe from
	// the intent's current state.
	ErrInvalidState = errors.New("messaging: invalid outbox state")

	// ErrUnsupported means the account bridge does not support the requested
	// messaging operation.
	ErrUnsupported = errors.New("messaging: operation unsupported by account bridge")

	// ErrNotImplemented marks API seams reserved for later rebuild items.
	ErrNotImplemented = errors.New("messaging: not implemented")

	// ErrDuplicateSend means a near-identical text was already submitted to
	// the same conversation moments ago and the new submission did not carry
	// Force. The wrapped DuplicateSendError names the prior intent.
	ErrDuplicateSend = errors.New("messaging: near-duplicate send blocked")
)

// DuplicateSendError reports the prior intent that triggered the
// near-duplicate guard so callers can decide between waiting, canceling the
// prior send, or forcing this one.
type DuplicateSendError struct {
	PriorOutboxID       string
	PriorState          OutboxState
	PriorIdempotencyKey string
	PriorAgeMS          int64
}

func (e *DuplicateSendError) Error() string {
	return fmt.Sprintf(
		"messaging: near-duplicate send blocked: a very similar message was submitted to this conversation %s ago (outbox %s, state %s); if this is intentional, resubmit with force",
		(time.Duration(e.PriorAgeMS) * time.Millisecond).Round(time.Second),
		e.PriorOutboxID,
		e.PriorState,
	)
}

func (e *DuplicateSendError) Unwrap() error { return ErrDuplicateSend }

type CommonCommand struct {
	AccountID      string
	ConversationID string
	IdempotencyKey string
	NotBefore      time.Time // zero means now

	// TTL bounds how long the intent may wait to cross the transport
	// boundary, measured from the later of submission and NotBefore. An
	// intent still queued when the window closes is canceled instead of
	// transmitted stale. Zero means the intent never expires.
	TTL time.Duration

	// Force bypasses the near-duplicate guard for a deliberate resend.
	Force bool
}

type SendTextCommand struct {
	CommonCommand
	Body             string
	ReplyToMessageID string
}

type SendMediaCommand struct {
	CommonCommand
	Content          io.Reader
	Filename         string
	MIME             string
	Caption          string
	ReplyToMessageID string
}

type SendReactionCommand struct {
	CommonCommand
	TargetMessageID string
	Emoji           string
	Action          bridge.ReactionAction // empty means ReactionAdd
}

type MarkReadCommand struct {
	CommonCommand
	DeviceID          string
	LastReadMessageID string
	LastReadAt        time.Time // zero means clock.Now()
}

type Submission struct {
	OutboxID       string
	LocalMessageID string
	State          OutboxState
	ScheduledFor   time.Time
	ExpiresAt      time.Time // zero means the intent never expires
	Deduplicated   bool
}

type Delivery struct {
	OutboxID        string
	AccountID       string
	ConversationID  string
	State           OutboxState
	LocalMessageID  string
	RemoteMessageID string
	ErrorClass      string
	ErrorCode       string
	Warning         string
	ExpiresAt       time.Time // zero means the intent never expires
}

// Expired reports whether the delivery was canceled by its send window
// closing rather than by an explicit cancel.
func (d Delivery) Expired() bool {
	return d.State == OutboxCanceled && d.ErrorClass == sqlite.TTLErrorClass
}

// TransportEcho is the transport-neutral correlation shape reserved for M5.
type TransportEcho struct {
	AccountID          string
	TransportRequestID string
	RemoteMessageID    string
}

// Timer is the one-shot timer seam used by Run and Wait.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock is MessageService's only source of time and timers.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// IDSource supplies opaque durable IDs.
type IDSource interface {
	NewID() (string, error)
}

// IDSourceFunc adapts a function to IDSource.
type IDSourceFunc func() (string, error)

func (f IDSourceFunc) NewID() (string, error) { return f() }

// CryptoIDSource produces 128-bit random hexadecimal IDs.
type CryptoIDSource struct{}

func (CryptoIDSource) NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate messaging ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// SystemClock is the production wall-clock implementation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.Timer.C }

// messageRepository is private so MessageService does not leak persistence
// operations through its application-facing API.
type messageRepository interface {
	GetMessage(context.Context, string) (sqlite.Message, error)
}
