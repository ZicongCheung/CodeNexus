// Package api exposes the HTTP surface of the gateway: the OpenAI-compatible
// protocol entry points, health and status endpoints.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/httpauth"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// ServiceName is reported by /healthz and /status.
const ServiceName = "codenexus"

// Server wires the gateway routes.
type Server struct {
	cfg   *config.Config
	pool  *pool.Pool
	panel http.Handler
	root  http.Handler
}

// NewServer builds the HTTP handler. panel may be nil to disable the console.
func NewServer(cfg *config.Config, p *pool.Pool, panel http.Handler) *Server {
	s := &Server{cfg: cfg, pool: p, panel: panel}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
	if panel != nil {
		mux.Handle("/panel/", panel)
		mux.Handle("/panel", panel)
	}
	s.root = mux
	return s
}

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

// resolveSource maps a request model to a source and the source-local model name.
//
// A model of the form "<source>/<name>" pins the source explicitly. Otherwise
// the model keeps its flat name and the source is chosen as the first
// registered upstream that has an available account. When no source has any
// account the first registered source is still returned, so the caller reports
// "service unavailable" rather than a client error.
func (s *Server) resolveSource(model string) (upstream.Source, string, error) {
	if i := strings.IndexByte(model, '/'); i > 0 {
		prefix := upstream.Source(model[:i])
		if _, ok := upstream.Get(prefix); ok {
			return prefix, model[i+1:], nil
		}
	}
	sources := upstream.Sources()
	for _, src := range sources {
		if s.pool.HasSource(src) {
			return src, model, nil
		}
	}
	if len(sources) > 0 {
		return sources[0], model, nil
	}
	return "", model, errNoUpstream
}
