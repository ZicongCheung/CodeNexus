package growth

import (
	"context"
	"fmt"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// miniProgramCodes are the task codes that only exist in the mini-program
// listing and require the mini-program request marker to be listed, accepted
// and claimed. A code that is not registered here is only looked up in the
// default listing, so an unknown code never costs an extra upstream call.
var miniProgramCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// IsMiniProgram reports whether a task code is mini-program-only.
func IsMiniProgram(code string) bool { return miniProgramCodes[code] }

// ProgressText renders a task's progress for the console.
func ProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	if t.AcceptStatus != "" {
		return t.AcceptStatus
	}
	return "?"
}

// taskByCode finds one task in the default listing, falling back to the
// mini-program listing for the codes that only exist there.
func (e *Env) taskByCode(ctx context.Context, code string) (*upstream.Task, error) {
	tasks, err := e.Tasks.ListTasks(ctx, e.Acc, false)
	if err != nil {
		return nil, err
	}
	if t := findTask(tasks, code); t != nil {
		return t, nil
	}
	if !IsMiniProgram(code) {
		return nil, nil
	}
	return e.taskByCodeMini(ctx, code)
}

// taskByCodeMini finds one task in the mini-program listing.
func (e *Env) taskByCodeMini(ctx context.Context, code string) (*upstream.Task, error) {
	tasks, err := e.Tasks.ListTasks(ctx, e.Acc, true)
	if err != nil {
		return nil, err
	}
	return findTask(tasks, code), nil
}

func findTask(tasks []upstream.Task, code string) *upstream.Task {
	for i := range tasks {
		if tasks[i].Code == code {
			return &tasks[i]
		}
	}
	return nil
}

// verify waits, within a bounded budget, for a task's asynchronous scoring to
// settle, then returns the latest reading. A query failure during the wait
// does not discard an earlier reading.
func (e *Env) verify(ctx context.Context, code string, mini bool) (*upstream.Task, error) {
	if mini {
		return e.verifyMini(ctx, code)
	}
	return e.verifyDefault(ctx, code)
}

func (e *Env) verifyDefault(ctx context.Context, code string) (*upstream.Task, error) {
	t, err := e.taskByCode(ctx, code)
	if err != nil || t == nil || t.Claimable || t.Claimed {
		return t, err
	}
	o := e.opts()
	for i := 1; i < o.VerifyAttempts; i++ {
		if err := e.sleep(ctx, o.VerifyGap); err != nil {
			return t, nil
		}
		t2, err2 := e.taskByCode(ctx, code)
		if err2 != nil {
			return t, nil
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

func (e *Env) verifyMini(ctx context.Context, code string) (*upstream.Task, error) {
	o := e.opts()
	t, err := e.taskByCodeMini(ctx, code)
	if err != nil {
		return t, err
	}
	for i := 0; i < o.MiniVerifyRounds; i++ {
		if t != nil && (t.Claimable || t.Claimed) {
			return t, nil
		}
		if err := e.sleep(ctx, o.MiniVerifyGap); err != nil {
			return t, nil
		}
		t2, err2 := e.taskByCodeMini(ctx, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
	}
	return t, nil
}

// acceptMini enrolls one mini-program task and confirms the enrolment took
// effect by reading it back. The upstream can answer 200 while the enrolment
// silently does not register, in which case the evidence never counts.
func (e *Env) acceptMini(ctx context.Context, code string) bool {
	o := e.opts()
	for attempt := 1; attempt <= 2; attempt++ {
		if err := e.Tasks.AcceptTasks(ctx, e.Acc, []string{code}, true); err != nil {
			e.logf("growth: accept %s attempt %d: %v", code, attempt, err)
			continue
		}
		if err := e.sleep(ctx, o.ActionGap); err != nil {
			return false
		}
		t, err := e.taskByCodeMini(ctx, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
	}
	return false
}

// targetOf returns the task target, defaulting to 1 when the upstream reports
// no target (which is what an un-enrolled mini-program task looks like).
func targetOf(t *upstream.Task) int64 {
	if t.Target > 0 {
		return t.Target
	}
	return 1
}
