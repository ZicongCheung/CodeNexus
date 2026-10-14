package rewrite

import "testing"

func TestPrepareNormalizesAndRepairs(t *testing.T) {
	in := map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "developer", "content": "sys"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "1"}}},
			map[string]any{"role": "tool", "tool_call_id": "2", "content": "orphan"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_completion_tokens": 8,
	}

	out := Prepare(in, Options{ForceStream: true, MinMaxTokens: 16, DefaultReasoningEffort: "high"})

	if out["stream"] != true {
		t.Error("stream not forced")
	}
	if _, ok := out["stream_options"]; !ok {
		t.Error("stream_options not set")
	}
	if _, ok := out["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens not folded")
	}
	if out["max_tokens"] != 16 {
		t.Errorf("max_tokens = %v, want clamped 16", out["max_tokens"])
	}
	if _, ok := out["thinking"]; !ok {
		t.Error("thinking not injected for reasoning model")
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", out["reasoning_effort"])
	}

	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected orphan tool dropped, got %d messages", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Error("developer role not normalized to system")
	}

	// the caller's input must be untouched
	inMsgs := in["messages"].([]any)
	if inMsgs[0].(map[string]any)["role"] != "developer" {
		t.Error("input mutated: developer role")
	}
	if _, ok := in["max_completion_tokens"]; !ok {
		t.Error("input mutated: max_completion_tokens removed")
	}
}

func TestPrepareKeepsPairedToolResult(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "1"}}},
			map[string]any{"role": "tool", "tool_call_id": "1", "content": "ok"},
			map[string]any{"role": "tool", "tool_call_id": "9", "content": "orphan"},
		},
	}
	out := Prepare(in, Options{})
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if _, ok := out["thinking"]; ok {
		t.Error("thinking should not be injected for a non-reasoning model")
	}
}

func TestPrepareBackfillsReasoning(t *testing.T) {
	in := map[string]any{
		"model": "deepseek-chat",
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "r"},
		},
	}
	out := Prepare(in, Options{})
	msg := out["messages"].([]any)[0].(map[string]any)
	if msg["reasoning"] != "r" {
		t.Errorf("reasoning = %v, want mirrored", msg["reasoning"])
	}
}

func TestPrepareForceStreamFalseLeavesStreamAlone(t *testing.T) {
	in := map[string]any{"model": "m", "stream": false}
	out := Prepare(in, Options{ForceStream: false})
	if out["stream"] != false {
		t.Error("stream should stay false")
	}
	if _, ok := out["stream_options"]; ok {
		t.Error("stream_options should not be added when not streaming")
	}
}
