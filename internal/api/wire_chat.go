package api

// The OpenAI chat-completions wire format: how a client's body becomes a
// neutral request, and how an answer is rendered back for it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// chatWireName labels the route family in logs.
const chatWireName = "chat"

// parseChatTurn reads an OpenAI chat-completions body.
func parseChatTurn(r *http.Request, body map[string]any) (*turn, error) {
	model, _ := body["model"].(string)
	if model == "" {
		return nil, errors.New("field 'model' is required")
	}
	stream, _ := body["stream"].(bool)
	msgs, repairs := upstream.RepairToolPairing(parseMessages(body["messages"]))
	for _, rep := range repairs {
		logx.Dialog.Warnf("inbound %s dropped (tool call id=%q)", rep.Reason, rep.ID)
	}
	user, _ := body["user"].(string)
	return &turn{
		Name:    chatWireName,
		Model:   model,
		Stream:  stream,
		Session: sessionKey(r, user, msgs, openAIToolNames(body["tools"])),
		Request: &upstream.ChatRequest{
			Model:       model,
			Messages:    msgs,
			Stream:      stream,
			Temperature: parseFloat(body["temperature"]),
			MaxTokens:   parseInt(body["max_tokens"]),
			Tools:       parseSlice(body["tools"]),
			Raw:         body,
		},
		Result: func(w http.ResponseWriter, resp *upstream.ChatResponse) {
			writeJSON(w, http.StatusOK, chatCompletionPayload(resp, model))
		},
		Open: func(w http.ResponseWriter) (responder, error) {
			const id = "chatcmpl-stream"
			return newSSEStream(w, sseDialect{
				Frame: func(d upstream.StreamDelta) string {
					return sseData(chatChunkPayload(id, model, d))
				},
				End: func() string { return "data: [DONE]\n\n" },
				Fail: func(err error) string {
					return sseData(chatErrorChunkPayload(id, model, err.Error())) + "data: [DONE]\n\n"
				},
			})
		},
	}, nil
}

// openAIToolNames pulls the offered function names out of an OpenAI tool array.
func openAIToolNames(v any) []string {
	tools, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func chatCompletionPayload(resp *upstream.ChatResponse, model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": resp.Content}
	if resp.ReasoningContent != "" {
		message["reasoning_content"] = resp.ReasoningContent
	}
	if len(resp.ToolCalls) > 0 {
		message["tool_calls"] = toolCallMessages(resp.ToolCalls)
	}
	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	id := resp.ID
	if id == "" {
		id = "chatcmpl"
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": resp.Usage,
	}
}

func chatChunkPayload(id, model string, d upstream.StreamDelta) []byte {
	delta := map[string]any{}
	if d.Content != "" {
		delta["content"] = d.Content
	}
	if d.ReasoningContent != "" {
		delta["reasoning_content"] = d.ReasoningContent
	}
	if len(d.ToolCalls) > 0 {
		delta["tool_calls"] = toolCallChunks(d.ToolCalls)
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if d.FinishReason != "" {
		choice["finish_reason"] = d.FinishReason
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	b, _ := json.Marshal(obj)
	return b
}

func chatErrorChunkPayload(id, model, msg string) []byte {
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{},
		"error":   map[string]string{"message": msg},
	}
	b, _ := json.Marshal(obj)
	return b
}

func parseMessages(v any) []upstream.Message {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]upstream.Message, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		msg := upstream.Message{Role: role, Content: m["content"]}
		if name, _ := m["name"].(string); name != "" {
			msg.Name = name
		}
		// The result of a tool call and the call itself are modelled as fields
		// rather than left inside Content, so every adapter can re-expand them
		// in its own vocabulary.
		if id, _ := m["tool_call_id"].(string); id != "" {
			msg.ToolCallID = id
		}
		if rc, _ := m["reasoning_content"].(string); rc != "" {
			msg.ReasoningContent = rc
		}
		msg.ToolCalls = parseToolCalls(m["tool_calls"])
		out = append(out, msg)
	}
	return out
}

// parseToolCalls folds an OpenAI tool_calls array onto the neutral shape,
// giving each call the index of its position.
func parseToolCalls(v any) []upstream.ToolCall {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]upstream.ToolCall, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tc := upstream.ToolCall{Index: i}
		tc.ID, _ = m["id"].(string)
		tc.Type, _ = m["type"].(string)
		if fn, ok := m["function"].(map[string]any); ok {
			tc.Function.Name, _ = fn["name"].(string)
			tc.Function.Arguments = argumentsText(fn["arguments"])
		}
		if tc.Function.Name == "" && tc.ID == "" {
			continue
		}
		out = append(out, tc)
	}
	return out
}

// argumentsText renders tool arguments as the JSON string the OpenAI protocol
// requires, accepting either a string or an object.
func argumentsText(v any) string {
	switch a := v.(type) {
	case string:
		return a
	case nil:
		return ""
	default:
		if b, err := json.Marshal(a); err == nil {
			return string(b)
		}
		return ""
	}
}

// toolCallChunks renders streaming tool calls, preserving whatever fragment
// granularity the upstream produced. The index is always written — a client
// groups fragments by it, and index 0 must survive serialization — while id and
// name are written only when known, so a continuation fragment never blanks the
// identity a client already holds.
func toolCallChunks(calls []upstream.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		chunk := map[string]any{
			"index":    c.Index,
			"type":     callType(c.Type),
			"function": map[string]any{"arguments": c.Function.Arguments},
		}
		if c.ID != "" {
			chunk["id"] = c.ID
		}
		if c.Function.Name != "" {
			chunk["function"].(map[string]any)["name"] = c.Function.Name
		}
		out = append(out, chunk)
	}
	return out
}

// toolCallMessages renders a completed tool call for a non-streaming answer. A
// finished call has no index, matching the OpenAI message shape.
func toolCallMessages(calls []upstream.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"id":   c.ID,
			"type": callType(c.Type),
			"function": map[string]any{
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			},
		})
	}
	return out
}

func callType(t string) string {
	if t == "" {
		return "function"
	}
	return t
}

// parseFloat reads a numeric field that may have arrived as JSON (float64) or
// been synthesized from another protocol as a Go integer.
func parseFloat(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	}
	return nil
}

func parseInt(v any) *int {
	switch n := v.(type) {
	case float64:
		i := int(n)
		return &i
	case int:
		i := n
		return &i
	case int64:
		i := int(n)
		return &i
	}
	return nil
}

func parseSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// sessionKey derives a stable conversation identifier from explicit client
// signals first (header, then a client-supplied user id) and falls back to a
// fingerprint of the system prompt, first user turn and the offered tools.
// Every wire format can supply those three things in its own vocabulary.
func sessionKey(r *http.Request, user string, msgs []upstream.Message, toolNames []string) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-Id")); v != "" {
		return "h:" + v
	}
	if v := strings.TrimSpace(user); v != "" {
		return "u:" + v
	}

	h := sha256.New()
	seeded := false
	for _, m := range msgs {
		if m.Role == "system" {
			fmt.Fprintf(h, "S:%v\n", m.Content)
			seeded = true
		}
	}
	for _, m := range msgs {
		if m.Role == "user" {
			fmt.Fprintf(h, "U:%v\n", m.Content)
			seeded = true
			break
		}
	}
	for _, name := range toolNames {
		fmt.Fprintf(h, "T:%s\n", name)
		seeded = true
	}
	if !seeded {
		return ""
	}
	return "f:" + hex.EncodeToString(h.Sum(nil)[:8])
}
