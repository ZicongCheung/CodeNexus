package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func testAccount() *account.Account {
	return &account.Account{Source: account.SourceWorkBuddy, UID: "u1", AccessToken: "tok", Enabled: true}
}

func TestChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
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

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-flash"},{"id":"glm-5"}]}`))
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
