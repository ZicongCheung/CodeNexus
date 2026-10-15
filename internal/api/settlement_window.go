package api

// Credit attribution window.
//
// A turn's charge normally comes from the upstream's own per-turn bill. When
// the upstream has none to hand, the charge can still be inferred: read the
// account's consumption counter before the turn and again after it, and take
// the difference. That inference is only sound while the account served a
// single turn, because concurrent turns on one account share one counter, and a
// difference measured across them would attribute somebody else's charge to
// this record.
//
// The gate below enforces exactly that. A window opens per account, and only
// the account's sole in-flight turn may be measured; the moment a second turn
// starts, the account is marked unattributable until it is idle again, so an
// ambiguous difference is dropped rather than guessed.

import (
	"context"
	"sync"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// snapshotGate tracks how many turns each account has in flight so a balance
// difference can only be attributed to a turn that ran alone.
type snapshotGate struct {
	mu     sync.Mutex
	active map[string]int
	unsafe map[string]struct{}
}

func newSnapshotGate() *snapshotGate {
	return &snapshotGate{active: map[string]int{}, unsafe: map[string]struct{}{}}
}

// begin registers one in-flight turn for acc and reports whether a balance
// difference measured across it can be attributed to that turn alone.
func (g *snapshotGate) begin(acc *account.Account) bool {
	key := acc.Key()
	g.mu.Lock()
	defer g.mu.Unlock()
	active := g.active[key]
	if active > 0 {
		g.unsafe[key] = struct{}{}
	}
	g.active[key] = active + 1
	return active == 0
}

// safe re-checks attribution at settlement time: a turn that was alone when it
// began is no longer attributable if another turn started meanwhile.
func (g *snapshotGate) safe(acc *account.Account) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, bad := g.unsafe[acc.Key()]
	return !bad
}

// end retires one in-flight turn. An account becomes usable for attribution
// again only once it has no turn in flight at all.
func (g *snapshotGate) end(acc *account.Account) {
	key := acc.Key()
	g.mu.Lock()
	defer g.mu.Unlock()
	active := g.active[key]
	if active <= 1 {
		delete(g.active, key)
		delete(g.unsafe, key)
		return
	}
	g.active[key] = active - 1
}

// usedReading is one reading of an account's consumption counter.
type usedReading struct {
	used float64
	ok   bool
}

// creditSnapshot is one turn's observation window: the before-reading taken
// when the turn started, and the gate slot that decides whether the difference
// across the turn means anything.
type creditSnapshot struct {
	acc    *account.Account
	gate   *snapshotGate
	usable bool

	ctx    context.Context
	cancel context.CancelFunc
	before chan usedReading
	once   sync.Once
}

// openSnapshot starts a window for one turn, reading the account's consumption
// counter in the background so the upstream call is never delayed by a billing
// endpoint. It returns nil when the adapter cannot report a balance at all.
func (s *Server) openSnapshot(u upstream.Upstream, acc *account.Account) *creditSnapshot {
	if acc == nil {
		return nil
	}
	observer, ok := u.(upstream.TurnUsage)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	snap := &creditSnapshot{
		acc:    acc,
		gate:   s.gate,
		ctx:    ctx,
		cancel: cancel,
		before: make(chan usedReading, 1),
	}
	snap.usable = s.gate.begin(acc)
	if !snap.usable {
		// Another turn is already in flight on this account, so any difference
		// across this one would be a mix. Take no reading at all.
		return snap
	}
	go func() {
		used, ok := observer.UsedCredits(ctx, acc)
		snap.before <- usedReading{used: used, ok: ok}
	}()
	return snap
}

// attributable reports whether the difference across this window belongs to
// this turn alone, checking both ends: it must have started alone, and still be
// alone now.
func (snap *creditSnapshot) attributable() bool {
	if snap == nil {
		return false
	}
	return snap.usable && snap.gate.safe(snap.acc)
}

// beforeUsed waits for the opening reading. It reports false when the reading
// never arrived, or when the wait outlived the settlement budget.
func (snap *creditSnapshot) beforeUsed(ctx context.Context) (float64, bool) {
	if snap == nil {
		return 0, false
	}
	select {
	case r := <-snap.before:
		return r.used, r.ok
	case <-ctx.Done():
		return 0, false
	}
}

// release retires the window, freeing the account for attribution once the
// settlement that owns it is done. It is safe to call more than once.
func (snap *creditSnapshot) release() {
	if snap == nil {
		return
	}
	snap.once.Do(func() {
		snap.cancel()
		snap.gate.end(snap.acc)
	})
}
