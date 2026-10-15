package api

// The OpenAI Responses wire format (/v1/responses, used by Codex).
//
// Inbound, Responses items are folded onto the same chat vocabulary the other
// two wires produce, so every source sees one shape. Outbound, the answer is
// rendered as sequence-numbered Responses events. Reasoning is private unless
// the caller explicitly asks for it, and a previous_response_id chain is served
// from an in-memory LRU (see responses_state.go) — never from disk.

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

const responsesWireName = "responses"

// responsesDefaultMaxTokens caps a completion when max_output_tokens is absent
// or unusable.
const responsesDefaultMaxTokens = 64000

// responseRequestError marks a malformed Responses request.
type responseRequestError struct {
	msg   string
	param string
}

func (e *responseRequestError) Error() string { return e.msg }

func responseErr(msg, param string) error { return &responseRequestError{msg: msg, param: param} }

// ── inbound ─────────────────────────────────────────────────────────────────

// responsesTurn reads a Responses body and registers the resulting turn.
func (s *Server) parseResponsesTurn(r *http.Request, body map[string]any) (*turn, error) {
	oai, ctx, err := s.responsesToOpenAI(body)
	if err != nil {
		return nil, err
	}
	stream, _ := body["stream"].(bool)
	msgs, repairs := upstream.RepairToolPairing(parseMessages(oai["messages"]))
	for _, rep := range repairs {
		logx.Warnf("responses inbound %s dropped (tool call id=%q)", rep.Reason, rep.ID)
	}
	return &turn{
		Name:      responsesWireName,
		Model:     ctx.model,
		Stream:    stream,
		Session:   sessionKey(r, "", msgs, openAIToolNames(oai["tools"])),
		IdleAfter: wireIdleKeepalive,
		Request: &upstream.ChatRequest{
			Model:       ctx.model,
			Messages:    msgs,
			Stream:      stream,
			Temperature: parseFloat(oai["temperature"]),
			MaxTokens:   parseInt(oai["max_tokens"]),
			Tools:       parseSlice(oai["tools"]),
			Raw:         oai,
		},
		Result: func(w http.ResponseWriter, resp *upstream.ChatResponse) {
			writeJSON(w, http.StatusOK, s.responsesObject(ctx, resp))
		},
		Open: func(w http.ResponseWriter) (responder, error) {
			return s.newResponsesStream(w, ctx)
		},
		Fail: responsesTurnFail,
	}, nil
}

// responsesToOpenAI translates a Responses body into the chat vocabulary, and
// returns the context the answer will be rendered against.
func (s *Server) responsesToOpenAI(body map[string]any) (map[string]any, *responsesContext, error) {
	if _, ok := body["input"]; !ok {
		return nil, nil, responseErr("input is required", "input")
	}

	var previous *responsesSession
	if raw, ok := body["previous_response_id"]; ok && raw != nil {
		id, ok := raw.(string)
		if !ok || id == "" {
			return nil, nil, responseErr("previous_response_id must be a non-empty string", "previous_response_id")
		}
		previous = s.respCache.get(id)
		if previous == nil {
			return nil, nil, responseErr("previous_response_id was not found or has expired; resend the full input", "previous_response_id")
		}
		if previous.UnavailableReason != "" {
			return nil, nil, responseErr(previous.UnavailableReason, "previous_response_id")
		}
	}

	responseID := randomID("resp_")
	upstreamSessionID := responseID
	if previous != nil && previous.UpstreamSessionID != "" {
		upstreamSessionID = previous.UpstreamSessionID
	}

	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = "auto"
	}

	// Tools declared now, plus any the client replayed as items, plus what the
	// chain already knew.
	declaredTools, hasDeclared := body["tools"]
	replayTools := collectReplayTools(body["input"])
	combined := declaredTools
	if len(replayTools) > 0 {
		list, _ := declaredTools.([]any)
		combined = append(append([]any{}, list...), replayTools...)
	}
	inheritedBindings := map[string]responsesBinding{}
	inheritedTools := []any{}
	if previous != nil {
		inheritedTools = cloneAnySlice(previous.Tools)
		for name, b := range previous.Bindings {
			inheritedBindings[name] = b
		}
		for _, b := range previous.CallBindings {
			if _, ok := inheritedBindings[b.ChatName]; !ok {
				inheritedBindings[b.ChatName] = b
			}
		}
	}
	chatTools, bindings, reverse, err := normalizeResponsesTools(combined, inheritedBindings)
	if err != nil {
		return nil, nil, err
	}

	callBindings := map[string]responsesBinding{}
	history := []any{}
	if previous != nil {
		history = cloneAnySlice(previous.Messages)
		for id, b := range previous.CallBindings {
			callBindings[id] = b
		}
	}

	current, err := responsesInputMessages(body["input"], bindings, callBindings, reverse)
	if err != nil {
		return nil, nil, err
	}

	var instructions []any
	switch v := body["instructions"].(type) {
	case string:
		if v != "" {
			instructions = append(instructions, map[string]any{"role": "developer", "content": v})
		}
	case []any:
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			copied := map[string]any{}
			for k, val := range item {
				copied[k] = val
			}
			if stringField(copied, "role") == "" {
				copied["role"] = "developer"
			}
			instructions = appendResponsesMessage(instructions, copied)
		}
	}

	merged := mergeResponsesHistory(history, current)
	messages := append(append([]any{}, instructions...), merged...)
	if len(messages) == 0 {
		return nil, nil, responseErr("input must contain at least one message", "input")
	}

	oai := map[string]any{"messages": messages}

	declared := hasDeclared == true || len(replayTools) > 0
	if declared {
		oai["tools"] = chatTools
	} else if len(inheritedTools) > 0 {
		oai["tools"] = inheritedTools
	}
	if raw, ok := body["tool_choice"]; ok && raw != nil {
		choice, err := normalizeResponsesToolChoice(raw, reverse)
		if err != nil {
			return nil, nil, err
		}
		oai["tool_choice"] = choice
	}
	if raw, ok := body["parallel_tool_calls"]; ok && raw != nil {
		oai["parallel_tool_calls"] = raw
	}
	ceiling := s.cfg.Upstreams.TraeCN.MaxTokens
	if ceiling <= 0 {
		ceiling = responsesDefaultMaxTokens
	}
	maxTokens := clampMaxTokens(body["max_output_tokens"], ceiling)
	if maxTokens == nil {
		defaulted := responsesDefaultMaxTokens
		maxTokens = &defaulted
	}
	oai["max_tokens"] = *maxTokens
	// Generation controls worth forwarding to the upstream.
	for _, key := range []string{
		"temperature", "top_p", "stop", "presence_penalty", "frequency_penalty",
		"seed", "reasoning_effort", "response_format", "service_tier", "user",
		"logprobs", "top_logprobs",
	} {
		if v, ok := body[key]; ok && v != nil {
			oai[key] = v
		}
	}
	if effort := responsesReasoningEffort(body["reasoning"]); effort != nil {
		if _, ok := oai["reasoning_effort"]; !ok {
			oai["reasoning_effort"] = effort
		}
	}

	thinking := reasoningRequested(body["thinking"]) || reasoningRequested(body["include_reasoning"])

	ctx := &responsesContext{
		responseID:        responseID,
		model:             model,
		createdAt:         nowUnix(),
		request:           body,
		messages:          cloneAnySlice(merged),
		bindings:          bindings,
		callBindings:      callBindings,
		tools:             cloneAnySlice(chatTools),
		upstreamSessionID: upstreamSessionID,
		thinking:          thinking,
		toolChoice:        body["tool_choice"],
	}
	if !declared && len(inheritedTools) > 0 {
		ctx.tools = cloneAnySlice(inheritedTools)
	}
	return oai, ctx, nil
}

// responsesInputMessages folds the `input` value (a string or a list of items)
// onto chat messages.
func responsesInputMessages(input any, bindings map[string]responsesBinding, callBindings map[string]responsesBinding, reverse map[bindingKey]string) ([]any, error) {
	switch v := input.(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": v}}, nil
	case []any:
		return responsesItems(v, bindings, callBindings, reverse)
	}
	return nil, responseErr("input must be a string or array", "input")
}

// collectReplayTools gathers tool declarations a client replayed as input items.
func collectReplayTools(input any) []any {
	list, ok := input.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(item, "type") {
		case "additional_tools", "tool_search_output":
			if tools, ok := item["tools"].([]any); ok {
				out = append(out, tools...)
			}
		}
	}
	return out
}

// ── outbound ────────────────────────────────────────────────────────────────

// responsesStream renders a streamed answer as Responses events, each carrying
// a sequence number.
type responsesStream struct {
	stream *sseStream
	ctx    *responsesContext
	seq    int

	outputs        []any
	textItem       map[string]any
	textIndex      int
	reasoningItem  map[string]any
	reasoningIndex int
	reasoningRaw   string
	tools          upstream.ToolCallTracker
	toolStates     map[int]*responsesToolState
	usage          *upstream.Usage
	finish         string
	remember       func()
}

type responsesToolState struct {
	item      map[string]any
	index     int
	binding   responsesBinding
	callID    string
	arguments string
}

func (srv *Server) newResponsesStream(w http.ResponseWriter, ctx *responsesContext) (responder, error) {
	s, err := newSSEStream(w, sseDialect{})
	if err != nil {
		return nil, err
	}
	r := &responsesStream{
		stream:     s,
		ctx:        ctx,
		textIndex:  -1,
		toolStates: map[int]*responsesToolState{},
	}
	r.remember = func() {
		if stored, ok := ctx.request["store"].(bool); ok && !stored {
			return
		}
		srv.rememberResponsesTurn(ctx, r.outputs)
	}
	// The envelope is written before the first token: Codex treats a missing
	// created event as a stalled request.
	r.event("response.created", map[string]any{"response": responseObject(ctx, "in_progress", []any{}, nil, nil, nil)})
	r.event("response.in_progress", map[string]any{"response": responseObject(ctx, "in_progress", []any{}, nil, nil, nil)})
	return r, nil
}

func (r *responsesStream) Delta(d upstream.StreamDelta) error {
	if d.ReasoningContent != "" && r.ctx.thinking {
		r.reasoningRaw += d.ReasoningContent
	}
	if d.Content != "" {
		r.appendText(d.Content)
	}
	for _, chunk := range r.tools.Feed(d.ToolCalls) {
		r.appendTool(chunk)
	}
	if d.Usage != nil {
		u := *d.Usage
		r.usage = &u
	}
	if d.FinishReason != "" {
		r.finish = d.FinishReason
	}
	return nil
}

func (r *responsesStream) End() {
	if r.ctx.thinking {
		if summary := compactReasoningSummary(r.reasoningRaw); summary != "" {
			r.finishReasoning(summary)
		}
	}
	if r.textItem != nil {
		if len(r.toolStates) > 0 {
			r.textItem["phase"] = "commentary"
		} else {
			r.textItem["phase"] = "final_answer"
		}
		text := r.textValue()
		r.event("response.output_text.done", map[string]any{
			"item_id": r.textItem["id"], "output_index": r.textIndex,
			"content_index": 0, "text": text, "logprobs": []any{},
		})
		r.event("response.content_part.done", map[string]any{
			"item_id": r.textItem["id"], "output_index": r.textIndex,
			"content_index": 0, "part": r.textPart(),
		})
		r.event("response.output_item.done", map[string]any{
			"output_index": r.textIndex, "item": r.textItem,
		})
	}
	indexes := r.toolIndexes()
	for _, idx := range indexes {
		r.finishTool(r.toolStates[idx])
	}

	status, incomplete := "completed", any(nil)
	switch strings.ToLower(strings.TrimSpace(r.finish)) {
	case "length", "max_tokens", "max_output_tokens":
		status, incomplete = "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status, incomplete = "incomplete", map[string]any{"reason": "content_filter"}
	}
	name := "response.completed"
	if status != "completed" {
		name = "response.incomplete"
	}
	resp := responseObject(r.ctx, status, r.outputs, responsesUsage(r.usage), nil, incomplete)
	r.store()
	r.event(name, map[string]any{"response": resp})
}

func (r *responsesStream) Fail(err error) {
	r.event("response.failed", map[string]any{"response": responseObject(
		r.ctx, "failed", r.outputs, responsesUsage(r.usage),
		map[string]any{"code": "upstream_error", "message": err.Error()}, nil,
	)})
}

func (r *responsesStream) appendText(content string) {
	if r.textItem == nil {
		r.textIndex = len(r.outputs)
		r.textItem = map[string]any{
			"id": randomID("msg_"), "type": "message", "role": "assistant",
			"status": "in_progress", "content": []any{r.textPart()},
		}
		r.outputs = append(r.outputs, r.textItem)
		opening := map[string]any{}
		for k, v := range r.textItem {
			opening[k] = v
		}
		opening["content"] = []any{}
		r.event("response.output_item.added", map[string]any{"output_index": r.textIndex, "item": opening})
		r.event("response.content_part.added", map[string]any{
			"item_id": r.textItem["id"], "output_index": r.textIndex,
			"content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""},
		})
	}
	r.textItem["content"] = []any{map[string]any{
		"type": "output_text", "annotations": []any{}, "text": r.textValue() + content,
	}}
	r.event("response.output_text.delta", map[string]any{
		"item_id": r.textItem["id"], "output_index": r.textIndex,
		"content_index": 0, "delta": content, "logprobs": []any{},
	})
}

func (r *responsesStream) textValue() string {
	if r.textItem == nil {
		return ""
	}
	parts, _ := r.textItem["content"].([]any)
	if len(parts) == 0 {
		return ""
	}
	part, _ := parts[0].(map[string]any)
	return stringField(part, "text")
}

func (r *responsesStream) textPart() map[string]any {
	return map[string]any{"type": "output_text", "annotations": []any{}, "text": r.textValue()}
}

func (r *responsesStream) appendTool(chunk upstream.ToolChunk) {
	state, ok := r.toolStates[chunk.Index]
	if !ok {
		state = &responsesToolState{index: chunk.Index}
		r.toolStates[chunk.Index] = state
	}
	if state.callID == "" {
		state.callID = chunk.ID
	}
	if state.binding.ChatName == "" && chunk.Name != "" {
		state.binding = r.ctx.resolveBinding(chunk.Name)
		if state.callID == "" {
			state.callID = randomID("call_")
		}
		r.ctx.callBindings[state.callID] = state.binding
	}

	if state.item == nil && state.binding.ChatName != "" {
		state.item = r.newToolItem(state)
		state.index = len(r.outputs)
		r.outputs = append(r.outputs, state.item)
		r.event("response.output_item.added", map[string]any{"output_index": state.index, "item": state.item})
	}
	if chunk.Args == "" || state.item == nil {
		return
	}
	state.arguments += chunk.Args
	if state.binding.ResponseType == "function" {
		r.event("response.function_call_arguments.delta", map[string]any{
			"item_id": state.item["id"], "output_index": state.index, "delta": chunk.Args,
		})
	}
}

func (r *responsesStream) newToolItem(state *responsesToolState) map[string]any {
	switch state.binding.ResponseType {
	case "custom":
		item := map[string]any{
			"id": randomID("ctc_"), "type": "custom_tool_call", "call_id": state.callID,
			"name": state.binding.Name, "input": "", "status": "in_progress",
		}
		addNamespace(item, state.binding)
		return item
	case "tool_search":
		return map[string]any{
			"id": randomID("ts_"), "type": "tool_search_call", "call_id": state.callID,
			"arguments": map[string]any{}, "execution": orDefault(state.binding.Execution, "client"),
			"status": "in_progress",
		}
	default:
		item := map[string]any{
			"id": randomID("fc_"), "type": "function_call", "call_id": state.callID,
			"name": state.binding.Name, "arguments": "", "status": "in_progress",
		}
		addNamespace(item, state.binding)
		return item
	}
}

func (r *responsesStream) finishTool(state *responsesToolState) {
	if state == nil || state.item == nil {
		return
	}
	switch state.binding.ResponseType {
	case "custom":
		input := decodeCustomInput(state.arguments)
		state.item["input"] = input
		state.item["status"] = "completed"
		if input != "" {
			r.event("response.custom_tool_call_input.delta", map[string]any{
				"item_id": state.item["id"], "output_index": state.index, "delta": input,
			})
		}
		r.event("response.custom_tool_call_input.done", map[string]any{
			"item_id": state.item["id"], "output_index": state.index, "input": input,
		})
	case "tool_search":
		state.item["arguments"] = parseLooseJSON(state.arguments, "query")
		state.item["status"] = "completed"
	default:
		state.item["arguments"] = state.arguments
		state.item["status"] = "completed"
		r.event("response.function_call_arguments.done", map[string]any{
			"item_id": state.item["id"], "output_index": state.index,
			"name": state.item["name"], "arguments": state.arguments,
		})
	}
	r.event("response.output_item.done", map[string]any{"output_index": state.index, "item": state.item})
}

func (r *responsesStream) finishReasoning(summary string) {
	if r.reasoningItem == nil {
		r.reasoningIndex = len(r.outputs)
		r.reasoningItem = map[string]any{
			"id": randomID("rs_"), "type": "reasoning", "status": "in_progress", "summary": []any{},
		}
		r.outputs = append(r.outputs, r.reasoningItem)
		r.event("response.output_item.added", map[string]any{
			"output_index": r.reasoningIndex, "item": r.reasoningItem,
		})
	}
	part := map[string]any{"type": "summary_text", "text": summary}
	r.reasoningItem["summary"] = []any{part}
	r.event("response.reasoning_summary_part.added", map[string]any{
		"item_id": r.reasoningItem["id"], "output_index": r.reasoningIndex,
		"summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""},
	})
	r.event("response.reasoning_summary_text.delta", map[string]any{
		"item_id": r.reasoningItem["id"], "output_index": r.reasoningIndex,
		"summary_index": 0, "delta": summary,
	})
	r.reasoningItem["status"] = "completed"
	r.event("response.reasoning_summary_text.done", map[string]any{
		"item_id": r.reasoningItem["id"], "output_index": r.reasoningIndex,
		"summary_index": 0, "text": summary,
	})
	r.event("response.reasoning_summary_part.done", map[string]any{
		"item_id": r.reasoningItem["id"], "output_index": r.reasoningIndex,
		"summary_index": 0, "part": part,
	})
	r.event("response.output_item.done", map[string]any{
		"output_index": r.reasoningIndex, "item": r.reasoningItem,
	})
}

func (r *responsesStream) toolIndexes() []int {
	out := make([]int, 0, len(r.toolStates))
	for idx := range r.toolStates {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func (r *responsesStream) event(name string, payload map[string]any) {
	body := make(map[string]any, len(payload)+2)
	body["type"] = name
	body["sequence_number"] = r.seq
	r.seq++
	for k, v := range payload {
		body[k] = v
	}
	_ = r.stream.write(sseEvent(name, body))
}

// remember stores the turn for a future previous_response_id, unless the client
// asked for it not to be stored.
func (r *responsesStream) store() {
	if r.remember != nil {
		r.remember()
	}
}

// responsesObject renders a completed (non-streaming) answer.
func (s *Server) responsesObject(ctx *responsesContext, resp *upstream.ChatResponse) map[string]any {
	output := []any{}
	if ctx.thinking {
		if summary := compactReasoningSummary(resp.ReasoningContent); summary != "" {
			output = append(output, reasoningOutputItem(summary))
		}
	}
	if resp.Content != "" {
		phase := "final_answer"
		if len(resp.ToolCalls) > 0 {
			phase = "commentary"
		}
		output = append(output, map[string]any{
			"id": randomID("msg_"), "type": "message", "role": "assistant", "status": "completed",
			"phase":   phase,
			"content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "text": resp.Content}},
		})
	}
	for _, call := range resp.ToolCalls {
		output = append(output, s.toolOutputItem(call, ctx))
	}
	status, incomplete := "completed", any(nil)
	switch strings.ToLower(strings.TrimSpace(resp.FinishReason)) {
	case "length", "max_tokens", "max_output_tokens":
		status, incomplete = "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status, incomplete = "incomplete", map[string]any{"reason": "content_filter"}
	}
	if entry, ok := ctx.request["store"].(bool); !ok || entry {
		s.rememberResponsesTurn(ctx, output)
	}
	return responseObject(ctx, status, output, responsesUsage(&resp.Usage), nil, incomplete)
}

// toolOutputItem renders one tool call as a Responses output item.
func (s *Server) toolOutputItem(call upstream.ToolCall, ctx *responsesContext) map[string]any {
	callID := call.ID
	if callID == "" {
		callID = randomID("call_")
	}
	binding := ctx.resolveBinding(call.Function.Name)
	ctx.callBindings[callID] = binding
	switch binding.ResponseType {
	case "custom":
		item := map[string]any{
			"id": randomID("ctc_"), "type": "custom_tool_call", "call_id": callID,
			"name": binding.Name, "input": decodeCustomInput(call.Function.Arguments), "status": "completed",
		}
		addNamespace(item, binding)
		return item
	case "tool_search":
		return map[string]any{
			"id": randomID("ts_"), "type": "tool_search_call", "call_id": callID,
			"arguments": parseLooseJSON(call.Function.Arguments, "query"),
			"execution": orDefault(binding.Execution, "client"), "status": "completed",
		}
	default:
		item := map[string]any{
			"id": randomID("fc_"), "type": "function_call", "call_id": callID,
			"name": binding.Name, "arguments": call.Function.Arguments, "status": "completed",
		}
		addNamespace(item, binding)
		return item
	}
}

// ── shared render helpers ───────────────────────────────────────────────────

func responseObject(ctx *responsesContext, status string, output []any, usage, errObj, incomplete any) map[string]any {
	request := ctx.request
	instructions := request["instructions"]
	obj := map[string]any{
		"id":                   ctx.responseID,
		"object":               "response",
		"created_at":           ctx.createdAt,
		"status":               status,
		"background":           false,
		"error":                errObj,
		"incomplete_details":   incomplete,
		"instructions":         instructions,
		"max_output_tokens":    request["max_output_tokens"],
		"model":                ctx.model,
		"output":               output,
		"parallel_tool_calls":  boolOr(request["parallel_tool_calls"], true),
		"previous_response_id": request["previous_response_id"],
		"reasoning":            request["reasoning"],
		"service_tier":         orDefaultAny(request["service_tier"], "default"),
		"temperature":          request["temperature"],
		"text":                 orDefaultAny(request["text"], map[string]any{"format": map[string]any{"type": "text"}}),
		"tool_choice":          orDefaultAny(request["tool_choice"], "auto"),
		"tools":                orDefaultAny(request["tools"], []any{}),
		"top_p":                request["top_p"],
		"truncation":           orDefaultAny(request["truncation"], "disabled"),
		"usage":                usage,
	}
	return obj
}

func reasoningOutputItem(summary string) map[string]any {
	summaryParts := []any{}
	if summary != "" {
		summaryParts = append(summaryParts, map[string]any{"type": "summary_text", "text": summary})
	}
	return map[string]any{
		"id": randomID("rs_"), "type": "reasoning", "status": "completed", "summary": summaryParts,
	}
}

func responsesUsage(u *upstream.Usage) any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":          u.PromptTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": 0, "cache_write_tokens": 0},
		"output_tokens":         u.CompletionTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          u.TotalTokens,
	}
}

func addNamespace(item map[string]any, binding responsesBinding) {
	if binding.Namespace != "" {
		item["namespace"] = binding.Namespace
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func orDefaultAny(v, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

func boolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// ── errors ──────────────────────────────────────────────────────────────────

func writeResponsesError(w http.ResponseWriter, code int, typ, msg, param string) {
	if typ == "" {
		typ = "invalid_request_error"
	}
	errObj := map[string]any{"type": typ, "message": msg}
	if param != "" {
		errObj["param"] = param
	}
	writeJSON(w, code, map[string]any{"error": errObj})
}

// responsesTurnFail renders a failure in the Responses error envelope.
func responsesTurnFail(w http.ResponseWriter, err error, client bool) {
	if client {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue != nil {
		code, typ := responseErrorShape(ue)
		if ue.RetryAfter > 0 {
			setRetryAfter(w, ue.RetryAfter)
		}
		writeResponsesError(w, code, typ, ue.Message, "")
		return
	}
	var na *pool.ErrNoAccount
	if errors.As(err, &na) {
		if na.RetryAfter > 0 {
			setRetryAfter(w, na.RetryAfter)
		}
		writeResponsesError(w, http.StatusServiceUnavailable, "server_error", "no available account: "+na.Reason, "")
		return
	}
	writeResponsesError(w, http.StatusServiceUnavailable, "server_error", err.Error(), "")
}

func responseErrorShape(ue *upstream.Error) (int, string) {
	switch ue.Kind {
	case upstream.KindBadRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case upstream.KindContentBlocked:
		return http.StatusBadRequest, "invalid_request_error"
	case upstream.KindAuth:
		return http.StatusUnauthorized, "invalid_request_error"
	case upstream.KindRateLimited, upstream.KindQuota:
		return http.StatusTooManyRequests, "rate_limit_error"
	case upstream.KindTimeout:
		return http.StatusGatewayTimeout, "server_error"
	default:
		return http.StatusInternalServerError, "server_error"
	}
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed", "")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	t, err := s.parseResponsesTurn(r, body)
	if err != nil {
		var rre *responseRequestError
		if errors.As(err, &rre) {
			writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", rre.msg, rre.param)
			return
		}
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	s.runChat(w, r, t)
}
