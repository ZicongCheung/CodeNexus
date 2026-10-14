package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

var errNoUpstream = errors.New("no upstream available for model")

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	enabled, total := s.pool.Stats()
	status := "ok"
	code := http.StatusOK
	if total > 0 && enabled == 0 {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, code, map[string]any{
		"status":   status,
		"service":  ServiceName,
		"accounts": map[string]int{"enabled": enabled, "total": total},
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, total := s.pool.Stats()
	sources := make([]string, 0)
	for _, src := range upstream.Sources() {
		sources = append(sources, string(src))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  ServiceName,
		"accounts": map[string]int{"enabled": enabled, "total": total},
		"sources":  sources,
	})
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	seen := map[string]bool{}
	data := make([]modelItem, 0)

	for _, src := range upstream.Sources() {
		u, ok := upstream.Get(src)
		if !ok {
			continue
		}
		acc, err := s.pool.Pick(src)
		if err != nil {
			continue
		}
		models, err := u.ListModels(ctx, acc)
		if err != nil {
			logx.Warnf("models: %s: %v", src, err)
			continue
		}
		for _, m := range models {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			data = append(data, modelItem{ID: m.ID, Object: "model", OwnedBy: string(m.Source)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "")
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		writeError(w, http.StatusBadRequest, "field 'model' is required", "")
		return
	}
	stream, _ := body["stream"].(bool)

	src, modelName, err := s.resolveSource(model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	u, ok := upstream.Get(src)
	if !ok {
		writeError(w, http.StatusBadRequest, "upstream not available: "+string(src), "")
		return
	}
	acc, err := s.pool.Pick(src)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "no available account for source "+string(src), "overloaded_error")
		return
	}

	req := &upstream.ChatRequest{
		Model:       modelName,
		Messages:    parseMessages(body["messages"]),
		Stream:      stream,
		Temperature: parseFloat(body["temperature"]),
		MaxTokens:   parseInt(body["max_tokens"]),
		Tools:       parseSlice(body["tools"]),
		Raw:         body,
	}

	if stream {
		s.streamChat(w, r, u, acc, req, model)
		return
	}
	resp, err := u.Chat(r.Context(), acc, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, nonStreamPayload(resp, model))
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, u upstream.Upstream, acc *account.Account, req *upstream.ChatRequest, clientModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-stream"
	emit := func(d upstream.StreamDelta) error {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkPayload(id, clientModel, d)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := u.ChatStream(r.Context(), acc, req, emit); err != nil {
		logx.Warnf("chat stream: %v", err)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errorChunkPayload(id, clientModel, err.Error()))
		flusher.Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func nonStreamPayload(resp *upstream.ChatResponse, model string) map[string]any {
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

func chunkPayload(id, model string, d upstream.StreamDelta) []byte {
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

func errorChunkPayload(id, model, msg string) []byte {
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
