package pool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

const wb = upstream.SourceWorkBuddy

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) add(d time.Duration)     { c.t = c.t.Add(d) }
func newClock() *fakeClock                   { return &fakeClock{t: time.Unix(1000, 0)} }

func newPool(clk *fakeClock, opts Options) *Pool {
	opts.Clock = clk.now
	opts.Seed = 1
	return New(opts)
}

func wbAccount(uid string, enabled bool) *account.Account {
	return &account.Account{Source: account.SourceWorkBuddy, UID: uid, Enabled: enabled}
}

func mustAcquire(t *testing.T, p *Pool, opts PickOptions) *Lease {
	t.Helper()
	l, err := p.Acquire(context.Background(), opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return l
}

func TestAcquireFiltersBySourceAndEnabled(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, StickyEnabled: false})
	p.Replace([]*account.Account{
		wbAccount("a", true),
		{Source: account.SourceTraeCN, UID: "b", Enabled: true},
		wbAccount("c", false),
	})
	l := mustAcquire(t, p, PickOptions{Source: wb, Model: "m"})
	if l.Account.UID != "a" {
		t.Fatalf("picked %q, want a", l.Account.UID)
	}
	l.Succeed()
}

func TestCreditFloorExcludesEmptyAccount(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, CreditFloor: 100})
	p.Replace([]*account.Account{wbAccount("a", true), wbAccount("b", true)})
	p.SetCredits("workbuddy:a", 5, true)

	l := mustAcquire(t, p, PickOptions{Source: wb, Model: "m"})
	if l.Account.UID != "b" {
		t.Fatalf("picked %q, want b (a is below the credit floor)", l.Account.UID)
	}
	l.Succeed()
}

func TestRateLimitCoolsAccount(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{
		MaxInFlight: 1, CooldownBase: 30 * time.Second, CooldownMax: time.Hour, WaitMax: time.Second,
	})
	p.Replace([]*account.Account{wbAccount("a", true)})

	l := mustAcquire(t, p, PickOptions{Source: wb, Model: "m"})
	l.Fail(upstream.NewError(upstream.KindRateLimited, 429, "slow down"))

	_, err := p.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"})
	var na *ErrNoAccount
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	if na.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want > 0", na.RetryAfter)
	}
}

func TestBreakerTripsAfterTransientFailures(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{
		MaxInFlight: 1, BreakerThreshold: 2, BreakerFor: time.Minute,
		CooldownBase: 30 * time.Second, WaitMax: time.Second,
	})
	p.Replace([]*account.Account{wbAccount("a", true)})

	serr := upstream.NewError(upstream.KindServer, 500, "boom")
	mustAcquire(t, p, PickOptions{Source: wb, Model: "m"}).Fail(serr)
	mustAcquire(t, p, PickOptions{Source: wb, Model: "m"}).Fail(serr)

	_, err := p.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"})
	var na *ErrNoAccount
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want ErrNoAccount after breaker tripped", err)
	}
}

func TestAuthErrorDisablesAccount(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, WaitMax: time.Second})
	p.Replace([]*account.Account{wbAccount("a", true)})

	mustAcquire(t, p, PickOptions{Source: wb, Model: "m"}).Fail(upstream.NewError(upstream.KindAuth, 401, "bad key"))

	_, err := p.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"})
	var na *ErrNoAccount
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	if na.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 for an administratively dead account", na.RetryAfter)
	}
}

func TestExcludeSkipsTriedAccount(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 2})
	a, b := wbAccount("a", true), wbAccount("b", true)
	p.Replace([]*account.Account{a, b})

	l := mustAcquire(t, p, PickOptions{Source: wb, Model: "m", Exclude: map[string]bool{a.Key(): true}})
	if l.Account.UID != "b" {
		t.Fatalf("picked %q, want b (a excluded)", l.Account.UID)
	}
	l.Release()
}

func TestStickyBindsSessionToAccount(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, StickyEnabled: true, StickyTTL: time.Minute})
	p.Replace([]*account.Account{wbAccount("a", true), wbAccount("b", true)})

	first := mustAcquire(t, p, PickOptions{Source: wb, Model: "m", SessionKey: "s"})
	key := first.Account.Key()
	first.Release()

	second := mustAcquire(t, p, PickOptions{Source: wb, Model: "m", SessionKey: "s"})
	second.Release()
	if second.Account.Key() != key {
		t.Fatalf("sticky mismatch: %s != %s", second.Account.Key(), key)
	}
}

func TestInFlightSlotGatesSelection(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, WaitMax: 20 * time.Millisecond})
	p.Replace([]*account.Account{wbAccount("a", true)})

	held := mustAcquire(t, p, PickOptions{Source: wb, Model: "m"})
	if _, err := p.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"}); err == nil {
		t.Fatal("expected the saturated pool to reject a second acquisition")
	}
	held.Release()
	if _, err := p.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"}); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool-state.json")
	clk := newClock()
	accts := []*account.Account{wbAccount("a", true)}
	opts := Options{
		MaxInFlight: 1, CooldownBase: 30 * time.Second, CooldownMax: time.Hour, WaitMax: time.Second,
	}

	p1 := newPool(clk, opts)
	p1.SetStatePath(path)
	p1.Replace(accts)
	mustAcquire(t, p1, PickOptions{Source: wb, Model: "m"}).
		Fail(upstream.NewError(upstream.KindRateLimited, 429, "slow down"))
	if err := p1.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	p2 := newPool(clk, opts)
	p2.SetStatePath(path)
	p2.Replace(accts)
	if err := p2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	_, err := p2.Acquire(context.Background(), PickOptions{Source: wb, Model: "m"})
	var na *ErrNoAccount
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want the restored cooldown to block selection", err)
	}
}

func TestGCStickyDropsExpiredBindings(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 2, StickyEnabled: true, StickyTTL: time.Minute})
	p.Replace([]*account.Account{wbAccount("a", true), wbAccount("b", true)})

	mustAcquire(t, p, PickOptions{Source: wb, Model: "m", SessionKey: "s"}).Release()
	if got := p.StickyCount(); got != 1 {
		t.Fatalf("StickyCount = %d, want 1", got)
	}
	clk.add(2 * time.Minute)
	if removed := p.GCSticky(); removed != 1 {
		t.Fatalf("GCSticky removed %d, want 1", removed)
	}
	if got := p.StickyCount(); got != 0 {
		t.Fatalf("StickyCount = %d, want 0", got)
	}
}
