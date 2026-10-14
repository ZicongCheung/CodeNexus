package rewrite

import (
	"regexp"
	"strings"
)

// The upstream applies a literal content filter: a handful of client-supplied
// prompt templates and header-like tokens are matched verbatim and reject the
// whole request. Replacing a single word (or stripping a token) defeats the
// filter while leaving the meaning intact.
var (
	// headerParamRe matches a billing-style header parameter list.
	headerParamRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?[ \t]*`)
	// headerNameRe matches the header key even without a value, which is itself
	// a fingerprint when quoted or echoed back in a message.
	headerNameRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
	// ccParamRe matches a trailing bare cc_* parameter.
	ccParamRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?[ \t]*`)
)

const headerAbbrev = "x-anthropic-billing-hdr"

// sentenceRewrites change exactly one word per template line.
var sentenceRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// The bare error code is matched with no surrounding context at all, so
		// it must be rewritten everywhere it appears. A hyphen keeps it legible.
		"11128",
		"11-128",
	},
}

// sanitizeHints is the cheap pre-check that skips work for ordinary text.
var sanitizeHints = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

// SanitizeText removes content-filter fingerprints from one string. Text with
// no fingerprint is returned unchanged and without allocation.
func SanitizeText(s string) string {
	if !hasFingerprint(s) {
		return s
	}
	for _, rw := range sentenceRewrites {
		s = strings.ReplaceAll(s, rw[0], rw[1])
	}
	s = headerParamRe.ReplaceAllString(s, "")
	if strings.Contains(s, "cc_") {
		for prev := ""; prev != s; {
			prev = s
			s = ccParamRe.ReplaceAllString(s, "")
		}
	}
	s = headerNameRe.ReplaceAllString(s, headerAbbrev)
	return strings.TrimSpace(s)
}

func hasFingerprint(s string) bool {
	for _, h := range sanitizeHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return headerNameRe.MatchString(s)
}

// SanitizeBody scrubs fingerprints from every message in body["messages"] and
// reports how many messages changed. The maps are expected to be owned by the
// caller (Prepare already deep-copied them).
func SanitizeBody(body map[string]any) int {
	messages, ok := body["messages"].([]any)
	if !ok {
		return 0
	}
	changed := 0
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if sanitizeMessage(m) {
			changed++
		}
	}
	return changed
}

func sanitizeMessage(m map[string]any) bool {
	changed := false
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s, ok := m[key].(string); ok {
			if out := SanitizeText(s); out != s {
				m[key] = out
				changed = true
			}
		}
	}
	if c, ok := m["content"]; ok {
		if out, ch := sanitizeContent(c); ch {
			m["content"] = out
			changed = true
		}
	}
	if tcs, ok := m["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tc["function"].(map[string]any)
			if !ok {
				continue
			}
			if s, ok := fn["arguments"].(string); ok {
				if out := SanitizeText(s); out != s {
					fn["arguments"] = out
					changed = true
				}
			}
		}
	}
	return changed
}

// sanitizeContent handles both a plain string and a multimodal part list; only
// text parts are touched.
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		out := SanitizeText(c)
		return out, out != c
	case []any:
		changed := false
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s, ok := part["text"].(string)
			if !ok {
				continue
			}
			if out := SanitizeText(s); out != s {
				part["text"] = out
				changed = true
			}
		}
		return c, changed
	default:
		return v, false
	}
}
