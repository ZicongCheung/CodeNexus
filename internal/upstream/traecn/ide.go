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
	"github.com/ZicongCheung/CodeNexus/internal/logx"
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
	requestID := newHexID()

	body := map[string]any{
		"messages":    agentMessages(withRuntimePrompt(req)),
		"config_name": modelName,
		"model":       modelName,
		"function":    agentFunction,
		"stream":      true,
		"request_id":  requestID,
		// The gateway correlates the session with the request id; the native
		// client sends the same 32-hex value in both fields.
		"session_id": requestID,
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		body["max_tokens"] = *req.MaxTokens
	} else if c.cfg.MaxTokens > 0 {
		body["max_tokens"] = c.cfg.MaxTokens
	}
	if native := nativeToolDefinitions(req.Tools); len(native) > 0 {
		body["tools"] = native
	}
	if req.Raw != nil {
		if v, ok := req.Raw["tool_choice"]; ok && v != nil {
			// The native form is a string: auto/none/required or a tool name.
			if choice := nativeToolChoice(v); choice != "" {
				body["tool_choice"] = choice
			}
		}
		for _, key := range []string{"parallel_tool_calls"} {
			if v, ok := req.Raw[key]; ok && v != nil {
				body[key] = v
			}
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if c.cfg.DebugUpstream {
		logx.Debugf("trae agent request: %s", clipJSON(payload, 6*1024))
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

	out := &upstream.ChatResponse{ID: requestID, Model: req.Model, Surface: "ide"}
	st := &snapshotState{emit: write}
	tools := &toolStream{}
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
		if c.cfg.DebugUpstream {
			logx.Debugf("trae agent event %s: %s", event, clipJSON([]byte(payload), 4*1024))
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		name := normalizeEventName(firstNonEmpty(event, eventNameOf(obj)))
		switch name {
		case "heartbeat", "keepalive", "ping", "model_config", "extra_info":
			// extra_info carries the upstream's own digest of the turn (its
			// content field is a machine summary, not the answer); reading it
			// would pollute the reply.
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
		if frags := tools.feed(obj); len(frags) > 0 {
			if write != nil {
				if err := write(upstream.StreamDelta{ToolCalls: frags, FinishReason: "tool_calls"}); err != nil {
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
	out.ToolCalls = tools.calls()
	for i := range out.ToolCalls {
		if strings.TrimSpace(out.ToolCalls[i].Function.Arguments) == "" {
			// The protocol requires a JSON object here; a call opened without
			// arguments still has to be renderable by strict clients.
			out.ToolCalls[i].Function.Arguments = "{}"
		}
	}
	if len(out.ToolCalls) > 0 && (out.FinishReason == "" || out.FinishReason == "stop") {
		// The native gateway ends a tool-calling turn with finish_reason=stop
		// even though it emitted calls; in the OpenAI vocabulary a turn that
		// asks for tools stops for that reason, and clients branch on it.
		out.FinishReason = "tool_calls"
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	return out, nil
}

// agentMessages converts normalized messages into the upstream's shape. The
// native transport reads content as an array of blocks, never a bare string —
// the upstream answers 4001 "cannot unmarshal string into ... content" for one —
// so every body is wrapped here. Tool calls and their results are re-expanded
// from the neutral fields, and an absent body stays null rather than "".
func agentMessages(msgs []upstream.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg := map[string]any{"role": m.Role, "content": ideNativeContent(m.Content)}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.ReasoningContent != "" {
			msg["reasoning_content"] = m.ReasoningContent
		}
		out = append(out, msg)
	}
	return out
}

// withRuntimePrompt prepends the system message that describes the caller's
// own tools. The upstream agent only knows its registry of server-side tools,
// so without it a caller tool is answered with "no such tool" instead of a
// structured call. The client-context block is part of that contract: it is
// what tells the agent that caller-declared tools exist at all, and a prompt
// without it is ignored.
func withRuntimePrompt(req *upstream.ChatRequest) []upstream.Message {
	if len(req.Tools) == 0 {
		return req.Messages
	}
	var b strings.Builder
	b.WriteString("You are Trae Code connected to the user's local application.\n")
	b.WriteString("The external Trae-compatible terminal client application that started this conversation provides the client tools below.\n")
	b.WriteString("Caller client context (JSON):\n")
	b.WriteString(runtimeClientContext())
	b.WriteString("\n")
	b.WriteString("Discover the local environment through the available client tools when the request requires it.\n")
	b.WriteString("If a tool can answer the question, call it proactively and wait for the external client result.\n")
	b.WriteString("Remote or server-side tools cannot write into the caller workspace. Never claim that a client file was downloaded, created, edited, patched, or saved unless a later matching client tool result with the same call id confirms success.\n")
	b.WriteString("A tool request, emitted tool-call block, plan, or assistant narration is not execution proof. If the matching client result reports failure, state the failure or request a corrective client tool; never report the operation as completed.\n")
	b.WriteString("Never repeat a completed tool call; do not describe that server's Linux filesystem as the caller workspace.\n")
	b.WriteString("Use native function calling for the client tools supplied in the request. Their definitions and input schemas are in the tools field.\n")
	b.WriteString("Return structured tool calls, not tool-call markup or JSON tool blocks in the response text. Use an exact declared function name and arguments matching its schema.\n")
	b.WriteString("The external API client executes these calls. Wait for the matching tool result before continuing; never substitute server-side tools to access the caller's workspace.\n")
	b.WriteString("Keep file paths exactly as the user or tool result gave them.\n")
	if name := namedToolChoice(req); name != "" {
		fmt.Fprintf(&b, "Tool choice requires the client tool named %s.\n", name)
	}
	b.WriteString("Response format: your visible reply must contain only the final answer. Start with the result itself. Never begin with a restatement of the request or a description of your plan. Do not write sentences about the user, the question, or yourself, such as \"The user wants\", \"The user is asking\", \"Let me\", \"I need to\", \"This doesn't require\", or \"No tools needed\". Keep the reasoning steps that the answer genuinely needs (calculations, key facts, code) and drop everything else. Emit each code block exactly once.")
	out := make([]upstream.Message, 0, len(req.Messages)+1)
	out = append(out, upstream.Message{Role: "system", Content: b.String()})
	return append(out, req.Messages...)
}

// runtimeClientContext describes the caller environment in the shape the
// gateway reads. tool_discovery is the part that matters: it is what tells the
// agent that caller-declared tools exist at all. The key order is fixed to
// match the native client byte for byte.
func runtimeClientContext() string {
	return `{"system_type":"Windows","terminal_context":[{"shell":"PowerShell","cwd":".","source":"request-inferred"}],"client_name":"OpenAI-compatible client","context_source":"request-auto-discovery","tool_discovery":{"mode":"automatic","plugin_namespaces":[],"tool_search_available":false,"tool_search_tools":[],"environment_probe_tools":[]}}`
}

// namedToolChoice returns the tool a named tool_choice requires, if any.
func namedToolChoice(req *upstream.ChatRequest) string {
	if req.Raw == nil {
		return ""
	}
	choice, ok := req.Raw["tool_choice"]
	if !ok || choice == nil {
		return ""
	}
	m, ok := choice.(map[string]any)
	if !ok {
		return ""
	}
	kind := strings.ToLower(strings.TrimSpace(stringField(m, "type")))
	if kind != "function" && kind != "custom" {
		return ""
	}
	if name := strings.TrimSpace(stringField(m, "name")); name != "" {
		return name
	}
	if fn, ok := m["function"].(map[string]any); ok {
		return strings.TrimSpace(stringField(fn, "name"))
	}
	return ""
}

// ideNativeContent renders one message body in the native block shape: text
// becomes a single text block, structured parts are normalized in place, and a
// missing body is null.
func ideNativeContent(content any) any {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		out := make([]any, 0, len(v))
		for _, raw := range v {
			switch block := raw.(type) {
			case string:
				out = append(out, map[string]any{"type": "text", "text": block})
			case map[string]any:
				kind := strings.ToLower(strings.TrimSpace(stringField(block, "type")))
				if kind == "" || kind == "input_text" {
					kind = "text"
				}
				copied := make(map[string]any, len(block))
				for k, val := range block {
					copied[k] = val
				}
				if kind == "text" {
					copied["type"] = "text"
					if _, ok := copied["text"]; !ok {
						copied["text"] = stringField(block, "content")
					}
					delete(copied, "value")
				}
				out = append(out, copied)
			default:
				out = append(out, raw)
			}
		}
		return out
	default:
		return []any{map[string]any{"type": "text", "text": fmt.Sprint(content)}}
	}
}

// stringField reads a string field from a decoded JSON object.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
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

// toolStream adapts this upstream's tool-call spellings onto the shared
// tracker. The events usually omit an index, so a call is identified by its id
// — or, failing that, its name and arguments — and mapped to a stable slot.
// Without that, a repeated snapshot would open a new call on every event
// instead of collapsing into the one it repeats. Argument assembly and
// de-duplication are the tracker's job, not this one's.
type toolStream struct {
	tracker upstream.ToolCallTracker
	slots   map[string]int
	next    int
}

// feed folds the tool calls of one event and returns the fragments to forward.
func (s *toolStream) feed(obj map[string]any) []upstream.ToolCall {
	raw := toolCallsRaw(obj)
	if len(raw) == 0 {
		return nil
	}
	frags := make([]upstream.ToolCall, 0, len(raw))
	for _, item := range raw {
		tc, hasIndex, ok := toolCallFrom(item)
		if !ok {
			continue
		}
		if !hasIndex {
			tc.Index = s.slotFor(tc)
		}
		frags = append(frags, tc)
	}
	chunks := s.tracker.Feed(frags)
	out := make([]upstream.ToolCall, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, upstream.NewToolCall(c.Index, c.ID, c.Name, c.Args))
	}
	return out
}

// slotFor returns the stable slot of a call that named no index.
func (s *toolStream) slotFor(tc upstream.ToolCall) int {
	key := tc.ID
	if key == "" {
		key = tc.Function.Name + "#" + tc.Function.Arguments
	}
	if s.slots == nil {
		s.slots = map[string]int{}
	}
	if idx, ok := s.slots[key]; ok {
		return idx
	}
	idx := s.next
	s.slots[key] = idx
	s.next++
	return idx
}

// calls returns the calls assembled so far.
func (s *toolStream) calls() []upstream.ToolCall { return s.tracker.Calls() }

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

// toolCallFrom flattens one tool-call object. hasIndex reports whether the
// object carried an index of its own; an explicit index always wins, so a
// fragmented upstream keeps its own numbering.
func toolCallFrom(item any) (tc upstream.ToolCall, hasIndex, ok bool) {
	obj, isMap := item.(map[string]any)
	if !isMap {
		return upstream.ToolCall{}, false, false
	}
	tc = upstream.ToolCall{Type: "function"}
	if idx, had := intField(obj, "index"); had {
		tc.Index, hasIndex = idx, true
	}
	tc.ID = firstString(obj, "id", "tool_call_id", "call_id")
	fn, _ := obj["function"].(map[string]any)
	if fn == nil {
		// The native SOLO stream names the field function_call and splits a
		// call across fragments: the first carries id and name, the following
		// ones only arguments.
		if fc, ok := obj["function_call"].(map[string]any); ok {
			fn = fc
		}
	}
	if fn == nil {
		fn = obj
	}
	tc.Function.Name = firstString(fn, "name", "tool_name", "function_name")
	tc.Function.Arguments = argumentsText(fn)
	if tc.Function.Name == "" && tc.Function.Arguments == "" {
		// Neither identity nor payload: nothing to fold into the slot.
		return upstream.ToolCall{}, false, false
	}
	return tc, hasIndex, true
}

// intField reads an integer field that may have arrived as a JSON number.
func intField(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// argumentsText renders tool arguments as the JSON string the OpenAI protocol
// requires, whether the upstream sent an object or a string.
func argumentsText(fn map[string]any) string {
	for _, key := range []string{"arguments", "partial_arguments", "args", "parameters", "input"} {
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
	// Empty means "this fragment carries no arguments", which is not the same
	// as "the arguments are an empty object": the native stream opens a call
	// with id and name and ships the arguments in a later fragment. Returning
	// "{}" here would be folded into the slot as a literal prefix.
	return ""
}

// nativeToolDefinitions converts the caller's OpenAI tool definitions into the
// native shape. The one difference is that `function.parameters` is a JSON
// *string* here, not an object — the upstream's protobuf/JSON adapter rejects
// an object with 4001.
func nativeToolDefinitions(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			// The compact shape some clients use.
			function = map[string]any{}
			for _, key := range []string{"name", "description", "parameters", "strict"} {
				if v, ok := tool[key]; ok {
					function[key] = v
				}
			}
		}
		name := stringField(function, "name")
		if name == "" {
			continue
		}
		parameters := function["parameters"]
		if parameters == nil {
			parameters = map[string]any{}
		}
		if _, isStr := parameters.(string); !isStr {
			parameters = jsonString(parameters)
		}
		fn := map[string]any{
			"name":        name,
			"description": stringField(function, "description"),
			"parameters":  parameters,
		}
		if strict, ok := function["strict"]; ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// nativeToolChoice maps an OpenAI tool_choice onto the native spelling, which
// is a string: auto/none/required or the name of one declared tool.
func nativeToolChoice(v any) string {
	switch c := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(c))
		}
		return strings.TrimSpace(c)
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringField(c, "type")))
		switch kind {
		case "auto", "none", "required":
			return kind
		case "function", "custom", "tool":
			name := stringField(c, "name")
			if fn, ok := c["function"].(map[string]any); ok {
				name = stringField(fn, "name")
			}
			return strings.TrimSpace(name)
		}
	}
	return ""
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
