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
	// AuthBaseURL overrides the regional base used by the device-login and
	// token-refresh endpoints. Empty uses the built-in regional defaults.
	AuthBaseURL string
	// BillingBaseURL and WebBaseURL override the hosts serving the metering and
	// web-growth task endpoints. Empty uses the built-in regional defaults;
	// tests point them at a stub.
	BillingBaseURL string
	WebBaseURL     string
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
		cfg: cfg,
		// Upstream calls go direct: a stale HTTP_PROXY / HTTPS_PROXY in the
		// launching shell (common in tool sandboxes) must not silently black-hole
		// device-login and billing traffic.
		http: &http.Client{
			Timeout:   time.Duration(cfg.TimeoutSeconds) * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
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
		if tok := acc.Token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if acc.UID != "" {
			req.Header.Set("X-User-Id", acc.UID)
		}
	}
	return req, nil
}

// ListModels implements upstream.Upstream.
//
// Two probes run concurrently — the IDE catalogue (/v3/config) and the
// console-web cli roster (/console/enterprises/personal/models) — and their
// results merge, because each holds models the other lacks. See catalog.go.
func (c *Client) ListModels(ctx context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	return c.listModels(ctx, acc)
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

// Chat implements upstream.Upstream (blocking).
//
// The upstream refuses non-streaming turns outright — it answers
// {"code":11101,"msg":"Non-stream chat request is currently not supported"} —
// so the blocking answer is assembled from the streaming call, which is the
// only request shape the server accepts. Sending a separate stream:false body
// would be a request the server never serves.
func (c *Client) Chat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	var (
		content   strings.Builder
		reasoning strings.Builder
		finish    string
		usage     upstream.Usage
		turnID    string
	)
	tools := &upstream.ToolCallTracker{}
	err := c.ChatStream(ctx, acc, req, func(d upstream.StreamDelta) error {
		content.WriteString(d.Content)
		reasoning.WriteString(d.ReasoningContent)
		if len(d.ToolCalls) > 0 {
			tools.Feed(d.ToolCalls)
		}
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
		if d.Usage != nil {
			usage = *d.Usage
		}
		if d.TurnID != "" {
			turnID = d.TurnID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := &upstream.ChatResponse{
		ID:               fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Model:            req.Model,
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        tools.Calls(),
		FinishReason:     finish,
		Usage:            usage,
		TurnID:           turnID,
		Surface:          "cloud",
	}
	// ChatStream already repairs markup on the way out; the call is idempotent
	// and covers the DisableMarkupRepair path, where text still carries it.
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
		PromptDetails    *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// cachedTokens mirrors the non-streaming clamp: a cache reading is only
// meaningful up to the size of the prompt it was measured against.
func (u *streamChunk) cachedTokens() int {
	if u.Usage == nil || u.Usage.PromptDetails == nil {
		return 0
	}
	n := u.Usage.PromptDetails.CachedTokens
	if n < 0 {
		return 0
	}
	if u.Usage.PromptTokens > 0 && n > u.Usage.PromptTokens {
		return u.Usage.PromptTokens
	}
	return n
}

// ChatStream implements upstream.Upstream (streaming SSE).
func (c *Client) ChatStream(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	body, err := c.buildBody(req, true)
	if err != nil {
		return err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v2/chat/completions", bytes.NewReader(body), acc)
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
				CachedTokens:     ch.cachedTokens(),
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

// Probe implements upstream.Upstream by reading the account's credit balance,
// which doubles as a liveness check on the credential.
func (c *Client) Probe(ctx context.Context, acc *account.Account) (*upstream.Probe, error) {
	if acc == nil || acc.Token() == "" {
		return &upstream.Probe{Healthy: false, Detail: "workbuddy: no credential"}, nil
	}
	credits, err := c.Credits(ctx, acc, 0)
	if err != nil {
		// A rejected credential is a definitive "not healthy"; a transient
		// failure is not.
		return &upstream.Probe{Healthy: !IsUnauthorized(err), Detail: err.Error()}, err
	}
	return &upstream.Probe{Credits: credits.Remain, Healthy: true, Detail: "workbuddy"}, nil
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
