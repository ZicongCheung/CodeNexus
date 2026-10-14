package rewrite

import (
	"strings"
	"testing"
)

func TestSanitizeTextRewritesTemplates(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"You are Claude Code, Anthropic's official CLI for Claude.",
			"You are Claude Code, Anthropic's official CLI tool for Claude.",
		},
		{
			"Main branch (you will usually use this for PRs)",
			"Default branch (you will usually use this for PRs)",
		},
		{
			"error 11128 reported",
			"error 11-128 reported",
		},
	}
	for _, c := range cases {
		if got := SanitizeText(c.in); got != c.want {
			t.Errorf("SanitizeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeTextStripsHeaderAndParameters(t *testing.T) {
	in := "x-anthropic-billing-header: 1.0; cc_entrypoint=cli; rest"
	if got := SanitizeText(in); got != "rest" {
		t.Fatalf("got %q, want %q", got, "rest")
	}
	bare := "see x-anthropic-billing-header for details"
	if got := SanitizeText(bare); got != "see x-anthropic-billing-hdr for details" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeTextNoFingerprintIsIdentity(t *testing.T) {
	in := "just an ordinary sentence"
	if got := SanitizeText(in); got != in {
		t.Fatalf("got %q, want identity", got)
	}
}

func TestSanitizeBodyWalksMessages(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Main branch (you will usually use this for PRs)"},
			}},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "x", "arguments": `{"note":"code 11128"}`}},
			}},
		},
	}
	if n := SanitizeBody(body); n != 3 {
		t.Fatalf("changed = %d, want 3", n)
	}
	msgs := body["messages"].([]any)
	if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "CLI tool") {
		t.Error("system content not rewritten")
	}
	part := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), "Default branch") {
		t.Error("multimodal text part not rewritten")
	}
	args := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, "11-128") {
		t.Error("tool arguments not rewritten")
	}
}

func TestPrepareSanitizesByDefaultAndLeavesInputAlone(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "code 11128"},
		},
	}
	out := Prepare(in, Options{})
	if got := out["messages"].([]any)[0].(map[string]any)["content"].(string); got != "code 11-128" {
		t.Fatalf("out content = %q", got)
	}
	if got := in["messages"].([]any)[0].(map[string]any)["content"].(string); got != "code 11128" {
		t.Fatalf("input was mutated: %q", got)
	}
}

func TestPrepareDisableSanitize(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "code 11128"},
		},
	}
	out := Prepare(in, Options{DisableSanitize: true})
	if got := out["messages"].([]any)[0].(map[string]any)["content"].(string); got != "code 11128" {
		t.Fatalf("content should be untouched, got %q", got)
	}
}
