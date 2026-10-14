package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// stubService emulates the device authorization and refresh endpoints.
type stubService struct {
	mu       sync.Mutex
	tokenOK  bool
	badUID   bool
	refreshN int
}

func (s *stubService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("platform") != "CLI" {
			http.Error(w, "missing platform", 400)
			return
		}
		writeOK(w, map[string]any{"state": "state-1", "authUrl": "https://example.test/authorize"})
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ok := s.tokenOK
		s.mu.Unlock()
		if !ok {
			// Pending: business code is non-zero.
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "msg": "login ing"})
			return
		}
		writeOK(w, map[string]any{"accessToken": "at-1", "refreshToken": "rt-1", "expiresIn": 3600, "domain": "www.codebuddy.cn"})
	})
	mux.HandleFunc("/v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer", 401)
			return
		}
		uid := "u-1234"
		if s.badUID {
			uid = "../../evil"
		}
		writeOK(w, map[string]any{"uid": uid, "nickname": "Nick"})
	})
	mux.HandleFunc("/v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Refresh-Token") == "" {
			http.Error(w, "missing refresh token", 400)
			return
		}
		s.mu.Lock()
		s.refreshN++
		s.mu.Unlock()
		writeOK(w, map[string]any{"accessToken": "at-2", "refreshToken": "rt-2", "expiresIn": 7200, "domain": "www.codebuddy.cn"})
	})
	return mux
}

func writeOK(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "", "data": data})
}

func newStubClient(t *testing.T, s *stubService) *Client {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return New(Config{AuthBaseURL: srv.URL, TimeoutSeconds: 5})
}

func TestDeviceStart(t *testing.T) {
	c := newStubClient(t, &stubService{})
	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	if code.State != "state-1" || code.URL != "https://example.test/authorize" || code.Realm != "cn" {
		t.Fatalf("code = %+v", code)
	}
}

func TestDevicePollPendingThenDone(t *testing.T) {
	s := &stubService{}
	c := newStubClient(t, s)

	if acc, done, err := c.DevicePoll(context.Background(), "cn", "state-1"); err != nil || done || acc != nil {
		t.Fatalf("pending: acc=%v done=%v err=%v", acc, done, err)
	}

	s.mu.Lock()
	s.tokenOK = true
	s.mu.Unlock()

	acc, done, err := c.DevicePoll(context.Background(), "cn", "state-1")
	if err != nil || !done {
		t.Fatalf("done: err=%v done=%v", err, done)
	}
	if acc.UID != "u-1234" || acc.Label != "Nick" || acc.Token() != "at-1" || acc.RefreshTokenValue() != "rt-1" {
		t.Fatalf("account = %+v", acc)
	}
	if acc.ExpiresAtValue() <= time.Now().Unix() {
		t.Fatalf("expiry not set: %d", acc.ExpiresAtValue())
	}
}

func TestDevicePollRejectsUnsafeUID(t *testing.T) {
	s := &stubService{tokenOK: true, badUID: true}
	c := newStubClient(t, s)
	if _, _, err := c.DevicePoll(context.Background(), "cn", "state-1"); err == nil {
		t.Fatal("expected an unsafe uid to be rejected")
	}
}

func TestRefreshRotatesTokens(t *testing.T) {
	s := &stubService{}
	c := newStubClient(t, s)

	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("at-old", "rt-old", "www.codebuddy.cn", 0)

	if err := c.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if acc.Token() != "at-2" || acc.RefreshTokenValue() != "rt-2" {
		t.Fatalf("tokens not rotated: %q %q", acc.Token(), acc.RefreshTokenValue())
	}
	if acc.ExpiresAtValue() <= time.Now().Unix() {
		t.Fatal("expiry not updated")
	}
}

func TestRefreshRequiresRefreshToken(t *testing.T) {
	c := newStubClient(t, &stubService{})
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("at", "", "", 0)
	if err := c.Refresh(context.Background(), acc); err == nil {
		t.Fatal("expected an error without a refresh token")
	}
}

func TestRealmOfInfersGlobal(t *testing.T) {
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1"}
	acc.SetCredentials("t", "", "www.codebuddy.cn", 0)
	if got := realmOf(acc); got != "cn" {
		t.Fatalf("realm = %q, want cn", got)
	}
	acc.SetCredentials("t", "", "www.workbuddy.ai", 0)
	if got := realmOf(acc); got != "global" {
		t.Fatalf("realm = %q, want global", got)
	}
}
