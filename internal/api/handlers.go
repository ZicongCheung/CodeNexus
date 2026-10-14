package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
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
		"status":          status,
		"service":         ServiceName,
		"accounts":        map[string]int{"enabled": enabled, "total": total},
		"sticky_sessions": s.pool.StickyCount(),
		"time":            time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, total := s.pool.Stats()
	sources := make([]string, 0)
	for _, src := range upstream.Sources() {
		sources = append(sources, string(src))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         ServiceName,
		"accounts":        map[string]int{"enabled": enabled, "total": total},
		"sources":         sources,
		"sticky_sessions": s.pool.StickyCount(),
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
		acc := s.pool.Candidate(src)
		if acc == nil {
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

	msgs := parseMessages(body["messages"])
	req := &upstream.ChatRequest{
		Model:       modelName,
		Messages:    msgs,
		Stream:      stream,
		Temperature: parseFloat(body["temperature"]),
		MaxTokens:   parseInt(body["max_tokens"]),
		Tools:       parseSlice(body["tools"]),
		Raw:         body,
	}
	sk := sessionKey(r, body, msgs)

	attempts := s.cfg.Pool.FailoverAttempts
	if attempts < 1 {
		attempts = 1
	}
	tried := map[string]bool{}
	refreshed := map[string]bool{}
	var lastErr *upstream.Error

	for i := 0; i < attempts; i++ {
		lease, aerr := s.pool.Acquire(r.Context(), pool.PickOptions{
			Source:     src,
			Model:      modelName,
			SessionKey: sk,
			Exclude:    tried,
		})
		if aerr != nil {
			writePoolError(w, aerr)
			return
		}

		if stream {
			emitted, serr := s.streamChat(w, r, u, lease.Account, req, model)
			if serr == nil {
				lease.Succeed()
				return
			}
			if !emitted && s.tryRefresh(r.Context(), lease.Account, serr, refreshed) {
				emitted, serr = s.streamChat(w, r, u, lease.Account, req, model)
				if serr == nil {
					lease.Succeed()
					return
				}
			}
			ue := lease.Fail(serr)
			logx.Warnf("chat stream attempt %d/%d failed: %v", i+1, attempts, serr)
			if emitted {
				// The stream already concluded in-band; the client saw the error.
				return
			}
			if !ue.Retryable() || i == attempts-1 {
				writeUpstreamError(w, ue)
				return
			}
			tried[lease.Account.Key()] = true
			lastErr = ue
			continue
		}

		resp, cerr := u.Chat(r.Context(), lease.Account, req)
		if cerr == nil {
			lease.Succeed()
			writeJSON(w, http.StatusOK, nonStreamPayload(resp, model))
			return
		}
		if s.tryRefresh(r.Context(), lease.Account, cerr, refreshed) {
			if resp, cerr = u.Chat(r.Context(), lease.Account, req); cerr == nil {
				lease.Succeed()
				writeJSON(w, http.StatusOK, nonStreamPayload(resp, model))
				return
			}
		}
		ue := lease.Fail(cerr)
		logx.Warnf("chat attempt %d/%d failed: %v", i+1, attempts, cerr)
		if !ue.Retryable() || i == attempts-1 {
			writeUpstreamError(w, ue)
			return
		}
		tried[lease.Account.Key()] = true
		lastErr = ue
	}
	writeUpstreamError(w, lastErr)
}

// tryRefresh attempts one token refresh when err looks like an auth failure and
// the account has not been refreshed yet during this request. It reports
// whether the caller should retry the same account.
func (s *Server) tryRefresh(ctx context.Context, acc *account.Account, err error, done map[string]bool) bool {
	if s.refresher == nil || !upstream.IsAuth(err) {
		return false
	}
	key := acc.Key()
	if done[key] {
		return false
	}
	done[key] = true
	if rerr := s.refresher.RefreshNow(ctx, acc); rerr != nil {
		logx.Warnf("refresh %s after auth error: %v", key, rerr)
		return false
	}
	logx.Infof("refreshed %s after auth error, retrying", key)
	return true
}

// streamChat forwards a streaming completion. It reports whether any bytes were
// sent to the client, which decides whether a retry on another account is still
// possible. When the failure happens after the first byte it writes the error
// into the stream and concludes it.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, u upstream.Upstream, acc *account.Account, req *upstream.ChatRequest, clientModel string) (bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false, errors.New("streaming unsupported")
	}
	h := w.Header()
	id := "chatcmpl-stream"
	started := false
	begin := func() {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	emit := func(d upstream.StreamDelta) error {
		if !started {
			started = true
			begin()
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunkPayload(id, clientModel, d)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := u.ChatStream(r.Context(), acc, req, emit); err != nil {
		if !started {
			return false, err
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errorChunkPayload(id, clientModel, err.Error()))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return true, err
	}
	if !started {
		begin()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	return started, nil
}

func writePoolError(w http.ResponseWriter, err error) {
	var na *pool.ErrNoAccount
	if errors.As(err, &na) {
		if na.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(na.RetryAfter.Seconds()))))
		}
		writeError(w, http.StatusServiceUnavailable, "no available account: "+na.Reason, "overloaded_error")
		return
	}
	writeError(w, http.StatusServiceUnavailable, err.Error(), "overloaded_error")
}

func writeUpstreamError(w http.ResponseWriter, ue *upstream.Error) {
	if ue == nil {
		writeError(w, http.StatusBadGateway, "upstream error", "upstream_error")
		return
	}
	code := http.StatusBadGateway
	typ := "upstream_error"
	switch ue.Kind {
	case upstream.KindBadRequest:
		code, typ = http.StatusBadRequest, "invalid_request_error"
	case upstream.KindContentBlocked:
		code, typ = http.StatusBadRequest, "content_filter"
	case upstream.KindRateLimited:
		code, typ = http.StatusTooManyRequests, "rate_limit_error"
	case upstream.KindQuota:
		code, typ = http.StatusTooManyRequests, "insufficient_quota"
	case upstream.KindTimeout:
		code, typ = http.StatusGatewayTimeout, "timeout_error"
	}
	if ue.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ue.RetryAfter.Seconds()))))
	}
	writeError(w, code, ue.Message, typ)
}

// sessionKey derives a stable conversation identifier from explicit client
// signals first (header, then the OpenAI user field) and falls back to a
// fingerprint of the system prompt, first user turn and tool set.
func sessionKey(r *http.Request, body map[string]any, msgs []upstream.Message) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-Id")); v != "" {
		return "h:" + v
	}
	if v, _ := body["user"].(string); strings.TrimSpace(v) != "" {
		return "u:" + strings.TrimSpace(v)
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
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok {
					fmt.Fprintf(h, "T:%v\n", fn["name"])
					seeded = true
				}
			}
		}
	}
	if !seeded {
		return ""
	}
	return "f:" + hex.EncodeToString(h.Sum(nil)[:8])
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
