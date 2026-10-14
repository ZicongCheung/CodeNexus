// Package growth implements the account growth-task executors: the recurring
// actions that keep an account topped up so it stays usable by the gateway.
//
// Every executor follows the same shape. It reads the task's current progress,
// produces the upstream evidence the task is keyed on, waits for the
// upstream's asynchronous scoring to settle, and collects the reward. The
// evidence is client telemetry and behaviour events, so the executors express
// everything through the upstream.Tasks primitives and never talk to the
// network directly; that keeps them testable against a stub adapter.
//
// Most tasks are not driven by a dedicated endpoint but by a fingerprint the
// upstream recognises: "use the desktop client", "use the mini program". The
// executors therefore replay the corresponding telemetry chains, which is why
// several of them have to run a real conversation first (the expert-usage
// evidence only counts when it quotes a server-issued request id).
package growth

import (
	"context"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Class groups the executors by the kind of evidence they send, which is also
// how the console lets a user enable or disable a whole family at once.
type Class string

const (
	// ClassAccount covers the executors that only use the account's own
	// maintenance primitives (activity reports, companion, night conversations).
	ClassAccount Class = "account"
	// ClassDesktop covers the executors whose evidence is a desktop-client
	// telemetry chain.
	ClassDesktop Class = "desktop"
	// ClassMiniProgram covers the executors whose evidence is a mini-program
	// telemetry chain.
	ClassMiniProgram Class = "mini_program"
)

// Options tunes pacing and polling. The defaults mirror the pacing observed on
// the wire; tests shorten them so a suite does not sleep for minutes.
type Options struct {
	// ReportGap separates consecutive activity reports.
	ReportGap time.Duration
	// ActionGap separates the accept / report / claim steps of a mini-program
	// task, which the upstream rate-limits.
	ActionGap time.Duration
	// MiniChatGap separates consecutive mini-program conversation events. The
	// upstream runs an anti-cheat check on the multi-conversation tasks and
	// rolls back events that arrive in a burst, so this gap is deliberately
	// long.
	MiniChatGap time.Duration
	// MiniChatJitter is the maximum random extra delay added before each
	// mini-program conversation event, so the pacing is not perfectly regular.
	MiniChatJitter time.Duration
	// ExpertSummonGap separates the summon-and-use cycles of the expert tasks,
	// keeping them at a human pace.
	ExpertSummonGap time.Duration
	// VerifyAttempts and VerifyGap bound the wait for a task's asynchronous
	// scoring to settle before the reward is collected.
	VerifyAttempts int
	VerifyGap      time.Duration
	// MiniVerifyRounds and MiniVerifyGap are the equivalent bound for
	// mini-program tasks, which settle faster.
	MiniVerifyRounds int
	MiniVerifyGap    time.Duration
}

// DefaultOptions returns the pacing observed on the wire.
func DefaultOptions() Options {
	return Options{
		ReportGap:        1050 * time.Millisecond,
		ActionGap:        2 * time.Second,
		MiniChatGap:      45 * time.Second,
		MiniChatJitter:   10 * time.Second,
		ExpertSummonGap:  6 * time.Second,
		VerifyAttempts:   4,
		VerifyGap:        3 * time.Second,
		MiniVerifyRounds: 2,
		MiniVerifyGap:    3 * time.Second,
	}
}

// normalize fills zero fields from the defaults so a partially specified
// Options is still usable.
func (o Options) normalize() Options {
	d := DefaultOptions()
	if o.ReportGap <= 0 {
		o.ReportGap = d.ReportGap
	}
	if o.ActionGap <= 0 {
		o.ActionGap = d.ActionGap
	}
	if o.MiniChatGap <= 0 {
		o.MiniChatGap = d.MiniChatGap
	}
	if o.ExpertSummonGap <= 0 {
		o.ExpertSummonGap = d.ExpertSummonGap
	}
	if o.VerifyAttempts <= 0 {
		o.VerifyAttempts = d.VerifyAttempts
	}
	if o.VerifyGap <= 0 {
		o.VerifyGap = d.VerifyGap
	}
	if o.MiniVerifyRounds <= 0 {
		o.MiniVerifyRounds = d.MiniVerifyRounds
	}
	if o.MiniVerifyGap <= 0 {
		o.MiniVerifyGap = d.MiniVerifyGap
	}
	return o
}

// Env carries everything an executor needs. The function fields are optional
// and default to the real clock, a context-aware sleep and no logging, so a
// test only overrides what it cares about.
type Env struct {
	Tasks upstream.Tasks
	Acc   *account.Account
	Opts  Options

	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
	Logf  func(format string, args ...any)
	// Rand returns a duration in [0, n); nil uses a real pseudo-random source.
	Rand func(n time.Duration) time.Duration
}

func (e *Env) opts() Options { return e.Opts.normalize() }

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// jitter returns a random extra delay of at most n.
func (e *Env) jitter(n time.Duration) time.Duration {
	if n <= 0 || e.Rand == nil {
		return 0
	}
	return e.Rand(n)
}

// Action is one runnable growth-task executor.
type Action struct {
	// Code is the upstream task code this action completes.
	Code string
	// Desc is a human-readable description of what the action does.
	Desc string
	// Class groups the action by the kind of evidence it sends.
	Class Class
	// Attempt marks an action the upstream has not confirmed is scriptable.
	// It is still run, but a console can present the result as best-effort.
	Attempt bool
	// Run produces the evidence. It does not claim the reward; the runner does
	// that once the progress has settled.
	Run func(ctx context.Context, e *Env) (string, error)
}

// Result is the outcome of running one action for one account.
type Result struct {
	Code      string `json:"task_code"`
	Desc      string `json:"desc,omitempty"`
	Class     Class  `json:"class,omitempty"`
	Status    string `json:"status"` // done | skipped | error
	Message   string `json:"message,omitempty"`
	Before    string `json:"progress_before,omitempty"`
	After     string `json:"progress_after,omitempty"`
	Claimable bool   `json:"claimable,omitempty"`
	Claimed   bool   `json:"claimed,omitempty"`
	Credit    int64  `json:"credit,omitempty"`
	Energy    int64  `json:"energy,omitempty"`
	Attempt   bool   `json:"attempt,omitempty"`
}

// Run executes one action and, when the progress settles at its target,
// collects the reward. It never returns an error: a failure is reported in the
// result so a batch can continue past a single broken task.
func Run(ctx context.Context, env *Env, act *Action) Result {
	res := Result{Code: act.Code, Desc: act.Desc, Class: act.Class, Attempt: act.Attempt}
	if env.Tasks == nil || env.Acc == nil {
		res.Status = "error"
		res.Message = "growth: no task adapter or account"
		return res
	}

	mini := IsMiniProgram(act.Code)
	before, err := env.taskByCode(ctx, act.Code)
	if err != nil {
		res.Status = "error"
		res.Message = "查询任务失败: " + err.Error()
		return res
	}
	if before == nil {
		res.Status = "skipped"
		res.Message = "该账号没有此任务"
		return res
	}
	res.Before = ProgressText(before)
	if before.Claimed {
		res.Status = "skipped"
		res.Message = "该任务奖励已领取"
		return res
	}

	msg, err := act.Run(ctx, env)
	if err != nil {
		res.Status = "error"
		res.Message = err.Error()
		return res
	}
	res.Message = msg

	after, verr := env.verify(ctx, act.Code, mini)
	if verr == nil && after != nil {
		res.After = ProgressText(after)
		res.Claimable = after.Claimable
	}
	if after == nil || !after.Claimable {
		res.Status = "done"
		return res
	}
	credit, energy, cerr := env.Tasks.ClaimReward(ctx, env.Acc, act.Code, mini)
	if cerr != nil {
		res.Status = "done"
		res.Message = msg + "；达标但领奖失败，下次重试"
		return res
	}
	res.Claimed = true
	res.Credit = credit
	res.Energy = energy
	res.Status = "done"
	res.Message = msg + "；已自动领奖"
	return res
}

// RunAll executes every applicable action for one account in table order,
// which is dependency order. A failing action does not stop the batch.
//
// Enrolling the pending tasks is done up front: the acceptance itself does not
// produce progress, but it moves the tasks into their normal state machine.
func RunAll(ctx context.Context, env *Env, actions []Action) []Result {
	env.acceptPending(ctx)
	out := make([]Result, 0, len(actions))
	for i := range actions {
		if err := ctx.Err(); err != nil {
			out = append(out, Result{Code: actions[i].Code, Status: "error", Message: err.Error()})
			return out
		}
		res := Run(ctx, env, &actions[i])
		out = append(out, res)
		// Only pace between actions that actually did something; a skipped
		// task costs no upstream traffic and needs no gap.
		if res.Status != "skipped" {
			_ = env.sleep(ctx, env.opts().ReportGap)
		}
	}
	return out
}

// acceptPending enrolls every not-yet-enrolled task, in both the default and
// the mini-program listing. Failures are ignored: progress is driven by the
// behaviour events, not by the enrolment.
func (e *Env) acceptPending(ctx context.Context) {
	for _, mini := range []bool{false, true} {
		tasks, err := e.Tasks.ListTasks(ctx, e.Acc, mini)
		if err != nil {
			continue
		}
		var codes []string
		for _, t := range tasks {
			if t.Claimed || t.Locked {
				continue
			}
			if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			codes = append(codes, t.Code)
		}
		if len(codes) == 0 {
			continue
		}
		if err := e.Tasks.AcceptTasks(ctx, e.Acc, codes, mini); err == nil {
			_ = e.sleep(ctx, e.opts().ReportGap)
		}
	}
}

// AcceptAll enrolls every pending task for the account, returning how many
// were enrolled in each listing.
func AcceptAll(ctx context.Context, env *Env) (def, mini int, err error) {
	for _, isMini := range []bool{false, true} {
		tasks, lerr := env.Tasks.ListTasks(ctx, env.Acc, isMini)
		if lerr != nil {
			return def, mini, lerr
		}
		var codes []string
		for _, t := range tasks {
			if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			codes = append(codes, t.Code)
		}
		if len(codes) == 0 {
			continue
		}
		if aerr := env.Tasks.AcceptTasks(ctx, env.Acc, codes, isMini); aerr != nil {
			return def, mini, aerr
		}
		if isMini {
			mini = len(codes)
		} else {
			def = len(codes)
		}
	}
	return def, mini, nil
}
