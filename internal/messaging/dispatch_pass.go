package messaging

import (
	"sync"
	"time"
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
