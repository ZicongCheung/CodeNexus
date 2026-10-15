// Package panel serves the embedded web console.
//
// The console has two surfaces: a static single-page app built from web/ and
// embedded at compile time, and a small admin API under /panel/api/ for account
// onboarding and operations. The API is reachable without a token from the
// loopback interface and requires the gateway api_key from anywhere else, so a
// default (loopback-only) install is usable immediately.
package panel

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
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
	usage  *usagestore.Store
	static http.Handler
	logf   func(string, ...any)

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
func WithUsage(s *usagestore.Store) Option { return func(c *Console) { c.usage = s } }

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
	mux.HandleFunc("GET /api/tasks/catalog", c.handleTaskCatalog)
	mux.HandleFunc("GET /api/accounts/{uid}/tasks", c.handleAccountTasks)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/accept", c.handleTaskAccept)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/claim", c.handleTaskClaim)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/auto", c.handleTaskAuto)
	mux.HandleFunc("POST /api/accounts/{uid}/tasks/auto_all", c.handleTaskAutoAll)
	mux.HandleFunc("GET /api/accounts/{uid}/packages", c.handleAccountPackages)
	mux.HandleFunc("POST /api/accounts/{uid}/{op}", c.handleAccountOp)

	// Task center: whole-pool operations.
	mux.HandleFunc("POST /api/batch/{op}", c.handleBatch)
	mux.HandleFunc("POST /api/tasks/scan_all", c.handleScanAll)
	mux.HandleFunc("GET /api/usage/summary", c.handleUsageSummary)
	mux.HandleFunc("GET /api/usage/daily", c.handleUsageDaily)
	mux.HandleFunc("GET /api/usage/records", c.handleUsageRecords)
	mux.HandleFunc("GET /api/tasks/queue", c.handleQueue)
	mux.HandleFunc("POST /api/tasks/run_queue", c.handleRunQueue)

	// Observability.
	mux.HandleFunc("GET /api/observability/metrics", c.handleMetrics)
	mux.HandleFunc("GET /api/observability/logs", c.handleLogs)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "unknown console endpoint")
	})
	return mux
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
	})
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
