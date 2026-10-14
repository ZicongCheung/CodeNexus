package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// flakyUpstream fails the first call (whatever account it lands on) with the
// given error, then succeeds. This makes failover deterministic.
type flakyUpstream struct {
	firstErr *upstream.Error

	mu   sync.Mutex
	call int
}

func (f *flakyUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (f *flakyUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}

func (f *flakyUpstream) isFirst() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call++
	return f.call == 1
}

func (f *flakyUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call
}

func (f *flakyUpstream) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	if f.isFirst() {
		return nil, f.firstErr
	}
	return &upstream.ChatResponse{ID: "1", Model: req.Model, Content: "ok", FinishReason: "stop"}, nil
}

func (f *flakyUpstream) ChatStream(_ context.Context, _ *account.Account, _ *upstream.ChatRequest, emit upstream.EmitFunc) error {
	if f.isFirst() {
		return f.firstErr
	}
	if err := emit(upstream.StreamDelta{Content: "he"}); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{Content: "llo", FinishReason: "stop"})
}

func (f *flakyUpstream) Refresh(context.Context, *account.Account) error { return nil }

func (f *flakyUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func failoverServer(t *testing.T, f upstream.Upstream, accounts []*account.Account) *Server {
	t.Helper()
	if f != nil {
		upstream.Register(f)
		t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })
	}
	p := pool.New(pool.Options{MaxInFlight: 1, StickyEnabled: false, WaitMax: 100 * time.Millisecond})
	p.Replace(accounts)
	cfg := config.Default()
	cfg.APIKey = ""
	return NewServer(&cfg, p, nil)
}

func postChat(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rec, req)
	return rec
}

func twoAccounts() []*account.Account {
	return []*account.Account{
		{Source: account.SourceWorkBuddy, UID: "bad", Enabled: true},
		{Source: account.SourceWorkBuddy, UID: "good", Enabled: true},
	}
}

func TestChatFailoverNonStream(t *testing.T) {
	s := failoverServer(t, &flakyUpstream{firstErr: upstream.NewError(upstream.KindRateLimited, 429, "slow down")}, twoAccounts())
	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestChatStreamFailoverBeforeFirstByte(t *testing.T) {
	s := failoverServer(t, &flakyUpstream{firstErr: upstream.NewError(upstream.KindRateLimited, 429, "slow down")}, twoAccounts())
	rec := postChat(s, `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "he") || !strings.Contains(body, "llo") {
		t.Fatalf("stream content missing: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream not terminated: %q", body)
	}
}

func TestChatNoAccountReturns503(t *testing.T) {
	s := failoverServer(t, &flakyUpstream{firstErr: upstream.NewError(upstream.KindRateLimited, 429, "x")}, nil)
	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestChatNonRetryableNotRetried(t *testing.T) {
	f := &flakyUpstream{firstErr: upstream.NewError(upstream.KindBadRequest, 400, "bad input")}
	s := failoverServer(t, f, twoAccounts())
	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if calls := f.calls(); calls != 1 {
		t.Fatalf("upstream called %d times, want exactly 1 (no retry on bad request)", calls)
	}
}
