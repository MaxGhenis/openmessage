package messaging

import (
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// FailedDispatchPass describes a dispatch pass that failed on local storage.
type FailedDispatchPass struct {
	AtMS  int64  `json:"at_ms"`
	Error string `json:"error"`
	// Consecutive counts the failed passes in a row, this one included.
	Consecutive int64 `json:"consecutive"`
	// RetryAtMS is when Run starts its next pass.
	RetryAtMS int64 `json:"retry_at_ms"`
}

// dispatchPasses is the bookkeeping behind DispatchHealth's pass fields and
// the OnPassFailure hook.
type dispatchPasses struct {
	mu          sync.Mutex
	failed      int64
	consecutive int64
	last        *FailedDispatchPass
	onFailure   func(error, FailedDispatchPass)
}

// fail records a failed pass, then reports it to the hook outside the lock.
func (p *dispatchPasses) fail(err error, now time.Time, backoff time.Duration) {
	p.mu.Lock()
	p.failed++
	p.consecutive++
	pass := FailedDispatchPass{
		AtMS:        now.UnixMilli(),
		Error:       err.Error(),
		Consecutive: p.consecutive,
		RetryAtMS:   now.Add(backoff).UnixMilli(),
	}
	p.last = &pass
	onFailure := p.onFailure
	p.mu.Unlock()
	if onFailure != nil {
		onFailure(err, pass)
	}
}

// succeed ends a run of failed passes.
func (p *dispatchPasses) succeed() {
	p.mu.Lock()
	p.consecutive = 0
	p.mu.Unlock()
}

func (p *dispatchPasses) report(health *DispatchHealth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	health.FailedPasses = p.failed
	health.ConsecutiveFailedPasses = p.consecutive
	if p.last != nil {
		last := *p.last
		health.LastFailedPass = &last
	}
}

// OnPassFailure sets fn to run on Run's goroutine after each dispatch pass
// that fails, before Run waits to retry. The daemon logs through it. Dispatch
// waits for fn, so it must return promptly. A nil fn removes the hook.
func (s *MessageService) OnPassFailure(fn func(err error, pass FailedDispatchPass)) {
	s.passes.mu.Lock()
	s.passes.onFailure = fn
	s.passes.mu.Unlock()
}

// FailedLease describes a due outbox row whose lease failed on the row itself:
// a constraint it cannot meet or a stored value that does not read back.
type FailedLease struct {
	OutboxID string `json:"outbox_id"`
	AtMS     int64  `json:"at_ms"`
	Error    string `json:"error"`
	// RetryAtMS gives the time the row comes due again, after the store set it
	// aside as not_dispatched with error code lease. It stays zero when the
	// store could not set the row aside either; SetAsideError then says why,
	// the row stays due, and its pass failed after sending the rows behind it.
	RetryAtMS     int64  `json:"retry_at_ms,omitempty"`
	SetAsideError string `json:"set_aside_error,omitempty"`
}

// dispatchLeaseFailures is the bookkeeping behind DispatchHealth's lease
// fields and the OnLeaseFailure hook.
type dispatchLeaseFailures struct {
	mu        sync.Mutex
	failed    int64
	last      *FailedLease
	onFailure func(FailedLease)
}

// fail records one failed lease, then reports it to the hook outside the lock.
func (l *dispatchLeaseFailures) fail(failure sqlite.LeaseFailure, now time.Time) {
	lease := FailedLease{
		OutboxID: failure.OutboxID,
		AtMS:     now.UnixMilli(),
		Error:    failure.Err.Error(),
	}
	if failure.SetAsideErr != nil {
		lease.SetAsideError = failure.SetAsideErr.Error()
	} else {
		lease.RetryAtMS = failure.RetryAt.UnixMilli()
	}
	l.mu.Lock()
	l.failed++
	l.last = &lease
	onFailure := l.onFailure
	l.mu.Unlock()
	if onFailure != nil {
		onFailure(lease)
	}
}

func (l *dispatchLeaseFailures) report(health *DispatchHealth) {
	l.mu.Lock()
	defer l.mu.Unlock()
	health.FailedLeases = l.failed
	if l.last != nil {
		last := *l.last
		health.LastFailedLease = &last
	}
}

// OnLeaseFailure sets fn to run on the dispatching goroutine for each due row
// whose lease failed on the row itself, whether the store set the row aside
// or left it due. The daemon logs through it. Dispatch waits for fn, so it
// must return promptly. A nil fn removes the hook.
func (s *MessageService) OnLeaseFailure(fn func(lease FailedLease)) {
	s.leaseFailures.mu.Lock()
	s.leaseFailures.onFailure = fn
	s.leaseFailures.mu.Unlock()
}
