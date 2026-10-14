package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
)

func newTestServer(apiKey string) *Server {
	cfg := config.Default()
	cfg.APIKey = apiKey
	return NewServer(&cfg, pool.New(pool.Options{}), nil)
}

func TestHealthz(t *testing.T) {
	s := newTestServer("")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["service"] != ServiceName {
		t.Fatalf("service = %v", body["service"])
	}
	if rec.Header().Get("X-Service") != ServiceName {
		t.Fatal("X-Service header missing")
	}
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer("secret")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestChatWithoutUpstream(t *testing.T) {
	s := newTestServer("")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}
