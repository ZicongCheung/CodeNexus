package api

import (
	"context"
	"sync"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// recordingUpstream succeeds immediately and keeps the request it was handed,
// so a test can inspect how the inbound body was normalized.
type recordingUpstream struct {
	mu   sync.Mutex
	last *upstream.ChatRequest
}

func (r *recordingUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (r *recordingUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}

func (r *recordingUpstream) remember(req *upstream.ChatRequest) {
	r.mu.Lock()
	r.last = req
	r.mu.Unlock()
}

func (r *recordingUpstream) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	r.remember(req)
	return &upstream.ChatResponse{ID: "1", Model: req.Model, Content: "ok", FinishReason: "stop"}, nil
}

func (r *recordingUpstream) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	r.remember(req)
	return emit(upstream.StreamDelta{Content: "ok", FinishReason: "stop"})
}

func (r *recordingUpstream) Refresh(context.Context, *account.Account) error { return nil }

func (r *recordingUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (r *recordingUpstream) request() *upstream.ChatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func TestParseMessagesKeepsToolCallAndResult(t *testing.T) {
	msgs := parseMessages([]any{
		map[string]any{"role": "user", "content": "weather?"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"SF"}`},
			},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	})
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", msgs[1].ToolCalls)
	}
	call := msgs[1].ToolCalls[0]
	if call.ID != "call_1" || call.Index != 0 || call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("call = %+v", call)
	}
	if msgs[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %q", msgs[2].ToolCallID)
	}
}

func TestParseToolCallsCoercesObjectArguments(t *testing.T) {
	msgs := parseMessages([]any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{
				"id": "c", "type": "function",
				"function": map[string]any{"name": "f", "arguments": map[string]any{"a": float64(1)}},
			},
		}},
	})
	if got := msgs[0].ToolCalls[0].Function.Arguments; got != `{"a":1}` {
		t.Fatalf("arguments = %q", got)
	}
}

func TestChatInboundCarriesToolFieldsToTheUpstream(t *testing.T) {
	up := &recordingUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postChat(s, `{"model":"fake-model","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 3 {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool calls were lost: %+v", req.Messages[1])
	}
	if req.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool result was lost: %+v", req.Messages[2])
	}
}

func TestChatInboundDropsAnOrphanToolResult(t *testing.T) {
	up := &recordingUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postChat(s, `{"model":"fake-model","messages":[
		{"role":"user","content":"hi"},
		{"role":"tool","tool_call_id":"ghost","content":"stale"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 1 {
		t.Fatalf("orphan not dropped: %+v", req)
	}
}

func TestToolCallChunksAlwaysCarryTheIndex(t *testing.T) {
	chunks := toolCallChunks([]upstream.ToolCall{upstream.NewToolCall(0, "call_1", "f", "{}")})
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0]["index"] != 0 {
		t.Fatalf("index = %v, want 0 to survive serialization", chunks[0]["index"])
	}
	if chunks[0]["id"] != "call_1" {
		t.Fatalf("id = %v", chunks[0]["id"])
	}
}

func TestToolCallContinuationDoesNotBlankIdentity(t *testing.T) {
	chunks := toolCallChunks([]upstream.ToolCall{upstream.NewToolCall(0, "", "", `{"a"`)})
	chunk := chunks[0]
	if _, ok := chunk["id"]; ok {
		t.Fatalf("a continuation must not carry an empty id: %+v", chunk)
	}
	fn := chunk["function"].(map[string]any)
	if _, ok := fn["name"]; ok {
		t.Fatalf("a continuation must not carry an empty name: %+v", fn)
	}
	if fn["arguments"] != `{"a"` {
		t.Fatalf("arguments = %v", fn["arguments"])
	}
}

func TestToolCallMessagesOmitTheIndex(t *testing.T) {
	msgs := toolCallMessages([]upstream.ToolCall{upstream.NewToolCall(0, "call_1", "f", "{}")})
	if _, ok := msgs[0]["index"]; ok {
		t.Fatalf("a finished call has no index: %+v", msgs[0])
	}
	if msgs[0]["type"] != "function" || msgs[0]["id"] != "call_1" {
		t.Fatalf("message = %+v", msgs[0])
	}
}
