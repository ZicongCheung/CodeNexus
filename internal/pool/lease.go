package pool

import (
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// ErrNoAccount is returned when no account can serve a request.
type ErrNoAccount struct {
	// RetryAfter is how long a caller should wait before retrying.
	RetryAfter time.Duration
	// Reason is a short human-readable explanation.
	Reason string
}

// Error implements error.
func (e *ErrNoAccount) Error() string {
	if e.Reason == "" {
		return "pool: no available account"
	}
	return "pool: " + e.Reason
}

// Lease is a reserved in-flight slot on one account. Every lease must be
// finished with exactly one of Release, Succeed or Fail.
type Lease struct {
	Account *account.Account

	pool *Pool
	key  string

	mu       sync.Mutex
	released bool
}

func (l *Lease) releaseLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false
	}
	l.released = true
	return true
}

// Release returns the in-flight slot without recording an outcome.
func (l *Lease) Release() {
	if !l.releaseLocked() {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if e, ok := l.pool.entries[l.key]; ok && e.inFlight > 0 {
		e.inFlight--
	}
}

// Succeed records a successful request and returns the in-flight slot.
func (l *Lease) Succeed() {
	if !l.releaseLocked() {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[l.key]; ok {
		if e.inFlight > 0 {
			e.inFlight--
		}
		e.success++
		e.fails = 0
		e.softStreak = 0
		e.lastSuccess = p.now()
	}
	p.dirty = true
}

// Fail records a failed request, applies the appropriate cooldown/breaker
// transition and returns the classified error.
func (l *Lease) Fail(err error) *upstream.Error {
	ue := upstream.Classify(err)
	if !l.releaseLocked() {
		return ue
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[l.key]; ok {
		if e.inFlight > 0 {
			e.inFlight--
		}
		p.applyFailureLocked(e, ue)
	}
	p.dirty = true
	return ue
}

// applyFailureLocked moves an entry into the state implied by ue.
func (p *Pool) applyFailureLocked(e *entry, ue *upstream.Error) {
	now := p.now()
	e.errors++
	if ue == nil {
		return
	}
	switch ue.Kind {
	case upstream.KindRateLimited:
		e.softStreak++
		d := backoff(p.opts.CooldownBase, e.softStreak, p.opts.CooldownMax)
		if ue.RetryAfter > d {
			d = ue.RetryAfter
		}
		e.coolingUntil = now.Add(d)
		e.coolKind = ue.Kind
	case upstream.KindQuota:
		e.coolingUntil = now.Add(p.opts.CooldownMax)
		e.coolKind = ue.Kind
	case upstream.KindAuth:
		// a rejected credential needs an operator, not a cooldown
		e.disabled = true
	case upstream.KindModelUnavailable:
		if e.modelCooldowns == nil {
			e.modelCooldowns = map[string]time.Time{}
		}
		if e.lastModel != "" {
			e.modelCooldowns[e.lastModel] = now.Add(p.opts.CooldownBase)
		}
	case upstream.KindTimeout, upstream.KindServer, upstream.KindSessionDead:
		e.fails++
		if e.fails >= p.opts.BreakerThreshold {
			e.breakerUntil = now.Add(p.opts.BreakerFor)
			e.fails = 0
		}
	default:
		// Bad requests and content blocks are the caller's fault: do not
		// penalise the account.
	}
}

// backoff returns base * 2^(streak-1) capped at max.
func backoff(base time.Duration, streak int, max time.Duration) time.Duration {
	if streak < 1 {
		streak = 1
	}
	if streak > 10 {
		streak = 10
	}
	d := base << (streak - 1)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// GCSticky drops expired session bindings and reports how many were removed.
func (p *Pool) GCSticky() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	removed := 0
	for k, b := range p.sticky {
		if !now.Before(b.until) {
			delete(p.sticky, k)
			removed++
		}
	}
	return removed
}

// StickyCount reports the number of live session bindings.
func (p *Pool) StickyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sticky)
}
