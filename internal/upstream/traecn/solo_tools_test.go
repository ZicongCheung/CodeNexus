package traecn

import (
	"context"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The SOLO model gateway ships native calls in a flatter shape than the OpenAI
// one and splits a call across fragments: the first carries id and name, the
// following ones only arguments. This is a transcript of a real turn.
const soloAgentEvents = `data: {"response":"","reasoning_content":" user wants me to call","tool_calls":null,"multimodal_contents":null,"phase":null}

data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"call_0a04","type":"function","function_call":{"name":"get_weather","arguments":"","partial_arguments":null,"namespace":null}}],"multimodal_contents":null,"phase":null}

data: {"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"","type":"function","function_call":{"name":"","arguments":"{\"city\": \"北京\"}","partial_arguments":null,"namespace":null}}],"multimodal_contents":null,"phase":null}

event: extra_info
data: {"model":"glm-5.3","version":1,"content":"{\"reasoning_content\":\"digest\",\"response\":\"should never be read as the reply\"}"}

data: {"finish_reason":"stop"}

data: [DONE]

`

// A fragment that opens a call carries no arguments yet; folding its "empty
// object" placeholder into the slot would prefix the real arguments with "{}".
func TestSoloToolCallFragmentsFoldIntoOneCall(t *testing.T) {
	s := newStub(t)
	s.agentBody = soloAgentEvents
	c := s.client(Config{UpstreamMode: "ide"})

	req := &upstream.ChatRequest{
		Model:    "glm-5.3",
		Messages: []upstream.Message{{Role: "user", Content: "北京天气"}},
		Tools:    []any{map[string]any{"type": "function"}},
	}
	out, err := c.Chat(context.Background(), testAccount(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d (%+v), want the fragments folded into one", len(out.ToolCalls), out.ToolCalls)
	}
	call := out.ToolCalls[0]
	if call.Function.Name != "get_weather" {
		t.Fatalf("name = %q, want the name from the opening fragment", call.Function.Name)
	}
	if call.Function.Arguments != `{"city": "北京"}` {
		t.Fatalf("arguments = %q, want the later fragment alone", call.Function.Arguments)
	}
	if call.ID != "call_0a04" {
		t.Fatalf("id = %q, want the id from the opening fragment", call.ID)
	}
}

// The gateway ends a tool-calling turn with finish_reason=stop, but a client
// branches on tool_calls to know it must answer the call first.
func TestSoloToolTurnFinishesAsToolCalls(t *testing.T) {
	s := newStub(t)
	s.agentBody = soloAgentEvents
	c := s.client(Config{UpstreamMode: "ide"})

	out, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.3",
		Messages: []upstream.Message{{Role: "user", Content: "北京天气"}},
		Tools:    []any{map[string]any{"type": "function"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", out.FinishReason)
	}
}

// extra_info carries the upstream's own digest of the turn. Its content field
// is machine summary, and reading it would put JSON into the user's reply.
func TestSoloExtraInfoDoesNotLeakIntoTheReply(t *testing.T) {
	s := newStub(t)
	s.agentBody = soloAgentEvents
	c := s.client(Config{UpstreamMode: "ide"})

	emit, deltas := collect()
	err := c.ChatStream(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.3",
		Messages: []upstream.Message{{Role: "user", Content: "北京天气"}},
		Tools:    []any{map[string]any{"type": "function"}},
	}, emit)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text string
	for _, d := range *deltas {
		text += d.Content
	}
	if text != "" {
		t.Fatalf("text = %q, want the digest excluded", text)
	}
	if text != "" {
		t.Fatalf("delta text = %q, want the digest excluded", text)
	}
}

// A native tool definition takes parameters as a JSON string; an object is
// rejected by the gateway's protobuf adapter with 4001.
func TestNativeToolDefinitionsStringifyParameters(t *testing.T) {
	out := nativeToolDefinitions([]any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "查询天气",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		},
	}})
	if len(out) != 1 {
		t.Fatalf("tools = %d, want 1", len(out))
	}
	tool, _ := out[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters = %T, want a JSON string", fn["parameters"])
	}
	if params != `{"properties":{"city":{"type":"string"}},"type":"object"}` {
		t.Fatalf("parameters = %s", params)
	}
	if fn["name"] != "get_weather" {
		t.Fatalf("name = %v", fn["name"])
	}
}

// The gateway reads the caller's tool availability from the client context in
// the runtime system message. Without it the agent answers "no such tool".
func TestRuntimePromptDeclaresCallerTools(t *testing.T) {
	msgs := withRuntimePrompt(&upstream.ChatRequest{
		Messages: []upstream.Message{{Role: "user", Content: "天气"}},
		Tools:    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	})
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the prompt prepended", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first role = %q, want system", msgs[0].Role)
	}
	// The tool names ride in the tools field; the prompt's job is to declare
	// that caller tools exist at all and how to call them.
	text, _ := msgs[0].Content.(string)
	for _, want := range []string{"Caller client context", "tool_discovery", "native function calling"} {
		if !contains(text, want) {
			t.Fatalf("prompt is missing %q: %s", want, text)
		}
	}
	// Without tools there is nothing to declare, and the caller's messages must
	// be handed over untouched.
	plain := []upstream.Message{{Role: "user", Content: "hi"}}
	if got := withRuntimePrompt(&upstream.ChatRequest{Messages: plain}); len(got) != 1 {
		t.Fatalf("messages = %d, want no prompt without tools", len(got))
	}
}

func TestNativeToolChoiceSpellings(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"auto", "auto"},
		{"REQUIRED", "required"},
		{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, "get_weather"},
		{map[string]any{"type": "none"}, "none"},
	}
	for _, c := range cases {
		if got := nativeToolChoice(c.in); got != c.want {
			t.Fatalf("nativeToolChoice(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
