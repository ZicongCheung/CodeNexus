package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

const sep = "\uFF5C\uFF5C"

func nativeMarkup() string {
	open := "<" + sep + "DSML" + sep + " calls>"
	closeTag := "</" + sep + "DSML" + sep + " calls>"
	invoke := "<" + sep + "DSML" + sep + ` invoke name="exec_command">`
	invokeClose := "</" + sep + "DSML" + sep + " invoke>"
	param := "<" + sep + "DSML" + sep + ` parameter name="cmd" string="true">`
	paramClose := "</" + sep + "DSML" + sep + " parameter>"
	return open + "\n" + invoke + "\n" + param + "ls -la" + paramClose + "\n" + invokeClose + "\n" + closeTag
}

func execTool() []any {
	return []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "exec_command"},
	}}
}

func TestChatRepairsNativeToolMarkup(t *testing.T) {
	// Chat aggregates ChatStream now, so the stub speaks SSE. The markup is
	// split over two frames on purpose: the rewriter has to buffer across
	// frames before it can recognise a complete DSML block.
	full := nativeMarkup()
	first, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": full[:20]}}},
	})
	second, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{"content": full[20:]},
			"finish_reason": "stop",
		}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(first)+"\n\n")
		_, _ = io.WriteString(w, "data: "+string(second)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	resp, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{Model: "m", Tools: execTool()})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "exec_command" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if !strings.Contains(resp.ToolCalls[0].Function.Arguments, "ls -la") {
		t.Fatalf("arguments = %q", resp.ToolCalls[0].Function.Arguments)
	}
	if strings.Contains(resp.Content, "DSML") {
		t.Fatalf("markup leaked into content: %q", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
}

func TestChatStreamRepairsNativeToolMarkup(t *testing.T) {
	full := "let me run that\n" + nativeMarkup() + "\ndone"
	// split the markup across SSE frames to exercise the buffering rewriter
	var frames []string
	for _, chunk := range []string{full[:12], full[12:40], full[40:]} {
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}}},
		})
		frames = append(frames, "data: "+string(b)+"\n\n")
	}
	frames = append(frames, "data: [DONE]\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	var text strings.Builder
	var calls []upstream.ToolCall
	err := c.ChatStream(context.Background(), testAccount(), &upstream.ChatRequest{Model: "m", Tools: execTool()},
		func(d upstream.StreamDelta) error {
			text.WriteString(d.Content)
			calls = append(calls, d.ToolCalls...)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "exec_command" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if strings.Contains(text.String(), "DSML") {
		t.Fatalf("markup leaked into stream: %q", text.String())
	}
	if !strings.Contains(text.String(), "let me run that") || !strings.Contains(text.String(), "done") {
		t.Fatalf("text lost: %q", text.String())
	}
}

func TestChatSanitizesOutboundFingerprint(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		_, _ = w.Write([]byte(`{"id":"1","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	_, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "m",
		Messages: []upstream.Message{{Role: "system", Content: "reply about error code 11128"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(captured, "11128") {
		t.Fatalf("fingerprint reached upstream: %s", captured)
	}
	if !strings.Contains(captured, "11-128") {
		t.Fatalf("fingerprint not rewritten: %s", captured)
	}
}
