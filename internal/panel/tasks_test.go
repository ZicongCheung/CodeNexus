package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/growth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/scheduler"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// taskAdapter is a task adapter for console tests.
type taskAdapter struct {
	upstream.Tasks
	defaultTasks []upstream.Task
	miniTasks    []upstream.Task
	accepted     int
	claimed      []string
	activity     int
	checkins     int
}

func (a *taskAdapter) ListTasks(_ context.Context, _ *account.Account, mini bool) ([]upstream.Task, error) {
	if mini {
		return a.miniTasks, nil
	}
	return a.defaultTasks, nil
}

func (a *taskAdapter) AcceptTasks(_ context.Context, _ *account.Account, codes []string, _ bool) error {
	a.accepted += len(codes)
	return nil
}

func (a *taskAdapter) ClaimReward(_ context.Context, _ *account.Account, code string, _ bool) (int64, int64, error) {
	a.claimed = append(a.claimed, code)
	return 12, 3, nil
}

func (a *taskAdapter) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	return &upstream.Credits{Remain: 77, Total: 100}, nil
}

func (a *taskAdapter) Checkin(context.Context, *account.Account) (*upstream.CheckinResult, error) {
	a.checkins++
	return &upstream.CheckinResult{}, nil
}

func (a *taskAdapter) ReportActivity(context.Context, *account.Account, string, string) error {
	a.activity++
	return nil
}

func (a *taskAdapter) Scope(*account.Account) upstream.TaskScope {
	return upstream.TaskScope{Checkin: true, Activity: true, Balance: true, Growth: true, Travel: true, Night: true}
}

func (a *taskAdapter) Travel(context.Context, *account.Account) (*upstream.TravelStatus, error) {
	return &upstream.TravelStatus{Buddy: &upstream.Buddy{ID: 1}, State: upstream.TravelState{State: "idle"}}, nil
}

func (a *taskAdapter) TravelDepart(context.Context, *account.Account, int) error { return nil }

func (a *taskAdapter) NightNeed(context.Context, *account.Account) (int64, error) { return 0, nil }

// newTaskConsole builds a console over one account with a task adapter and a
// request log attached.
func newTaskConsole(t *testing.T, adapter *taskAdapter, rec *reqlog.Recorder) (*Console, *pool.Pool) {
	t.Helper()
	c, store, p := newConsole(t)
	_ = store
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u-1", Label: "主号", Enabled: true}
	acc.SetCredentials("tok", "rt", "www.codebuddy.cn", 0)
	p.Add(acc)

	cfg := config.Default()
	cfg.Schedule.Timezone = "Asia/Shanghai"
	// Shorten the growth pacing so the suite does not sleep for seconds while
	// waiting for the (stubbed) upstream scoring to settle.
	gopts := growth.DefaultOptions()
	gopts.ReportGap = time.Millisecond
	gopts.ActionGap = time.Millisecond
	gopts.MiniChatGap = time.Millisecond
	gopts.ExpertSummonGap = time.Millisecond
	gopts.VerifyAttempts = 2
	gopts.VerifyGap = time.Millisecond
	gopts.MiniVerifyGap = time.Millisecond
	sched := scheduler.New(scheduler.Options{
		Cfg:        cfg.Schedule,
		Pool:       p,
		Tasks:      func(upstream.Source) (upstream.Tasks, bool) { return adapter, true },
		GrowthOpts: gopts,
		Now:        time.Now,
	})
	c.sched = sched
	c.reqlog = rec
	return c, p
}

func decode(t *testing.T, rec *httptestResponse) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.body, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.body)
	}
	return out
}

type httptestResponse struct {
	code int
	body []byte
}

func do(c *Console, method, target, body string) httptestResponse {
	rec := call(c, method, target, body, true, "")
	return httptestResponse{code: rec.Code, body: rec.Body.Bytes()}
}

func TestTaskCatalogListsEveryExecutor(t *testing.T) {
	c, _ := newTaskConsole(t, &taskAdapter{}, nil)
	res := do(c, http.MethodGet, "/panel/api/tasks/catalog", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["count"].(float64) != 25 {
		t.Fatalf("count = %v, want 25", out["count"])
	}
}

func TestAccountTasksMergeBothListings(t *testing.T) {
	adapter := &taskAdapter{
		defaultTasks: []upstream.Task{{Code: "chat_5", Target: 5}},
		miniTasks:    []upstream.Task{{Code: "Sequential_Tasks_1", Target: 1}},
	}
	c, _ := newTaskConsole(t, adapter, nil)
	res := do(c, http.MethodGet, "/panel/api/accounts/u-1/tasks", "")
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (default + mini-program)", len(tasks))
	}
	var sawMini bool
	for _, raw := range tasks {
		tm := raw.(map[string]any)
		if tm["code"] == "Sequential_Tasks_1" && tm["mini_program"] == true {
			sawMini = true
		}
	}
	if !sawMini {
		t.Fatal("the mini-program task must be flagged")
	}
}

func TestAccountTasksUnknownAccount(t *testing.T) {
	c, _ := newTaskConsole(t, &taskAdapter{}, nil)
	if res := do(c, http.MethodGet, "/panel/api/accounts/nope/tasks", ""); res.code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", res.code)
	}
}

func TestTaskAcceptSingleAndAll(t *testing.T) {
	adapter := &taskAdapter{
		defaultTasks: []upstream.Task{{Code: "chat_5", AcceptStatus: "not_accepted"}},
		miniTasks:    []upstream.Task{{Code: "school_season"}},
	}
	c, _ := newTaskConsole(t, adapter, nil)

	res := do(c, http.MethodPost, "/panel/api/accounts/u-1/tasks/accept", `{"task_code":"chat_5"}`)
	if res.code != 200 || adapter.accepted != 1 {
		t.Fatalf("code=%d accepted=%d", res.code, adapter.accepted)
	}

	adapter.accepted = 0
	res = do(c, http.MethodPost, "/panel/api/accounts/u-1/tasks/accept", `{"all":true}`)
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["accepted_default"].(float64) != 1 || out["accepted_mini"].(float64) != 1 {
		t.Fatalf("accepted = %v", out)
	}
}

func TestTaskClaimPassesTheMiniProgramFlag(t *testing.T) {
	adapter := &taskAdapter{}
	c, _ := newTaskConsole(t, adapter, nil)
	res := do(c, http.MethodPost, "/panel/api/accounts/u-1/tasks/claim", `{"task_code":"school_season"}`)
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	if len(adapter.claimed) != 1 || adapter.claimed[0] != "school_season" {
		t.Fatalf("claimed = %v", adapter.claimed)
	}
	out := decode(t, &res)
	if out["credit"].(float64) != 12 || out["energy"].(float64) != 3 {
		t.Fatalf("reward = %v", out)
	}
}

func TestTaskAutoUnknownCode(t *testing.T) {
	c, _ := newTaskConsole(t, &taskAdapter{}, nil)
	res := do(c, http.MethodPost, "/panel/api/accounts/u-1/tasks/auto", `{"task_code":"nope"}`)
	if res.code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", res.code)
	}
}

func TestTaskAutoRunsOneExecutor(t *testing.T) {
	adapter := &taskAdapter{
		defaultTasks: []upstream.Task{{Code: "chat_5", Target: 2, Current: 0, AcceptStatus: "accepted"}},
	}
	c, _ := newTaskConsole(t, adapter, nil)
	res := do(c, http.MethodPost, "/panel/api/accounts/u-1/tasks/auto", `{"task_code":"chat_5"}`)
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["status"] != "done" {
		t.Fatalf("result = %v", out)
	}
	if adapter.activity != 2 {
		t.Fatalf("activity reports = %d, want 2", adapter.activity)
	}
}

func TestAccountStateToggles(t *testing.T) {
	c, p := newTaskConsole(t, &taskAdapter{}, nil)
	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/pause", ""); res.code != 200 {
		t.Fatalf("pause code = %d", res.code)
	}
	if !p.Describe()[0].Paused {
		t.Fatal("account should be paused")
	}
	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/resume", ""); res.code != 200 {
		t.Fatalf("resume code = %d", res.code)
	}
	if p.Describe()[0].Paused {
		t.Fatal("account should be resumed")
	}
	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/disable", ""); res.code != 200 {
		t.Fatalf("disable code = %d", res.code)
	}
	if p.Describe()[0].Enabled {
		t.Fatal("account should be disabled")
	}
}

func TestAccountCheckinTriggersTheSweep(t *testing.T) {
	adapter := &taskAdapter{}
	c, p := newTaskConsole(t, adapter, nil)
	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/checkin", ""); res.code != 200 {
		t.Fatalf("code = %d", res.code)
	}
	if adapter.checkins != 1 {
		t.Fatalf("check-ins = %d", adapter.checkins)
	}
	if st := p.Describe()[0]; !st.CreditsKnown || st.Credits != 77 {
		t.Fatalf("balance not recorded: %+v", st)
	}
}

func TestBatchDispatchesKnownOperations(t *testing.T) {
	adapter := &taskAdapter{}
	c, _ := newTaskConsole(t, adapter, nil)
	if res := do(c, http.MethodPost, "/panel/api/batch/activity_all", ""); res.code != 200 {
		t.Fatalf("code = %d", res.code)
	}
	if adapter.activity != 1 {
		t.Fatalf("activity = %d", adapter.activity)
	}
	if res := do(c, http.MethodPost, "/panel/api/batch/nope", ""); res.code != http.StatusNotFound {
		t.Fatalf("unknown batch = %d, want 404", res.code)
	}
}

func TestQueueAndScanAll(t *testing.T) {
	adapter := &taskAdapter{defaultTasks: []upstream.Task{
		{Code: "chat_5", Claimed: true},
		{Code: "first_buddy", Target: 1, Claimable: true},
	}}
	c, _ := newTaskConsole(t, adapter, nil)

	res := do(c, http.MethodGet, "/panel/api/tasks/queue", "")
	if res.code != 200 {
		t.Fatalf("queue code = %d", res.code)
	}
	out := decode(t, &res)
	queue, _ := out["queue"].([]any)
	if len(queue) != 1 {
		t.Fatalf("queue = %d, want 1 account with pending tasks", len(queue))
	}
	item := queue[0].(map[string]any)
	if item["pending"].(float64) != 1 || item["claimable"].(float64) != 1 {
		t.Fatalf("queue item = %v", item)
	}

	res = do(c, http.MethodPost, "/panel/api/tasks/scan_all", "")
	if res.code != 200 {
		t.Fatalf("scan_all code = %d", res.code)
	}
	out = decode(t, &res)
	if len(out["accounts"].([]any)) != 1 {
		t.Fatalf("scan_all = %v", out)
	}
}

func TestObservabilityEndpoints(t *testing.T) {
	rec := reqlog.New(10, false)
	rec.Begin()
	rec.Record(reqlog.Event{RequestID: "r1", Path: "/v1/chat/completions", OK: true, Status: 200})
	c, _ := newTaskConsole(t, &taskAdapter{}, rec)

	res := do(c, http.MethodGet, "/panel/api/observability/metrics", "")
	if res.code != 200 {
		t.Fatalf("metrics code = %d", res.code)
	}
	out := decode(t, &res)
	if out["completed"].(float64) != 1 {
		t.Fatalf("metrics = %v", out)
	}

	res = do(c, http.MethodGet, "/panel/api/observability/logs", "")
	out = decode(t, &res)
	if len(out["recent"].([]any)) != 1 {
		t.Fatalf("logs = %v", out)
	}
}

func TestObservabilityDisabledWithoutRecorder(t *testing.T) {
	c, _ := newTaskConsole(t, &taskAdapter{}, nil)
	res := do(c, http.MethodGet, "/panel/api/observability/metrics", "")
	out := decode(t, &res)
	if out["enabled"] != false {
		t.Fatalf("metrics without a recorder = %v", out)
	}
}

func TestGrowthExecutorDoesNotBlockTheConsole(t *testing.T) {
	adapter := &taskAdapter{}
	c, _ := newTaskConsole(t, adapter, nil)
	// The catalogue is a pure read and must never require an account.
	if res := do(c, http.MethodGet, "/panel/api/tasks/catalog", ""); res.code != 200 {
		t.Fatalf("catalog code = %d", res.code)
	}
	// The executor table is the same one the scheduler runs.
	for _, a := range growth.Actions() {
		if a.Code == "" || a.Run == nil {
			t.Fatalf("incomplete action %+v", a)
		}
	}
}

// maintOnly is an adapter with a check-in and a balance but no growth surface,
// which is what a source like Trae CN looks like. It deliberately implements
// upstream.Maintenance and not upstream.Tasks.
type maintOnly struct {
	checkins int
	balances int
}

func (m *maintOnly) Scope(*account.Account) upstream.TaskScope {
	return upstream.TaskScope{Checkin: true, Balance: true}
}

func (m *maintOnly) Checkin(context.Context, *account.Account) (*upstream.CheckinResult, error) {
	m.checkins++
	return &upstream.CheckinResult{Credit: 5}, nil
}

func (m *maintOnly) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	m.balances++
	return &upstream.Credits{Remain: 40, Total: 100}, nil
}

// newMaintConsole wires a console whose only maintenance surface is the narrow
// interface, so the panel has to route through it.
func newMaintConsole(t *testing.T, m upstream.Maintenance) (*Console, *pool.Pool) {
	t.Helper()
	c, _, p := newConsole(t)
	acc := &account.Account{Source: account.SourceTraeCN, UID: "u-1", Label: "trae", Enabled: true}
	acc.SetCredentials("tok", "", "trae", 0)
	p.Add(acc)

	cfg := config.Default()
	cfg.Schedule.Timezone = "Asia/Shanghai"
	c.sched = scheduler.New(scheduler.Options{
		Cfg:         cfg.Schedule,
		Pool:        p,
		Maintenance: func(upstream.Source) (upstream.Maintenance, bool) { return m, true },
		Now:         time.Now,
	})
	return c, p
}

func TestPanelRoutesAMaintenanceOnlyAdapter(t *testing.T) {
	m := &maintOnly{}
	c, p := newMaintConsole(t, m)

	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/checkin", ""); res.code != 200 {
		t.Fatalf("checkin code = %d (%s)", res.code, res.body)
	}
	if m.checkins != 1 {
		t.Fatalf("check-ins = %d, want 1", m.checkins)
	}
	// The check-in sweep also refreshes the balance, which is the observation
	// the pool uses to lift a quota cooldown.
	if m.balances != 1 {
		t.Fatalf("balance reads = %d, want 1", m.balances)
	}
	if st := p.Describe()[0]; !st.CreditsKnown || st.Credits != 40 {
		t.Fatalf("balance not recorded: %+v", st)
	}

	if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/balance", ""); res.code != 200 {
		t.Fatalf("balance code = %d (%s)", res.code, res.body)
	}
	if m.balances != 2 {
		t.Fatalf("balance reads = %d, want 2", m.balances)
	}

	// A growth-only operation has nothing to run for this source, but that is
	// not a failure: the console must not report one.
	for _, op := range []string{"activity", "travel"} {
		if res := do(c, http.MethodPost, "/panel/api/accounts/u-1/"+op, ""); res.code != 200 {
			t.Fatalf("%s code = %d (%s)", op, res.code, res.body)
		}
	}

	// The whole-pool sweep must reach it too.
	m.checkins = 0
	if res := do(c, http.MethodPost, "/panel/api/batch/checkin_all", ""); res.code != 200 {
		t.Fatalf("checkin_all code = %d (%s)", res.code, res.body)
	}
	if m.checkins != 1 {
		t.Fatalf("check-ins after the batch = %d, want 1", m.checkins)
	}
}

// balanceOnly is an adapter with a credit balance but no check-in programme,
// which is what an enterprise account looks like.
type balanceOnly struct{ balances int }

func (b *balanceOnly) Scope(*account.Account) upstream.TaskScope {
	return upstream.TaskScope{Balance: true}
}

func (b *balanceOnly) Checkin(context.Context, *account.Account) (*upstream.CheckinResult, error) {
	return nil, errors.New("no check-in programme")
}

func (b *balanceOnly) Credits(context.Context, *account.Account, time.Duration) (*upstream.Credits, error) {
	b.balances++
	return &upstream.Credits{Remain: 10, Total: 10}, nil
}

func TestAccountsReportTodayCheckinState(t *testing.T) {
	m := &maintOnly{}
	c, _ := newMaintConsole(t, m)

	res := do(c, http.MethodGet, "/panel/api/accounts", "")
	if res.code != 200 {
		t.Fatalf("accounts code = %d (%s)", res.code, res.body)
	}
	before := accountRow(t, &res)
	if before["checkin_applicable"] != true {
		t.Fatalf("a check-in source must be reported as applicable: %v", before)
	}
	if before["checkin_done"] == true {
		t.Fatalf("nothing has been signed in yet: %v", before)
	}

	res = do(c, http.MethodPost, "/panel/api/accounts/u-1/checkin", "")
	if res.code != 200 {
		t.Fatalf("checkin code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["checkin_applicable"] != true || out["checkin_done"] != true {
		t.Fatalf("the check-in must report itself done: %v", out)
	}

	res = do(c, http.MethodGet, "/panel/api/accounts", "")
	after := accountRow(t, &res)
	if after["checkin_done"] != true {
		t.Fatalf("the listing must carry today's check-in state: %v", after)
	}
	if after["checkin_day"] == nil || after["checkin_day"] == "" {
		t.Fatalf("the check-in day must be reported: %v", after)
	}
}

func TestAccountsMarkASourceWithoutCheckinAsNotApplicable(t *testing.T) {
	c, _ := newMaintConsole(t, &balanceOnly{})

	res := do(c, http.MethodGet, "/panel/api/accounts", "")
	row := accountRow(t, &res)
	if row["checkin_applicable"] != false {
		t.Fatalf("a source without a check-in must not claim one: %v", row)
	}
	if row["checkin_done"] != false {
		t.Fatalf("checkin_done = %v, want false", row["checkin_done"])
	}

	// The operation still runs — the balance refresh is the useful half — but
	// the response must not pretend a check-in happened.
	res = do(c, http.MethodPost, "/panel/api/accounts/u-1/checkin", "")
	if res.code != 200 {
		t.Fatalf("checkin code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["checkin_applicable"] != false || out["checkin_done"] != false {
		t.Fatalf("response = %v", out)
	}
}

// accountRow returns the first account of an accounts listing.
func accountRow(t *testing.T, res *httptestResponse) map[string]any {
	t.Helper()
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, res)
	rows, _ := out["accounts"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no accounts in %v", out)
	}
	row, _ := rows[0].(map[string]any)
	if row == nil {
		t.Fatalf("account row is not an object: %v", rows[0])
	}
	return row
}
