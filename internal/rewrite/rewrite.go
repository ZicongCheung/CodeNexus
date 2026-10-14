// Package rewrite implements the outbound request normalization shared by every
// upstream adapter: forcing streaming, normalizing roles and token fields,
// repairing tool-call pairing, and injecting reasoning hints.
//
// Every function is pure with respect to the caller: Prepare returns a new
// top-level map and never writes back into the input.
package rewrite

import "strings"

// Options tunes the pipeline for a single request.
type Options struct {
	// ForceStream turns the request into a streaming request and asks the
	// upstream to include usage in the final chunk.
	ForceStream bool

	// MinMaxTokens raises a too-small max_tokens for reasoning models. Zero
	// disables the clamp.
	MinMaxTokens int

	// DefaultReasoningEffort is applied when a reasoning model carries no
	// explicit effort. Empty means "do not inject".
	DefaultReasoningEffort string

	// DisableSanitize turns off fingerprint scrubbing. The zero value keeps it
	// enabled, matching the product default.
	DisableSanitize bool
}

// Prepare returns a normalized copy of body.
func Prepare(body map[string]any, opts Options) map[string]any {
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		out[k] = v
	}

	model := str(out["model"])

	if msgs := cloneMessages(out["messages"]); msgs != nil {
		normalizeRoles(msgs)
		out["messages"] = repairToolPairing(msgs)
	}

	normalizeMaxTokens(out, model, opts)

	if isReasoningModel(model) {
		injectThinking(out, opts.DefaultReasoningEffort)
		backfillReasoning(out)
	}

	if opts.ForceStream {
		out["stream"] = true
		if _, ok := out["stream_options"]; !ok {
			out["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	if !opts.DisableSanitize {
		SanitizeBody(out)
	}
	return out
}

// normalizeRoles rewrites the OpenAI "developer" role to "system", which is the
// only form older upstreams accept.
func normalizeRoles(msgs []any) {
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if str(m["role"]) == "developer" {
			m["role"] = "system"
		}
	}
}

// normalizeMaxTokens folds max_completion_tokens into max_tokens and raises a
// too-small value for reasoning models.
func normalizeMaxTokens(body map[string]any, model string, opts Options) {
	if v, ok := body["max_completion_tokens"]; ok {
		if _, exists := body["max_tokens"]; !exists {
			body["max_tokens"] = v
		}
		delete(body, "max_completion_tokens")
	}
	if opts.MinMaxTokens <= 0 || !isReasoningModel(model) {
		return
	}
	if n, ok := toInt(body["max_tokens"]); ok && n < opts.MinMaxTokens {
		body["max_tokens"] = opts.MinMaxTokens
	}
}

// repairToolPairing drops tool messages that do not answer the tool_calls of
// the immediately preceding assistant message. Orphan tool results make strict
// upstreams reject the whole request.
func repairToolPairing(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	var pending map[string]bool // tool_call ids the last assistant message expects
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			pending = nil
			continue
		}
		switch str(m["role"]) {
		case "assistant":
			pending = map[string]bool{}
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, t := range tcs {
					tm, ok := t.(map[string]any)
					if !ok {
						continue
					}
					if id := str(tm["id"]); id != "" {
						pending[id] = true
					}
				}
			}
			out = append(out, m)
		case "tool":
			id := str(m["tool_call_id"])
			if pending == nil || !pending[id] {
				continue // orphan result, drop
			}
			delete(pending, id)
			out = append(out, m)
		default:
			pending = nil
			out = append(out, m)
		}
	}
	return out
}

// injectThinking turns on the reasoning channel for models that expose one.
func injectThinking(body map[string]any, defaultEffort string) {
	if _, ok := body["thinking"]; ok {
		return
	}
	body["thinking"] = map[string]any{"type": "enabled"}
	if _, ok := body["reasoning_effort"]; !ok && defaultEffort != "" {
		body["reasoning_effort"] = defaultEffort
	}
}

// backfillReasoning mirrors reasoning_content and reasoning so that a strict
// upstream sees the field it validates on every assistant turn.
func backfillReasoning(body map[string]any) {
	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || str(m["role"]) != "assistant" {
			continue
		}
		rc, hasRC := m["reasoning_content"].(string)
		r, hasR := m["reasoning"].(string)
		switch {
		case hasRC && !hasR:
			m["reasoning"] = rc
		case hasR && !hasRC:
			m["reasoning_content"] = r
		}
	}
}

// cloneMessages deep-copies the message list (and each message map) so the
// pipeline never mutates the caller's decoded body.
func cloneMessages(v any) []any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		cp := make(map[string]any, len(m))
		for k, val := range m {
			cp[k] = val
		}
		out = append(out, cp)
	}
	return out
}

var reasoningHints = []string{"deepseek", "reasoner", "reasoning", "thinking", "-r1", "qwq"}

func isReasoningModel(model string) bool {
	m := strings.ToLower(model)
	for _, h := range reasoningHints {
		if strings.Contains(m, h) {
			return true
		}
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
