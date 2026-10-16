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

// target is one account paired with the adapters that serve it and the task
// families that apply to it. Full is nil for a source that has no growth
// surface, which is why the growth-only families check it before use.
type target struct {
	Key   string
	Acc   *account.Account
	Maint upstream.Maintenance
	Full  upstream.Tasks
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
		maint, ok := s.maintenanceFor(acc)
		if !ok {
			continue
		}
		full, _ := s.tasksFor(acc)
		out = append(out, target{
			Key:   st.Key,
			Acc:   acc,
			Maint: maint,
			Full:  full,
			Scope: maint.Scope(acc),
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

// maintenanceFor resolves the check-in and balance surface, preferring the
// narrow lookup so an adapter never has to claim a growth interface it cannot
// serve.
func (s *Scheduler) maintenanceFor(acc *account.Account) (upstream.Maintenance, bool) {
	if s.maintenance != nil {
		if m, ok := s.maintenance(acc.Source); ok {
			return m, true
		}
	}
	if s.tasks != nil {
		if t, ok := s.tasks(acc.Source); ok {
			return t, true
		}
	}
	return nil, false
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
	day := s.LocalDay()
	if t.Scope.Checkin {
		res, err := t.Maint.Checkin(ctx, t.Acc)
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

// RunCheckinRetryNow retries the check-in for accounts that have not completed
// today's. It is what keeps an account the upstream throttled at the scheduled
// slot from losing the day: the adapter's own 9074 backoff decides how much of
// this loop actually reaches upstream, so an account still in backoff costs
// nothing.
func (s *Scheduler) RunCheckinRetryNow(ctx context.Context) {
	today := s.LocalDay()
	pending := s.pendingCheckins(today)
	if len(pending) == 0 {
		return
	}
	delay := accountDelay(s.config())
	first := true
	for _, t := range pending {
		if ctx.Err() != nil {
			return
		}
		if !first && !sleepCtx(ctx, delay) {
			return
		}
		first = false
		s.checkinOne(ctx, t)
	}
}

// pendingCheckins returns the targets whose daily check-in is still open.
func (s *Scheduler) pendingCheckins(today string) []target {
	var out []target
	for _, t := range s.targets(s.config().IncludeDisabledInTasks, nil) {
		if !t.Scope.Checkin {
			continue
		}
		if s.pool != nil && s.pool.CheckinDay(t.Key) == today {
			continue
		}
		out = append(out, t)
	}
	return out
}

// StartCheckinRetry starts the independent check-in retry loop. It stops when
// the context ends.
func (s *Scheduler) StartCheckinRetry(ctx context.Context) {
	interval := time.Duration(s.config().CheckinRetrySeconds) * time.Second
	if !s.config().CheckinRetryEnabled || interval <= 0 {
		return
	}
	go func() {
		for {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.RunCheckinRetryNow(ctx)
			}
		}
	}()
}

// RunCheckinFor runs the check-in and balance refresh for one account.
func (s *Scheduler) RunCheckinFor(ctx context.Context, acc *account.Account) {
	s.forEach(ctx, true, acc, hasCheckinOrBalance, s.checkinOne)
}

func hasCheckinOrBalance(sc upstream.TaskScope) bool { return sc.Checkin || sc.Balance }

// CheckinScope reports the maintenance families that apply to one account. The
// console uses it to tell "this source has no check-in" apart from "it did not
// work", which is the difference between a misleading and an honest message.
func (s *Scheduler) CheckinScope(acc *account.Account) upstream.TaskScope {
	if acc == nil {
		return upstream.TaskScope{}
	}
	maint, ok := s.maintenanceFor(acc)
	if !ok {
		return upstream.TaskScope{}
	}
	return maint.Scope(acc)
}

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
	c, err := t.Maint.Credits(ctx, t.Acc, expiring)
	if err != nil {
		s.logf("balance %s: %v", t.Key, err)
		return
	}
	s.pool.SetCreditsDetail(t.Key, pool.Credits{
		Remain:            c.Remain,
		Total:             c.Total,
		Known:             true,
		Unlimited:         c.Unlimited,
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
	if t.Full == nil {
		return
	}
	if err := t.Full.ReportActivity(ctx, t.Acc, "", ""); err != nil {
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
	if t.Full == nil {
		return
	}
	st, err := t.Full.Travel(ctx, t.Acc)
	if err != nil {
		s.logf("travel %s: %v", t.Key, err)
		return
	}
	if st.Buddy == nil {
		if err := t.Full.BuddyAdopt(ctx, t.Acc); err != nil {
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
		reward, err := t.Full.TravelClaim(ctx, t.Acc, st.State.RecordID)
		if err != nil {
			s.logf("travel %s: claim: %v", t.Key, err)
			return
		}
		s.logf("travel %s: claimed %d", t.Key, reward)
	case "idle", "":
		if st.State.DailyLimitReached {
			return
		}
		if err := t.Full.TravelDepart(ctx, t.Acc, travelLocationID); err != nil {
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
	if t.Full == nil {
		return
	}
	need, err := t.Full.NightNeed(ctx, t.Acc)
	if err != nil {
		s.logf("blackcat %s: %v", t.Key, err)
		return
	}
	if need <= 0 {
		return
	}
	if err := t.Full.NightRun(ctx, t.Acc, int(need)); err != nil {
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

// Maintenance exposes the narrower check-in and balance surface for one
// account. Callers that only need a balance or a check-in must use this rather
// than Adapter: a source can serve those two without serving the growth
// features, and asking for the wider interface is what silently excludes it.
func (s *Scheduler) Maintenance(acc *account.Account) (upstream.Maintenance, bool) {
	return s.maintenanceFor(acc)
}

// Pool returns the account pool the scheduler operates on.
func (s *Scheduler) Pool() *pool.Pool { return s.pool }
