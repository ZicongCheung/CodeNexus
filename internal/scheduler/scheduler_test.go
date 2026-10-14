package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// fakeTasks implements upstream.Tasks with call counters.
type fakeTasks struct {
	upstream.Tasks // nil: an unexercised method panics loudly

	scope upstream.TaskScope

	mu        sync.Mutex
	checkins  int
	activity  int
	credits   int
	travels   int
	travelSet int
	adopts    int
	nights    int
	growth    int
}

func (f *fakeTasks) Scope(*account.Account) upstream.TaskScope { return f.scope }

func (f *fakeTasks) Checkin(context.Context, *account.Account) (*upstream.CheckinResult, error) {
	f.mu.Lock()
	f.checkins++
	f.mu.Unlock()
	return &upstream.CheckinResult{Credit: 10}, nil
}

func (f *fakeTasks) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	f.mu.Lock()
	f.credits++
	f.mu.Unlock()
	return &upstream.Credits{Remain: 100, Total: 200}, nil
}

func (f *fakeTasks) ReportActivity(context.Context, *account.Account, string, string) error {
	f.mu.Lock()
	f.activity++
	f.mu.Unlock()
	return nil
}

func (f *fakeTasks) Travel(context.Context, *account.Account) (*upstream.TravelStatus, error) {
	f.mu.Lock()
	f.travels++
	f.mu.Unlock()
	return &upstream.TravelStatus{
		Buddy: &upstream.Buddy{ID: 1, Name: "x"},
		State: upstream.TravelState{State: "arrived", RecordID: 7, RewardCredit: 5},
	}, nil
}

func (f *fakeTasks) TravelClaim(context.Context, *account.Account, int64) (int64, error) {
	f.mu.Lock()
	f.travelSet++
	f.mu.Unlock()
	return 5, nil
}

func (f *fakeTasks) BuddyAdopt(context.Context, *account.Account) error {
	f.mu.Lock()
	f.adopts++
	f.mu.Unlock()
	return nil
}

func (f *fakeTasks) NightNeed(context.Context, *account.Account) (int64, error) { return 2, nil }

func (f *fakeTasks) NightRun(context.Context, *account.Account, int) error {
	f.mu.Lock()
	f.nights++
	f.mu.Unlock()
	return nil
}

func (f *fakeTasks) ListTasks(context.Context, *account.Account, bool) ([]upstream.Task, error) {
	f.mu.Lock()
	f.growth++
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeTasks) AcceptTasks(context.Context, *account.Account, []string, bool) error { return nil }

func (f *fakeTasks) ClaimReward(context.Context, *account.Account, string, bool) (int64, int64, error) {
	return 0, 0, nil
}

func counts(f *fakeTasks) (checkins, activity, credits, travels, nights, growth int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkins, f.activity, f.credits, f.travels, f.nights, f.growth
}

func fullScope() upstream.TaskScope {
	return upstream.TaskScope{Checkin: true, Activity: true, Balance: true, Growth: true, Travel: true, Night: true}
}

// newTestScheduler builds a scheduler over one enabled account.
func newTestScheduler(t *testing.T, cfg config.ScheduleConfig, f *fakeTasks, now func() time.Time) *Scheduler {
	t.Helper()
	p := pool.New(pool.Options{})
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("tok", "refresh", "www.codebuddy.cn", 0)
	p.Add(acc)
	if cfg.Timezone == "" {
		cfg.Timezone = "Asia/Shanghai"
	}
	return New(Options{
		Cfg:   cfg,
		Pool:  p,
		Tasks: func(upstream.Source) (upstream.Tasks, bool) { return f, true },
		State: LoadState(filepath.Join(t.TempDir(), "sched.json")),
		Now:   now,
		Logf:  func(string, ...any) {},
	})
}

func TestNextFirePicksTheNextHourToday(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 15, 10, 30, 0, 0, loc)
	got := nextFire(now, []int{9, 21}, loc)
	if got.Hour() != 21 || got.Day() != 15 {
		t.Fatalf("nextFire = %s, want 21:00 on the 15th", got)
	}
}

func TestNextFireRollsToTomorrow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 15, 22, 0, 0, 0, loc)
	got := nextFire(now, []int{9, 21}, loc)
	if got.Hour() != 9 || got.Day() != 16 {
		t.Fatalf("nextFire = %s, want 09:00 on the 16th", got)
	}
}

func TestNextFireUsesTheConfiguredTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// 02:00 UTC is 10:00 in Shanghai, so the 09:00 slot has passed.
	now := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	got := nextFire(now, []int{9}, loc)
	if got.In(loc).Hour() != 9 || got.In(loc).Day() != 16 {
		t.Fatalf("nextFire = %s, want 09:00 Shanghai on the 16th", got.In(loc))
	}
}

func TestNextWakeGroupsFamiliesSharingAnHour(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{
		CheckinHours: []int{9, 21}, TravelHours: []int{9, 21},
		ActivityHours: []int{10}, KeepaliveHours: []int{22},
		BlackcatHours: []int{23}, GrowthHours: []int{1},
		CheckinEnabled: true, TravelEnabled: true, ActivityEnabled: true,
		KeepaliveEnabled: true, BlackcatEnabled: true, GrowthEnabled: true,
	}, &fakeTasks{scope: fullScope()}, time.Now)

	loc := s.Location()
	now := time.Date(2026, 10, 15, 8, 0, 0, 0, loc)
	at, kinds := s.nextWake(now)
	if at.Hour() != 9 {
		t.Fatalf("next slot = %s, want 09:00", at)
	}
	got := map[Kind]bool{}
	for _, k := range kinds {
		got[k] = true
	}
	if !got[KindCheckin] || !got[KindTravel] {
		t.Fatalf("09:00 must dispatch checkin and travel, got %v", kinds)
	}
	if got[KindActivity] {
		t.Fatalf("activity runs at 10:00, not 09:00: %v", kinds)
	}
}

func TestNextWakeExcludesDisabledFamilies(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{
		CheckinHours: []int{9}, TravelHours: []int{9},
		CheckinEnabled: true, TravelEnabled: false,
	}, &fakeTasks{scope: fullScope()}, time.Now)
	_, kinds := s.nextWake(time.Date(2026, 10, 15, 8, 0, 0, 0, s.Location()))
	for _, k := range kinds {
		if k == KindTravel {
			t.Fatal("a disabled family must not be scheduled")
		}
	}
}

func TestWaitSlotFiresSegmented(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{Timezone: "Asia/Shanghai"}, &fakeTasks{}, time.Now)
	next := time.Now().Add(60 * time.Millisecond)
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotFired {
		t.Fatalf("waitSlot = %v, want slotFired", got)
	}
}

func TestWaitSlotFiresImmediatelyWhenOverdue(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{Timezone: "Asia/Shanghai"}, &fakeTasks{}, time.Now)
	if got := s.waitSlot(context.Background(), time.Now().Add(-time.Hour), time.Minute); got != slotFired {
		t.Fatalf("waitSlot = %v, want slotFired for an overdue slot", got)
	}
}

func TestWaitSlotRearms(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{}, &fakeTasks{}, time.Now)
	poke(s.rearm)
	if got := s.waitSlot(context.Background(), time.Now().Add(time.Hour), time.Minute); got != slotRearm {
		t.Fatalf("waitSlot = %v, want slotRearm", got)
	}
}

func TestWaitSlotCancels(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{}, &fakeTasks{}, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.waitSlot(ctx, time.Now().Add(time.Hour), time.Minute); got != slotCancel {
		t.Fatalf("waitSlot = %v, want slotCancel", got)
	}
}

func TestAwaitWakeupGraceOnTimeIsImmediate(t *testing.T) {
	now := time.Now()
	if !awaitWakeupGrace(context.Background(), now, now, func(string, ...any) {}) {
		t.Fatal("an on-time slot must not be delayed")
	}
}

func TestAwaitWakeupGraceOnLateSlot(t *testing.T) {
	old := wakeupGraceDelay
	wakeupGraceDelay = time.Millisecond
	t.Cleanup(func() { wakeupGraceDelay = old })

	now := time.Now()
	if !awaitWakeupGrace(context.Background(), now.Add(-time.Minute), now, func(string, ...any) {}) {
		t.Fatal("a late slot must still dispatch after the grace delay")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if awaitWakeupGrace(ctx, now.Add(-time.Minute), now, func(string, ...any) {}) {
		t.Fatal("a cancelled context must abandon the batch")
	}
}

func TestCatchUpRunsEachMaintenanceFamilyOnce(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	noon := time.Date(2026, 10, 15, 12, 0, 0, 0, loc)
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{
		Timezone: "Asia/Shanghai",
		// 09:00 is already behind us at noon; growth at 01:00 must never be
		// caught up because it carries no continuity penalty.
		CheckinHours: []int{9}, ActivityHours: []int{9}, KeepaliveHours: []int{9},
		TravelHours: []int{9}, GrowthHours: []int{1},
		CheckinEnabled: true, ActivityEnabled: true, KeepaliveEnabled: true,
		TravelEnabled: true, GrowthEnabled: true,
		CatchUpOnStart: true,
	}, f, func() time.Time { return noon })

	s.CatchUp(context.Background())
	checkins, activity, credits, travels, _, growth := counts(f)
	if checkins != 1 || activity != 1 || credits != 1 || travels != 1 {
		t.Fatalf("catch-up counts = %d/%d/%d/%d, want 1 each", checkins, activity, credits, travels)
	}
	if growth != 0 {
		t.Fatal("growth must not be caught up")
	}

	s.CatchUp(context.Background())
	checkins2, _, _, _, _, _ := counts(f)
	if checkins2 != 1 {
		t.Fatalf("a second catch-up on the same day must be a no-op, got %d check-ins", checkins2)
	}
}

func TestCatchUpSkipsSlotsStillAhead(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	early := time.Date(2026, 10, 15, 3, 0, 0, 0, loc)
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{
		Timezone:     "Asia/Shanghai",
		CheckinHours: []int{9}, CheckinEnabled: true, CatchUpOnStart: true,
	}, f, func() time.Time { return early })
	s.CatchUp(context.Background())
	if checkins, _, _, _, _, _ := counts(f); checkins != 0 {
		t.Fatal("a slot later today must not be caught up")
	}
}

func TestRunCheckinNotesBalanceAndSignInDay(t *testing.T) {
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{
		Timezone: "Asia/Shanghai", CheckinEnabled: true,
	}, f, time.Now)
	s.RunCheckinNow(context.Background())

	st := s.pool.Describe()[0]
	if !st.CreditsKnown || st.Credits != 100 {
		t.Fatalf("balance not recorded: %+v", st)
	}
	if st.CheckinDay == "" {
		t.Fatal("sign-in day not recorded")
	}
}

func TestTravelClaimsAnArrivedTrip(t *testing.T) {
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{Timezone: "Asia/Shanghai"}, f, time.Now)
	s.RunTravelNow(context.Background())
	if f.travelSet != 1 {
		t.Fatalf("travel claims = %d, want 1", f.travelSet)
	}
}

func TestTargetsSkipPausedAccounts(t *testing.T) {
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{Timezone: "Asia/Shanghai"}, f, time.Now)
	s.pool.SetPaused(s.pool.Describe()[0].Key, true)
	if got := len(s.targets(true, nil)); got != 0 {
		t.Fatalf("targets = %d, a paused account must be skipped", got)
	}
}

func TestTargetsIncludeDisabledOnlyWhenAsked(t *testing.T) {
	f := &fakeTasks{scope: fullScope()}
	s := newTestScheduler(t, config.ScheduleConfig{Timezone: "Asia/Shanghai"}, f, time.Now)
	key := s.pool.Describe()[0].Key
	s.pool.SetDisabled(key, true)

	if got := len(s.targets(false, nil)); got != 0 {
		t.Fatalf("targets = %d, want 0 without the include-disabled flag", got)
	}
	if got := len(s.targets(true, nil)); got != 1 {
		t.Fatalf("targets = %d, want 1 with the include-disabled flag", got)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := newTestScheduler(t, config.ScheduleConfig{
		Timezone: "Asia/Shanghai", CheckinHours: []int{9}, CheckinEnabled: true,
	}, &fakeTasks{scope: fullScope()}, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestStatePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := LoadState(path)
	if err := s.Mark("checkin", "2026-10-15"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	reloaded := LoadState(path)
	if got := reloaded.Last("checkin"); got != "2026-10-15" {
		t.Fatalf("reloaded last = %q", got)
	}
	if got := reloaded.Last("travel"); got != "" {
		t.Fatalf("unset family = %q, want empty", got)
	}
}

func TestStateMarkIsIdempotent(t *testing.T) {
	s := LoadState("")
	if err := s.Mark("checkin", "2026-10-15"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mark("checkin", "2026-10-15"); err != nil {
		t.Fatal(err)
	}
	if got := s.Last("checkin"); got != "2026-10-15" {
		t.Fatalf("last = %q", got)
	}
}

func TestNilStateIsSafe(t *testing.T) {
	var s *State
	if err := s.Mark("x", "y"); err != nil {
		t.Fatalf("Mark on a nil state: %v", err)
	}
	if got := s.Last("x"); got != "" {
		t.Fatalf("Last on a nil state = %q", got)
	}
}
