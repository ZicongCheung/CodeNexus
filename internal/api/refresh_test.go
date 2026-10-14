package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/authrefresh"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// healingUpstream rejects requests made with a stale token and accepts once the
// account has been refreshed.
type healingUpstream struct {
	mu    sync.Mutex
	calls int
	// refreshFails makes the refresh endpoint fail, so healing cannot happen.
	refreshFails bool
}

func (u *healingUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (u *healingUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "m", Source: upstream.SourceWorkBuddy}}, nil
}

func (u *healingUpstream) call(acc *account.Account) error {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	if acc.Token() == "stale" {
		return upstream.NewError(upstream.KindAuth, 401, "expired token")
	}
	return nil
}

func (u *healingUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *healingUpstream) Chat(_ context.Context, acc *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	if err := u.call(acc); err != nil {
		return nil, err
	}
	return &upstream.ChatResponse{ID: "1", Model: req.Model, Content: "healed", FinishReason: "stop"}, nil
}

func (u *healingUpstream) ChatStream(_ context.Context, acc *account.Account, _ *upstream.ChatRequest, emit upstream.EmitFunc) error {
	if err := u.call(acc); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{Content: "healed", FinishReason: "stop"})
}

func (u *healingUpstream) Refresh(_ context.Context, acc *account.Account) error {
	if u.refreshFails {
		return upstream.NewError(upstream.KindAuth, 401, "refresh rejected")
	}
	acc.SetCredentials("fresh", "new-refresh", "", 0)
	return nil
}

func (u *healingUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func healingServer(t *testing.T, u *healingUpstream) (*Server, *pool.Pool) {
	t.Helper()
	upstream.Register(u)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })

	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("stale", "rt", "", 0)
	p := pool.New(pool.Options{MaxInFlight: 1, StickyEnabled: false, WaitMax: 50 * time.Millisecond})
	p.Replace([]*account.Account{acc})

	cfg := config.Default()
	cfg.APIKey = ""
	cfg.Pool.FailoverAttempts = 1 // isolate the refresh path from cross-account failover
	r := authrefresh.New(p, nil, authrefresh.Options{Within: time.Hour})
	return NewServer(&cfg, p, nil, WithRefresher(r)), p
}

func TestChatRefreshesOnAuthErrorNonStream(t *testing.T) {
	u := &healingUpstream{}
	s, p := healingServer(t, u)

	rec := postChat(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "healed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got := u.callCount(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial 401 + retry)", got)
	}
	if tok := p.List()[0].Token(); tok != "fresh" {
		t.Fatalf("token = %q, want refreshed", tok)
	}
}

func TestChatRefreshesOnAuthErrorStream(t *testing.T) {
	u := &healingUpstream{}
	s, _ := healingServer(t, u)

	rec := postChat(s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "healed") {
		t.Fatalf("stream body = %s", rec.Body.String())
	}
	if got := u.callCount(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestChatAuthErrorWithoutHealingDisablesAccount(t *testing.T) {
	u := &healingUpstream{refreshFails: true}
	s, p := healingServer(t, u)

	rec := postChat(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == 200 {
		t.Fatalf("expected failure, got 200: %s", rec.Body.String())
	}
	if got := u.callCount(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry when refresh fails)", got)
	}
	if st := p.Describe()[0]; st.Enabled {
		t.Fatal("account should be disabled after an unrecoverable auth failure")
	}
}
