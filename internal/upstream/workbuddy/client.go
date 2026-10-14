// Package workbuddy implements the WorkBuddy / CodeBuddy upstream adapter.
//
// It speaks the OpenAI-compatible chat surface of the upstream over plain HTTP
// and SSE, adapting responses back into the gateway's normalized types.
package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/rewrite"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// DefaultBaseURL is the upstream base used when none is configured.
const DefaultBaseURL = "https://copilot.tencent.com"

// DefaultUserAgent identifies the gateway to the upstream.
const DefaultUserAgent = "CodeNexus/0.1"

// Config configures the adapter.
type Config struct {
	BaseURL        string
	UserAgent      string
	TimeoutSeconds int
	// DisableSanitize turns off content-filter fingerprint scrubbing.
	DisableSanitize bool
	// DisableMarkupRepair turns off native tool-call markup restoration.
	DisableMarkupRepair bool
}

// Client is the WorkBuddy / CodeBuddy upstream adapter.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds an adapter, applying defaults for zero-valued fields.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 120
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

// Source implements upstream.Upstream.
func (c *Client) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + path
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, acc *account.Account) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if acc != nil {
		if acc.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		}
		if acc.UID != "" {
			req.Header.Set("X-User-Id", acc.UID)
		}
	}
	return req, nil
}

type modelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels implements upstream.Upstream.
func (c *Client) ListModels(ctx context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/models", nil, acc)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError("models", resp)
	}
	var ml modelList
	if err := json.NewDecoder(resp.Body).Decode(&ml); err != nil {
		return nil, err
	}
	out := make([]upstream.ModelSpec, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, upstream.ModelSpec{ID: m.ID, Source: upstream.SourceWorkBuddy})
	}
	return out, nil
}

// buildBody assembles the outbound body and runs it through the shared rewrite
// pipeline so every adapter sends the same normalized shape.
func (c *Client) buildBody(req *upstream.ChatRequest, stream bool) ([]byte, error) {
	body := make(map[string]any, len(req.Raw)+4)
	for k, v := range req.Raw {
		body[k] = v
	}
	body["model"] = req.Model
	body["stream"] = stream
	if _, ok := body["messages"]; !ok {
		msgs := make([]any, 0, len(req.Messages))
		for _, m := range req.Messages {
			msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
		}
		body["messages"] = msgs
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	body = rewrite.Prepare(body, rewrite.Options{
		ForceStream:            stream,
		MinMaxTokens:           16,
		DefaultReasoningEffort: "high",
		DisableSanitize:        c.cfg.DisableSanitize,
	})
	return json.Marshal(body)
}

type chatCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			ToolCalls        []upstream.ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Chat implements upstream.Upstream (non-streaming).
func (c *Client) Chat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	body, err := c.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body), acc)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError("chat", resp)
	}
	var cc chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&cc); err != nil {
		return nil, err
	}
	out := &upstream.ChatResponse{ID: cc.ID, Model: cc.Model}
	if len(cc.Choices) > 0 {
		out.Content = cc.Choices[0].Message.Content
		out.ReasoningContent = cc.Choices[0].Message.ReasoningContent
		out.ToolCalls = cc.Choices[0].Message.ToolCalls
		out.FinishReason = cc.Choices[0].FinishReason
	}
	out.Usage = upstream.Usage{
		PromptTokens:     cc.Usage.PromptTokens,
		CompletionTokens: cc.Usage.CompletionTokens,
		TotalTokens:      cc.Usage.TotalTokens,
	}
	c.repairMarkup(out, req)
	return out, nil
}

// repairMarkup restores native tool markup when the upstream returned plain
// text instead of structured tool calls.
func (c *Client) repairMarkup(out *upstream.ChatResponse, req *upstream.ChatRequest) {
	if c.cfg.DisableMarkupRepair || len(out.ToolCalls) > 0 {
		return
	}
	if out.FinishReason != "" && out.FinishReason != "stop" {
		return
	}
	calls, text, ok := rewrite.ExtractMarkup(out.Content, requestToolNames(req))
	if !ok {
		return
	}
	out.ToolCalls = toUpstreamCalls(calls, 0)
	out.Content = text
	out.FinishReason = "tool_calls"
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			ToolCalls        []upstream.ToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatStream implements upstream.Upstream (streaming SSE).
func (c *Client) ChatStream(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	body, err := c.buildBody(req, true)
	if err != nil {
		return err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body), acc)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError("chat", resp)
	}

	emitRaw := emit
	var rw *rewrite.MarkupRewriter
	nextID := 0
	if !c.cfg.DisableMarkupRepair {
		rw = rewrite.NewMarkupRewriter(requestToolNames(req))
		emit = func(d upstream.StreamDelta) error {
			if len(d.ToolCalls) > 0 {
				// real structured calls win: release anything held verbatim
				if text := rw.Disable(); text != "" {
					if err := emitRaw(upstream.StreamDelta{Content: text}); err != nil {
						return err
					}
				}
				return emitRaw(d)
			}
			if d.Content == "" {
				return emitRaw(d)
			}
			text, calls := rw.Write(d.Content)
			if text != "" {
				if err := emitRaw(upstream.StreamDelta{Content: text}); err != nil {
					return err
				}
			}
			if len(calls) > 0 {
				batch := toUpstreamCalls(calls, nextID)
				nextID += len(calls)
				return emitRaw(upstream.StreamDelta{ToolCalls: batch, FinishReason: "tool_calls", Usage: d.Usage})
			}
			if d.ReasoningContent != "" || d.FinishReason != "" || d.Usage != nil {
				return emitRaw(upstream.StreamDelta{
					ReasoningContent: d.ReasoningContent,
					FinishReason:     d.FinishReason,
					Usage:            d.Usage,
				})
			}
			return nil
		}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue
		}
		delta := upstream.StreamDelta{}
		if len(ch.Choices) > 0 {
			delta.Content = ch.Choices[0].Delta.Content
			delta.ReasoningContent = ch.Choices[0].Delta.ReasoningContent
			delta.ToolCalls = ch.Choices[0].Delta.ToolCalls
			delta.FinishReason = ch.Choices[0].FinishReason
		}
		if ch.Usage != nil {
			delta.Usage = &upstream.Usage{
				PromptTokens:     ch.Usage.PromptTokens,
				CompletionTokens: ch.Usage.CompletionTokens,
				TotalTokens:      ch.Usage.TotalTokens,
			}
		}
		if err := emit(delta); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return upstream.Classify(err)
	}
	if rw != nil {
		if text := rw.Flush(); text != "" {
			if err := emitRaw(upstream.StreamDelta{Content: text}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Refresh implements upstream.Upstream. Token refresh lands with the account
// lifecycle work in a later phase.
func (c *Client) Refresh(_ context.Context, _ *account.Account) error { return nil }

// Probe implements upstream.Upstream with a minimal liveness check.
func (c *Client) Probe(_ context.Context, acc *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: acc != nil && acc.AccessToken != "", Detail: "workbuddy"}, nil
}

// requestToolNames extracts the declared function names from a request.
func requestToolNames(req *upstream.ChatRequest) []string {
	if len(req.Tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(req.Tools))
	for _, raw := range req.Tools {
		tm, ok := raw.(map[string]any)
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

func toUpstreamCalls(calls []rewrite.MarkupCall, base int) []upstream.ToolCall {
	out := make([]upstream.ToolCall, 0, len(calls))
	for i, c := range calls {
		tc := upstream.ToolCall{ID: fmt.Sprintf("call_%d", base+i), Type: "function", Index: base + i}
		tc.Function.Name = c.Name
		tc.Function.Arguments = c.Arguments
		out = append(out, tc)
	}
	return out
}

func statusError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	e := upstream.ClassifyHTTP(resp.StatusCode, string(b))
	if e.Message != "" {
		e.Message = op + ": " + e.Message
	} else {
		e.Message = op
	}
	if d := parseRetryAfter(resp.Header.Get("Retry-After")); d > 0 {
		e.RetryAfter = d
	}
	return e
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
