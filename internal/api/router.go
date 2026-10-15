package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// errNeedPrefix reports a bare model name on the prefixed naming surface.
var errNeedPrefix = errors.New("model must be given as 'source/model' when router.naming is 'prefixed'")

// The router turns a client-supplied model name into the set of sources allowed
// to serve it.
//
// A source catalogue is the only honest way to know who serves what, but
// fetching one can require a network round trip, so the catalogues are cached
// and refreshed in the background: a request never waits on a catalogue. While
// a catalogue is unknown the router errs on the side of trying, because the
// failover loop already recovers from a source that turns out not to serve the
// model.
//
// Two naming forms are understood:
//
//	bare name        resolved across sources (the flat surface)
//	source/name      pins the source explicitly
type modelRouter struct {
	cfg  config.RouterConfig
	pool PoolView

	mu      sync.RWMutex
	catalog map[upstream.Source]catalogEntry
	// refreshing guards the background load per source.
	refreshing map[upstream.Source]bool

	ttl time.Duration
	now func() time.Time
}

// PoolView is the slice of the account pool the router needs.
type PoolView interface {
	HasSource(source upstream.Source) bool
	// Candidate returns a selectable account of a source, or nil.
	Candidate(source upstream.Source) *account.Account
}

// catalogEntry is one source's cached model catalogue.
type catalogEntry struct {
	models map[string]bool
	// at is when the catalogue was loaded; a zero value means "never".
	at time.Time
	// failed records that the last load failed, so the router does not retry
	// on every request.
	failed bool
}

// catalogTTL is how long a source catalogue is trusted.
const catalogTTL = 10 * time.Minute

// SetConfig replaces the naming and policy in place. The loaded catalogues are
// kept, so switching between flat and prefixed naming costs no refetch.
func (r *modelRouter) SetConfig(cfg config.RouterConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}

func newModelRouter(cfg config.RouterConfig, p PoolView) *modelRouter {
	return &modelRouter{
		cfg:        cfg,
		pool:       p,
		catalog:    map[upstream.Source]catalogEntry{},
		refreshing: map[upstream.Source]bool{},
		ttl:        catalogTTL,
		now:        time.Now,
	}
}

// Plan resolves a model name into the ordered list of sources that may serve
// it, together with the name handed to the adapter. The adapter translates the
// name to whatever its own source calls the model, so the flat name survives
// the hand-off.
func (r *modelRouter) Plan(model string) ([]upstream.Source, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", errNoUpstream
	}
	sources := upstream.Sources()

	// An explicit prefix always wins, whatever the naming mode says.
	if i := strings.IndexByte(model, '/'); i > 0 {
		prefix := upstream.Source(model[:i])
		if _, ok := upstream.Get(prefix); ok && (r.cfg.AllowPrefix || r.cfg.Naming == "prefixed") {
			return []upstream.Source{prefix}, strings.TrimSpace(model[i+1:]), nil
		}
	}
	if r.cfg.Naming == "prefixed" {
		// The prefixed surface is opt-in, so a bare name is a client mistake
		// worth reporting rather than guessing at.
		return nil, model, errNeedPrefix
	}

	if len(sources) == 0 {
		return nil, model, errNoUpstream
	}

	declared := r.declaredBy(sources, model)
	if len(declared) > 0 {
		if strings.EqualFold(r.cfg.SameNamePolicy, "first") {
			declared = declared[:1]
		}
		return declared, model, nil
	}

	// No catalogue knows the name. Try every source that currently holds an
	// account, which keeps an unlisted-but-valid model working; failing over
	// across them costs nothing on the happy path.
	live := make([]upstream.Source, 0, len(sources))
	for _, src := range sources {
		if r.pool.HasSource(src) {
			live = append(live, src)
		}
	}
	if len(live) == 0 {
		// Nothing is usable; report the first registered source so the caller
		// produces "service unavailable" rather than a client error.
		return []upstream.Source{sources[0]}, model, nil
	}
	return live, model, nil
}

// declaredBy returns the sources whose catalogue lists the model, in
// registration order. An unknown catalogue is not evidence, so a source with no
// cached catalogue is skipped here and picked up by the fallback instead.
func (r *modelRouter) declaredBy(sources []upstream.Source, model string) []upstream.Source {
	key := strings.ToLower(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []upstream.Source
	for _, src := range sources {
		entry, ok := r.catalog[src]
		if !ok || entry.models == nil {
			continue
		}
		if entry.models[key] {
			out = append(out, src)
		}
	}
	return out
}

// ModelCatalog returns the union of the cached catalogues, used by
// /v1/models. It never blocks: a source that has not been loaded yet simply
// contributes nothing.
func (r *modelRouter) ModelCatalog() []upstream.ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []upstream.ModelSpec
	for _, src := range upstream.Sources() {
		entry, ok := r.catalog[src]
		if !ok {
			continue
		}
		for id := range entry.models {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, upstream.ModelSpec{ID: id, Source: src})
		}
	}
	return out
}

// Warm starts the background catalogue loads. It returns immediately; the
// router works without a catalogue and refines its routing as they land.
func (r *modelRouter) Warm(ctx context.Context) {
	for _, src := range upstream.Sources() {
		r.refresh(ctx, src)
	}
	go r.loop(ctx)
}

// loop keeps the catalogues fresh.
func (r *modelRouter) loop(ctx context.Context) {
	t := time.NewTicker(r.ttl)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, src := range upstream.Sources() {
				r.refresh(ctx, src)
			}
		}
	}
}

// refresh reloads one source's catalogue in the background, at most once at a
// time. A failed load is remembered for one TTL so an unreachable upstream is
// not retried on every tick.
func (r *modelRouter) refresh(ctx context.Context, src upstream.Source) {
	u, ok := upstream.Get(src)
	if !ok {
		return
	}
	r.mu.Lock()
	entry := r.catalog[src]
	if r.refreshing[src] {
		r.mu.Unlock()
		return
	}
	if !entry.at.IsZero() && r.now().Sub(entry.at) < r.ttl {
		r.mu.Unlock()
		return
	}
	r.refreshing[src] = true
	r.mu.Unlock()

	acc := r.pool.Candidate(src)
	var (
		models []upstream.ModelSpec
		err    error
	)
	models, err = u.ListModels(ctx, acc)

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.refreshing, src)
	if err != nil || len(models) == 0 {
		r.catalog[src] = catalogEntry{models: entry.models, at: r.now(), failed: true}
		return
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		if m.ID != "" {
			set[strings.ToLower(m.ID)] = true
		}
	}
	r.catalog[src] = catalogEntry{models: set, at: r.now()}
}
