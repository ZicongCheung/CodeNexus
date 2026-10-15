package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func postResponses(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rec, req)
	return rec
}

// ── inbound ─────────────────────────────────────────────────────────────────

func TestResponsesStringInputBecomesAUserTurn(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","input":"hello","instructions":"be terse"}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req)
	}
	if req.Messages[0].Role != "developer" || req.Messages[0].Content != "be terse" {
		t.Fatalf("instructions = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "hello" {
		t.Fatalf("input = %+v", req.Messages[1])
	}
}

func TestResponsesFoldsToolCallItems(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{
		"model":"fake-model",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
		"input":[
			{"type":"message","role":"user","content":"weather?"},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req)
	}
	if len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].Function.Name != "get_weather" ||
		req.Messages[1].ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call = %+v", req.Messages[1].ToolCalls)
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_1" || req.Messages[2].Content != "sunny" {
		t.Fatalf("tool output = %+v", req.Messages[2])
	}
	if req.Raw["tool_choice"] != nil {
		t.Fatalf("unexpected tool_choice: %v", req.Raw["tool_choice"])
	}
}

func TestResponsesKeepsAnOrphanToolOutputAsInert(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{
		"model":"fake-model",
		"input":[{"type":"message","role":"user","content":"hi"},
		         {"type":"function_call_output","call_id":"ghost","output":"stale"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req)
	}
	if req.Messages[1].Role != "user" {
		t.Fatalf("role = %s, want an inert user block", req.Messages[1].Role)
	}
	text, _ := req.Messages[1].Content.(string)
	if !strings.Contains(text, "Untrusted client tool result") || !strings.Contains(text, "stale") {
		t.Fatalf("inert block = %q", text)
	}
}

func TestResponsesMapsToolChoiceAndCustomTools(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{
		"model":"fake-model",
		"tools":[{"type":"custom","name":"notes tool","format":{"type":"text"}}],
		"tool_choice":{"type":"custom","name":"notes tool"},
		"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Tools) != 1 {
		t.Fatalf("tools = %+v", req)
	}
	fn, _ := req.Tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "notes_tool" {
		t.Fatalf("chat name = %v, want the sanitized name", fn["name"])
	}
	params, _ := fn["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["input"]; !ok {
		t.Fatalf("custom tool was not degraded to an input parameter: %v", params)
	}
	choice, _ := req.Raw["tool_choice"].(map[string]any)
	fnChoice, _ := choice["function"].(map[string]any)
	if fnChoice["name"] != "notes_tool" {
		t.Fatalf("tool_choice = %v", req.Raw["tool_choice"])
	}
}

// ── previous_response_id ────────────────────────────────────────────────────

func TestResponsesContinuesWithPreviousResponseID(t *testing.T) {
	up := &scriptedUpstream{resp: &upstream.ChatResponse{ID: "cmpl", Content: "ok", FinishReason: "stop"}}
	s := failoverServer(t, up, twoAccounts())

	first := postResponses(s, `{"model":"fake-model","input":"hi"}`)
	if first.Code != 200 {
		t.Fatalf("first code = %d, body = %s", first.Code, first.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("no response id: %v", resp)
	}

	second := postResponses(s, `{"model":"fake-model","input":"again","previous_response_id":"`+id+`"}`)
	if second.Code != 200 {
		t.Fatalf("second code = %d, body = %s", second.Code, second.Body.String())
	}
	req := up.request()
	if req == nil || len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req)
	}
	if req.Messages[0].Content != "hi" || req.Messages[1].Content != "ok" || req.Messages[2].Content != "again" {
		t.Fatalf("history = %+v", req.Messages)
	}
	if req.Messages[1].Role != "assistant" {
		t.Fatalf("cached assistant turn lost: %+v", req.Messages[1])
	}
}

func TestResponsesReplaysOverlapWithoutDuplicating(t *testing.T) {
	up := &scriptedUpstream{resp: &upstream.ChatResponse{ID: "cmpl", Content: "ok", FinishReason: "stop"}}
	s := failoverServer(t, up, twoAccounts())
	first := postResponses(s, `{"model":"fake-model","input":"hi"}`)
	var resp map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)

	// A client that also replays the visible transcript must not duplicate it.
	rec := postResponses(s, `{"model":"fake-model","previous_response_id":"`+id+`",
		"input":[{"type":"message","role":"user","content":"hi"},
		         {"type":"message","role":"assistant","content":"ok"},
		         {"type":"message","role":"user","content":"next"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	got := []string{}
	for _, m := range req.Messages {
		got = append(got, m.Role+":"+m.Content.(string))
	}
	want := []string{"user:hi", "assistant:ok", "user:next"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestResponsesRejectsAnUnknownPreviousResponseID(t *testing.T) {
	s := failoverServer(t, &scriptedUpstream{}, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","input":"hi","previous_response_id":"resp_missing"}`)
	if rec.Code != 400 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestResponsesStoreFalseSkipsTheCache(t *testing.T) {
	up := &scriptedUpstream{resp: &upstream.ChatResponse{ID: "cmpl", Content: "ok", FinishReason: "stop"}}
	s := failoverServer(t, up, twoAccounts())
	first := postResponses(s, `{"model":"fake-model","input":"hi","store":false}`)
	var resp map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)

	rec := postResponses(s, `{"model":"fake-model","input":"again","previous_response_id":"`+id+`"}`)
	if rec.Code != 400 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// ── outbound ────────────────────────────────────────────────────────────────

func TestResponsesNonStreamingRendersOutputItems(t *testing.T) {
	call := upstream.NewToolCall(0, "call_1", "get_weather", `{"city":"SF"}`)
	up := &scriptedUpstream{resp: &upstream.ChatResponse{
		ID: "cmpl", Content: "checking", ReasoningContent: "thinking hard",
		ToolCalls: []upstream.ToolCall{call}, FinishReason: "stop",
		Usage: upstream.Usage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7},
	}}
	s := failoverServer(t, up, twoAccounts())
	// Reasoning stays private unless the client asked for it.
	quiet := postResponses(s, `{"model":"fake-model","input":"hi"}`)
	var quietResp map[string]any
	_ = json.Unmarshal(quiet.Body.Bytes(), &quietResp)
	for _, item := range quietResp["output"].([]any) {
		if item.(map[string]any)["type"] == "reasoning" {
			t.Fatal("reasoning leaked without an explicit request")
		}
	}

	loud := postResponses(s, `{"model":"fake-model","input":"hi","thinking":true}`)
	var resp map[string]any
	_ = json.Unmarshal(loud.Body.Bytes(), &resp)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("response = %v", resp)
	}
	output := resp["output"].([]any)
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output[0] = %#v", output[0])
	}
	summary := output[0].(map[string]any)["summary"].([]any)
	if summary[0].(map[string]any)["text"] != "thinking hard" {
		t.Fatalf("summary = %#v", summary)
	}
	if output[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("output[2] = %#v", output[2])
	}
	if call := output[2].(map[string]any); call["call_id"] != "call_1" || call["name"] != "get_weather" {
		t.Fatalf("function_call = %#v", call)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 4 || usage["output_tokens"].(float64) != 3 || usage["total_tokens"].(float64) != 7 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesStreamingEventSequence(t *testing.T) {
	usage := upstream.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{Content: "he"}, {Content: "llo"}, {Usage: &usage}, {FinishReason: "stop"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","stream":true,"input":"hi"}`)
	events := sseEvents(t, rec.Body.String())
	got := eventTypes(events)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", got)
	}
	created := events[0]
	if created["response"].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("created = %#v", created)
	}
	if seq := created["sequence_number"].(float64); seq != 0 {
		t.Fatalf("first sequence_number = %v", seq)
	}
	last := events[len(events)-1]
	completed := last["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Fatalf("completed = %#v", completed)
	}
	if completed["usage"].(map[string]any)["output_tokens"].(float64) != 2 {
		t.Fatalf("usage = %#v", completed["usage"])
	}
}

func TestResponsesStreamingReasoningIsPrivateByDefault(t *testing.T) {
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{ReasoningContent: "secret"}, {Content: "answer"}, {FinishReason: "stop"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","stream":true,"input":"hi"}`)
	for _, e := range sseEvents(t, rec.Body.String()) {
		if e["type"] == "response.output_item.added" &&
			e["item"].(map[string]any)["type"] == "reasoning" {
			t.Fatal("reasoning leaked without an explicit request")
		}
	}
}

func TestResponsesStreamingEmitsFunctionCallEvents(t *testing.T) {
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "call_1", "get_weather", `{"ci`)}},
		{ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "", "", `ty":"SF"}`)}},
		{FinishReason: "tool_calls"},
	}}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","stream":true,"input":"hi",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	events := sseEvents(t, rec.Body.String())

	var deltas []string
	var done map[string]any
	for _, e := range events {
		switch e["type"] {
		case "response.function_call_arguments.delta":
			deltas = append(deltas, e["delta"].(string))
		case "response.function_call_arguments.done":
			done = e
		}
	}
	if strings.Join(deltas, "") != `{"city":"SF"}` {
		t.Fatalf("argument deltas = %v", deltas)
	}
	if done == nil || done["arguments"] != `{"city":"SF"}` {
		t.Fatalf("arguments.done = %#v", done)
	}
	if events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("terminal event = %v", eventTypes(events))
	}
}

// A tool call made in a streamed turn must be remembered, so the next turn's
// function_call_output can be paired with it.
func TestResponsesStreamingRemembersCallBindings(t *testing.T) {
	up := &scriptedUpstream{deltas: []upstream.StreamDelta{
		{ToolCalls: []upstream.ToolCall{upstream.NewToolCall(0, "call_9", "get_weather", "{}")}},
		{FinishReason: "tool_calls"},
	}}
	s := failoverServer(t, up, twoAccounts())
	first := postResponses(s, `{"model":"fake-model","stream":true,"input":"hi",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	if first.Code != 200 {
		t.Fatalf("first code = %d", first.Code)
	}
	var id string
	for _, e := range sseEvents(t, first.Body.String()) {
		if e["type"] == "response.created" {
			id, _ = e["response"].(map[string]any)["id"].(string)
			break
		}
	}
	if id == "" {
		t.Fatal("no response id in the created event")
	}

	rec := postResponses(s, `{"model":"fake-model","previous_response_id":"`+id+`",
		"input":[{"type":"function_call_output","call_id":"call_9","output":"sunny"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	req := up.request()
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	cached := req.Messages[1]
	if cached.Role != "assistant" || len(cached.ToolCalls) != 1 || cached.ToolCalls[0].ID != "call_9" {
		t.Fatalf("cached call lost: %+v", cached)
	}
	last := req.Messages[2]
	if last.Role != "tool" || last.ToolCallID != "call_9" || last.Content != "sunny" {
		t.Fatalf("tool output was not paired: %+v", last)
	}
}

func TestResponsesClampsMaxOutputTokens(t *testing.T) {
	up := &scriptedUpstream{}
	s := failoverServer(t, up, twoAccounts())
	rec := postResponses(s, `{"model":"fake-model","input":"hi","max_output_tokens":999999}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	req := up.request()
	if req == nil || req.MaxTokens == nil || *req.MaxTokens > responsesDefaultMaxTokens {
		t.Fatalf("max_tokens = %v", req)
	}
}
