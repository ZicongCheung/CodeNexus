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
	msgs := parseMessages(body["messages"])
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
		message["tool_calls"] = resp.ToolCalls
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
		delta["tool_calls"] = d.ToolCalls
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
		out = append(out, upstream.Message{Role: role, Content: m["content"]})
	}
	return out
}

func parseFloat(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

func parseInt(v any) *int {
	if f, ok := v.(float64); ok {
		n := int(f)
		return &n
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
