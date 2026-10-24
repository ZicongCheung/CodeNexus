package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// The rail renders the mark before any session exists, so the logo route is
// public like the shell itself and answers a conditional GET with a 304 —
// otherwise every panel open pays for a PNG the browser already has.
func TestConsoleLogoServesEmbeddedArtwork(t *testing.T) {
	c, _, _ := newConsole(t)
	rec := call(c, http.MethodGet, "/panel/logo.png", "", false, "")
	if rec.Code != 200 {
		t.Fatalf("GET /panel/logo.png = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("no Cache-Control: the artwork is embedded and must be cacheable")
	}
	body := rec.Body.Bytes()
	if len(body) < 8 || string(body[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("body is %d bytes and does not start with a PNG signature", len(body))
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first response")
	}
	req := httptest.NewRequest(http.MethodGet, "/panel/logo.png", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	c.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body", rec2.Body.Len())
	}
}

func TestConsoleLogoRejectsWrites(t *testing.T) {
	c, _, _ := newConsole(t)
	if rec := call(c, http.MethodPost, "/panel/logo.png", "", false, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /panel/logo.png = %d, want 405", rec.Code)
	}
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

// fakeAcceptor is a Trae-shaped source that accepts a pasted credential, so the
// console's manual-onboarding path can be exercised without a live upstream.
type fakeAcceptor struct {
	fakeUpstream
	token, refresh, uid, label string
	err                        error
}

// errRejected is the sentinel a fake acceptor returns for a bad credential.
var errRejected = errors.New("credential rejected")

func (f *fakeAcceptor) Source() upstream.Source { return upstream.SourceTraeCN }

func (f *fakeAcceptor) AcceptCredential(token, refresh, uid, label string) (*account.Account, error) {
	f.token, f.refresh, f.uid, f.label = token, refresh, uid, label
	if f.err != nil {
		return nil, f.err
	}
	acc := &account.Account{Source: account.SourceTraeCN, UID: uid, Label: label, Enabled: true}
	acc.SetCredentials(token, refresh, "", 0)
	return acc, nil
}

func TestConsoleAuthTokenOnboardsPastedCredential(t *testing.T) {
	c, store, p := newConsole(t)
	fake := &fakeAcceptor{}
	upstream.Register(fake)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	body := `{"source":"trae-cn","token":"tok","refresh_token":"rt","uid":"t1","label":"Trae One"}`
	rec := call(c, http.MethodPost, "/panel/api/auth/token", body, true, "")
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if fake.uid != "t1" || fake.label != "Trae One" {
		t.Fatalf("adapter received uid=%q label=%q", fake.uid, fake.label)
	}
	st := p.Describe()
	if len(st) != 1 || st[0].Source != account.SourceTraeCN || st[0].UID != "t1" {
		t.Fatalf("pool = %+v", st)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 || loaded[0].Token() != "tok" {
		t.Fatalf("store = %+v err = %v", loaded, err)
	}
}

func TestConsoleAuthTokenRejectsBadCredential(t *testing.T) {
	c, _, p := newConsole(t)
	fake := &fakeAcceptor{err: errRejected}
	upstream.Register(fake)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	rec := call(c, http.MethodPost, "/panel/api/auth/token", `{"source":"trae-cn","token":"bad"}`, true, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if len(p.Describe()) != 0 {
		t.Fatal("a rejected credential must not enter the pool")
	}
}

func TestConsoleAuthTokenRejectsUnsupportedSource(t *testing.T) {
	c, _, _ := newConsole(t)
	// The WorkBuddy fake implements Upstream and DeviceAuth but not
	// CredentialAcceptor, so manual entry must be reported as unsupported.
	fake := &fakeUpstream{}
	upstream.Register(fake)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })

	rec := call(c, http.MethodPost, "/panel/api/auth/token", `{"source":"workbuddy","token":"tok","uid":"u1"}`, true, "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not accept") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
}

// fakeLocal is a Trae-shaped source that can import from this machine, so the
// console's one-click local import can be exercised without a real profile.
type fakeLocal struct {
	fakeUpstream
	accounts []*account.Account
	err      error
}

func (f *fakeLocal) Source() upstream.Source { return upstream.SourceTraeCN }

func (f *fakeLocal) DiscoverLocal() ([]*account.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.accounts, nil
}

func TestConsoleAuthLocalImportsDiscoveredAccounts(t *testing.T) {
	c, store, p := newConsole(t)
	first := &account.Account{Source: account.SourceTraeCN, UID: "u-1", Label: "本机 Trae CN", Enabled: true}
	first.SetCredentials("tok-1", "rt-1", "", 0)
	second := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Label: "本机 Trae CN", Enabled: true}
	second.SetCredentials("tok-2", "rt-2", "", 0)
	fake := &fakeLocal{accounts: []*account.Account{first, second}}
	upstream.Register(fake)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	rec := call(c, http.MethodPost, "/panel/api/auth/local", `{"source":"trae-cn"}`, true, "")
	if rec.Code != 200 {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"uid":"u-1"`) || !strings.Contains(rec.Body.String(), `"uid":"u-2"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if len(p.Describe()) != 2 {
		t.Fatalf("pool size = %d, want 2", len(p.Describe()))
	}
	loaded, err := store.Load()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("store = %+v err = %v", loaded, err)
	}
}

func TestConsoleAuthLocalRejectsUnsupportedSource(t *testing.T) {
	c, _, _ := newConsole(t)
	upstream.Register(&fakeUpstream{})
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })

	rec := call(c, http.MethodPost, "/panel/api/auth/local", `{"source":"workbuddy"}`, true, "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not support") {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestConsoleAuthLocalReportsDiscoveryFailure(t *testing.T) {
	c, _, p := newConsole(t)
	upstream.Register(&fakeLocal{err: errors.New("no local client credential found")})
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	rec := call(c, http.MethodPost, "/panel/api/auth/local", `{"source":"trae-cn"}`, true, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if len(p.Describe()) != 0 {
		t.Fatal("a failed discovery must not touch the pool")
	}
}

func TestConsoleSessionReportsLocalSources(t *testing.T) {
	c, _, _ := newConsole(t)
	upstream.Register(&fakeLocal{})
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	rec := call(c, http.MethodGet, "/panel/api/session", "", true, "")
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"local_sources":["trae-cn"]`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestConsoleAuthStartReportsUnregisteredSource(t *testing.T) {
	c, _, _ := newConsole(t)
	// No Trae adapter is registered in this console, so an interactive login
	// for it must fail loudly instead of silently falling back to WorkBuddy.
	rec := call(c, http.MethodPost, "/panel/api/auth/start", `{"source":"trae-cn"}`, true, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
}
