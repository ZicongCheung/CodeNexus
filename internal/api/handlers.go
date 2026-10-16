package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

var errNoUpstream = errors.New("no upstream available for model")

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	enabled, total := s.pool.Stats()
	status := "ok"
	code := http.StatusOK
	if total > 0 && enabled == 0 {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, code, map[string]any{
		"status":          status,
		"service":         ServiceName,
		"accounts":        map[string]int{"enabled": enabled, "total": total},
		"sticky_sessions": s.pool.StickyCount(),
		"time":            time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, total := s.pool.Stats()
	sources := make([]string, 0)
	for _, src := range upstream.Sources() {
		sources = append(sources, string(src))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         ServiceName,
		"accounts":        map[string]int{"enabled": enabled, "total": total},
		"sources":         sources,
		"sticky_sessions": s.pool.StickyCount(),
	})
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	seen := map[string]bool{}
	data := make([]modelItem, 0)

	// The live catalogue is the authority, because it carries what an account is
	// actually entitled to. Reading it also primes the router's cache, so a
	// client that lists models gets faster routing afterwards.
	for _, src := range upstream.Sources() {
		u, ok := upstream.Get(src)
		if !ok {
			continue
		}
		acc := s.pool.Candidate(src)
		if acc == nil && !s.pool.HasSource(src) {
			continue
		}
		models, err := u.ListModels(ctx, acc)
		if err != nil {
			logx.Warnf("models: %s: %v", src, err)
			continue
		}
		s.router.prime(src, models)
		for _, m := range models {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			data = append(data, modelItem{ID: m.ID, Object: "model", OwnedBy: string(m.Source)})
		}
	}
	// Fall back to the cached union for any source that could not be read
	// live, so a transient upstream failure does not empty the list.
	for _, m := range s.router.ModelCatalog() {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		data = append(data, modelItem{ID: m.ID, Object: "model", OwnedBy: string(m.Source)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	// A TraeWork custom model may post to the ordinary chat URL, but its
	// headers and body are distinct enough to route it without changing what
	// an OpenAI caller gets.
	if looksLikeTraeWork(r.URL.Path, r.Header, body) {
		s.serveTraeWork(w, r, body)
		return
	}
	t, err := parseChatTurn(r, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	s.runChat(w, r, t)
}

// decodeBody reads a JSON object request body.
func decodeBody(r *http.Request) (map[string]any, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("invalid JSON body: " + err.Error())
	}
	return body, nil
}

// defaultTurnFail renders a failure in the OpenAI error shape: a client-side
// failure becomes a 400, and a pool or upstream failure is dispatched on its
// own type.
func defaultTurnFail(w http.ResponseWriter, err error, client bool) {
	if client {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		writeUpstreamError(w, ue)
		return
	}
	writePoolError(w, err)
}

// writePoolError reports that no account could be leased.
func writePoolError(w http.ResponseWriter, err error) {
	var na *pool.ErrNoAccount
	if errors.As(err, &na) {
		if na.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(na.RetryAfter.Seconds()))))
		}
		writeError(w, http.StatusServiceUnavailable, "no available account: "+na.Reason, "overloaded_error")
		return
	}
	writeError(w, http.StatusServiceUnavailable, err.Error(), "overloaded_error")
}

func writeUpstreamError(w http.ResponseWriter, ue *upstream.Error) {
	if ue == nil {
		writeError(w, http.StatusBadGateway, "upstream error", "upstream_error")
		return
	}
	code := http.StatusBadGateway
	typ := "upstream_error"
	switch ue.Kind {
	case upstream.KindBadRequest:
		code, typ = http.StatusBadRequest, "invalid_request_error"
	case upstream.KindContentBlocked:
		code, typ = http.StatusBadRequest, "content_filter"
	case upstream.KindRateLimited:
		code, typ = http.StatusTooManyRequests, "rate_limit_error"
	case upstream.KindQuota:
		code, typ = http.StatusTooManyRequests, "insufficient_quota"
	case upstream.KindTimeout:
		code, typ = http.StatusGatewayTimeout, "timeout_error"
	}
	if ue.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ue.RetryAfter.Seconds()))))
	}
	writeError(w, code, ue.Message, typ)
}

// accountLabel renders an account for the request log. It prefers the display
// label so an operator recognises the account without seeing a token.
func accountLabel(acc *account.Account) string {
	if acc == nil {
		return ""
	}
	if acc.Label != "" {
		return acc.Label
	}
	return acc.UID
}
