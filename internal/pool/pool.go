// Package pool holds the unified account pool and selects accounts for requests.
//
// Selection is health-aware: accounts sit in one of a few orthogonal states
// (enabled, cooling, circuit-broken, model-cooled, in-flight-saturated) and a
// composite weight decides which healthy account serves the next request.
package pool

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// TopK is how many of the heaviest candidates enter the weighted draw.
const TopK = 5

// pollInterval is how long to wait when every account is merely busy (rather
// than cooling) so a freed in-flight slot can be picked up.
const pollInterval = 200 * time.Millisecond

// Options configures pool behavior. Zero values fall back to defaults.
type Options struct {
	MaxInFlight      int
	CreditFloor      int64
	CooldownBase     time.Duration
	CooldownMax      time.Duration
	BreakerThreshold int
	BreakerFor       time.Duration
	WaitMax          time.Duration
	StickyEnabled    bool
	StickyTTL        time.Duration
	WeightCredit     float64
	WeightIdle       float64
	WeightSuccess    float64
	WeightExpiring   float64
	// Seed makes the weighted draw reproducible in tests. Zero means time-based.
	Seed int64
	// Clock overrides the time source in tests.
	Clock func() time.Time
}

func (o Options) withDefaults() Options {
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = 3
	}
	if o.CooldownBase <= 0 {
		o.CooldownBase = 30 * time.Second
	}
	if o.CooldownMax <= 0 {
		o.CooldownMax = time.Hour
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = 5
	}
	if o.BreakerFor <= 0 {
		o.BreakerFor = 2 * time.Minute
	}
	if o.WaitMax <= 0 {
		o.WaitMax = 8 * time.Second
	}
	if o.StickyTTL <= 0 {
		o.StickyTTL = 30 * time.Minute
	}
	if o.WeightCredit == 0 && o.WeightIdle == 0 && o.WeightSuccess == 0 && o.WeightExpiring == 0 {
		o.WeightCredit, o.WeightIdle, o.WeightSuccess, o.WeightExpiring = 0.35, 0.25, 0.25, 0.15
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return o
}

// stickyBinding pins a session key to an account until it expires.
type stickyBinding struct {
	key   string
	until time.Time
}

// Pool is a concurrency-safe account pool with health-aware selection.
type Pool struct {
	mu      sync.Mutex
	opts    Options
	entries map[string]*entry
	order   []string
	sticky  map[string]stickyBinding
	rnd     *rand.Rand

	statePath string
	dirty     bool
	stopCh    chan struct{}
	stopped   bool
	errFn     func(error)
}

// New returns an empty pool.
func New(opts Options) *Pool {
	opts = opts.withDefaults()
	seed := opts.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Pool{
		opts:    opts,
		entries: map[string]*entry{},
		sticky:  map[string]stickyBinding{},
		rnd:     rand.New(rand.NewSource(seed)),
		stopCh:  make(chan struct{}),
	}
}

// Reconfigure swaps the pool's tuning without disturbing live accounts. It
// exists so a config edit takes effect immediately instead of waiting for a
// restart: entries, bindings and in-flight counts are untouched.
func (p *Pool) Reconfigure(opts Options) {
	opts = opts.withDefaults()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = opts
}

func (p *Pool) now() time.Time { return p.opts.Clock() }

// Replace swaps the whole account set, preserving runtime state for accounts
// that survive the swap.
func (p *Pool) Replace(accts []*account.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	next := make(map[string]*entry, len(accts))
	order := make([]string, 0, len(accts))
	for _, a := range accts {
		key := a.Key()
		if prev, ok := p.entries[key]; ok {
			prev.acc = a
			next[key] = prev
		} else {
			next[key] = newEntry(a)
		}
		order = append(order, key)
	}
	p.entries = next
	p.order = order
	p.dirty = true
}

// Add inserts or replaces one account, preserving nothing (used when a new
// account is added at runtime, e.g. through the console).
func (p *Pool) Add(acc *account.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := acc.Key()
	if _, ok := p.entries[key]; !ok {
		p.order = append(p.order, key)
	}
	p.entries[key] = newEntry(acc)
	p.dirty = true
}

// Remove drops an account from the pool.
func (p *Pool) Remove(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.entries[key]; !ok {
		return
	}
	delete(p.entries, key)
	for i, k := range p.order {
		if k == key {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.dirty = true
}

// Get returns the account for key.
func (p *Pool) Get(key string) (*account.Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		return nil, false
	}
	return e.acc, true
}

// Enabled reports whether the account for key is administratively enabled.
func (p *Pool) Enabled(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	return ok && e.active()
}

// List returns a copy of all accounts.
func (p *Pool) List() []*account.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*account.Account, 0, len(p.order))
	for _, k := range p.order {
		out = append(out, p.entries[k].acc)
	}
	return out
}

// Stats reports enabled and total account counts.
func (p *Pool) Stats() (enabled, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.order {
		total++
		if p.entries[k].acc.Enabled {
			enabled++
		}
	}
	return
}

// HasSource reports whether any enabled account exists for source.
func (p *Pool) HasSource(source upstream.Source) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.order {
		e := p.entries[k]
		if e.acc.Source == source && e.acc.Enabled {
			return true
		}
	}
	return false
}

// Candidate returns a selectable account for source without reserving a slot.
// It is used for read-only operations such as listing models.
func (p *Pool) Candidate(source upstream.Source) *account.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, k := range p.order {
		e := p.entries[k]
		if e.acc.Source == source && e.active() && !e.cooling(now) && !e.broken(now) {
			return e.acc
		}
	}
	return nil
}

// SetCredits records an externally observed credit balance for an account.
func (p *Pool) SetCredits(key string, credits int64, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.credits = credits
		e.creditsKnown = known
		e.creditsUnlimited = false
		p.dirty = true
	}
}

// Credits describes the account's credit picture as last observed upstream.
// Unlimited marks a grant that is not metered, which must not be read as a
// zero balance.
type Credits struct {
	Remain            int64
	Total             int64
	Known             bool
	Unlimited         bool
	Expiring          int64
	EarliestAt        time.Time
	EarliestRemaining int64
}

// SetCreditsDetail records a detailed credit observation and clears a quota
// cooldown, since a freshly observed positive balance means the account can be
// used again.
func (p *Pool) SetCreditsDetail(key string, c Credits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		return
	}
	e.credits = c.Remain
	e.creditsKnown = c.Known
	e.creditsTotal = c.Total
	e.creditsUnlimited = c.Unlimited
	e.expiring = c.Expiring
	e.earliestAt = c.EarliestAt
	e.earliestRemaining = c.EarliestRemaining
	if c.Known && (c.Remain > 0 || c.Unlimited) && e.coolKind == upstream.KindQuota {
		e.coolingUntil = time.Time{}
		e.coolKind = ""
	}
	p.dirty = true
}

// Credits returns the last observed credit picture for key.
func (p *Pool) Credits(key string) (Credits, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok {
		return Credits{}, false
	}
	return Credits{
		Remain:            e.credits,
		Total:             e.creditsTotal,
		Known:             e.creditsKnown,
		Unlimited:         e.creditsUnlimited,
		Expiring:          e.expiring,
		EarliestAt:        e.earliestAt,
		EarliestRemaining: e.earliestRemaining,
	}, true
}

// NoteCheckin records the schedule-local day of a successful check-in.
func (p *Pool) NoteCheckin(key, day string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.checkinDay = day
		p.dirty = true
	}
}

// CheckinDay returns the schedule-local day of the last successful check-in.
func (p *Pool) CheckinDay(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		return e.checkinDay
	}
	return ""
}

// SetDisabled administratively disables or revives an account.
func (p *Pool) SetDisabled(key string, disabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.disabled = disabled
		if !disabled {
			e.coolingUntil = time.Time{}
			e.breakerUntil = time.Time{}
			e.softStreak = 0
			e.fails = 0
		}
		p.dirty = true
	}
}

// SetPaused parks an account: it stays configured and keeps its health, but is
// not selected for traffic. Pausing does not reset any failure state, so
// resuming restores exactly the previous posture.
func (p *Pool) SetPaused(key string, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		e.paused = paused
		p.dirty = true
	}
}

// Paused reports whether the account is administratively parked.
func (p *Pool) Paused(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok {
		return e.paused
	}
	return false
}

// PickOptions describes one acquisition attempt.
type PickOptions struct {
	Source upstream.Source
	// Sources, when non-empty, replaces Source with an allow-list. Selection
	// then weighs the accounts of every listed source on the same scale, which
	// is how a model served by several sources is routed to whichever source
	// currently holds the most attractive account.
	Sources    []upstream.Source
	Model      string
	SessionKey string
	// Exclude holds account keys already tried for this request.
	Exclude map[string]bool
}

// allowsSource reports whether an account of src may serve this request.
func (o PickOptions) allowsSource(src upstream.Source) bool {
	if len(o.Sources) == 0 {
		return src == o.Source
	}
	for _, s := range o.Sources {
		if s == src {
			return true
		}
	}
	return false
}

// Acquire reserves an account for a request. When every matching account is
// cooling it waits for the earliest recovery, up to Options.WaitMax.
func (p *Pool) Acquire(ctx context.Context, opts PickOptions) (*Lease, error) {
	deadline := p.now().Add(p.opts.WaitMax)
	for {
		lease, wait := p.tryAcquire(opts)
		if lease != nil {
			return lease, nil
		}
		if wait <= 0 {
			return nil, &ErrNoAccount{Reason: "no matching account"}
		}
		now := p.now()
		if now.Add(wait).After(deadline) {
			return nil, &ErrNoAccount{RetryAfter: wait, Reason: "all accounts cooling"}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// tryAcquire attempts one selection. It returns a lease on success, or the
// delay after which a retry might succeed (zero when waiting cannot help).
func (p *Pool) tryAcquire(opts PickOptions) (*Lease, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	cands := p.candidatesLocked(now, opts)
	if len(cands) == 0 {
		return nil, p.nextReadyLocked(now, opts)
	}

	// Sticky sessions keep a conversation on the account that served it.
	if opts.SessionKey != "" && p.opts.StickyEnabled {
		if b, ok := p.sticky[opts.SessionKey]; ok && now.Before(b.until) {
			for _, e := range cands {
				if e.key == b.key {
					return p.grantLocked(e, now, opts), 0
				}
			}
		}
	}
	return p.grantLocked(p.chooseWeightedLocked(cands, now), now, opts), 0
}

// candidatesLocked returns the selectable entries for the request.
func (p *Pool) candidatesLocked(now time.Time, opts PickOptions) []*entry {
	out := make([]*entry, 0, len(p.order))
	for _, k := range p.order {
		e := p.entries[k]
		if !opts.allowsSource(e.acc.Source) || opts.Exclude[k] {
			continue
		}
		if !e.selectable(now, opts.Model, p.opts.MaxInFlight) {
			continue
		}
		// credit floor: never spend an account known to be nearly empty. An
		// unmetered grant is not empty, however the number reads.
		if p.opts.CreditFloor > 0 && e.creditsKnown && !e.creditsUnlimited && e.credits < p.opts.CreditFloor {
			continue
		}
		out = append(out, e)
	}
	return out
}

// nextReadyLocked estimates how long until any excluded/unavailable account
// becomes a candidate again.
func (p *Pool) nextReadyLocked(now time.Time, opts PickOptions) time.Duration {
	best := time.Duration(0)
	busy := false
	for _, k := range p.order {
		e := p.entries[k]
		if !opts.allowsSource(e.acc.Source) || opts.Exclude[k] || !e.active() {
			continue
		}
		if e.inFlight >= p.opts.MaxInFlight &&
			!e.cooling(now) && !e.broken(now) && !e.modelCooled(now, opts.Model) {
			busy = true
		}
		if d := e.blockDelay(now, opts.Model); d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	if best == 0 && busy {
		return pollInterval
	}
	return best
}

// grantLocked reserves an in-flight slot and records the sticky binding.
func (p *Pool) grantLocked(e *entry, now time.Time, opts PickOptions) *Lease {
	e.inFlight++
	e.lastUsed = now
	e.lastModel = opts.Model
	if opts.SessionKey != "" && p.opts.StickyEnabled {
		p.sticky[opts.SessionKey] = stickyBinding{key: e.key, until: now.Add(p.opts.StickyTTL)}
	}
	p.dirty = true
	return &Lease{Account: e.acc, pool: p, key: e.key}
}

type scored struct {
	e *entry
	w float64
}

// chooseWeightedLocked scores every candidate, keeps the heaviest TopK, and
// draws one proportionally to its weight.
func (p *Pool) chooseWeightedLocked(cands []*entry, now time.Time) *entry {
	if len(cands) == 1 {
		return cands[0]
	}
	var maxCredits int64 = 1
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	scores := make([]scored, 0, len(cands))
	for _, e := range cands {
		credit := 0.5
		switch {
		case e.creditsUnlimited:
			credit = 1
		case e.creditsKnown:
			credit = float64(e.credits) / float64(maxCredits)
			if credit > 1 {
				credit = 1
			}
		}
		idle := 1.0
		if !e.lastUsed.IsZero() {
			idle = math.Min(1, now.Sub(e.lastUsed).Seconds()/300)
		}
		success := 0.5
		if tot := e.success + e.errors; tot > 0 {
			success = float64(e.success+1) / float64(tot+2)
		}
		w := p.opts.WeightCredit*credit + p.opts.WeightIdle*idle + p.opts.WeightSuccess*success +
			p.opts.WeightExpiring*e.expiringScore() + 0.01
		scores = append(scores, scored{e: e, w: w})
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].w > scores[j].w })

	k := TopK
	if k > len(scores) {
		k = len(scores)
	}
	top := scores[:k]
	total := 0.0
	for _, s := range top {
		total += s.w
	}
	r := p.rnd.Float64() * total
	for i, s := range top {
		r -= s.w
		if r <= 0 || i == len(top)-1 {
			return s.e
		}
	}
	return top[len(top)-1].e
}
