package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// modelRow is one model as one source can offer it.
//
// The catalogue is read per account, but the page answers a different question:
// "what can I ask for, at what price". Accounts of the same source collapse
// into one row with a count; the two sources never do, because the same model
// name is often carried by both and their windows, prices and promotions
// differ. Merging them would print one platform's price under the other's name.
type modelRow struct {
	ID string `json:"id"`
	// Source is the single upstream this row belongs to. The console groups by
	// it, so one row can never stand for two platforms.
	Source string `json:"source"`
	// Accounts is how many accounts of this source carry this model, and
	// AccountKeys names them so the connectivity test can aim at a specific one
	// instead of re-reading every catalogue to find a match.
	Accounts    int      `json:"accounts"`
	AccountKeys []string `json:"account_keys"`
	Context     int      `json:"context_length,omitempty"`
	MaxOutput   int      `json:"max_output_tokens,omitempty"`
	Efforts     []string `json:"thinking_efforts,omitempty"`
	Caps        []string `json:"capabilities,omitempty"`
	// Credits is the listed multiplier; PromoCredits the discount in force,
	// with PromoLabel naming it and PromoNote explaining when it applies.
	Credits      string `json:"credits,omitempty"`
	PromoCredits string `json:"promo_credits,omitempty"`
	PromoLabel   string `json:"promo_label,omitempty"`
	PromoNote    string `json:"promo_note,omitempty"`
	// PromoFactor is the discount fraction (0 = free); nil when the upstream
	// only states a label, which is how off-peak offers arrive.
	PromoFactor *float64 `json:"promo_factor,omitempty"`
	Default      bool   `json:"default,omitempty"`
}

// merge folds one account's view of a model into the row. Windows, output caps
// and capability bits widen: if any account of this source can serve 1M or take
// tools, the model can, because routing may pick that account.
func (row *modelRow) merge(key string, m upstream.ModelSpec) {
	row.Accounts++
	row.AccountKeys = append(row.AccountKeys, key)
	if m.ContextLength > row.Context {
		row.Context = m.ContextLength
	}
	if m.MaxOutput > row.MaxOutput {
		row.MaxOutput = m.MaxOutput
	}
	row.Efforts = unionStrings(row.Efforts, m.ThinkingEfforts)
	row.Caps = unionStrings(row.Caps, m.Capabilities)
	if row.Credits == "" {
		row.Credits = m.Credits
	}
	if row.PromoCredits == "" && m.PromoCredits != "" {
		row.PromoCredits = m.PromoCredits
	}
	if row.PromoLabel == "" && m.PromoLabel != "" {
		row.PromoLabel = m.PromoLabel
	}
	if row.PromoNote == "" && m.PromoNote != "" {
		row.PromoNote = m.PromoNote
	}
	if row.PromoFactor == nil && m.PromoFactor != nil {
		f := *m.PromoFactor
		row.PromoFactor = &f
	}
	if m.Default {
		row.Default = true
	}
}

// sourceRank orders the platform groups on the model page. The gateway's own
// source leads; anything else follows alphabetically so a new adapter still
// lands somewhere predictable.
func sourceRank(source string) int {
	if source == string(upstream.SourceWorkBuddy) {
		return 0
	}
	return 1
}

// handleModels aggregates every account's catalogue into one row per model and
// source.
//
// Reading per account is safe because each adapter answers from its own cached
// catalogue: the expensive fetch happens on a timer, not on this request.
func (c *Console) handleModels(w http.ResponseWriter, r *http.Request) {
	statuses := c.pool.Describe()
	failed := map[string]string{}
	catalogued := map[string]int{}

	var mu sync.Mutex
	// The key carries the source so the same name from two platforms stays two
	// rows. Merging by name alone used to hide the fact that both sources carry
	// it, and printed whichever catalogue answered last.
	type modelKey struct{ source, id string }
	byID := map[modelKey]*modelRow{}

	var wg sync.WaitGroup
	sem := make(chan struct{}, creditProbeLimit)
	for _, st := range statuses {
		if st.Local {
			continue
		}
		wg.Add(1)
		go func(st pool.AccountStatus) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			acc, ok := c.pool.Get(st.Key)
			if !ok || acc == nil {
				mu.Lock()
				failed[st.UID] = "account left the pool"
				mu.Unlock()
				return
			}
			u, ok := upstream.Get(st.Source)
			if !ok {
				mu.Lock()
				failed[st.UID] = "source not registered: " + string(st.Source)
				mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), creditProbeTimeout)
			defer cancel()
			models, err := u.ListModels(ctx, acc)
			if err != nil {
				mu.Lock()
				failed[st.UID] = err.Error()
				mu.Unlock()
				return
			}

			mu.Lock()
			defer mu.Unlock()
			catalogued[string(st.Source)]++
			for _, m := range models {
				if m.ID == "" {
					continue
				}
				key := modelKey{source: string(st.Source), id: m.ID}
				row, ok := byID[key]
				if !ok {
					row = &modelRow{ID: m.ID, Source: key.source}
					byID[key] = row
				}
				row.merge(st.Key, m)
			}
		}(st)
	}
	wg.Wait()

	rows := make([]modelRow, 0, len(byID))
	for _, row := range byID {
		sort.SliceStable(row.AccountKeys, func(a, b int) bool {
			return row.AccountKeys[a] < row.AccountKeys[b]
		})
		rows = append(rows, *row)
	}
	// Source first so a group's rows arrive together and the console can split
	// them without re-sorting; the rank puts the first-party source on top
	// instead of letting an alphabetical accident decide the page order.
	sort.SliceStable(rows, func(a, b int) bool {
		if ra, rb := sourceRank(rows[a].Source), sourceRank(rows[b].Source); ra != rb {
			return ra < rb
		}
		if rows[a].Source != rows[b].Source {
			return rows[a].Source < rows[b].Source
		}
		if rows[a].Accounts != rows[b].Accounts {
			return rows[a].Accounts > rows[b].Accounts
		}
		return rows[a].ID < rows[b].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"models":   rows,
		"failed":   failed,
		"accounts": catalogued,
	})
}

// probeRequest is one connectivity test as the console sends it.
type probeRequest struct {
	Model    string `json:"model"`
	Mode     string `json:"mode"`
	Effort   string `json:"effort"`
	Thinking bool   `json:"thinking"`
	Timeout  int    `json:"timeout"`
	// Account pins the test to one account. Empty lets the handler pick.
	Account string `json:"account"`
}

// probeToolCall is one invocation the probe model made. The name comes first:
// a bare count cannot tell the expected relay_probe from a hallucinated tool.
type probeToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// probeResult reports what happened, in the shape the console renders.
//
// It carries the raw material of the test, not just its verdict: the detail
// drawer shows the request beside the full reply, so an operator can read what
// the model actually said without spending a second request to find out.
type probeResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	Source    string `json:"source,omitempty"`
	Account   string `json:"account,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	// Prompt is the exact text the probe sent. The drawer pairs it with the
	// reply so "what was asked" is never reconstructed from memory.
	Prompt string `json:"prompt,omitempty"`
	// Content is the whole reply, untruncated. The one-line summary the result
	// row needs is derived from it in the console.
	Content string `json:"content,omitempty"`
	// Reasoning is the thinking the model returned, when thinking was asked
	// for. It is the only way to tell "thought quietly" from "never thought".
	Reasoning string `json:"reasoning,omitempty"`
	// ToolCalls lists the invocations the model made in tool mode.
	ToolCalls []probeToolCall `json:"tool_calls,omitempty"`
	// FinishReason is why the model stopped, straight from the upstream.
	FinishReason string `json:"finish_reason,omitempty"`
	TotalToken   int64  `json:"total_tokens,omitempty"`
	Credits      float64 `json:"credits,omitempty"`
}

// probeTool is the function the tool-mode probe asks the model to call. It has
// no side effects: the upstream never executes it, the gateway only checks that
// the model emitted a call at all.
func probeTool() []any {
	return []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "relay_probe",
			"description": "Echo a probe token back to the relay",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"token": map[string]any{"type": "string"}},
				"required":   []string{"token"},
			},
		},
	}}
}

// handleModelTest probes one model through the real adapter path.
//
// It sends an actual completion rather than checking a configuration table: the
// question this answers is "does this model work on this account right now",
// and only a real request can answer that. The prompt asks for a single word and
// the token cap is small, so the cost of asking is negligible — but it is not
// zero, which is why the console states it before the button.
func (c *Console) handleModelTest(w http.ResponseWriter, r *http.Request) {
	var body probeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	if body.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model is required"})
		return
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = "text"
	}
	if mode != "text" && mode != "tool" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode must be text or tool"})
		return
	}
	timeout := time.Duration(clampInt(body.Timeout, 10, 600)) * time.Second
	// The console's own default is 30s: a connectivity check that outlasts the
	// operator's patience has already answered "not usable". Callers that send
	// no timeout at all get the same bound instead of a stale 120s.
	if body.Timeout <= 0 {
		timeout = 30 * time.Second
	}

	acc, source, err := c.probeAccount(body.Account, body.Model)
	if err != nil {
		writeJSON(w, http.StatusOK, probeResult{
			Model: body.Model, Mode: mode, Error: err.Error(),
		})
		return
	}
	u, ok := upstream.Get(source)
	if !ok {
		writeJSON(w, http.StatusOK, probeResult{
			Model: body.Model, Mode: mode, Error: "source not registered: " + string(source),
		})
		return
	}

	// Thinking needs room: a reasoning model spends most of a 64-token budget
	// before it says anything, and a truncated answer would read as "broken".
	maxTokens := 64
	if body.Effort != "" || body.Thinking {
		maxTokens = 4096
	}
	raw := map[string]any{}
	if body.Effort != "" {
		raw["reasoning_effort"] = body.Effort
	}
	if body.Thinking {
		raw["thinking"] = map[string]any{"type": "enabled"}
	}

	req := &upstream.ChatRequest{
		Model:     body.Model,
		Messages:  []upstream.Message{{Role: "user", Content: probePrompt(mode)}},
		MaxTokens: &maxTokens,
		Raw:       raw,
	}
	if mode == "tool" {
		req.Tools = probeTool()
		raw["tool_choice"] = "auto"
		raw["parallel_tool_calls"] = false
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	started := time.Now()
	resp, err := u.Chat(ctx, acc, req)
	elapsed := time.Since(started).Milliseconds()

	out := probeResult{
		Model:     body.Model,
		Mode:      mode,
		Source:    string(source),
		Account:   acc.UID,
		ElapsedMS: elapsed,
	}
	if err != nil {
		out.Error = err.Error()
		if ue, ok := err.(*upstream.Error); ok {
			out.Status = ue.Status
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if ctx.Err() == context.DeadlineExceeded {
		// The bound is part of the message: "timed out" alone cannot tell a
		// 15s preset from a 600s one, and the console renders it verbatim.
		out.Error = fmt.Sprintf("timed out after %ds", int(timeout.Seconds()))
		writeJSON(w, http.StatusOK, out)
		return
	}

	out.OK = true
	out.Prompt = probePrompt(mode)
	out.Content = resp.Content
	out.Reasoning = resp.ReasoningContent
	out.ToolCalls = probeToolCalls(resp.ToolCalls)
	out.FinishReason = resp.FinishReason
	out.TotalToken = int64(resp.Usage.TotalTokens)
	out.Credits = resp.CreditsConsumed
	if mode == "tool" && len(out.ToolCalls) == 0 {
		// The request succeeded but the model ignored the tool, which is the
		// difference between "reachable" and "usable": it must not read as a
		// pass just because the HTTP status was fine.
		out.OK = false
		out.Error = "model answered without calling the tool"
	}
	writeJSON(w, http.StatusOK, out)
}

// probeToolCalls normalizes what the model invoked for the console. Arguments
// are kept whole: the drawer shows them, and a truncated JSON string reads as a
// broken one.
func probeToolCalls(calls []upstream.ToolCall) []probeToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]probeToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, probeToolCall{Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return out
}

// probeAccount resolves the account to test against.
//
// A pinned account wins — the console learned which accounts carry the model
// from the same request that drew the table, so it normally pins. Without a pin
// the first account whose own catalogue lists the model is taken, because the
// pool mixes sources: picking the first usable entry blindly would test the
// model against an upstream that has never heard of it and report the model as
// broken.
func (c *Console) probeAccount(key, model string) (*account.Account, upstream.Source, error) {
	if key != "" {
		if acc, ok := c.pool.Get(key); ok && acc != nil {
			return acc, acc.Source, nil
		}
		return nil, "", errors.New("account not in pool: " + key)
	}

	var fallback *account.Account
	var fallbackSource upstream.Source
	for _, st := range c.pool.Describe() {
		if st.Local || !st.Enabled {
			continue
		}
		acc, ok := c.pool.Get(st.Key)
		if !ok || acc == nil {
			continue
		}
		if fallback == nil {
			fallback, fallbackSource = acc, st.Source
		}
		u, ok := upstream.Get(st.Source)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		models, err := u.ListModels(ctx, acc)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range models {
			if m.ID == model {
				return acc, st.Source, nil
			}
		}
	}
	if fallback != nil {
		return fallback, fallbackSource, nil
	}
	return nil, "", errors.New("no usable account")
}

func probePrompt(mode string) string {
	if mode == "tool" {
		return `Call relay_probe with token="pong". Tool call only, no explanation.`
	}
	return "Reply with exactly: pong"
}

// unionStrings merges two string slices keeping first-seen order.
func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := a
	for _, v := range b {
		if !containsString(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
