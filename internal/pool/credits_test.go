package pool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func TestExpiringScore(t *testing.T) {
	cases := []struct {
		name     string
		credits  int64
		expiring int64
		want     float64
	}{
		{"none expiring", 100, 0, 0},
		{"no balance observed", 0, 10, 0},
		{"half expiring", 100, 50, 0.5},
		{"clamped", 50, 80, 1},
	}
	for _, tc := range cases {
		e := &entry{credits: tc.credits, expiring: tc.expiring}
		if got := e.expiringScore(); got != tc.want {
			t.Errorf("%s: score = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSetCreditsDetailClearsQuotaCooldown(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, CooldownBase: time.Minute, WaitMax: time.Millisecond})
	p.Add(wbAccount("u1", true))

	lease := mustAcquire(t, p, PickOptions{Source: wb})
	lease.Fail(&upstream.Error{Kind: upstream.KindQuota, Message: "quota exhausted"})

	if _, err := p.Acquire(context.Background(), PickOptions{Source: wb}); err == nil {
		t.Fatal("account should be cooling right after a quota failure")
	}

	p.SetCreditsDetail("workbuddy:u1", Credits{Remain: 500, Total: 1000, Known: true})

	got, ok := p.Credits("workbuddy:u1")
	if !ok || got.Remain != 500 || got.Total != 1000 || !got.Known {
		t.Fatalf("credits = %+v ok=%v", got, ok)
	}
	if _, err := p.Acquire(context.Background(), PickOptions{Source: wb}); err != nil {
		t.Fatalf("account should be selectable again once a balance is observed: %v", err)
	}
}

func TestCreditsDetailAndCheckinPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.json")
	clk := newClock()
	opts := Options{MaxInFlight: 1, Clock: clk.now, Seed: 1}

	p1 := newPool(clk, opts)
	p1.SetStatePath(path)
	p1.Add(wbAccount("u1", true))
	p1.SetCreditsDetail("workbuddy:u1", Credits{
		Remain:     700,
		Total:      1000,
		Known:      true,
		Expiring:   200,
		EarliestAt: clk.t.Add(48 * time.Hour),
	})
	p1.NoteCheckin("workbuddy:u1", "2026-10-15")
	if err := p1.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	p2 := newPool(clk, opts)
	p2.SetStatePath(path)
	p2.Add(wbAccount("u1", true))
	if err := p2.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got, _ := p2.Credits("workbuddy:u1")
	if got.Remain != 700 || got.Expiring != 200 || got.EarliestAt.IsZero() {
		t.Fatalf("restored credits = %+v", got)
	}
	if day := p2.CheckinDay("workbuddy:u1"); day != "2026-10-15" {
		t.Fatalf("restored checkin day = %q", day)
	}
	st := p2.Describe()
	if len(st) != 1 || st[0].CreditsTotal != 1000 || st[0].Expiring != 200 || st[0].EarliestExpiry == 0 {
		t.Fatalf("Describe = %+v", st)
	}
}

func TestPausedAccountIsNotSelected(t *testing.T) {
	clk := newClock()
	p := newPool(clk, Options{MaxInFlight: 1, WaitMax: time.Millisecond})
	p.Add(wbAccount("u1", true))
	p.Add(wbAccount("u2", true))

	p.SetPaused("workbuddy:u1", true)
	if !p.Paused("workbuddy:u1") {
		t.Fatal("Paused should report true")
	}
	for i := 0; i < 5; i++ {
		lease := mustAcquire(t, p, PickOptions{Source: wb})
		if lease.Account.UID == "u1" {
			t.Fatal("paused account must never be selected")
		}
		lease.Release()
	}

	p.SetPaused("workbuddy:u1", false)
	if p.Paused("workbuddy:u1") {
		t.Fatal("Paused should report false after resume")
	}
	if _, err := p.Acquire(context.Background(), PickOptions{Source: wb, Exclude: map[string]bool{"workbuddy:u2": true}}); err != nil {
		t.Fatalf("resumed account should be selectable: %v", err)
	}
}
