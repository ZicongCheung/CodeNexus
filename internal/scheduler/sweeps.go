package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/growth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// travelLocationID is the trip destination. The upstream treats the locations
// as interchangeable, so a fixed one is used.
const travelLocationID = 1

// target is one account paired with the adapter that serves its tasks and the
// task families that apply to it.
type target struct {
	Key   string
	Acc   *account.Account
	Tasks upstream.Tasks
	Scope upstream.TaskScope
}

// targets returns the accounts a sweep should visit. Administratively disabled
// accounts are skipped unless the caller asks for them, which the
// credential-maintenance families do when configured: a disabled account still
// should not lose its sign-in streak or its token. A paused account is never
// visited.
//
// only, when set, restricts the result to that one account.
func (s *Scheduler) targets(includeDisabled bool, only *account.Account) []target {
	if s.pool == nil {
		return nil
	}
	var out []target
	for _, st := range s.pool.Describe() {
		if only != nil && st.UID != only.UID {
			continue
		}
		if st.Paused {
			continue
		}
		if !st.Enabled && !includeDisabled {
			continue
		}
		acc, ok := s.pool.Get(st.Key)
		if !ok || acc == nil {
			continue
		}
		tasks, ok := s.tasksFor(acc)
		if !ok {
			continue
		}
		out = append(out, target{
			Key:   st.Key,
			Acc:   acc,
			Tasks: tasks,
			Scope: tasks.Scope(acc),
		})
	}
	return out
}

func (s *Scheduler) tasksFor(acc *account.Account) (upstream.Tasks, bool) {
	if s.tasks == nil {
		return nil, false
	}
	return s.tasks(acc.Source)
}

// forEach visits the applicable accounts in order, spacing the visits by the
// configured per-account delay so one sweep does not burst upstream.
func (s *Scheduler) forEach(ctx context.Context, includeDisabled bool, only *account.Account, want func(upstream.TaskScope) bool, fn func(context.Context, target)) {
	delay := accountDelay(s.config())
	first := true
	for _, t := range s.targets(includeDisabled, only) {
		if ctx.Err() != nil {
			return
		}
		if want != nil && !want(t.Scope) {
			continue
		}
		if !first && !sleepCtx(ctx, delay) {
			return
		}
		first = false
		fn(ctx, t)
	}
}

// checkinOne signs one account in and refreshes its balance.
func (s *Scheduler) checkinOne(ctx context.Context, t target) {
	day := s.now().In(s.Location()).Format("2006-01-02")
	if t.Scope.Checkin {
		res, err := t.Tasks.Checkin(ctx, t.Acc)
		switch {
		case err != nil:
			s.logf("checkin %s: %v", t.Key, err)
		case res != nil && res.Already:
			s.pool.NoteCheckin(t.Key, day)
			s.logf("checkin %s: already signed in", t.Key)
		default:
			s.pool.NoteCheckin(t.Key, day)
			s.logf("checkin %s: ok", t.Key)
		}
	}
	if t.Scope.Balance {
		s.refreshCredits(ctx, t, expiringWindow(s.config()))
	}
}

// RunCheckinNow signs every applicable account in and refreshes its balance.
// The balance read doubles as the observation the pool uses to lift a quota
// cooldown, so it runs even for accounts that have no check-in programme.
func (s *Scheduler) RunCheckinNow(ctx context.Context) {
	s.forEach(ctx, s.config().IncludeDisabledInTasks, nil, hasCheckinOrBalance, s.checkinOne)
}

// RunCheckinFor runs the check-in and balance refresh for one account.
func (s *Scheduler) RunCheckinFor(ctx context.Context, acc *account.Account) {
	s.forEach(ctx, true, acc, hasCheckinOrBalance, s.checkinOne)
}

func hasCheckinOrBalance(sc upstream.TaskScope) bool { return sc.Checkin || sc.Balance }

// RunBalanceNow refreshes every account's credit balance concurrently. Credits
// are a minute-scale observation, so this runs on its own interval.
func (s *Scheduler) RunBalanceNow(ctx context.Context) {
	cfg := s.config()
	expiring := expiringWindow(cfg)
	var wg sync.WaitGroup
	for _, t := range s.targets(cfg.IncludeDisabledInTasks, nil) {
		if !t.Scope.Balance {
			continue
		}
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			s.refreshCredits(ctx, t, expiring)
		}(t)
	}
	wg.Wait()
}

// RunBalanceFor refreshes one account's balance.
func (s *Scheduler) RunBalanceFor(ctx context.Context, acc *account.Account) {
	expiring := expiringWindow(s.config())
	s.forEach(ctx, true, acc, hasBalance, func(ctx context.Context, t target) {
		s.refreshCredits(ctx, t, expiring)
	})
}

func hasBalance(sc upstream.TaskScope) bool { return sc.Balance }

// refreshCredits reads and records one account's balance.
func (s *Scheduler) refreshCredits(ctx context.Context, t target, expiring time.Duration) {
	c, err := t.Tasks.Credits(ctx, t.Acc, expiring)
	if err != nil {
		s.logf("balance %s: %v", t.Key, err)
		return
	}
	s.pool.SetCreditsDetail(t.Key, pool.Credits{
		Remain:            c.Remain,
		Total:             c.Total,
		Known:             true,
		Expiring:          c.Expiring,
		EarliestAt:        c.EarliestAt,
		EarliestRemaining: c.EarliestRemaining,
	})
}

// RunActivityNow reports one conversation-activity event per account, which is
// what lights the consecutive sign-in day and unlocks the tasks that depend on
// it.
func (s *Scheduler) RunActivityNow(ctx context.Context) {
	s.forEach(ctx, s.config().IncludeDisabledInTasks, nil, hasActivity, s.activityOne)
}

// RunActivityFor reports activity for one account.
func (s *Scheduler) RunActivityFor(ctx context.Context, acc *account.Account) {
	s.forEach(ctx, true, acc, hasActivity, s.activityOne)
}

func hasActivity(sc upstream.TaskScope) bool { return sc.Activity }

func (s *Scheduler) activityOne(ctx context.Context, t target) {
	if err := t.Tasks.ReportActivity(ctx, t.Acc, "", ""); err != nil {
		s.logf("activity %s: %v", t.Key, err)
	}
}

// RunKeepaliveNow refreshes every account's credential so idle accounts do not
// lose their session.
func (s *Scheduler) RunKeepaliveNow(ctx context.Context) {
	s.forEach(ctx, s.config().IncludeDisabledInTasks, nil, nil, s.keepaliveOne)
}

// RunKeepaliveFor refreshes one account's credential. It reports whether the
// refresh subsystem is unavailable.
func (s *Scheduler) RunKeepaliveFor(ctx context.Context, acc *account.Account) bool {
	if s.refresh == nil {
		return true
	}
	s.forEach(ctx, true, acc, nil, s.keepaliveOne)
	return false
}

func (s *Scheduler) keepaliveOne(ctx context.Context, t target) {
	if s.refresh == nil || t.Acc.RefreshTokenValue() == "" {
		return
	}
	if err := s.refresh.RefreshNow(ctx, t.Acc); err != nil {
		s.logf("keepalive %s: %v", t.Key, err)
	}
}

// RunTravelNow advances the companion trip state machine by at most one step
// per account: adopt when there is no companion, collect an arrived trip, or
// send an idle one out.
func (s *Scheduler) RunTravelNow(ctx context.Context) {
	s.forEach(ctx, false, nil, hasTravel, s.travelOne)
}

// RunTravelFor advances the companion trip of one account.
func (s *Scheduler) RunTravelFor(ctx context.Context, acc *account.Account) {
	s.forEach(ctx, false, acc, hasTravel, s.travelOne)
}

func hasTravel(sc upstream.TaskScope) bool { return sc.Travel }

func (s *Scheduler) travelOne(ctx context.Context, t target) {
	st, err := t.Tasks.Travel(ctx, t.Acc)
	if err != nil {
		s.logf("travel %s: %v", t.Key, err)
		return
	}
	if st.Buddy == nil {
		if err := t.Tasks.BuddyAdopt(ctx, t.Acc); err != nil {
			s.logf("travel %s: adopt: %v", t.Key, err)
			return
		}
		s.logf("travel %s: companion adopted", t.Key)
		return
	}
	switch st.State.State {
	case "arrived":
		if st.State.RecordID == 0 {
			s.logf("travel %s: arrived without a record id", t.Key)
			return
		}
		reward, err := t.Tasks.TravelClaim(ctx, t.Acc, st.State.RecordID)
		if err != nil {
			s.logf("travel %s: claim: %v", t.Key, err)
			return
		}
		s.logf("travel %s: claimed %d", t.Key, reward)
	case "idle", "":
		if st.State.DailyLimitReached {
			return
		}
		if err := t.Tasks.TravelDepart(ctx, t.Acc, travelLocationID); err != nil {
			s.logf("travel %s: depart: %v", t.Key, err)
			return
		}
		s.logf("travel %s: departed", t.Key)
	default:
		// A trip in progress: nothing to do this pass.
	}
}

// RunBlackcatNow tops up the night-owl conversations for every applicable
// account. The family is scheduled inside the counting window.
func (s *Scheduler) RunBlackcatNow(ctx context.Context) {
	s.forEach(ctx, false, nil, hasNight, s.blackcatOne)
}

func hasNight(sc upstream.TaskScope) bool { return sc.Night }

func (s *Scheduler) blackcatOne(ctx context.Context, t target) {
	need, err := t.Tasks.NightNeed(ctx, t.Acc)
	if err != nil {
		s.logf("blackcat %s: %v", t.Key, err)
		return
	}
	if need <= 0 {
		return
	}
	if err := t.Tasks.NightRun(ctx, t.Acc, int(need)); err != nil {
		s.logf("blackcat %s: %v", t.Key, err)
	}
}

// RunGrowthNow runs the growth-task executors for every applicable account.
// Each account's tasks are idempotent, so re-running a sweep that was cut short
// simply resumes where it stopped.
func (s *Scheduler) RunGrowthNow(ctx context.Context) {
	s.forEach(ctx, false, nil, hasGrowth, s.growthOne)
}

func hasGrowth(sc upstream.TaskScope) bool { return sc.Growth }

func (s *Scheduler) growthOne(ctx context.Context, t target) {
	results := s.RunGrowthFor(ctx, t.Acc)
	var done, skipped, failed int
	for _, r := range results {
		switch r.Status {
		case "done":
			done++
		case "skipped":
			skipped++
		default:
			failed++
		}
	}
	s.logf("growth %s: %d done, %d skipped, %d failed", t.Key, done, skipped, failed)
}

// RunGrowthFor runs the growth executors for one account, used by the console's
// manual trigger.
func (s *Scheduler) RunGrowthFor(ctx context.Context, acc *account.Account) []growth.Result {
	tasks, ok := s.tasksFor(acc)
	if !ok {
		return nil
	}
	env := &growth.Env{Tasks: tasks, Acc: acc, Opts: s.growthOpts, Logf: s.logf}
	return growth.RunAll(ctx, env, growth.Actions())
}

// RunGrowthAction runs one growth executor for one account.
func (s *Scheduler) RunGrowthAction(ctx context.Context, acc *account.Account, code string) (growth.Result, bool) {
	tasks, ok := s.tasksFor(acc)
	if !ok {
		return growth.Result{}, false
	}
	act, ok := growth.ActionByCode(code)
	if !ok {
		return growth.Result{}, false
	}
	env := &growth.Env{Tasks: tasks, Acc: acc, Opts: s.growthOpts, Logf: s.logf}
	return growth.Run(ctx, env, act), true
}

// Adapter returns the task adapter for an account, used by the console for the
// read-only task and balance calls.
func (s *Scheduler) Adapter(acc *account.Account) (upstream.Tasks, bool) {
	return s.tasksFor(acc)
}

// Pool returns the account pool the scheduler operates on.
func (s *Scheduler) Pool() *pool.Pool { return s.pool }
