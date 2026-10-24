// Package panel serves the embedded web console.
//
// The console has two surfaces: a static single-page app built from web/ and
// embedded at compile time, and a small admin API under /panel/api/ for account
// onboarding and operations. The API is reachable without a token from the
// loopback interface and requires the gateway api_key from anywhere else, so a
// default (loopback-only) install is usable immediately.
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/assets"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/httpauth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/scheduler"
	"github.com/ZicongCheung/CodeNexus/internal/usagestore"
)

//go:embed dist
var distFS embed.FS

// ServiceName is reported by the console session endpoint.
const ServiceName = "codenexus"

// loginTTL bounds how long an unfinished device authorization is remembered.
const loginTTL = 15 * time.Minute

type loginSession struct {
	realm   string
	source  account.Source
	created time.Time
}

// Console is the web console handler.
type Console struct {
	cfg    *config.Config
	store  *authstore.Store
	pool   *pool.Pool
	sched  *scheduler.Scheduler
	reqlog *reqlog.Recorder
	// usage is swapped atomically so the console can turn the history on or off
	// without a restart.
	usage  atomic.Pointer[usagestore.Store]
	static http.Handler
	logf   func(string, ...any)

	// cfgPath is the file the console rewrites, and applyHot/restart let a save
	// take effect without the operator touching the terminal.
	cfgPath  string
	applyHot func(next *config.Config, diff []string) (applied, restart []string)
	restart  func() error

	// version, startedAt and listen describe this process to the console's
	// service panel. They are injected rather than read from a package global
	// so the panel never reaches back into the command that built it.
	version   string
	startedAt time.Time
	listen    string

	mux *http.ServeMux

	mu     sync.Mutex
	logins map[string]loginSession
	// busy guards one in-flight task action per account, so a double click
	// cannot start two sweeps over the same credential.
	busy map[string]bool
}

// Option customizes a Console.
type Option func(*Console)

// WithLogger installs a logger for console-side warnings.
func WithLogger(fn func(string, ...any)) Option { return func(c *Console) { c.logf = fn } }

// WithScheduler connects the console to the task scheduler, enabling the task
// center. Without it the task endpoints report the subsystem as unavailable.
func WithScheduler(s *scheduler.Scheduler) Option { return func(c *Console) { c.sched = s } }

// WithReqLog connects the console to the request log, enabling the
// observability endpoints.
func WithReqLog(r *reqlog.Recorder) Option { return func(c *Console) { c.reqlog = r } }

// WithUsage connects the console to the persisted usage history, enabling the
// usage page. Without it the page reports itself as unavailable.
func WithUsage(s *usagestore.Store) Option { return func(c *Console) { c.usage.Store(s) } }

// WithConfigPath points the console at the file it may rewrite. Without it the
// settings page is read-only.
func WithConfigPath(path string) Option { return func(c *Console) { c.cfgPath = path } }

// WithHotApply installs the hook that pushes a saved config into the running
// components. It reports which paths took effect and which need a restart.
func WithHotApply(fn func(next *config.Config, diff []string) (applied, restart []string)) Option {
	return func(c *Console) { c.applyHot = fn }
}

// WithRestart enables the restart button for changes that cannot be applied in
// place. Without it the console tells the user to restart by hand.
func WithRestart(fn func() error) Option { return func(c *Console) { c.restart = fn } }

// WithProcessInfo supplies what the service panel shows about this process.
// All three are optional; anything left unset is reported as null so the
// console can show a placeholder instead of guessing.
func WithProcessInfo(version, listen string, startedAt time.Time) Option {
	return func(c *Console) {
		c.version = version
		c.listen = listen
		c.startedAt = startedAt
	}
}

// SetUsage swaps the persisted usage history at runtime.
func (c *Console) SetUsage(s *usagestore.Store) { c.usage.Store(s) }

// New builds the console handler. store and p may be nil to disable account
// management (the static console still serves).
func New(cfg *config.Config, store *authstore.Store, p *pool.Pool, opts ...Option) *Console {
	c := &Console{
		cfg:    cfg,
		store:  store,
		pool:   p,
		logins: map[string]loginSession{},
		busy:   map[string]bool{},
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		c.static = http.NotFoundHandler()
	} else {
		c.static = http.FileServer(http.FS(sub))
	}
	c.mux = c.buildMux()
	return c
}

// ServeHTTP implements http.Handler.
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p := strings.TrimPrefix(r.URL.Path, "/panel")
	if p == "" {
		p = "/"
	}
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		c.serveAPI(w, r, p)
		return
	}
	// The rail's mark ships inside the binary next to the shell itself: the
	// console renders it before any session exists, so it cannot live behind
	// the API's key. See handleLogo.
	if p == "/logo.png" {
		c.handleLogo(w, r)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = p
	c.static.ServeHTTP(w, r2)
}

// buildMux wires the admin API routes. Method-qualified patterns keep the
// read and write halves of an endpoint distinct.
func (c *Console) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/session", c.handleSession)

	// Account onboarding.
	mux.HandleFunc("GET /api/accounts", c.handleAccounts)
	mux.HandleFunc("POST /api/accounts/remove", c.handleRemoveAccount)
	mux.HandleFunc("POST /api/auth/start", c.handleAuthStart)
	mux.HandleFunc("GET /api/auth/poll", c.handleAuthPoll)
	mux.HandleFunc("POST /api/auth/token", c.handleAuthToken)
	mux.HandleFunc("POST /api/auth/local", c.handleAuthLocal)
	mux.HandleFunc("POST /api/auth/import", c.handleImport)

	// Task center: catalogue and per-account operations.
	mux.HandleFunc("GET /api/accounts/{uid}/tasks", c.handleAccountTasks)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/accept", c.handleTaskAccept)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/claim", c.handleTaskClaim)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/auto", c.handleTaskAuto)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/auto_all", c.handleTaskAutoAll)
	mux.HandleFunc("GET /api/accounts/{uid}/packages", c.handleAccountPackages)
	mux.HandleFunc("GET /api/credits", c.handleCredits)
	mux.HandleFunc("GET /api/models", c.handleModels)
	mux.HandleFunc("POST /api/models/test", c.handleModelTest)
	mux.HandleFunc("POST /api/accounts/{uid}/{op}", c.handleAccountOp)

	// Task center: whole-pool operations.
	mux.HandleFunc("POST /api/batch/{op}", c.handleBatch)
	mux.HandleFunc("POST /api/tasks/scan_all", c.handleScanAll)
	mux.HandleFunc("GET /api/config", c.handleConfigGet)
	mux.HandleFunc("POST /api/config", c.handleConfigSave)
	mux.HandleFunc("GET /api/config/snapshots", c.handleConfigSnapshots)
	mux.HandleFunc("POST /api/config/rollback", c.handleConfigRollback)
	mux.HandleFunc("POST /api/restart", c.handleRestart)
	mux.HandleFunc("GET /api/usage/summary", c.handleUsageSummary)
	mux.HandleFunc("GET /api/usage/daily", c.handleUsageDaily)
	mux.HandleFunc("GET /api/usage/records", c.handleUsageRecords)
	mux.HandleFunc("GET /api/tasks/queue", c.handleQueue)
	mux.HandleFunc("POST /api/tasks/run_queue", c.handleRunQueue)

	// Observability.
	mux.HandleFunc("GET /api/observability/metrics", c.handleMetrics)
	mux.HandleFunc("GET /api/observability/logs", c.handleLogs)
	// The process log is streamed, not polled: see handleLogStream.
	mux.HandleFunc("GET /api/logs/stream", c.handleLogStream)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "unknown console endpoint")
	})
	return mux
}

// The logo's validator is derived from the embedded bytes, so a build that
// ships new artwork changes the ETag and the browser refetches — a fixed
// constant would either go stale forever or refetch on every reload.
var (
	logoOnce sync.Once
	logoTag  string
)

func logoETag() string {
	logoOnce.Do(func() {
		sum := sha256.Sum256(assets.IconPNG)
		logoTag = `"` + hex.EncodeToString(sum[:8]) + `"`
	})
	return logoTag
}

// handleLogo serves the brand artwork. It is public like the shell itself —
// the rail renders the mark before any session exists — and it answers
// If-None-Match so a cached copy costs one 304 instead of the full PNG.
func (c *Console) handleLogo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "logo is read-only")
		return
	}
	etag := logoETag()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(assets.IconPNG)
}

// handleAccountOp separates the single-account state toggles from the
// single-account task triggers, which share the {uid}/{op} shape.
func (c *Console) handleAccountOp(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("op") {
	case "pause", "resume", "disable", "revive":
		c.handleAccountState(w, r)
	case "checkin", "activity", "balance", "travel", "keepalive":
		c.handleAccountAction(w, r)
	default:
		writeErr(w, http.StatusNotFound, "unknown console endpoint")
	}
}

// serveAPI dispatches admin API calls after enforcing access control.
func (c *Console) serveAPI(w http.ResponseWriter, r *http.Request, path string) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if !c.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "console access requires the gateway api_key")
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	c.mux.ServeHTTP(w, r2)
}

// authorized reports whether the caller may use the admin API. Loopback callers
// are trusted; everyone else must present the gateway api_key.
func (c *Console) authorized(r *http.Request) bool {
	if isLoopback(r.RemoteAddr) {
		return true
	}
	return httpauth.VerifyBearer(r, c.cfg.APIKey)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// tryLock claims the per-account task slot, reporting false when an action for
// that account is already running.
func (c *Console) tryLock(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[key] {
		return false
	}
	c.busy[key] = true
	return true
}

func (c *Console) unlock(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, key)
}

func (c *Console) warn(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// handleSession describes the running gateway to the console. It carries no
// credentials and no upstream calls: everything here is either a constant of
// this process or a counter it already keeps, so the overview can render
// without touching either vendor.
func (c *Console) handleSession(w http.ResponseWriter, _ *http.Request) {
	accounts := 0
	if c.pool != nil {
		accounts = len(c.pool.Describe())
	}
	device, manual := onboardingModes()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        ServiceName,
		"accounts":       accounts,
		"sources":        sourceNames(),
		"device_sources": device,
		"manual_sources": manual,
		"local_sources":  localSources(),
		"tasks":          c.sched != nil,
		"reqlog":         c.reqlog != nil,
		// The overview's service panel reads these. Absent fields stay null so
		// the console can show a placeholder rather than inventing a value.
		"version": c.sessionVersion(),
		"uptime":  c.sessionUptime(),
		"listen":  c.sessionListen(),
		"capabilities": map[string]any{
			"sticky_sessions": c.sessionBool(c.sessionSticky),
			"checkin_enabled": c.sessionBool(c.sessionCheckin),
			"max_mode":        c.sessionBool(c.sessionMaxMode),
		},
	})
}

// The service panel's values are read from the live config and from constants
// of this process. Every accessor tolerates a missing config so a console
// opened before the gateway is fully wired still renders.

func (c *Console) sessionVersion() any {
	if c.version == "" {
		return nil
	}
	return c.version
}

func (c *Console) sessionUptime() any {
	if c.startedAt.IsZero() {
		return nil
	}
	return int64(time.Since(c.startedAt).Seconds())
}

func (c *Console) sessionListen() any {
	if c.listen != "" {
		return c.listen
	}
	if c.cfg != nil && c.cfg.Listen != "" {
		return c.cfg.Listen
	}
	return nil
}

func (c *Console) sessionBool(get func(*config.Config) (bool, bool)) any {
	if c.cfg == nil {
		return nil
	}
	if v, ok := get(c.cfg); ok {
		return v
	}
	return nil
}

func (c *Console) sessionSticky(cfg *config.Config) (bool, bool) {
	return cfg.SessionSticky.Enabled, true
}

func (c *Console) sessionCheckin(cfg *config.Config) (bool, bool) {
	return cfg.Schedule.CheckinEnabled, true
}

// sessionMaxMode reports the 1M profile switch. It lives on the Trae upstream
// because that is the surface the profile applies to, so the panel reads it
// there rather than from a generic feature flag.
func (c *Console) sessionMaxMode(cfg *config.Config) (bool, bool) {
	return cfg.Upstreams.TraeCN.MaxMode.Enabled, true
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}
