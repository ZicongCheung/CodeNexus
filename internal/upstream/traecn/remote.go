package traecn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The remote surface is a two-step conversation: create a session, then read
// its event stream. Step one returns the session and turn identifiers; step two
// is a server-sent event stream keyed on that turn.
//
// The event payloads carry *cumulative* snapshots rather than increments, so
// the adapter diffs each snapshot against the previous one before emitting.
// Heartbeats are deliberately not forwarded: they carry no content, and
// treating one as the first byte would disable failover for a request that has
// not actually produced anything yet.

const (
	// remoteAgent is the executor that answers ordinary coding chat.
	remoteAgent = "solo_agent_remote"
	// remoteAgentWork is the executor for the workspace-flavoured variant.
	remoteAgentWork = "solo_work_remote"

	remoteModeCode = "code"
	remoteModeWork = "work"

	// remoteChatPath creates a session; remoteEventsPath streams its events.
	remoteChatPath   = "/chat_sessions"
	remoteEventsPath = "/chat_sessions/%s/events"
)

// chatSession is the identifier pair the session-creation call returns.
type chatSession struct {
	SessionID string
	MessageID string
	// MaxContext is the 1M window the session was pinned to, or 0 when the
	// max profile did not apply. It travels with the session so the response
	// can report what the outbound request actually asked for.
	MaxContext int
}

// createSession opens a remote conversation and returns its identifiers.
func (c *Client) createSession(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, agentType, mode, modelName string) (*chatSession, error) {
	initial := map[string]any{
		"chat_session_id":          "",
		"content":                  []any{},
		"query":                    encodeQuery(req),
		"model_name":               modelName,
		"agent_type":               agentType,
		"agent_id":                 agentType,
		"model_selection_strategy": "manual",
		"model_config_source":      1,
	}
	applied, maxContext := c.applyModelProfile(ctx, acc, req, agentType, modelName, initial)
	payload := map[string]any{
		"mode":                mode,
		"environment_id":      "default",
		"env":                 "remote",
		"auto_create_project": false,
		"origin":              "web",
		"initial_message":     initial,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if c.cfg.DebugUpstream {
		logx.Debugf("trae remote create: %s", clipJSON(body, 6*1024))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.remoteEndpoint(remoteChatPath), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(httpReq, acc, false)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, upstream.ClassifyHTTP(resp.StatusCode, string(raw))
	}
	var out struct {
		Code any            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae: create session: %w", err)
	}
	data := out.Data
	if len(data) == 0 {
		// Some responses put the payload at the top level.
		if err := json.Unmarshal(raw, &data); err != nil || data == nil {
			data = map[string]any{}
		}
	}
	sess := &chatSession{
		SessionID: firstString(data, "chat_session_id", "session_id", "chatSessionId"),
		MessageID: firstString(data, "message_id", "user_message_id", "messageId"),
	}
	if applied {
		sess.MaxContext = maxContext
	}
	if sess.SessionID == "" || sess.MessageID == "" {
		if code := remoteErrorCode(raw); code != "" {
			return nil, remoteError(resp.StatusCode, code, string(raw))
		}
		return nil, upstream.NewError(upstream.KindServer, resp.StatusCode, "trae: session response carried no identifiers")
	}
	return sess, nil
}

// encodeQuery flattens the conversation into the `query` field the remote
// surface reads: a JSON array of parts, each `{"type":"text","data":{"content":
// ...}}` — not a plain string. A bare string is accepted syntactically but the
// model never sees it, which is why a request would answer with an empty
// message. Roles are marked the way a transcript would be, and image parts
// become renderer media payloads so they reach a multimodal model.
func encodeQuery(req *upstream.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		content := flattenContent(m.Content)
		switch m.Role {
		case "system":
			if content == "" {
				continue
			}
			fmt.Fprintf(&b, "\n\n[System]\n%s", content)
		case "assistant":
			parts := make([]string, 0, 2)
			if content != "" {
				parts = append(parts, content)
			}
			if len(m.ToolCalls) > 0 {
				parts = append(parts, toolCallHistory(m.ToolCalls))
			}
			if len(parts) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n\n[Assistant]\n%s", strings.Join(parts, "\n\n"))
		case "tool", "function":
			if content == "" && m.ToolCallID == "" {
				continue
			}
			fmt.Fprintf(&b, "\n\n[Client Tool Result: %s %s status=succeeded]\n%s",
				m.ToolCallID, toolResultName(m), content)
		default:
			if content == "" {
				continue
			}
			b.WriteString("\n\n")
			b.WriteString(content)
		}
	}
	parts := []any{map[string]any{"type": "text", "data": map[string]any{"content": strings.TrimPrefix(b.String(), "\n\n")}}}
	parts = append(parts, mediaParts(req)...)
	encoded, err := json.Marshal(parts)
	if err != nil {
		return `[]`
	}
	return string(encoded)
}

// roleLabel renders a role the way a chat transcript would. It feeds the CLI's
// plain-text prompt, not the remote query field.
func roleLabel(role string) string {
	switch role {
	case "system":
		return "System"
	case "assistant":
		return "Assistant"
	case "tool", "function":
		return "Tool"
	default:
		return "User"
	}
}

// toolCallHistory renders an assistant turn's calls so the model sees what it
// asked for earlier.
func toolCallHistory(calls []upstream.ToolCall) string {
	list := make([]any, 0, len(calls))
	for _, c := range calls {
		list = append(list, map[string]any{
			"id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments,
		})
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return "[Client Tool Calls] " + string(encoded)
}

// toolResultName names the call a tool result answers.
func toolResultName(m upstream.Message) string {
	if m.Name != "" {
		return m.Name
	}
	return "tool"
}

var dataURLPattern = regexp.MustCompile(`^data:([-\w.+/]+);base64,(.*)$`)

// mediaParts converts OpenAI image parts into the renderer media payload the
// remote executor reads. Text-only requests contribute nothing.
func mediaParts(req *upstream.ChatRequest) []any {
	var out []any
	for _, m := range req.Messages {
		blocks, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if !strings.EqualFold(firstString(block, "type"), "image_url") {
				continue
			}
			nested, _ := block["image_url"].(map[string]any)
			url := firstString(nested, "url")
			mime, data := "", ""
			if matches := dataURLPattern.FindStringSubmatch(url); matches != nil {
				mime, data = matches[1], matches[2]
			}
			if data == "" {
				continue
			}
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, map[string]any{
				"type":  "image_url",
				"value": map[string]any{"mimeType": mime, "data": data},
			})
		}
	}
	return out
}

// flattenContent turns a message body into plain text. Structured parts keep
// their text and describe anything else by type, so nothing is silently lost.
func flattenContent(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				if s, ok := part.(string); ok {
					b.WriteString(s)
				}
				continue
			}
			if text, _ := pm["text"].(string); text != "" {
				b.WriteString(text)
				continue
			}
			if typ, _ := pm["type"].(string); typ != "" {
				fmt.Fprintf(&b, "[%s]", typ)
			}
		}
		return b.String()
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// remoteChat runs one remote conversation, forwarding deltas through emit and
// returning the accumulated response.
func (c *Client) remoteChat(ctx context.Context, acc *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) (*upstream.ChatResponse, error) {
	modelName := ResolveConfigName(req.Model)
	agentType, mode := remoteAgent, remoteModeCode
	if c.cfg.UpstreamMode == "work-agent" || modelName == WorkModelID {
		agentType, mode = remoteAgentWork, remoteModeWork
	}

	sess, err := c.createSession(ctx, acc, req, agentType, mode, modelName)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf(remoteEventsPath, sess.SessionID) + "?reply_to_message_id=" + sess.MessageID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.remoteEndpoint(path), nil)
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(httpReq, acc, true)

	resp, err := c.stream.Do(httpReq)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, upstream.ClassifyHTTP(resp.StatusCode, string(raw))
	}

	out := &upstream.ChatResponse{ID: sess.SessionID, Model: req.Model, TurnID: sess.MessageID,
		Surface: "remote", MaxProfile: sess.MaxContext > 0, MaxContext: sess.MaxContext}
	// Announce the turn id before any content so a streaming caller learns what
	// to settle the charge against even if the answer is long.
	if emit != nil {
		if err := emit(upstream.StreamDelta{TurnID: sess.MessageID, Route: routeOf(out)}); err != nil {
			return nil, err
		}
	}
	st := &snapshotState{emit: emit}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	sawDone := false

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // heartbeat comment
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
			sawDone = true
			break
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		name := normalizeEventName(firstNonEmpty(event, eventNameOf(obj)))
		if c.cfg.DebugUpstream {
			logx.Debugf("trae remote event %s: %s", name, debugJSON(obj))
		}
		done, err := c.applyRemoteEvent(name, obj, out, st)
		if err != nil {
			return out, err
		}
		if done {
			sawDone = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return out, upstream.Classify(ctx.Err())
		}
		return out, upstream.Classify(err)
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	if !sawDone && out.Content == "" && out.ReasoningContent == "" && len(out.ToolCalls) == 0 {
		return out, upstream.NewError(upstream.KindServer, 0, "trae: the session stream produced no answer")
	}
	return out, nil
}

// applyRemoteEvent folds one upstream event into the accumulating response and
// returns whether the stream is complete.
func (c *Client) applyRemoteEvent(name string, obj map[string]any, out *upstream.ChatResponse, st *snapshotState) (bool, error) {
	switch name {
	case "heartbeat", "keepalive", "ping", "model_config", "request_wait_in_queue":
		// Metadata and liveness only: never the first byte of an answer.
		return false, nil

	case "error":
		return false, remoteError(0, remoteErrorCodeOf(obj), rawJSON(obj))

	case "token_usage":
		if usage := usageOf(obj); usage != nil {
			out.Usage = *usage
		}
		return false, nil

	case "done":
		out.FinishReason = normalizeFinish(firstString(obj, "finish_reason", "stop_reason", "finishReason"))
		return true, st.emitUsage(out)
	}

	text, reasoning := visibleText(name, obj)
	if text == "" && reasoning == "" {
		return false, nil
	}
	if err := st.write(text, reasoning, out); err != nil {
		return false, err
	}
	return false, nil
}

// snapshotState turns cumulative upstream snapshots into increments.
type snapshotState struct {
	emit    upstream.EmitFunc
	text    string
	reason  string
	emitted bool
	// routed records that the turn's routing note has been sent, so the
	// caller learns the executor once rather than on every chunk.
	routed bool
}

// routeOf narrows a response into the routing note a streaming caller needs.
func routeOf(out *upstream.ChatResponse) *upstream.Route {
	if out == nil || out.Surface == "" {
		return nil
	}
	return &upstream.Route{Surface: out.Surface, MaxProfile: out.MaxProfile, MaxContext: out.MaxContext}
}

func (s *snapshotState) write(text, reasoning string, out *upstream.ChatResponse) error {
	d := upstream.StreamDelta{}
	if text != "" {
		delta := prefixDelta(s.text, text)
		s.text = text
		out.Content = text
		d.Content = delta
	}
	if reasoning != "" {
		delta := prefixDelta(s.reason, reasoning)
		s.reason = reasoning
		out.ReasoningContent = reasoning
		d.ReasoningContent = delta
	}
	if d.Content == "" && d.ReasoningContent == "" {
		return nil
	}
	if !s.routed {
		s.routed = true
		d.Route = routeOf(out)
	}
	s.emitted = true
	if s.emit == nil {
		return nil
	}
	return s.emit(d)
}

func (s *snapshotState) emitUsage(out *upstream.ChatResponse) error {
	if s.emit == nil || out.Usage.TotalTokens == 0 {
		return nil
	}
	u := out.Usage
	return s.emit(upstream.StreamDelta{Usage: &u})
}

// prefixDelta computes the increment between two cumulative snapshots. When the
// upstream restarts the snapshot (a new message after a tool round) the whole
// value is emitted rather than dropping text.
func prefixDelta(prev, next string) string {
	if next == prev {
		return ""
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	return next
}

// visibleText extracts the answer and reasoning snapshots from an event.
//
// plan_item has two layouts, and the presence of the "reasoning_content" key is
// what tells them apart: the current one ships the reply in "thought" (for
// example thought="你好", reasoning_content="") while the trace lives in
// "reasoning_content"; the older variant omits that key and puts the trace in
// "thought". Reading "thought" as reasoning in the current layout turns the
// answer into invisible reasoning and answers with an empty message.
func visibleText(name string, obj map[string]any) (text, reasoning string) {
	switch name {
	case "plan_item":
		_, split := obj["reasoning_content"]
		if !split {
			_, split = obj["reasoningContent"]
		}
		if split {
			reasoning = firstString(obj, "reasoning_content", "reasoningContent")
			text = firstString(obj, "content")
			if text == "" {
				text = firstString(obj, "thought")
			}
			return text, reasoning
		}
		text = firstString(obj, "content")
		reasoning = firstString(obj, "thought")
		return text, reasoning
	case "message", "assistant_message", "response", "text", "output":
		return messageText(obj), messageReasoning(obj)
	}
	return "", ""
}

// messageText reads the answer out of one of the message-shaped events.
func messageText(obj map[string]any) string {
	if s := firstString(obj, "content"); s != "" {
		return s
	}
	if s := contentBlocksText(obj["content"]); s != "" {
		return s
	}
	for _, key := range []string{"text", "response", "answer", "output"} {
		if s := firstString(obj, key); s != "" {
			return s
		}
	}
	if nested, ok := obj["message"].(map[string]any); ok {
		return firstString(nested, "content", "text", "response", "answer")
	}
	for _, key := range []string{"agent_message", "assistant_message"} {
		if nested, ok := obj[key].(map[string]any); ok {
			if s := firstString(nested, "content", "text"); s != "" {
				return s
			}
		}
	}
	return ""
}

func messageReasoning(obj map[string]any) string {
	if s := firstString(obj, "reasoning_content", "reasoningContent", "thought", "thinking"); s != "" {
		return s
	}
	if nested, ok := obj["message"].(map[string]any); ok {
		return firstString(nested, "reasoning_content", "reasoningContent", "thought")
	}
	return ""
}

// contentBlocksText joins the text blocks of a structured content array,
// skipping reasoning blocks so they are not duplicated into the answer.
func contentBlocksText(v any) string {
	blocks, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, raw := range blocks {
		bm, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				b.WriteString(s)
			}
			continue
		}
		switch bm["type"] {
		case "reasoning", "thinking", "reasoning_text":
			continue
		}
		if s, _ := bm["text"].(string); s != "" {
			b.WriteString(s)
		} else if s, _ := bm["content"].(string); s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

// eventNameOf reads the event name out of the payload when the stream did not
// carry an SSE event line.
func eventNameOf(obj map[string]any) string {
	return firstString(obj, "event", "event_type", "eventType", "type")
}

// normalizeEventName folds the upstream's spelling variants onto one spelling.
func normalizeEventName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "keepalive", "ping":
		return "heartbeat"
	case "modelconfig":
		return "model_config"
	case "planitem":
		return "plan_item"
	case "requestwaitinqueue":
		return "request_wait_in_queue"
	case "responsedone", "streamdone", "response_done", "stream_done":
		return "done"
	case "tokenusage":
		return "token_usage"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

// normalizeFinish maps an upstream stop reason onto the OpenAI vocabulary.
func normalizeFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_calls", "function_call", "tool_use":
		return "tool_calls"
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "length"
	case "content_filter", "safety":
		return "content_filter"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

// usageOf extracts token accounting from a token_usage event.
func usageOf(obj map[string]any) *upstream.Usage {
	src := obj
	if nested, ok := obj["usage"].(map[string]any); ok {
		src = nested
	}
	prompt := intOf(src, "prompt_tokens", "input_tokens", "promptTokens")
	completion := intOf(src, "completion_tokens", "output_tokens", "completionTokens")
	total := intOf(src, "total_tokens", "totalTokens")
	if total == 0 {
		total = prompt + completion
	}
	if prompt == 0 && completion == 0 && total == 0 {
		return nil
	}
	return &upstream.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CachedTokens:     cachedOf(src),
	}
}

// cachedOf reads the cache-hit share of the prompt. Every upstream spells it
// differently and several send more than one spelling, so all known keys are
// tried and the largest wins: a turn reported twice under different names
// should still be counted once, and taking the maximum of near-duplicate
// readings is closer to that than summing them.
func cachedOf(src map[string]any) int {
	best := 0
	for _, key := range []string{"cached_tokens", "cache_read_input_tokens", "cacheReadInputTokens"} {
		if v := intOf(src, key); v > best {
			best = v
		}
	}
	// Nested detail objects carry the same figure on the OpenAI and Anthropic
	// spellings alike.
	for _, holder := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details, ok := src[holder].(map[string]any); ok {
			for _, key := range []string{"cached_tokens", "cache_read_input_tokens"} {
				if v := intOf(details, key); v > best {
					best = v
				}
			}
		}
	}
	if best < 0 {
		return 0
	}
	// A cache reading can never exceed the prompt it was measured against.
	// Upstreams occasionally report a total that includes cached tokens while
	// the prompt figure is smaller, and clamping keeps the stacked chart's
	// "input minus cache" segment from going negative.
	if prompt := intOf(src, "prompt_tokens", "input_tokens", "promptTokens"); prompt > 0 && best > prompt {
		return prompt
	}
	return best
}

func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		case string:
			if n, err := parseAmount(v); err == nil {
				return int(n)
			}
		}
	}
	return 0
}

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// rawJSON re-encodes an event for an error message, bounded so a pathological
// payload cannot flood the log.
func rawJSON(obj map[string]any) string {
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	if len(b) > 512 {
		return string(b[:512])
	}
	return string(b)
}

// jsonString renders a value as compact JSON.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// clipJSON bounds a debug dump of raw bytes.
func clipJSON(b []byte, limit int) string {
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

// debugJSON re-encodes an event for the debug log with a generous bound, so a
// protocol drift can be diagnosed from the log alone.
func debugJSON(obj map[string]any) string {
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	if len(b) > 16*1024 {
		return string(b[:16*1024]) + "…"
	}
	return string(b)
}

// ErrSessionIncomplete reports a stream that stopped before its terminal event.
var ErrSessionIncomplete = errors.New("trae: session stream ended early")
