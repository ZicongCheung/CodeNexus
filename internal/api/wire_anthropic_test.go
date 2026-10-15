package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// scriptedUpstream answers with a fixed response or a fixed delta script, and
// keeps the request it was handed.
type scriptedUpstream struct {
	resp   *upstream.ChatResponse
	deltas []upstream.StreamDelta

	mu   sync.Mutex
	last *upstream.ChatRequest
}

func (u *scriptedUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (u *scriptedUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}

func (u *scriptedUpstream) remember(req *upstream.ChatRequest) {
	u.mu.Lock()
	u.last = req
	u.mu.Unlock()
}

func (u *scriptedUpstream) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	u.remember(req)
	if u.resp == nil {
		return &upstream.ChatResponse{Model: req.Model, FinishReason: "stop"}, nil
	}
	out := *u.resp
	return &out, nil
}

func (u *scriptedUpstream) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	u.remember(req)
	for _, d := range u.deltas {
		if err := emit(d); err != nil {
			return err
		}
	}
	return nil
}

func (u *scriptedUpstream) Refresh(context.Context, *account.Account) error { return nil }

func (u *scriptedUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (u *scriptedUpstream) request() *upstream.ChatRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func postAnthropic(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rec, req)
	return rec
}

// sseEvents parses the data payloads of an SSE body.
func sseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		var data string
		for _, line := range strings.Split(block, "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
				data = strings.TrimSpace(s)
			}
		}
		if data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad SSE data %q: %v", data, err)
		}
		out = append(out, m)
	}
	return out
}

func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e["type"].(string))
	}
	return out
}

// ── inbound translation ─────────────────────────────────────────────────────

func TestAnthropicInboundTranslatesToolsAndSystem(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{
		"model":"fake-model","max_tokens":64,
		"system":"be terse",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"secret"},
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}
		]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 4 {
		t.Fatalf("messages = %+v", req)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "be terse" {
		t.Fatalf("system not translated: %+v", req.Messages[0])
	}
	asst := req.Messages[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "toolu_1" ||
		asst.ToolCalls[0].Function.Name != "get_weather" ||
		asst.ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool_use not translated: %+v", asst.ToolCalls)
	}
	if asst.Content != "checking" {
		t.Fatalf("assistant text = %v", asst.Content)
	}
	if req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "toolu_1" ||
		req.Messages[3].Content != "sunny" {
		t.Fatalf("tool_result not translated: %+v", req.Messages[3])
	}
}

func TestAnthropicInboundMapsToolsAndChoice(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	postAnthropic(s, `{
		"model":"fake-model","messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"}}`)
	req := up.request()
	if req == nil || len(req.Tools) != 1 {
		t.Fatalf("tools = %+v", req)
	}
	if req.Raw["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", req.Raw["tool_choice"])
	}
}

func TestAnthropicInboundPassesImagesThrough(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	postAnthropic(s, `{
		"model":"fake-model","messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}`)
	req := up.request()
	if req == nil || len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req)
	}
	parts, ok := req.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", req.Messages[0].Content)
	}
	img, _ := parts[1].(map[string]any)["image_url"].(map[string]any)
	if img == nil || img["url"] != "data:image/png;base64,AAA" {
		t.Fatalf("image part = %#v", parts[1])
	}
}

func TestAnthropicInboundReportsAnImageInsideAToolResult(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	postAnthropic(s, `{
		"model":"fake-model","messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"image","source":{"type":"base64","data":"AAA"}}]}]}]}`)
	req := up.request()
	if req == nil || len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req)
	}
	if req.Messages[1].Content != "[image tool result omitted]" {
		t.Fatalf("tool result = %v", req.Messages[1].Content)
	}
}

func TestAnthropicInboundDefaultsMaxTokens(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	req := up.request()
	if req == nil || req.MaxTokens == nil || *req.MaxTokens != anthropicDefaultMaxTokens {
		t.Fatalf("max_tokens = %v", req)
	}
}

func TestAnthropicInboundRejectsAMissingMessagesField(t *testing.T) {
	s := failoverServer(t, &scriptedUpstream{}, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model"}`)
	if rec.Code != 400 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// ── outbound rendering ──────────────────────────────────────────────────────

func TestAnthropicNonStreamingRendersContentBlocks(t *testing.T) {
	up := &scriptedUpstream{resp: &upstream.ChatResponse{
		ID: "cmpl", Content: "hello", FinishReason: "stop",
		Usage: upstream.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("message = %v", msg)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", msg["stop_reason"])
	}
	blocks := msg["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("blocks = %#v", blocks)
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 3 || usage["output_tokens"].(float64) != 2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicNonStreamingRendersToolUseAndThinking(t *testing.T) {
	call := upstream.NewToolCall(0, "toolu_1", "get_weather", `{"city":"SF"}`)
	up := &scriptedUpstream{resp: &upstream.ChatResponse{
		Content: "checking", ReasoningContent: "thinking hard",
		ToolCalls: []upstream.ToolCall{call}, FinishReason: "tool_calls",
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", msg["stop_reason"])
	}
	blocks := msg["content"].([]any)
	thinking := blocks[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "" || thinking["thinking"] != "thinking hard" {
		t.Fatalf("thinking block = %#v", thinking)
	}
	tool := blocks[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "toolu_1" || tool["name"] != "get_weather" {
		t.Fatalf("tool block = %#v", tool)
	}
	if input := tool["input"].(map[string]any); input["city"] != "SF" {
		t.Fatalf("tool input = %#v", input)
	}
}

func TestAnthropicStreamingRendersTextEventSequence(t *testing.T) {
	usage := upstream.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{Content: "he"},
		{Content: "llo"},
		{Usage: &usage},
		{FinishReason: "stop"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := sseEvents(t, rec.Body.String())
	got := eventTypes(events)
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", got)
	}
	if events[2]["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("block start = %#v", events[2])
	}
	delta := events[3]["delta"].(map[string]any)
	if delta["type"] != "text_delta" || delta["text"] != "he" {
		t.Fatalf("delta = %#v", delta)
	}
	final := events[6]
	if final["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta = %#v", final)
	}
	if final["usage"].(map[string]any)["output_tokens"].(float64) != 2 {
		t.Fatalf("usage = %#v", final["usage"])
	}
}

func TestAnthropicStreamingRendersThinkingBlock(t *testing.T) {
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{ReasoningContent: "pon"},
		{ReasoningContent: "der"},
		{Content: "answer"},
		{FinishReason: "stop"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := sseEvents(t, rec.Body.String())
	starts := map[string]map[string]any{}
	for _, e := range events {
		if e["type"] == "content_block_start" {
			block := e["content_block"].(map[string]any)
			starts[block["type"].(string)] = block
		}
	}
	thinking, ok := starts["thinking"]
	if !ok {
		t.Fatalf("no thinking block: %v", eventTypes(events))
	}
	if thinking["signature"] != "" {
		t.Fatalf("signature = %v, want empty", thinking["signature"])
	}
	if _, ok := starts["text"]; !ok {
		t.Fatalf("no text block: %v", eventTypes(events))
	}
}

func TestAnthropicStreamingRendersToolUseFragments(t *testing.T) {
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "toolu_1", "get_weather", `{"ci`)}},
		{ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "", "", `ty":"SF"}`)}},
		{FinishReason: "tool_calls"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postAnthropic(s, `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := sseEvents(t, rec.Body.String())

	var partials []string
	var toolStart map[string]any
	for _, e := range events {
		switch e["type"] {
		case "content_block_start":
			block := e["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolStart = block
			}
		case "content_block_delta":
			delta := e["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				partials = append(partials, delta["partial_json"].(string))
			}
		}
	}
	if toolStart == nil || toolStart["name"] != "get_weather" || toolStart["id"] != "toolu_1" {
		t.Fatalf("tool block = %#v", toolStart)
	}
	if strings.Join(partials, "") != `{"city":"SF"}` {
		t.Fatalf("fragments = %v", partials)
	}
	last := events[len(events)-2]
	if last["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %#v", last)
	}
}

// ── count_tokens ────────────────────────────────────────────────────────────

func TestAnthropicCountTokensEstimates(t *testing.T) {
	s := failoverServer(t, &scriptedUpstream{}, twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hello world"}]}`))
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if n := out["input_tokens"].(float64); n <= 0 {
		t.Fatalf("input_tokens = %v", n)
	}
}

func TestAnthropicCountTokensChargesAnImageFlat(t *testing.T) {
	s := failoverServer(t, &scriptedUpstream{}, twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","data":"`+strings.Repeat("A", 4000)+`"}}]}]}`))
	s.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	n := out["input_tokens"].(float64)
	if n < estimateImageTokens || n > estimateImageTokens+10 {
		t.Fatalf("input_tokens = %v, want about %d", n, estimateImageTokens)
	}
}

// ── the idle keepalive ──────────────────────────────────────────────────────

// stallingUpstream stays silent until it is released or its context is done.
type stallingUpstream struct{ release chan struct{} }

func (u *stallingUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (u *stallingUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}

func (u *stallingUpstream) Chat(ctx context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	return &upstream.ChatResponse{Model: req.Model, FinishReason: "stop"}, nil
}

func (u *stallingUpstream) ChatStream(ctx context.Context, _ *account.Account, _ *upstream.ChatRequest, emit upstream.EmitFunc) error {
	select {
	case <-u.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(upstream.StreamDelta{Content: "late", FinishReason: "stop"})
}

func (u *stallingUpstream) Refresh(context.Context, *account.Account) error { return nil }

func (u *stallingUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

// TestAnthropicOpensBeforeTheFirstToken proves the keepalive commits the answer
// while the upstream is still silent, which is what keeps Claude Code's
// message_start timeout from firing.
func TestAnthropicOpensBeforeTheFirstToken(t *testing.T) {
	old := anthropicIdleKeepalive
	anthropicIdleKeepalive = 50 * time.Millisecond
	t.Cleanup(func() { anthropicIdleKeepalive = old })

	stall := &stallingUpstream{release: make(chan struct{})}
	s := failoverServer(t, stall, twoAccounts())
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer close(stall.release)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	// The headers only arrive once something is written; the keepalive writes
	// message_start well before the upstream releases its first token.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(line, "message_start") {
		t.Fatalf("first frame = %q, want a message_start", line)
	}
}
