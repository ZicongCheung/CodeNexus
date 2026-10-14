package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
)

// Request logging records one event per completed chat request. A middleware
// owns the timing and the status code; the handler enriches the record with the
// account and model it settled on, which the middleware cannot know.

// recHint is the per-request enrichment the handler fills in.
type recHint struct {
	Account  string
	Model    string
	Stream   bool
	Attempts int
	Usage    *struct{ Prompt, Completion, Total int64 }
}

type recHintKey struct{}

func withRecHint(r *http.Request, h *recHint) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), recHintKey{}, h))
}

// hintFrom returns the record hint attached to a request, creating a detached
// one when logging is disabled so callers can write unconditionally.
func hintFrom(r *http.Request) *recHint {
	if h, ok := r.Context().Value(recHintKey{}).(*recHint); ok {
		return h
	}
	return &recHint{}
}

// statusWriter captures the response status while passing everything else
// through, including the flush that streaming depends on.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.status = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards the flush so SSE streaming keeps working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// record wraps a handler with request logging. With no recorder installed it
// returns the handler unchanged, so the wrapper costs nothing when disabled.
func (s *Server) record(source string, h http.HandlerFunc) http.HandlerFunc {
	if s.recorder == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		hint := &recHint{}
		start := time.Now()
		s.recorder.Begin()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, withRecHint(r, hint))
		dur := time.Since(start)

		ev := reqlog.Event{
			RequestID:  reqlog.NewRequestID(),
			Path:       r.URL.Path,
			Source:     source,
			Account:    hint.Account,
			Model:      hint.Model,
			Stream:     hint.Stream,
			Status:     sw.status,
			OK:         sw.status < 400,
			Outcome:    outcomeFor(sw.status),
			Attempts:   hint.Attempts,
			DurationMS: dur.Milliseconds(),
		}
		if hint.Usage != nil {
			ev.PromptTokens = hint.Usage.Prompt
			ev.CompletionTokens = hint.Usage.Completion
			ev.TotalTokens = hint.Usage.Total
			ev.TokensPerSecond = reqlog.Rate(hint.Usage.Completion, dur, 0)
		}
		s.recorder.Record(ev)
	}
}

func outcomeFor(status int) string {
	if status < 400 {
		return reqlog.OutcomeSuccess
	}
	return reqlog.OutcomeHTTPError
}
