// Package authrefresh keeps account credentials fresh.
//
// Two paths use it: a background pass refreshes tokens that are nearing
// expiry, and the request path forces a refresh when the upstream rejects a
// token, so a stale credential heals without the user noticing. Concurrent
// refreshes of the same account are collapsed into one call.
package authrefresh

import (
	"context"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Options configures a Refresher.
type Options struct {
	// Within refreshes a token that expires within this window.
	Within time.Duration
	// Interval is the background scan interval.
	Interval time.Duration
	// Timeout bounds a single refresh call.
	Timeout time.Duration
	// Logf receives warnings.
	Logf func(string, ...any)
}

func (o Options) withDefaults() Options {
	if o.Within <= 0 {
		o.Within = 30 * time.Minute
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Minute
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	return o
}

// Refresher refreshes account tokens, de-duplicating concurrent attempts.
type Refresher struct {
	pool  *pool.Pool
	store *authstore.Store
	opts  Options

	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	err  error
}

// New builds a Refresher. store may be nil to skip persistence.
func New(p *pool.Pool, store *authstore.Store, opts Options) *Refresher {
	return &Refresher{pool: p, store: store, opts: opts.withDefaults(), inflight: map[string]*call{}}
}

func (r *Refresher) warn(format string, args ...any) {
	if r.opts.Logf != nil {
		r.opts.Logf(format, args...)
	}
}

// EnsureFresh refreshes acc only when its token is close to expiring.
func (r *Refresher) EnsureFresh(ctx context.Context, acc *account.Account) error {
	if acc == nil || !acc.NeedsRefresh(r.opts.Within) {
		return nil
	}
	return r.RefreshNow(ctx, acc)
}

// RefreshNow forces a refresh regardless of the recorded expiry.
func (r *Refresher) RefreshNow(ctx context.Context, acc *account.Account) error {
	if acc == nil {
		return nil
	}
	key := acc.Key()
	r.mu.Lock()
	if c, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	r.inflight[key] = c
	r.mu.Unlock()

	c.err = r.doRefresh(ctx, acc)
	close(c.done)
	r.mu.Lock()
	delete(r.inflight, key)
	r.mu.Unlock()
	return c.err
}

func (r *Refresher) doRefresh(ctx context.Context, acc *account.Account) error {
	u, ok := upstream.Get(acc.Source)
	if !ok {
		return nil // no adapter registered; nothing to refresh
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	before := acc.Token()
	if err := u.Refresh(ctx, acc); err != nil {
		r.warn("refresh %s: %v", acc.Key(), err)
		return err
	}
	if acc.Token() != before {
		r.persist(acc)
	}
	return nil
}

func (r *Refresher) persist(acc *account.Account) {
	if r.store == nil || acc.Ephemeral {
		return
	}
	if err := r.store.Save(acc); err != nil {
		r.warn("persist refreshed %s: %v", acc.Key(), err)
	}
}

// Start runs the background refresh loop until ctx is cancelled.
func (r *Refresher) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.opts.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.sweep(ctx)
			}
		}
	}()
}

func (r *Refresher) sweep(ctx context.Context) {
	if r.pool == nil {
		return
	}
	for _, acc := range r.pool.List() {
		if !acc.Enabled || acc.Ephemeral {
			continue
		}
		if !acc.NeedsRefresh(r.opts.Within) {
			continue
		}
		_ = r.EnsureFresh(ctx, acc)
	}
}
