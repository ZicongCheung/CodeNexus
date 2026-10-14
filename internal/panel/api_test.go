package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/crypto"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// fakeUpstream implements Upstream and DeviceAuth for console tests.
type fakeUpstream struct {
	mu   sync.Mutex
	done bool
}

func (f *fakeUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (f *fakeUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return nil, nil
}
func (f *fakeUpstream) Chat(context.Context, *account.Account, *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	return &upstream.ChatResponse{}, nil
}
func (f *fakeUpstream) ChatStream(context.Context, *account.Account, *upstream.ChatRequest, upstream.EmitFunc) error {
	return nil
}
func (f *fakeUpstream) Refresh(context.Context, *account.Account) error { return nil }
func (f *fakeUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (f *fakeUpstream) DeviceStart(_ context.Context, realm string) (*upstream.DeviceCode, error) {
	return &upstream.DeviceCode{State: "state-1", URL: "https://example.test/authorize", Realm: realm}, nil
}

func (f *fakeUpstream) DevicePoll(_ context.Context, _, _ string) (*account.Account, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done {
		return nil, false, nil
	}
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u-device", Label: "Device", Enabled: true}
	acc.SetCredentials("tok", "rt", "", 0)
	return acc, true, nil
}

func newConsole(t *testing.T) (*Console, *authstore.Store, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	store := authstore.New(dir, authstore.WithKeeper(crypto.NewMemoryKeeper()))
	p := pool.New(pool.Options{})
	cfg := config.Default()
	cfg.APIKey = "secret"
	cfg.AuthDir = dir
	return New(&cfg, store, p), store, p
}

func call(c *Console, method, target, body string, loopback bool, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if loopback {
		req.RemoteAddr = "127.0.0.1:5555"
	} else {
		req.RemoteAddr = "203.0.113.7:4444"
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec
}

func TestConsoleLoopbackNeedsNoKey(t *testing.T) {
	c, _, _ := newConsole(t)
	if rec := call(c, http.MethodGet, "/panel/api/session", "", true, ""); rec.Code != 200 {
		t.Fatalf("loopback session code = %d", rec.Code)
	}
}

func TestConsoleRemoteRequiresKey(t *testing.T) {
	c, _, _ := newConsole(t)
	if rec := call(c, http.MethodGet, "/panel/api/session", "", false, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote without key code = %d, want 401", rec.Code)
	}
	if rec := call(c, http.MethodGet, "/panel/api/session", "", false, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with wrong key code = %d, want 401", rec.Code)
	}
	if rec := call(c, http.MethodGet, "/panel/api/session", "", false, "secret"); rec.Code != 200 {
		t.Fatalf("remote with key code = %d, want 200", rec.Code)
	}
}

func TestConsoleUnknownEndpoint(t *testing.T) {
	c, _, _ := newConsole(t)
	if rec := call(c, http.MethodGet, "/panel/api/nope", "", true, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestConsoleImportInline(t *testing.T) {
	c, store, p := newConsole(t)
	body := `{"accounts":[
		{"uid":"a1","access_token":"t1","refresh_token":"r1","nickname":"Alice"},
		{"uid":"a2","access_token":"t2"},
		{"uid":"","access_token":"t3"}
	]}`
	rec := call(c, http.MethodPost, "/panel/api/auth/import", body, true, "")
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Imported != 2 || out.Skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want 2/1", out.Imported, out.Skipped)
	}
	if got := len(p.Describe()); got != 2 {
		t.Fatalf("pool size = %d, want 2", got)
	}
	// Credentials must be persisted (encrypted) on disk.
	loaded, err := store.Load()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("store load = %d accounts, err = %v", len(loaded), err)
	}
}

func TestConsoleImportFromPath(t *testing.T) {
	c, _, p := newConsole(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	raw := `[{"uid":"p1","access_token":"tok","expires_at":1700000000000}]`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": path})
	rec := call(c, http.MethodPost, "/panel/api/auth/import", string(body), true, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"imported":1`) {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	st := p.Describe()
	if len(st) != 1 || st[0].UID != "p1" {
		t.Fatalf("pool = %+v", st)
	}
	// Millisecond expiry should be normalized to seconds.
	if exp := p.List()[0].ExpiresAtValue(); exp != 1700000000 {
		t.Fatalf("expiry = %d, want 1700000000", exp)
	}
}

func TestConsoleRemoveAccount(t *testing.T) {
	c, store, p := newConsole(t)
	body := `{"accounts":[{"uid":"a1","access_token":"t1"}]}`
	if rec := call(c, http.MethodPost, "/panel/api/auth/import", body, true, ""); rec.Code != 200 {
		t.Fatalf("import code = %d", rec.Code)
	}
	key := p.Describe()[0].Key

	rec := call(c, http.MethodPost, "/panel/api/accounts/remove", `{"key":"`+key+`"}`, true, "")
	if rec.Code != 200 {
		t.Fatalf("remove code = %d body = %s", rec.Code, rec.Body.String())
	}
	if len(p.Describe()) != 0 {
		t.Fatal("account still in pool after removal")
	}
	if loaded, _ := store.Load(); len(loaded) != 0 {
		t.Fatal("credential file still on disk after removal")
	}
}

func TestConsoleDeviceAuthFlow(t *testing.T) {
	c, store, p := newConsole(t)
	fake := &fakeUpstream{}
	upstream.Register(fake)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })

	rec := call(c, http.MethodPost, "/panel/api/auth/start", `{"realm":"cn"}`, true, "")
	if rec.Code != 200 {
		t.Fatalf("start code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "state-1") {
		t.Fatalf("start body = %s", rec.Body.String())
	}

	// Not yet authorized.
	rec = call(c, http.MethodGet, "/panel/api/auth/poll?state=state-1", "", true, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"done":false`) {
		t.Fatalf("poll body = %s", rec.Body.String())
	}

	// User completes authorization.
	fake.mu.Lock()
	fake.done = true
	fake.mu.Unlock()

	rec = call(c, http.MethodGet, "/panel/api/auth/poll?state=state-1", "", true, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("poll body = %s", rec.Body.String())
	}
	if len(p.Describe()) != 1 {
		t.Fatalf("pool size = %d, want 1", len(p.Describe()))
	}
	if loaded, _ := store.Load(); len(loaded) != 1 || loaded[0].Token() != "tok" {
		t.Fatalf("store = %+v", loaded)
	}

	// The state is single-use.
	if rec := call(c, http.MethodGet, "/panel/api/auth/poll?state=state-1", "", true, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("reused state code = %d, want 404", rec.Code)
	}
}

func TestConsoleAccountsListHidesSecrets(t *testing.T) {
	c, _, _ := newConsole(t)
	body := `{"accounts":[{"uid":"a1","access_token":"top-secret-token"}]}`
	if rec := call(c, http.MethodPost, "/panel/api/auth/import", body, true, ""); rec.Code != 200 {
		t.Fatalf("import failed: %d", rec.Code)
	}
	rec := call(c, http.MethodGet, "/panel/api/accounts", "", true, "")
	if rec.Code != 200 {
		t.Fatalf("accounts code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "top-secret-token") {
		t.Fatal("accounts listing leaks the access token")
	}
}
