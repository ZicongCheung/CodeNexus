package traecn

import (
	"context"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The account's largest context profile is an entitlement, not a parameter the
// client may simply request. The desktop client only offers it for models the
// server marks `max_mode`, and the session body then has to carry the whole
// profile (window sizes, prompt/output ceilings, turn budget) rather than a
// single "1M" flag. This file reproduces that shape.
//
// Two rules keep it honest:
//   - nothing is sent unless the account's catalogue marks the model
//     max-capable, so the relay never fabricates an entitlement;
//   - nothing is sent for the work tier, which has no max profile.
const (
	maxModeContextTokens = 1_000_000
	maxModePromptTokens  = 936_000
	maxModeOutputTokens  = 64_000
	maxModeDefaultTurn   = 500
	maxModeDevContext    = 200_000
	// maxModeType is the wire discriminator the upstream expects alongside the
	// max strategy.
	maxModeType = 1
)

// MaxModeConfig mirrors the configuration for the max profile.
type MaxModeConfig struct {
	Enabled bool
	// Models is an allow-list of client-facing model ids. Empty means every
	// model the catalogue marks as max-capable.
	Models []string
}

// maxModeApplies decides whether this request may use the max profile.
func maxModeApplies(cfg MaxModeConfig, agentType, modelName string, entry map[string]any) bool {
	if !cfg.Enabled || agentType != remoteAgent {
		return false
	}
	if !maxCapable(entry) {
		return false
	}
	if len(cfg.Models) == 0 {
		return true
	}
	for _, allowed := range cfg.Models {
		if allowed == "*" || allowed == modelName {
			return true
		}
	}
	return false
}

// enrichMaxModel mirrors the desktop client's max-enriched model entry.
//
// The model-list endpoint returns a slim object: it says the entry is
// max-capable and gives the window sizes, but omits the fields the upstream
// validates a max session against. They are restored from the entry when it
// has them, and from the documented defaults otherwise, so the server sees the
// same profile the local client would have sent.
func enrichMaxModel(entry map[string]any) map[string]any {
	cws, _ := entry["context_window_size"].(map[string]any)
	maxContext := intFromAny(firstListValue(cws["max"]), maxModeContextTokens)
	if maxContext == maxModeContextTokens {
		if v := nested(entry, "features", "context_windows", "data", "max_context"); v != nil {
			maxContext = intFromAny(v, maxModeContextTokens)
		} else if v = nested(entry, "context_window_tokens", "max"); v != nil {
			maxContext = intFromAny(v, maxModeContextTokens)
		}
	}
	devContext := intFromAny(firstNonNil(cws["default"], nested(entry, "features", "context_windows", "data", "dev_context"),
		nested(entry, "context_window_tokens", "dev")), maxModeDevContext)
	promptMax := intFromAny(entry["prompt_max_tokens"], maxModePromptTokens)
	outputMax := intFromAny(entry["max_tokens"], maxModeOutputTokens)
	maxTurns := intFromAny(firstNonNil(entry["max_turn"], nested(entry, "max_turns", "default"),
		nested(entry, "features", "context_windows", "data", "max_turns")), maxModeDefaultTurn)

	out := make(map[string]any, len(entry)+8)
	for k, v := range entry {
		out[k] = v
	}
	out["max_mode"] = true
	out["context_window_size"] = map[string]any{"default": devContext, "max": []any{maxContext}}
	out["context_window_tokens"] = map[string]any{"dev": devContext, "max": maxContext}
	out["prompt_max_tokens"] = promptMax
	out["max_tokens"] = outputMax
	out["max_turn"] = maxTurns
	out["max_turns"] = map[string]any{"default": maxTurns, "max": maxTurns}

	features := copyMap(entry["features"])
	cw := copyMap(features["context_windows"])
	cw["enable"] = true
	cw["data"] = map[string]any{
		"dev_context":      devContext,
		"max_context":      maxContext,
		"max_context_list": []any{maxContext},
		"dev_turns":        maxTurns,
		"max_turns":        maxTurns,
	}
	features["context_windows"] = cw
	out["features"] = features
	return out
}

// maxModeWireFields returns the fields that pin a remote session to the max
// profile. They travel next to the enriched model entry.
func maxModeWireFields(entry map[string]any) map[string]any {
	cws, _ := entry["context_window_size"].(map[string]any)
	maxContext := intFromAny(firstListValue(cws["max"]), maxModeContextTokens)
	promptMax := intFromAny(entry["prompt_max_tokens"], maxModePromptTokens)
	outputMax := intFromAny(entry["max_tokens"], maxModeOutputTokens)
	return map[string]any{
		"model_auto_selection": map[string]any{
			"strategy":                  "max",
			"fallback_to_advance_model": nil,
			"entitlement_id":            nil,
		},
		"model_selection_strategy": "max",
		"mode_type":                maxModeType,
		"context_window_size":      maxContext,
		"prompt_max_tokens":        promptMax,
		"max_tokens":               outputMax,
	}
}

// ── small helpers ───────────────────────────────────────────────────────────

// nested walks a chain of map keys, returning nil when any step is missing.
func nested(m map[string]any, keys ...string) any {
	var cur any = m
	for _, key := range keys {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = asMap[key]
	}
	return cur
}

// firstListValue returns the first element of a list, or the value itself when
// it is not a list. The upstream spells a single window as either an int or a
// one-element list depending on the endpoint.
func firstListValue(v any) any {
	if list, ok := v.([]any); ok {
		if len(list) == 0 {
			return nil
		}
		return list[0]
	}
	return v
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func intFromAny(v any, fallback int) int {
	if n, ok := asInt(v); ok && n > 0 {
		return n
	}
	return fallback
}

// copyMap returns a shallow copy of m, or an empty map when m is not one.
func copyMap(v any) map[string]any {
	out := map[string]any{}
	if src, ok := v.(map[string]any); ok {
		for k, val := range src {
			out[k] = val
		}
	}
	return out
}

// applyModelProfile consults the account's own model catalogue and, only when
// the account really advertises them, pins the session to the max profile
// and/or a thinking level.
//
// A catalogue that cannot be reached, or that does not list the model, leaves
// the request exactly as it was. Sending a profile the account does not own
// would either be refused upstream or silently mis-bill, and this gateway
// never invents a capability it cannot verify.
func (c *Client) applyModelProfile(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, agentType, modelName string, initial map[string]any) {
	entry, ok := c.modelEntry(ctx, acc, agentType, modelName)
	if !ok {
		return
	}
	// The work tier's entries omit the thinking config; the same model's agent
	// tier advertises it, so borrow that entry when reading the strength.
	effortSource := entry
	if len(supportedEfforts(effortSource)) == 0 && agentType != remoteAgent {
		if agentEntry, found := c.modelEntry(ctx, acc, remoteAgent, modelName); found {
			effortSource = withEffortConfig(entry, agentEntry)
		}
	}
	model, level := applyEffort(effortSource, req.Raw)

	if maxModeApplies(c.cfg.MaxMode, agentType, modelName, entry) {
		model = enrichMaxModel(model)
		initial["custom_model"] = model
		applyManualCompanions(initial, entry)
		fields := maxModeWireFields(model)
		for key, value := range fields {
			initial[key] = value
		}
		// The profile otherwise leaves no trace in the request log, and "did
		// switching the setting on change anything?" is not answerable from
		// the outside. The window is the account's own, so printing it is also
		// how a smaller-than-expected ceiling becomes visible.
		logx.Infof("trae: max profile model=%s context=%v prompt_max=%v effort=%s",
			modelName, fields["context_window_size"], fields["prompt_max_tokens"], level)
		return
	}
	if level == "" {
		return
	}
	initial["custom_model"] = model
	applyManualCompanions(initial, entry)
	logx.Debugf("trae: reasoning effort model=%s level=%s", modelName, level)
}

// applyManualCompanions adds the sibling fields the upstream reads alongside a
// manually-bound model entry. They are only written when the entry actually
// travels, so a request without a catalogue hit is byte-identical to before.
func applyManualCompanions(initial, entry map[string]any) {
	initial["model_config_source"] = intFromAny(entry["config_source"], 1)
	initial["model_is_preset"] = boolFromAny(entry["is_preset"], true)
	initial["model_provider"] = strFromAny(entry["provider"])
}

// withEffortConfig copies the thinking config from the agent-tier entry onto
// the work-tier entry so a strength can be clamped against the levels the
// model actually advertises.
func withEffortConfig(entry, agentEntry map[string]any) map[string]any {
	cfg, ok := agentEntry["reasoning_effort_config"]
	if !ok {
		return entry
	}
	out := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		out[k] = v
	}
	out["reasoning_effort_config"] = cfg
	return out
}

func boolFromAny(v any, fallback bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	case float64:
		return t != 0
	}
	return fallback
}

func strFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
