package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/growth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The task center is the console's operational surface for the scheduled
// maintenance: it reads the task catalogue, shows each account's progress, and
// lets the operator run one task, one account or a whole sweep by hand. Every
// write endpoint is idempotent because it delegates to the same idempotent
// tasks the scheduler runs.

// actionView is the JSON shape of one growth executor.
type actionView struct {
	Code    string       `json:"task_code"`
	Desc    string       `json:"desc"`
	Class   growth.Class `json:"class"`
	Attempt bool         `json:"attempt"`
}

// handleTaskCatalog lists the growth executors with their evidence class.
func (c *Console) handleTaskCatalog(w http.ResponseWriter, _ *http.Request) {
	actions := growth.Actions()
	views := make([]actionView, 0, len(actions))
	for _, a := range actions {
		views = append(views, actionView{Code: a.Code, Desc: a.Desc, Class: a.Class, Attempt: a.Attempt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": views, "count": len(views)})
}

// accountRef resolves one account from its uid.
func (c *Console) accountRef(uid string) (*account.Account, string, bool) {
	if c.pool == nil || uid == "" {
		return nil, "", false
	}
	for _, st := range c.pool.Describe() {
		if st.UID == uid {
			acc, ok := c.pool.Get(st.Key)
			if !ok || acc == nil {
				return nil, "", false
			}
			return acc, st.Key, true
		}
	}
	return nil, "", false
}

// tasksFor resolves the task adapter for one account.
func (c *Console) tasksFor(acc *account.Account) (upstream.Tasks, bool) {
	if c.sched == nil {
		return nil, false
	}
	return c.sched.Adapter(acc)
}

// handleAccountTasks merges the default and mini-program task listings.
func (c *Console) handleAccountTasks(w http.ResponseWriter, r *http.Request) {
	acc, key, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	tasks, ok := c.tasksFor(acc)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "task subsystem not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	merged := map[string]upstream.Task{}
	for _, listing := range []bool{false, true} {
		list, err := tasks.ListTasks(ctx, acc, listing)
		if err != nil {
			if listing {
				continue // the mini-program listing is optional
			}
			writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
			return
		}
		for _, t := range list {
			if _, seen := merged[t.Code]; seen {
				continue
			}
			t.MiniProgram = growth.IsMiniProgram(t.Code)
			merged[t.Code] = t
		}
	}
	codes := make([]string, 0, len(merged))
	for code := range merged {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	out := make([]upstream.Task, 0, len(codes))
	for _, code := range codes {
		out = append(out, merged[code])
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": key, "tasks": out})
}

// handleTaskAccept enrolls one task or every pending task.
func (c *Console) handleTaskAccept(w http.ResponseWriter, r *http.Request) {
	acc, _, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	tasks, ok := c.tasksFor(acc)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "task subsystem not available")
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
		All      bool   `json:"all"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if body.All {
		def, mini, err := c.acceptAll(ctx, acc, tasks)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "accepted": def + mini, "accepted_default": def, "accepted_mini": mini,
		})
		return
	}
	if strings.TrimSpace(body.TaskCode) == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	mini := growth.IsMiniProgram(body.TaskCode)
	if err := tasks.AcceptTasks(ctx, acc, []string{body.TaskCode}, mini); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task_code": body.TaskCode})
}

// acceptAll enrolls every pending task in both listings.
func (c *Console) acceptAll(ctx context.Context, acc *account.Account, tasks upstream.Tasks) (def, mini int, err error) {
	for _, isMini := range []bool{false, true} {
		list, lerr := tasks.ListTasks(ctx, acc, isMini)
		if lerr != nil {
			if isMini {
				continue
			}
			return 0, 0, lerr
		}
		var codes []string
		for _, t := range list {
			if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			codes = append(codes, t.Code)
		}
		if len(codes) == 0 {
			continue
		}
		if aerr := tasks.AcceptTasks(ctx, acc, codes, isMini); aerr != nil {
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

// handleTaskClaim collects one task's reward.
func (c *Console) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	acc, _, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	tasks, ok := c.tasksFor(acc)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "task subsystem not available")
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.TaskCode) == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	credit, energy, err := tasks.ClaimReward(ctx, acc, body.TaskCode, growth.IsMiniProgram(body.TaskCode))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}

// handleTaskAuto runs one growth executor for one account.
func (c *Console) handleTaskAuto(w http.ResponseWriter, r *http.Request) {
	acc, _, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := decodeJSON(r, &body); err != nil || strings.TrimSpace(body.TaskCode) == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	if c.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}
	if !c.tryLock(acc.Key()) {
		writeErr(w, http.StatusConflict, "this account already has a task running")
		return
	}
	defer c.unlock(acc.Key())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, found := c.sched.RunGrowthAction(ctx, acc, body.TaskCode)
	if !found {
		writeErr(w, http.StatusNotFound, "unknown task code")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleTaskAutoAll runs every growth executor for one account.
func (c *Console) handleTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	acc, _, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	if c.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}
	if !c.tryLock(acc.Key()) {
		writeErr(w, http.StatusConflict, "this account already has a task running")
		return
	}
	defer c.unlock(acc.Key())

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	results := c.sched.RunGrowthFor(ctx, acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// handleBatch dispatches the whole-pool sweeps.
func (c *Console) handleBatch(w http.ResponseWriter, r *http.Request) {
	if c.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}
	var run func(context.Context)
	switch r.PathValue("op") {
	case "checkin_all":
		run = c.sched.RunCheckinNow
	case "travel_all":
		run = c.sched.RunTravelNow
	case "activity_all":
		run = c.sched.RunActivityNow
	case "keepalive_all":
		run = c.sched.RunKeepaliveNow
	case "balance_all":
		run = c.sched.RunBalanceNow
	default:
		writeErr(w, http.StatusNotFound, "unknown batch operation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	run(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountAction handles the single-account operations that map onto a
// scheduler sweep restricted to one account.
func (c *Console) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	acc, _, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	if c.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}
	if !c.tryLock(acc.Key()) {
		writeErr(w, http.StatusConflict, "this account already has a task running")
		return
	}
	defer c.unlock(acc.Key())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	switch r.PathValue("op") {
	case "checkin":
		c.sched.RunCheckinFor(ctx, acc)
	case "activity":
		c.sched.RunActivityFor(ctx, acc)
	case "balance":
		c.sched.RunBalanceFor(ctx, acc)
	case "travel":
		c.sched.RunTravelFor(ctx, acc)
	case "keepalive":
		if c.sched.RunKeepaliveFor(ctx, acc) {
			writeErr(w, http.StatusServiceUnavailable, "token refresh not available")
			return
		}
	default:
		writeErr(w, http.StatusNotFound, "unknown account operation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountPackages reports the detailed credit picture of one account.
func (c *Console) handleAccountPackages(w http.ResponseWriter, r *http.Request) {
	acc, key, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	tasks, ok := c.tasksFor(acc)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "task subsystem not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	window := time.Duration(c.cfg.Schedule.ExpiringSoonHours) * time.Hour
	credits, err := tasks.Credits(ctx, acc, window)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if c.pool != nil {
		c.pool.SetCreditsDetail(key, pool.Credits{
			Remain:            credits.Remain,
			Total:             credits.Total,
			Known:             true,
			Expiring:          credits.Expiring,
			EarliestAt:        credits.EarliestAt,
			EarliestRemaining: credits.EarliestRemaining,
		})
	}
	writeJSON(w, http.StatusOK, credits)
}

// handleAccountState toggles the administrative state of one account.
func (c *Console) handleAccountState(w http.ResponseWriter, r *http.Request) {
	acc, key, ok := c.accountRef(r.PathValue("uid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	if c.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "pool not available")
		return
	}
	switch r.PathValue("op") {
	case "pause":
		c.pool.SetPaused(key, true)
	case "resume":
		c.pool.SetPaused(key, false)
	case "disable":
		c.pool.SetDisabled(key, true)
		c.persistState(acc, false)
	case "revive":
		c.pool.SetDisabled(key, false)
		c.persistState(acc, true)
	default:
		writeErr(w, http.StatusNotFound, "unknown account state operation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// persistState writes the enabled flag back to the credential store so a
// restart does not undo an administrative change.
func (c *Console) persistState(acc *account.Account, enabled bool) {
	if c.store == nil || acc.Ephemeral {
		return
	}
	snap := acc.Snapshot()
	snap.Enabled = enabled
	if err := c.store.Save(&snap); err != nil {
		c.warn("panel: persisting account state: %v", err)
	}
}

// handleScanAll lists the tasks of every account, for the console's overview.
func (c *Console) handleScanAll(w http.ResponseWriter, r *http.Request) {
	if c.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "pool not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type accountTasks struct {
		UID    string          `json:"uid"`
		Label  string          `json:"label,omitempty"`
		Tasks  []upstream.Task `json:"tasks"`
		Failed string          `json:"error,omitempty"`
	}
	out := []accountTasks{}
	for _, st := range c.pool.Describe() {
		acc, ok := c.pool.Get(st.Key)
		if !ok || acc == nil {
			continue
		}
		entry := accountTasks{UID: st.UID, Label: st.Label}
		tasks, ok := c.tasksFor(acc)
		if !ok {
			entry.Failed = "no task adapter"
			out = append(out, entry)
			continue
		}
		list, err := tasks.ListTasks(ctx, acc, false)
		if err != nil {
			entry.Failed = err.Error()
		} else {
			entry.Tasks = list
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// handleQueue reports which accounts still have actionable tasks.
func (c *Console) handleQueue(w http.ResponseWriter, r *http.Request) {
	if c.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "pool not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type queueItem struct {
		UID       string `json:"uid"`
		Label     string `json:"label,omitempty"`
		Pending   int    `json:"pending"`
		Claimable int    `json:"claimable"`
	}
	out := []queueItem{}
	for _, st := range c.pool.Describe() {
		acc, ok := c.pool.Get(st.Key)
		if !ok || acc == nil {
			continue
		}
		tasks, ok := c.tasksFor(acc)
		if !ok {
			continue
		}
		list, err := tasks.ListTasks(ctx, acc, false)
		if err != nil {
			continue
		}
		item := queueItem{UID: st.UID, Label: st.Label}
		for _, t := range list {
			if t.Claimed {
				continue
			}
			item.Pending++
			if t.Claimable {
				item.Claimable++
			}
		}
		if item.Pending > 0 {
			out = append(out, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": out})
}

// handleRunQueue runs the growth executors for every enabled account.
func (c *Console) handleRunQueue(w http.ResponseWriter, r *http.Request) {
	if c.sched == nil {
		writeErr(w, http.StatusServiceUnavailable, "scheduler not available")
		return
	}
	if c.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "pool not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	type result struct {
		UID     string          `json:"uid"`
		Results []growth.Result `json:"results"`
	}
	out := []result{}
	for _, st := range c.pool.Describe() {
		if !st.Enabled {
			continue
		}
		acc, ok := c.pool.Get(st.Key)
		if !ok || acc == nil {
			continue
		}
		out = append(out, result{UID: st.UID, Results: c.sched.RunGrowthFor(ctx, acc)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// handleMetrics reports the request log snapshot.
func (c *Console) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if c.reqlog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, c.reqlog.Snapshot())
}

// handleLogs reports the recent request log entries.
func (c *Console) handleLogs(w http.ResponseWriter, _ *http.Request) {
	if c.reqlog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "recent": []any{}})
		return
	}
	snap := c.reqlog.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "recent": snap.Recent})
}
