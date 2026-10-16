package traecn

import (
	"encoding/json"
	"strconv"
	"strings"
)

// The upstream expresses thinking strength as a native level on the model
// entry, not as a token budget: the client sends one of light / high /
// extra_high and the model decides what that means. Clients speak a different
// vocabulary (OpenAI's reasoning_effort, Anthropic's thinking budget), so the
// mapping lives here.
//
// The level is only ever written when the account's own catalogue advertises
// the level: a model that does not declare `reasoning_effort_config` cannot
// accept one, and sending it anyway would be rejected or ignored.
const (
	effortLight = "light"
	effortHigh  = "high"
	effortExtra = "extra_high"
)

// effortOrder is weakest to strongest; clamping walks it in this order.
var effortOrder = []string{effortLight, effortHigh, effortExtra}

// effortAliases folds every spelling a client may use onto a native level.
var effortAliases = map[string]string{
	"none": effortLight, "minimal": effortLight, "min": effortLight,
	"low": effortLight, "light": effortLight,
	"medium": effortHigh, "mid": effortHigh, "normal": effortHigh,
	"default": effortHigh, "high": effortHigh,
	"xhigh": effortExtra, "x-high": effortExtra, "extra_high": effortExtra,
	"extra-high": effortExtra, "very_high": effortExtra,
	"max": effortExtra, "maximum": effortExtra,
}

// normalizeEffort maps a client spelling onto a native level, or "".
func normalizeEffort(v any) string {
	text, _ := v.(string)
	text = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), " ", "_")
	return effortAliases[text]
}

// effortFromBudget maps an Anthropic-style thinking budget onto a level.
func effortFromBudget(v any) string {
	tokens, ok := asInt(v)
	if !ok || tokens <= 0 {
		return ""
	}
	switch {
	case tokens < 4096:
		return effortLight
	case tokens < 16384:
		return effortHigh
	default:
		return effortExtra
	}
}

// requestedEffort reads the strength a client asked for, from any of the three
// JSON dialects the ingress surfaces accept.
func requestedEffort(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "thinking_effort"} {
		if level := normalizeEffort(raw[key]); level != "" {
			return level
		}
	}
	if reasoning, ok := raw["reasoning"].(map[string]any); ok {
		if level := normalizeEffort(reasoning["effort"]); level != "" {
			return level
		}
	}
	if thinking, ok := raw["thinking"].(map[string]any); ok {
		if level := normalizeEffort(thinking["effort"]); level != "" {
			return level
		}
		if level := normalizeEffort(thinking["level"]); level != "" {
			return level
		}
		if level := effortFromBudget(firstOf(thinking, "budget_tokens", "budgetTokens")); level != "" {
			return level
		}
	}
	return ""
}

// supportedEfforts lists the levels a model entry advertises, weakest first.
// An entry without a thinking-capable config supports nothing.
func supportedEfforts(entry map[string]any) []string {
	if entry == nil {
		return nil
	}
	cfg, _ := entry["reasoning_effort_config"].(map[string]any)
	if cfg == nil || !truthy(cfg["support_thinking"]) {
		return nil
	}
	options, _ := cfg["options"].([]any)
	present := make(map[string]bool, len(options))
	for _, item := range options {
		if level := normalizeEffort(item); level != "" {
			present[level] = true
		}
	}
	var out []string
	for _, level := range effortOrder {
		if present[level] {
			out = append(out, level)
		}
	}
	return out
}

// clampEffort picks the strongest supported level that does not exceed the
// requested one, falling back to the weakest supported level.
func clampEffort(level string, supported []string) string {
	if level == "" || len(supported) == 0 {
		return ""
	}
	rank := indexOfEffort(level)
	best := ""
	for _, candidate := range supported {
		if indexOfEffort(candidate) <= rank {
			best = candidate
		}
	}
	if best == "" {
		return supported[0]
	}
	return best
}

func indexOfEffort(level string) int {
	for i, candidate := range effortOrder {
		if candidate == level {
			return i
		}
	}
	return -1
}

// applyEffort writes a clamped thinking level onto a copy of the model entry.
//
// The upstream reads the level from `reasoning_effort_level` when the entry
// already declares that key and from `reasoning_effort` otherwise; sending
// both would leave the effective value up to the server.
func applyEffort(entry map[string]any, raw map[string]any) (map[string]any, string) {
	if entry == nil {
		return nil, ""
	}
	level := clampEffort(requestedEffort(raw), supportedEfforts(entry))
	if level == "" {
		return entry, ""
	}
	out := make(map[string]any, len(entry)+2)
	for k, v := range entry {
		out[k] = v
	}
	if _, hasLevelKey := out["reasoning_effort_level"]; hasLevelKey {
		out["reasoning_effort_level"] = level
		delete(out, "reasoning_effort")
	} else {
		out["reasoning_effort"] = level
	}
	return out, level
}

// asInt accepts the numeric spellings a JSON decoder and a hand-written config
// both produce.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

// firstOf returns the first value present under any of the given keys.
func firstOf(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			return v
		}
	}
	return nil
}
