package traecn

import (
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The agent surface speaks the OpenAI message shape, so the neutral tool fields
// must be re-expanded on the way out or a multi-turn tool loop loses its calls.
func TestAgentMessagesExpandsToolFields(t *testing.T) {
	req := &upstream.ChatRequest{Messages: []upstream.Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "call_1", "get_weather", "{}")}},
		{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
	}}
	out := agentMessages(req)
	if len(out) != 3 {
		t.Fatalf("messages = %d", len(out))
	}
	asst := out[1].(map[string]any)
	if _, ok := asst["tool_calls"]; !ok {
		t.Fatalf("assistant tool calls were dropped: %+v", asst)
	}
	result := out[2].(map[string]any)
	if result["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id = %v", result["tool_call_id"])
	}
}
