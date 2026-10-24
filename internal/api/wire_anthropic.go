package api

// The Anthropic Messages wire format (/v1/messages and /v1/messages/count_tokens).
//
// Inbound, an Anthropic request is translated into the OpenAI-shaped vocabulary
// the rest of the pipeline already speaks, and the neutral messages are parsed
// back out of it. Outbound, the neutral answer is rendered as Anthropic
// content blocks. Reasoning travels as a thinking block (with an empty
// signature: the upstream is not Anthropic, so there is no real one to relay,
// and the field is opaque to a client either way).
//
// Two behaviours follow the reference relay rather than the letter of the
// official spec, because a relay exists to keep a turn moving:
//   - a missing max_tokens is filled with a default instead of rejected;
//   - an image is passed through to the upstream, and an image inside a tool
//     result is reported as a visible placeholder rather than dropped.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

const anthropicWireName = "anthropic"

// anthropicDefaultMaxTokens caps a completion when the client omits max_tokens.
const anthropicDefaultMaxTokens = 32000

// wireIdleKeepalive is how long the upstream may stay silent before the
// answer is opened anyway with message_start + ping. Claude Code aborts a
// request whose message_start never arrives, so the opening must not wait on a
// slow first token forever; the window is short enough that a fast failure
// still fails over on another account. It is a variable so a test can shrink
// it.
var wireIdleKeepalive = 15 * time.Second

// anthropicRequestError marks a malformed Anthropic request, carrying the field
// at fault so the response reads like the official one.
type anthropicRequestError struct {
	msg   string
	param string
}

func (e *anthropicRequestError) Error() string { return e.msg }

func anthropicErr(msg, param string) error { return &anthropicRequestError{msg: msg, param: param} }

// ── inbound ─────────────────────────────────────────────────────────────────

// parseAnthropicTurn reads an Anthropic Messages body.
func parseAnthropicTurn(r *http.Request, body map[string]any) (*turn, error) {
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, anthropicErr("model: field required", "model")
	}
	oai, err := anthropicToOpenAI(body)
	if err != nil {
		return nil, err
	}
	stream, _ := body["stream"].(bool)
	msgs, repairs := upstream.RepairToolPairing(parseMessages(oai["messages"]))
	for _, rep := range repairs {
		logx.Dialog.Warnf("anthropic inbound %s dropped (tool call id=%q)", rep.Reason, rep.ID)
	}
	inputTokens := anthropicInputEstimate(body)

	return &turn{
		Name:      anthropicWireName,
		Model:     model,
		Stream:    stream,
		Session:   sessionKey(r, "", msgs, openAIToolNames(oai["tools"])),
		IdleAfter: wireIdleKeepalive,
		Request: &upstream.ChatRequest{
			Model:       model,
			Messages:    msgs,
			Stream:      stream,
			Temperature: parseFloat(oai["temperature"]),
			MaxTokens:   parseInt(oai["max_tokens"]),
			Tools:       parseSlice(oai["tools"]),
			Raw:         oai,
		},
		Result: func(w http.ResponseWriter, resp *upstream.ChatResponse) {
			writeJSON(w, http.StatusOK, anthropicMessage(resp, model, inputTokens))
		},
		Open: func(w http.ResponseWriter) (responder, error) {
			return newAnthropicStream(w, model, inputTokens)
		},
		Fail: anthropicTurnFail,
	}, nil
}

// anthropicTurnFail renders a failure in the Anthropic error envelope, mapping
// the gateway's error kinds onto the official error types.
func anthropicTurnFail(w http.ResponseWriter, err error, client bool) {
	if client {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		if ue != nil && ue.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ue.RetryAfter.Seconds()))))
		}
		code, typ := anthropicErrorShape(ue)
		msg := "upstream error"
		if ue != nil && ue.Message != "" {
			msg = ue.Message
		}
		writeAnthropicError(w, code, typ, msg)
		return
	}
	var na *pool.ErrNoAccount
	if errors.As(err, &na) {
		if na.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(na.RetryAfter.Seconds()))))
		}
		writeAnthropicError(w, statusAnthropicOverloaded, "overloaded_error", "no available account: "+na.Reason)
		return
	}
	writeAnthropicError(w, statusAnthropicOverloaded, "overloaded_error", err.Error())
}

// statusAnthropicOverloaded is the official status for an overloaded service.
const statusAnthropicOverloaded = 529

// anthropicErrorShape maps an upstream failure onto the official status and
// error type.
func anthropicErrorShape(ue *upstream.Error) (int, string) {
	if ue == nil {
		return http.StatusBadGateway, "api_error"
	}
	switch ue.Kind {
	case upstream.KindBadRequest, upstream.KindContentBlocked:
		return http.StatusBadRequest, "invalid_request_error"
	case upstream.KindAuth:
		return http.StatusUnauthorized, "authentication_error"
	case upstream.KindRateLimited, upstream.KindQuota:
		return http.StatusTooManyRequests, "rate_limit_error"
	case upstream.KindTimeout:
		return http.StatusGatewayTimeout, "timeout_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}

// anthropicToOpenAI translates an Anthropic Messages body into the
// OpenAI-shaped vocabulary every adapter already understands. The translation is
// the single place the two vocabularies meet, so the messages the adapters send
// and the ones the neutral layer parses are guaranteed to agree.
func anthropicToOpenAI(body map[string]any) (map[string]any, error) {
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, anthropicErr("messages: field required", "messages")
	}

	out := map[string]any{}
	var msgs []any
	if sys := anthropicSystemText(body["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	for i, raw := range rawMessages {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, anthropicErr(fmt.Sprintf("messages.%d: must be an object", i), fmt.Sprintf("messages.%d", i))
		}
		role := strings.ToLower(strings.TrimSpace(stringField(m, "role")))
		switch role {
		case "assistant":
			if msg, ok := assistantToOpenAI(m["content"]); ok {
				msgs = append(msgs, msg)
			}
		case "user":
			msgs = append(msgs, userToOpenAI(m["content"])...)
		default:
			return nil, anthropicErr(fmt.Sprintf("messages.%d.role: must be user or assistant", i), fmt.Sprintf("messages.%d.role", i))
		}
	}

	if !hasConversationalTurn(msgs) {
		return nil, anthropicErr("messages: must contain a user turn", "messages")
	}
	out["messages"] = msgs

	maxTokens := intOfAny(body["max_tokens"])
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	out["max_tokens"] = maxTokens
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := body[key]; ok && v != nil {
			out[key] = v
		}
	}
	if v, ok := body["stop_sequences"]; ok && v != nil {
		out["stop"] = v
	}
	if tools := anthropicTools(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice := anthropicToolChoice(body["tool_choice"]); choice != nil {
		out["tool_choice"] = choice
	}
	return out, nil
}

// assistantToOpenAI folds an assistant turn into a Chat Completions message.
// tool_use blocks become tool_calls; text blocks become content; thinking
// blocks are inert on the way up and are dropped, as the upstream regenerates
// its own.
func assistantToOpenAI(content any) (map[string]any, bool) {
	var texts []string
	var toolCalls []any
	for _, block := range anthropicBlockList(content) {
		switch blockType(block) {
		case "tool_use":
			name := stringField(block, "name")
			if name == "" {
				continue
			}
			id := stringField(block, "id")
			if id == "" {
				id = randomID("call_")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": jsonString(orEmptyObject(block["input"])),
				},
			})
		case "text", "input_text":
			if t := stringField(block, "text"); t != "" {
				texts = append(texts, t)
			}
		}
	}
	msg := map[string]any{"role": "assistant"}
	text := strings.Join(texts, "\n")
	switch {
	case text != "":
		msg["content"] = text
	case len(toolCalls) == 0:
		msg["content"] = ""
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if text == "" && len(toolCalls) == 0 {
		return nil, false
	}
	return msg, true
}

// userToOpenAI folds a user turn into one or more messages: each tool_result
// becomes a Chat Completions tool message so call correlation survives, and the
// remaining text and images stay in a user turn.
func userToOpenAI(content any) []any {
	var out []any
	var textParts []string
	var imageParts []any
	for _, block := range anthropicBlockList(content) {
		switch blockType(block) {
		case "tool_result":
			text := anthropicToolResultText(block["content"])
			if b, _ := block["is_error"].(bool); b {
				text = "Client tool result status=failed:\n" + text
			}
			id := stringField(block, "tool_use_id")
			if id == "" {
				id = "unknown"
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
		case "text", "input_text":
			if t := stringField(block, "text"); t != "" {
				textParts = append(textParts, t)
			}
		case "image":
			if url := anthropicImageURL(block["source"]); url != "" {
				imageParts = append(imageParts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		}
	}
	switch {
	case len(imageParts) > 0:
		parts := make([]any, 0, len(textParts)+len(imageParts))
		for _, t := range textParts {
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
		parts = append(parts, imageParts...)
		out = append(out, map[string]any{"role": "user", "content": parts})
	case len(textParts) > 0:
		out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
	}
	return out
}

// anthropicSystemText flattens the top-level system prompt, which may be a
// string or a list of text blocks.
func anthropicSystemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, raw := range s {
			block, ok := raw.(map[string]any)
			if !ok {
				if text, ok := raw.(string); ok {
					parts = append(parts, text)
				}
				continue
			}
			if t := stringField(block, "text"); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// anthropicBlockList normalizes a message body into a list of content blocks.
func anthropicBlockList(content any) []map[string]any {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		return []map[string]any{{"type": "text", "text": v}}
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				out = append(out, map[string]any{"type": "text", "text": s})
				continue
			}
			if block, ok := raw.(map[string]any); ok {
				out = append(out, block)
			}
		}
		return out
	}
	return nil
}

// anthropicToolResultText flattens a tool_result body. A nested image cannot be
// forwarded as text, so it is reported as a visible placeholder rather than
// silently dropped.
func anthropicToolResultText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, raw := range v {
			block, ok := raw.(map[string]any)
			if !ok {
				if s, ok := raw.(string); ok {
					parts = append(parts, s)
				}
				continue
			}
			switch blockType(block) {
			case "text", "input_text":
				parts = append(parts, stringField(block, "text"))
			case "image":
				parts = append(parts, "[image tool result omitted]")
			default:
				parts = append(parts, jsonString(block))
			}
		}
		return strings.Join(nonEmpty(parts), "\n")
	default:
		return jsonString(content)
	}
}

// anthropicImageURL turns an Anthropic image source into a URL an upstream
// accepts: a base64 source becomes a data URL, a url source is passed through.
func anthropicImageURL(source any) string {
	src, ok := source.(map[string]any)
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(stringField(src, "type"))) {
	case "base64":
		data := stringField(src, "data")
		if data == "" {
			return ""
		}
		media := stringField(src, "media_type")
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		return stringField(src, "url")
	}
	return ""
}

// anthropicTools converts Anthropic tool definitions into OpenAI ones.
func anthropicTools(v any) []any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := stringField(tool, "name")
		if name == "" {
			continue
		}
		params := tool["input_schema"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": stringField(tool, "description"),
				"parameters":  params,
			},
		})
	}
	return out
}

// anthropicToolChoice maps the Anthropic tool_choice onto its OpenAI spelling.
func anthropicToolChoice(v any) any {
	choice, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(stringField(choice, "type"))) {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name := stringField(choice, "name"); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "auto"
	default:
		return "auto"
	}
}

// anthropicToolList returns the raw Anthropic tool array, for token estimation.
func anthropicToolList(v any) []any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	return list
}

// ── outbound ────────────────────────────────────────────────────────────────

// anthropicStream renders a streamed answer as Anthropic events. It opens the
// message before the first token so a client with a silence timeout never
// stalls, then opens one content block per kind of content it sees.
type anthropicStream struct {
	stream *sseStream
	model  string
	msgID  string
	input  int

	tools    upstream.ToolCallTracker
	nextIdx  int
	blocks   []int
	thinkIdx int
	textIdx  int
	toolBlk  map[int]int

	finish  string
	usage   *upstream.Usage
	sawTool bool
}

func newAnthropicStream(w http.ResponseWriter, model string, inputTokens int) (responder, error) {
	s, err := newSSEStream(w, sseDialect{})
	if err != nil {
		return nil, err
	}
	a := &anthropicStream{
		stream:   s,
		model:    model,
		msgID:    randomID("msg_"),
		input:    inputTokens,
		thinkIdx: -1,
		textIdx:  -1,
		toolBlk:  map[int]int{},
	}
	a.begin()
	return a, nil
}

// begin writes the message envelope and a ping, committing the response.
func (a *anthropicStream) begin() {
	a.emit("message_start", map[string]any{"message": map[string]any{
		"id":            a.msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         a.model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": a.input, "output_tokens": 0},
	}})
	a.emit("ping", nil)
}

func (a *anthropicStream) Delta(d upstream.StreamDelta) error {
	if d.ReasoningContent != "" {
		if a.thinkIdx < 0 {
			a.thinkIdx = a.openBlock(map[string]any{"type": "thinking", "thinking": "", "signature": ""})
		}
		a.blockDelta(a.thinkIdx, map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent})
	}
	if d.Content != "" {
		if a.textIdx < 0 {
			a.textIdx = a.openBlock(map[string]any{"type": "text", "text": ""})
		}
		a.blockDelta(a.textIdx, map[string]any{"type": "text_delta", "text": d.Content})
	}
	if len(d.ToolCalls) > 0 {
		for _, chunk := range a.tools.Feed(d.ToolCalls) {
			block, ok := a.toolBlk[chunk.Index]
			if !ok {
				id := chunk.ID
				if id == "" {
					id = randomID("toolu_")
				}
				block = a.openBlock(map[string]any{
					"type": "tool_use", "id": id, "name": chunk.Name, "input": map[string]any{},
				})
				a.toolBlk[chunk.Index] = block
				a.sawTool = true
			}
			if chunk.Args != "" {
				a.blockDelta(block, map[string]any{"type": "input_json_delta", "partial_json": chunk.Args})
			}
		}
	}
	if d.Usage != nil {
		u := *d.Usage
		a.usage = &u
	}
	if d.FinishReason != "" {
		a.finish = d.FinishReason
	}
	return nil
}

func (a *anthropicStream) End() {
	a.closeBlocks()
	a.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": a.stopReason(), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.outputTokens()},
	})
	a.emit("message_stop", nil)
}

func (a *anthropicStream) Fail(err error) {
	a.emit("error", map[string]any{"error": map[string]any{
		"type":    "api_error",
		"message": err.Error(),
	}})
}

// openBlock starts a content block and returns its index.
func (a *anthropicStream) openBlock(block map[string]any) int {
	index := a.nextIdx
	a.nextIdx++
	a.blocks = append(a.blocks, index)
	a.emit("content_block_start", map[string]any{"index": index, "content_block": block})
	return index
}

func (a *anthropicStream) blockDelta(index int, delta map[string]any) {
	a.emit("content_block_delta", map[string]any{"index": index, "delta": delta})
}

// closeBlocks stops every open block, in the order they were opened.
func (a *anthropicStream) closeBlocks() {
	for _, index := range a.blocks {
		a.emit("content_block_stop", map[string]any{"index": index})
	}
}

func (a *anthropicStream) stopReason() string {
	return anthropicStopReason(a.finish, a.sawTool)
}

func (a *anthropicStream) outputTokens() int {
	if a.usage != nil {
		return a.usage.CompletionTokens
	}
	return 0
}

func (a *anthropicStream) emit(name string, payload map[string]any) {
	body := make(map[string]any, len(payload)+1)
	body["type"] = name
	for k, v := range payload {
		body[k] = v
	}
	_ = a.stream.write(sseEvent(name, body))
}

// anthropicMessage renders a completed answer as one Anthropic message.
func anthropicMessage(resp *upstream.ChatResponse, model string, inputEstimate int) map[string]any {
	blocks := make([]any, 0, 3)
	if resp.ReasoningContent != "" {
		blocks = append(blocks, map[string]any{
			"type": "thinking", "thinking": resp.ReasoningContent, "signature": "",
		})
	}
	if resp.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": resp.Content})
	}
	for _, call := range resp.ToolCalls {
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    orRandom(call.ID, "toolu_"),
			"name":  call.Function.Name,
			"input": parseToolInput(call.Function.Arguments),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	input := inputEstimate
	if resp.Usage.PromptTokens > 0 {
		input = resp.Usage.PromptTokens
	}
	return map[string]any{
		"id":            orRandom(resp.ID, "msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   anthropicStopReason(resp.FinishReason, len(resp.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  input,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
}

// anthropicStopReason maps a neutral finish reason onto the official
// vocabulary. A turn that asked for tools always ends as tool_use, whatever the
// upstream called it.
func anthropicStopReason(finish string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "max_tokens"
	case "content_filter", "refusal", "safety":
		return "refusal"
	case "stop_sequence":
		return "stop_sequence"
	case "pause_turn":
		return "pause_turn"
	default:
		return "end_turn"
	}
}

// parseToolInput turns a tool call's JSON arguments into the object an
// Anthropic tool_use block carries.
func parseToolInput(arguments string) map[string]any {
	raw := strings.TrimSpace(arguments)
	if raw == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
		// Not an object: keep the text rather than lose it.
		return map[string]any{"_raw": arguments}
	}
	return parsed
}

// ── errors ──────────────────────────────────────────────────────────────────

// writeAnthropicError renders the Anthropic error envelope.
func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	if typ == "" {
		typ = "invalid_request_error"
	}
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	t, err := parseAnthropicTurn(r, body)
	if err != nil {
		var are *anthropicRequestError
		if errors.As(err, &are) {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", are.msg)
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.runChat(w, r, t)
}

// handleAnthropicCountTokens estimates the input tokens of a request. It never
// reaches an upstream: the number is an estimate (see tokens.go) and bills
// nothing.
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if _, err := anthropicToOpenAI(body); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": anthropicInputEstimate(body)})
}

// ── small helpers ───────────────────────────────────────────────────────────

// blockType reads a content block's type, defaulting to text.
func blockType(block map[string]any) string {
	t := strings.ToLower(strings.TrimSpace(stringField(block, "type")))
	if t == "" {
		return "text"
	}
	return t
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func intOfAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func orEmptyObject(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// hasConversationalTurn reports whether the translated list holds anything but
// system messages, which is what the Anthropic API requires.
func hasConversationalTurn(msgs []any) bool {
	for _, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && m["role"] != "system" {
			return true
		}
	}
	return false
}

func nonEmpty(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// randomID mints an opaque identifier for a message or a tool call.
func randomID(prefix string) string {
	var buf [9]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return prefix + "0"
	}
	return prefix + hex.EncodeToString(buf[:])
}

// orRandom returns id when it is set, otherwise a fresh one.
func orRandom(id, prefix string) string {
	if strings.TrimSpace(id) != "" {
		return id
	}
	return randomID(prefix)
}
