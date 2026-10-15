package traecn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The agent surface is a single streaming completion that speaks the native
// tools protocol. It is the only surface whose tool calls belong to the caller:
// the remote surface runs inside a workspace of its own and may satisfy tools
// itself, which would silently swallow them.
//
// Every request is streamed, even when the client asked for a single response;
// the non-streaming case is assembled locally.

// agentChatPath is the agent completion endpoint.
const agentChatPath = "/api/agent/v3/llm_utils_chat"

// agentFunction names the server-side executor the request is addressed to.
const agentFunction = "solo_work_lite"

// agentChat runs one agent completion and accumulates the response.
func (c *Client) agentChat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, write func(upstream.StreamDelta) error) (*upstream.ChatResponse, error) {
	modelName := ResolveConfigName(req.Model)
	if modelName == WorkModelID {
		// "work" is a routing id, not a model; the agent surface needs a real
		// config name, so fall back to the default model.
		modelName = ResolveConfigName("auto")
	}
	requestID := newUUID()

	body := map[string]any{
		"messages":    agentMessages(req),
		"config_name": modelName,
		"model":       modelName,
		"function":    agentFunction,
		"stream":      true,
		"request_id":  requestID,
		"session_id":  newUUID(),
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		body["max_tokens"] = *req.MaxTokens
	} else if c.cfg.MaxTokens > 0 {
		body["max_tokens"] = c.cfg.MaxTokens
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.Raw != nil {
		for _, key := range []string{"tool_choice", "parallel_tool_calls"} {
			if v, ok := req.Raw[key]; ok && v != nil {
				body[key] = v
			}
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(agentChatPath), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.setAgentHeaders(httpReq, acc)

	resp, err := c.stream.Do(httpReq)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if code := remoteErrorCode(raw); code != "" {
			return nil, remoteError(resp.StatusCode, code, string(raw))
		}
		return nil, upstream.ClassifyHTTP(resp.StatusCode, string(raw))
	}

	out := &upstream.ChatResponse{ID: requestID, Model: req.Model}
	st := &snapshotState{emit: write}
	tools := &toolAccumulator{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	finished := false

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if name, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(name)
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			finished = true
			break
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		name := normalizeEventName(firstNonEmpty(event, eventNameOf(obj)))
		switch name {
		case "heartbeat", "keepalive", "ping", "model_config":
			continue
		case "error":
			return out, remoteError(0, remoteErrorCodeOf(obj), rawJSON(obj))
		}
		if usage := usageOf(obj); usage != nil {
			out.Usage = *usage
		}
		text, reasoning := agentText(obj)
		if err := st.write(text, reasoning, out); err != nil {
			return out, err
		}
		if calls := tools.add(obj); len(calls) > 0 {
			out.ToolCalls = append(out.ToolCalls, calls...)
			if write != nil {
				if err := write(upstream.StreamDelta{ToolCalls: calls, FinishReason: "tool_calls"}); err != nil {
					return out, err
				}
			}
			out.FinishReason = "tool_calls"
		}
		if reason := firstString(obj, "finish_reason", "stop_reason"); reason != "" {
			out.FinishReason = normalizeFinish(reason)
		}
		if name == "done" || boolOf(obj, "done", "finished") {
			finished = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, upstream.Classify(ctx.Err())
		}
		return out, upstream.Classify(err)
	}
	_ = finished
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	return out, nil
}

// agentMessages converts normalized messages into the upstream's shape. The
// agent surface takes the OpenAI message array as-is, including tool roles and
// structured content, so this is a pass-through with defaults filled in.
func agentMessages(req *upstream.ChatRequest) []any {
	out := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := map[string]any{"role": m.Role}
		if m.Content != nil {
			msg["content"] = m.Content
		} else {
			msg["content"] = ""
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		out = append(out, msg)
	}
	return out
}

// agentText reads the answer and reasoning snapshots of one agent event.
func agentText(obj map[string]any) (text, reasoning string) {
	text = firstString(obj, "response", "content", "text", "answer", "output")
	if text == "" {
		text = contentBlocksText(obj["content"])
	}
	if text == "" {
		if nested, ok := obj["message"].(map[string]any); ok {
			text = firstString(nested, "content", "text", "response")
		}
	}
	// A delta-shaped event carries the increment in choices[0].delta.
	if text == "" {
		text = choiceDeltaText(obj)
	}
	reasoning = firstString(obj, "reasoning_content", "reasoningContent", "thought", "thinking")
	if reasoning == "" {
		if nested, ok := obj["message"].(map[string]any); ok {
			reasoning = firstString(nested, "reasoning_content", "reasoningContent", "thought")
		}
	}
	return text, reasoning
}

// choiceDeltaText reads an OpenAI-shaped delta, which some events use.
func choiceDeltaText(obj map[string]any) string {
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	delta, ok := first["delta"].(map[string]any)
	if !ok {
		return ""
	}
	return firstString(delta, "content", "reasoning_content")
}

// toolAccumulator collects tool calls, de-duplicating the ones the upstream
// repeats across events.
type toolAccumulator struct {
	seen map[string]bool
	next int
}

// add extracts any tool calls in an event and returns only the new ones.
func (a *toolAccumulator) add(obj map[string]any) []upstream.ToolCall {
	raw := toolCallsRaw(obj)
	if len(raw) == 0 {
		return nil
	}
	var out []upstream.ToolCall
	for _, item := range raw {
		tc, key := normalizeToolCall(item, a.next)
		if key == "" {
			continue
		}
		if a.seen == nil {
			a.seen = map[string]bool{}
		}
		if a.seen[key] {
			continue
		}
		a.seen[key] = true
		a.next++
		tc.Index = a.next - 1
		out = append(out, tc)
	}
	return out
}

// toolCallsRaw finds the tool-call list of an event. The upstream nests it
// differently per executor, so several shapes are accepted.
func toolCallsRaw(obj map[string]any) []any {
	for _, key := range []string{"tool_calls", "toolCalls"} {
		if v, ok := obj[key].([]any); ok && len(v) > 0 {
			return v
		}
	}
	if info, ok := obj["tool_call_info"]; ok && info != nil {
		return []any{info}
	}
	if info, ok := obj["tool_call"]; ok && info != nil {
		if list, ok := info.([]any); ok {
			return list
		}
		return []any{info}
	}
	if nested, ok := obj["message"].(map[string]any); ok {
		if v, ok := nested["tool_calls"].([]any); ok && len(v) > 0 {
			return v
		}
	}
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]any); ok {
			if delta, ok := first["delta"].(map[string]any); ok {
				if v, ok := delta["tool_calls"].([]any); ok && len(v) > 0 {
					return v
				}
			}
		}
	}
	return nil
}

// normalizeToolCall flattens one tool-call object and returns a stable key used
// to de-duplicate repeated emissions.
func normalizeToolCall(item any, index int) (upstream.ToolCall, string) {
	obj, ok := item.(map[string]any)
	if !ok {
		return upstream.ToolCall{}, ""
	}
	tc := upstream.ToolCall{Type: "function", Index: index}
	tc.ID = firstString(obj, "id", "tool_call_id", "call_id")
	fn, _ := obj["function"].(map[string]any)
	if fn == nil {
		fn = obj
	}
	tc.Function.Name = firstString(fn, "name", "tool_name", "function_name")
	tc.Function.Arguments = argumentsText(fn)
	if tc.Function.Name == "" {
		return upstream.ToolCall{}, ""
	}
	key := tc.ID
	if key == "" {
		key = fmt.Sprintf("%s#%s", tc.Function.Name, tc.Function.Arguments)
	}
	return tc, key
}

// argumentsText renders tool arguments as the JSON string the OpenAI protocol
// requires, whether the upstream sent an object or a string.
func argumentsText(fn map[string]any) string {
	for _, key := range []string{"arguments", "args", "parameters", "input"} {
		switch v := fn[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case nil:
		default:
			if b, err := json.Marshal(v); err == nil {
				return string(b)
			}
		}
	}
	return "{}"
}

// boolOf reports whether any of the named boolean fields is true.
func boolOf(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok && v {
			return true
		}
	}
	return false
}
