// Package scheduler runs the recurring account maintenance: the daily
// check-in, the activity report that lights the sign-in streak, token
// keep-alive, the companion trip, the night-owl conversations and the growth
// task sweep.
//
// Each family runs on its own list of clock hours in a configurable timezone.
// The main loop waits for the next slot and dispatches every family that shares
// it in parallel, so a slow family never delays a fast one.
//
// Two details matter for correctness on a machine that sleeps:
//
//   - The wait to the next slot is done in short wall-clock segments rather
//     than one long timer. A timer is driven by the monotonic clock, which
//     Modern Standby freezes; a segmented wait re-reads the wall clock each
//     segment, so a frozen machine still fires late rather than never.
//   - A slot that is already overdue when it fires is a late wake-up. The
//     network stack needs a moment to come back after standby, so a short
//     grace delay precedes dispatch. An on-time slot is not delayed.
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/growth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Kind identifies one scheduled family.
type Kind int

const (
	KindCheckin Kind = iota
	KindTravel
	KindActivity
	KindKeepalive
	KindBlackcat
	KindGrowth
)

// Name returns the stable identifier used in state and logs.
func (k Kind) Name() string {
	switch k {
	case KindCheckin:
		return "checkin"
	case KindTravel:
		return "travel"
	case KindActivity:
		return "activity"
	case KindKeepalive:
		return "keepalive"
	case KindBlackcat:
		return "blackcat"
	case KindGrowth:
		return "growth"
	default:
		return "unknown"
	}
}

// maintenanceKinds are the families that protect an account's continuity: a
// missed day costs a streak or a lapsed token, so they are the ones caught up
// after downtime. Growth tasks reset daily and carry no penalty, and the
// night-owl window is self-selecting, so neither is caught up.
var maintenanceKinds = []Kind{KindCheckin, KindActivity, KindKeepalive, KindTravel}

// Refresher refreshes an account's credential. It is satisfied by
// internal/authrefresh.
type Refresher interface {
	RefreshNow(ctx context.Context, acc *account.Account) error
}

// Options are the scheduler's dependencies.
type Options struct {
	Cfg   config.ScheduleConfig
	Pool  *pool.Pool
	Tasks func(upstream.Source) (upstream.Tasks, bool)
	// Maintenance resolves the narrower check-in and balance surface. It is
	// consulted first so a source that has no growth features can still be
	// scheduled; when nil the full Tasks lookup is used as a fallback.
	Maintenance func(upstream.Source) (upstream.Maintenance, bool)
	// Refresh performs the keep-alive token refresh. When nil the keep-alive
	// family is skipped.
	Refresh Refresher
	// State persists the last completed day per family. When nil the scheduler
	// simply has no catch-up memory.
	State *State
	Logf  func(format string, args ...any)
	// Now returns the current time; nil uses time.Now.
	Now func() time.Time
	// GrowthOpts tunes growth-task pacing.
	GrowthOpts growth.Options
}

// Scheduler drives the scheduled families.
type Scheduler struct {
	pool        *pool.Pool
	tasks       func(upstream.Source) (upstream.Tasks, bool)
	maintenance func(upstream.Source) (upstream.Maintenance, bool)
	refresh     Refresher
	state       *State
	logf        func(string, ...any)
	now         func() time.Time

	growthOpts growth.Options

	loc *time.Location

	mu  sync.Mutex
	cfg config.ScheduleConfig

	// rearm wakes the main loop after a configuration change so the new slots
	// take effect immediately. It has its own channel from the balance loop's
	// so a signal is never consumed by the wrong loop.
	rearm        chan struct{}
	rearmBalance chan struct{}

	balanceInterval   time.Duration
	balanceIntervalOK bool
}

// New builds a scheduler.
func New(opts Options) *Scheduler {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	loc, err := time.LoadLocation(opts.Cfg.Timezone)
	if err != nil || loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return &Scheduler{
		pool:         opts.Pool,
		tasks:        opts.Tasks,
		maintenance:  opts.Maintenance,
		refresh:      opts.Refresh,
		state:        opts.State,
		logf:         logf,
		now:          now,
		growthOpts:   opts.GrowthOpts,
		loc:          loc,
		cfg:          opts.Cfg,
		rearm:        make(chan struct{}, 1),
		rearmBalance: make(chan struct{}, 1),
	}
}

// Location returns the timezone the schedule is evaluated in.
func (s *Scheduler) Location() *time.Location { return s.loc }

// LocalDay is today in the schedule's timezone. A completed check-in is stored
// under this day, so the scheduler and the console agree on what "today" means
// regardless of the host's clock.
func (s *Scheduler) LocalDay() string {
	s.mu.Lock()
	loc := s.loc
	s.mu.Unlock()
	return s.now().In(loc).Format("2006-01-02")
}

// Reconfigure hot-updates the schedule and wakes the loops so the change takes
// effect immediately.
func (s *Scheduler) Reconfigure(cfg config.ScheduleConfig) {
	if loc, err := time.LoadLocation(cfg.Timezone); err == nil && loc != nil {
		s.mu.Lock()
		s.loc = loc
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	poke(s.rearm)
	poke(s.rearmBalance)
}

// config returns a snapshot of the schedule.
func (s *Scheduler) config() config.ScheduleConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// poke sends a non-blocking wake-up, coalescing with any pending signal.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// slotFor returns the configured hours and whether a family is enabled.
func slotFor(c config.ScheduleConfig, k Kind) ([]int, bool) {
	switch k {
	case KindCheckin:
		return c.CheckinHours, c.CheckinEnabled
	case KindTravel:
		return c.TravelHours, c.TravelEnabled
	case KindActivity:
		return c.ActivityHours, c.ActivityEnabled
	case KindKeepalive:
		return c.KeepaliveHours, c.KeepaliveEnabled
	case KindBlackcat:
		return c.BlackcatHours, c.BlackcatEnabled
	case KindGrowth:
		return c.GrowthHours, c.GrowthEnabled
	default:
		return nil, false
	}
}

func expiringWindow(c config.ScheduleConfig) time.Duration {
	if c.ExpiringSoonHours <= 0 {
		return 0
	}
	return time.Duration(c.ExpiringSoonHours) * time.Hour
}

func accountDelay(c config.ScheduleConfig) time.Duration {
	if c.AccountDelayMS <= 0 {
		return 0
	}
	return time.Duration(c.AccountDelayMS) * time.Millisecond
}

// nextFire returns the next occurrence of one of the given clock hours in loc,
// strictly after now.
func nextFire(now time.Time, hours []int, loc *time.Location) time.Time {
	local := now.In(loc)
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(local.Year(), local.Month(), local.Day(), h, 0, 0, 0, loc)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// nextWake returns the next slot and every family that runs at it. Families
// that share an hour are dispatched together.
func (s *Scheduler) nextWake(now time.Time) (time.Time, []Kind) {
	cfg := s.config()
	loc := s.Location()
	type slot struct {
		at   time.Time
		kind Kind
	}
	var slots []slot
	for _, k := range []Kind{KindCheckin, KindTravel, KindActivity, KindKeepalive, KindBlackcat, KindGrowth} {
		hours, enabled := slotFor(cfg, k)
		if !enabled {
			continue
		}
		slots = append(slots, slot{nextFire(now, hours, loc), k})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []Kind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

var (
	// wakeupGraceDelay is the pre-dispatch grace for a late wake-up: after
	// standby the network stack and DNS need a moment to come back, so firing
	// immediately would spend the one catch-up attempt on a doomed request.
	wakeupGraceDelay = 5 * time.Second
	// wakeupLateThreshold is how late a slot must be before it counts as a
	// late wake-up. Timer jitter is far below this.
	wakeupLateThreshold = time.Second
	// wallclockCheckStep bounds one wait segment. It trades slot precision
	// against how often an idle machine wakes up.
	wallclockCheckStep = time.Minute
)

// slotWake is the outcome of waiting for a slot.
type slotWake int

const (
	slotFired  slotWake = iota // the wall clock reached the slot
	slotRearm                  // the schedule changed: recompute
	slotCancel                 // the context was cancelled
)

// waitSlot waits, in segments, until the wall clock reaches next. Each segment
// re-reads the wall clock, so a machine that slept through the slot fires as
// soon as it wakes rather than after the frozen timer expires.
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		remain := time.Until(next)
		if remain <= 0 {
			return slotFired
		}
		d := remain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearm:
			timer.Stop()
			return slotRearm
		case <-timer.C:
		}
	}
}

// awaitWakeupGrace applies the network grace delay when a slot is overdue.
// It reports whether dispatch should proceed.
func awaitWakeupGrace(ctx context.Context, planned, now time.Time, logf func(string, ...any)) bool {
	if late := now.Sub(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil
	}
	logf("scheduler: late wake-up for the %s slot, waiting %s for the network", planned.Format("15:04"), wakeupGraceDelay)
	return sleepCtx(ctx, wakeupGraceDelay)
}

// Run drives the schedule, blocking until ctx is cancelled. When catch-up is
// enabled it first makes up any maintenance family whose slot already passed
// today and which never completed.
func (s *Scheduler) Run(ctx context.Context) {
	if s.config().CatchUpOnStart {
		s.CatchUp(ctx)
	}
	for {
		next, kinds := s.nextWake(s.now())
		if next.IsZero() {
			// Every family is disabled: idle until reconfigured or cancelled.
			select {
			case <-ctx.Done():
				return
			case <-s.rearm:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue
		case slotFired:
			if !awaitWakeupGrace(ctx, next, s.now(), s.logf) {
				return
			}
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch dispatches every family that shares a slot in parallel and waits for
// all of them, so a slow family does not delay a fast one.
func (s *Scheduler) runBatch(ctx context.Context, kinds []Kind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k Kind) {
			defer wg.Done()
			s.runKind(ctx, k)
		}(k)
	}
	wg.Wait()
}

// runKind dispatches one family, recording the day it completed.
func (s *Scheduler) runKind(ctx context.Context, k Kind) {
	start := time.Now()
	switch k {
	case KindCheckin:
		s.RunCheckinNow(ctx)
	case KindTravel:
		s.RunTravelNow(ctx)
	case KindActivity:
		s.RunActivityNow(ctx)
	case KindKeepalive:
		s.RunKeepaliveNow(ctx)
	case KindBlackcat:
		s.RunBlackcatNow(ctx)
	case KindGrowth:
		s.RunGrowthNow(ctx)
	}
	day := s.now().In(s.Location()).Format("2006-01-02")
	if err := s.state.Mark(k.Name(), day); err != nil {
		s.logf("scheduler: %s state: %v", k.Name(), err)
	}
	s.logf("scheduler: %s sweep finished in %s", k.Name(), time.Since(start).Round(time.Second))
}

// CatchUp runs a maintenance family once when its slot for today has already
// passed and it never completed, so a restart does not silently lose a day.
func (s *Scheduler) CatchUp(ctx context.Context) {
	cfg := s.config()
	now := s.now()
	loc := s.Location()
	today := now.In(loc).Format("2006-01-02")
	for _, k := range maintenanceKinds {
		hours, enabled := slotFor(cfg, k)
		if !enabled || len(hours) == 0 {
			continue
		}
		if s.state.Last(k.Name()) == today {
			continue
		}
		// Only catch up when the earliest slot of the day is already behind us.
		local := now.In(loc)
		earliest := time.Date(local.Year(), local.Month(), local.Day(), hours[0], 0, 0, 0, loc)
		if now.Before(earliest) {
			continue
		}
		s.logf("scheduler: catch-up run for %s (slot %02d:00 already passed)", k.Name(), hours[0])
		s.runKind(ctx, k)
	}
}

// StartBalanceRefresh starts the independent balance-refresh loop. Credits are
// a minute-scale observation, so they are refreshed on their own interval
// rather than on the hourly schedule.
func (s *Scheduler) StartBalanceRefresh(ctx context.Context) {
	cfg := s.config()
	if !cfg.BalanceRefreshEnabled || cfg.BalanceRefreshMinutes <= 0 {
		return
	}
	s.mu.Lock()
	s.balanceInterval = time.Duration(cfg.BalanceRefreshMinutes) * time.Minute
	s.balanceIntervalOK = true
	s.mu.Unlock()
	go func() {
		for {
			s.mu.Lock()
			interval := s.balanceInterval
			s.mu.Unlock()
			if interval <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop()
			case <-timer.C:
				s.RunBalanceNow(ctx)
			}
		}
	}()
}

// sleepCtx waits for d, returning false if the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
