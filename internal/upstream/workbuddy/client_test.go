package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func testAccount() *account.Account {
	return &account.Account{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "tok", Enabled: true}
}

func TestChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		// The upstream only serves /v2 and only in streaming mode, so Chat has
		// to answer from an SSE body even for a blocking call.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	resp, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "m",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 3 {
		t.Fatalf("total tokens = %d", resp.Usage.TotalTokens)
	}
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	var got string
	err := c.ChatStream(context.Background(), testAccount(), &upstream.ChatRequest{Model: "m"}, func(d upstream.StreamDelta) error {
		got += d.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got != "hello" {
		t.Fatalf("content = %q", got)
	}
}

// A streamed answer that outlasts the configured request timeout must still
// arrive. The timeout bounds how long the answer may take to start, not how
// long it may run: a whole-request ceiling cut healthy answers off mid-answer
// and surfaced as a timeout for a turn another account could not have served
// any faster.
func TestChatStreamOutlivesTheRequestTimeout(t *testing.T) {
	const timeout = 1 // seconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		// Stall well past the ceiling the adapter is configured with.
		time.Sleep(3 * timeout * time.Second)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: timeout})
	var got string
	err := c.ChatStream(context.Background(), testAccount(), &upstream.ChatRequest{Model: "m"}, func(d upstream.StreamDelta) error {
		got += d.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got != "hello" {
		t.Fatalf("content = %q", got)
	}
}

// Dropping the whole-request ceiling must not drop the bound that matters: an
// upstream that never starts answering has to be given up on, or a hung
// connection would hold the account's slot forever instead of failing over.
func TestChatStreamBoundsTheFirstByte(t *testing.T) {
	const timeout = 1 // seconds
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never answer before the client gives up
	}))
	defer func() { close(release); srv.Close() }()

	c := New(Config{BaseURL: srv.URL, TimeoutSeconds: timeout})
	err := c.ChatStream(context.Background(), testAccount(), &upstream.ChatRequest{Model: "m"}, func(upstream.StreamDelta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream: want a first-byte timeout, got nil")
	}
}

func TestListModels(t *testing.T) {
	// Both probes answer from the same stub, so the merge sees the same ids on
	// both sides and the catalogue's richer shape wins — the contract the
	// console's model table is built on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case configPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"deepseek-v4-flash","credits":"x1"},
				{"id":"glm-5","credits":"x1"}]}}`))
		case enterpriseModelsPath:
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"deepseek-v4-flash"},{"id":"glm-5"}],
				"agents":[{"name":"cli","models":["deepseek-v4-flash","glm-5"]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	models, err := c.ListModels(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("models = %+v", models)
	}
}
