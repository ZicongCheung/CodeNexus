// Package api exposes the HTTP surface of the gateway: the OpenAI-compatible
// protocol entry points, health and status endpoints.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/httpauth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
	"github.com/ZicongCheung/CodeNexus/internal/usagestore"
)

// ServiceName is reported by /healthz and /status.
const ServiceName = "codenexus"

// Refresher renews an account's credentials on demand. It is optional; when
// absent the gateway never attempts an in-request token refresh.
type Refresher interface {
	RefreshNow(ctx context.Context, acc *account.Account) error
}

// Option customizes a Server.
type Option func(*Server)

// WithRefresher installs a credential refresher used to heal auth failures.
func WithRefresher(r Refresher) Option { return func(s *Server) { s.refresher = r } }

// WithRecorder installs the request log recorder used by the console's
// observability endpoints.
func WithRecorder(r *reqlog.Recorder) Option { return func(s *Server) { s.recorder = r } }

// WithUsageStore installs the persisted usage history. Nil disables persistence
// and the console falls back to the in-memory ring alone.
func WithUsageStore(s *usagestore.Store) Option { return func(srv *Server) { srv.usage.Store(s) } }

// SetUsage swaps the persisted usage history at runtime, so the console's
// on/off switch takes effect without a restart. Nil turns persistence off.
func (s *Server) SetUsage(st *usagestore.Store) { s.usage.Store(st) }

// SetRouterConfig rebuilds model naming and cross-source policy in place. The
// loaded catalogues are kept, so a naming change does not cost a refetch.
func (s *Server) SetRouterConfig(cfg config.RouterConfig) { s.router.SetConfig(cfg) }

// Server wires the gateway routes.
type Server struct {
	cfg       *config.Config
	pool      *pool.Pool
	panel     http.Handler
	refresher Refresher
	recorder  *reqlog.Recorder
	// usage is swapped atomically: the console can turn the history on or off
	// without a restart, and request handlers read it concurrently.
	usage     atomic.Pointer[usagestore.Store]
	router    *modelRouter
	gate      *snapshotGate
	respCache *responsesCache
	// autoContinue is swapped atomically so the console can retune the
	// truncation re-issue without a restart.
	autoCont atomic.Pointer[config.AutoContinueConfig]
	// stall is the stream watchdog's silence bound, swapped atomically for the
	// same reason: it is retuned from the console.
	stall atomic.Pointer[time.Duration]
	root  http.Handler
}

// Responses session cache defaults, mirroring the reference relay: bound both
// the entry count and the bytes so a long chain cannot grow without limit.
// The defaults are the fallbacks; the config section overrides them.
const (
	responsesCacheTTL      = time.Hour
	responsesCacheMaxN     = 1024
	responsesCacheMaxBytes = 2 << 20
	responsesCacheMaxMsgs  = 256
)

// responsesCacheLimits resolves the configured bounds, keeping the built-in
// values for anything the configuration left at zero.
func responsesCacheLimits(cfg config.ResponsesConfig) (time.Duration, int, int, int) {
	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = responsesCacheTTL
	}
	maxN, maxBytes, maxMsgs := cfg.MaxEntries, cfg.MaxBytes, cfg.MaxMessages
	if maxN <= 0 {
		maxN = responsesCacheMaxN
	}
	if maxBytes <= 0 {
		maxBytes = responsesCacheMaxBytes
	}
	if maxMsgs <= 0 {
		maxMsgs = responsesCacheMaxMsgs
	}
	return ttl, maxN, maxBytes, maxMsgs
}

// NewServer builds the HTTP handler. panel may be nil to disable the console.
func NewServer(cfg *config.Config, p *pool.Pool, panel http.Handler, opts ...Option) *Server {
	ttl, maxN, maxBytes, maxMsgs := responsesCacheLimits(cfg.Responses)
	s := &Server{
		cfg:       cfg,
		pool:      p,
		panel:     panel,
		gate:      newSnapshotGate(),
		respCache: newResponsesCache(ttl, maxN, maxBytes, maxMsgs),
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	s.SetAutoContinue(cfg.Upstreams.TraeCN.AutoContinue)
	s.SetStreamStall(cfg.Stream.StallSeconds)
	s.router = newModelRouter(cfg.Router, p)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.record("chat", s.withAuth(s.handleChat)))
	mux.HandleFunc("/v1/messages", s.record("anthropic", s.withAuth(s.handleAnthropicMessages)))
	mux.HandleFunc("/v1/messages/count_tokens", s.withAuth(s.handleAnthropicCountTokens))
	mux.HandleFunc("/v1/responses", s.record("responses", s.withAuth(s.handleResponses)))
	// The TraeWork custom-model ingress. Its own alias paths take the dialect
	// directly; the ordinary chat URL is recognized by the detector inside
	// handleChat, so an OpenAI caller is never hijacked. The connectivity probe
	// is deliberately unauthenticated: the client runs it before it has been
	// given anything to authenticate with, and it only echoes the model
	// identity back.
	for _, path := range traeWorkRawAliases {
		mux.HandleFunc(path, s.record("traework", s.withAuth(s.handleTraeWorkChat)))
	}
	for _, path := range traeWorkConnectivityAliases {
		mux.HandleFunc(path, s.handleTraeWorkConnectivity)
	}
	if panel != nil {
		mux.Handle("/panel/", panel)
		mux.Handle("/panel", panel)
	}
	s.root = mux
	return s
}

// WarmModels loads the source catalogues in the background. It is best-effort:
// routing works without the catalogues and refines itself as they land.
func (s *Server) WarmModels(ctx context.Context) { s.router.Warm(ctx) }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.root.ServeHTTP(w, r) }

func (s *Server) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, s.cfg.APIKey) {
			writeError(w, http.StatusUnauthorized, "invalid api key", "authentication_error")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg, typ string) {
	if typ == "" {
		typ = "invalid_request_error"
	}
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
		},
	})
}

// primedByModels records a catalogue read by the /v1/models handler so the
// router does not have to fetch the same catalogue again for routing.
func (r *modelRouter) prime(src upstream.Source, models []upstream.ModelSpec) {
	if len(models) == 0 {
		return
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		if m.ID != "" {
			set[strings.ToLower(m.ID)] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.catalog[src] = catalogEntry{models: set, at: r.now()}
}
