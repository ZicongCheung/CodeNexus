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

	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/httpauth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
)

//go:embed dist
var distFS embed.FS

// ServiceName is reported by the console session endpoint.
const ServiceName = "codenexus"

// loginTTL bounds how long an unfinished device authorization is remembered.
const loginTTL = 15 * time.Minute

type loginSession struct {
	realm   string
	created time.Time
}

// Console is the web console handler.
type Console struct {
	cfg    *config.Config
	store  *authstore.Store
	pool   *pool.Pool
	static http.Handler
	logf   func(string, ...any)

	mu     sync.Mutex
	logins map[string]loginSession
}

// Option customizes a Console.
type Option func(*Console)

// WithLogger installs a logger for console-side warnings.
func WithLogger(fn func(string, ...any)) Option { return func(c *Console) { c.logf = fn } }

// New builds the console handler. store and p may be nil to disable account
// management (the static console still serves).
func New(cfg *config.Config, store *authstore.Store, p *pool.Pool, opts ...Option) *Console {
	c := &Console{cfg: cfg, store: store, pool: p, logins: map[string]loginSession{}}
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

// serveAPI dispatches admin API calls after enforcing access control.
func (c *Console) serveAPI(w http.ResponseWriter, r *http.Request, path string) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if !c.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "console access requires the gateway api_key")
		return
	}
	switch {
	case path == "/api/session" && r.Method == http.MethodGet:
		c.handleSession(w, r)
	case path == "/api/accounts" && r.Method == http.MethodGet:
		c.handleAccounts(w, r)
	case path == "/api/accounts/remove" && r.Method == http.MethodPost:
		c.handleRemoveAccount(w, r)
	case path == "/api/auth/start" && r.Method == http.MethodPost:
		c.handleAuthStart(w, r)
	case path == "/api/auth/poll" && r.Method == http.MethodGet:
		c.handleAuthPoll(w, r)
	case path == "/api/auth/import" && r.Method == http.MethodPost:
		c.handleImport(w, r)
	default:
		writeErr(w, http.StatusNotFound, "unknown console endpoint")
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  ServiceName,
		"accounts": accounts,
		"sources":  sourceNames(),
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
