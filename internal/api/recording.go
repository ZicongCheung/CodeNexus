package api

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
	"github.com/ZicongCheung/CodeNexus/internal/usagestore"
)

// Request logging records one event per completed chat request. A middleware
// owns the timing and the status code; the handler enriches the record with the
// account and model it settled on, which the middleware cannot know.

// recHint is the per-request enrichment the handler fills in.
type recHint struct {
	// RequestID is minted before the handler runs so an out-of-band settlement
	// can find the record it belongs to.
	RequestID string
	Account   string
	Model     string
	Stream    bool
	Attempts  int
	Usage     *struct{ Prompt, Completion, Total int64 }
	// TurnID is the upstream turn identifier the billing lookup is keyed on.
	TurnID string
	// Credits and CreditsSource are a charge the response already reported.
	Credits       float64
	CreditsSource string
	// Snapshot is the open attribution window for the turn in flight. It is
	// owned by whichever settlement consumes it, and must be released on every
	// path that leaves the handler.
	Snapshot *creditSnapshot
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
		hint := &recHint{RequestID: reqlog.NewRequestID()}
		start := time.Now()
		s.recorder.Begin()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, withRecHint(r, hint))
		dur := time.Since(start)

		ev := reqlog.Event{
			RequestID:  hint.RequestID,
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
		// Tier one: a charge the response already carried.
		if hint.CreditsSource != "" || hint.Credits > 0 {
			ev.CreditsConsumed = hint.Credits
			ev.CreditsSource = hint.CreditsSource
			if ev.CreditsSource == "" {
				ev.CreditsSource = creditsSourceUpstream
			}
		}
		s.recorder.Record(ev)
		// The persisted row is the console's history; the in-memory record is
		// its live view. Neither waits on the other.
		if st := s.usage.Load(); st != nil {
			go st.Insert(usageRecord(ev))
		}
	}
}

// usageRecord narrows an event into the row the usage history keeps. A label
// identifies the account; no prompt, answer or credential is carried.
func usageRecord(ev reqlog.Event) usagestore.Record {
	return usagestore.Record{
		Time:             ev.Time,
		RequestID:        ev.RequestID,
		Wire:             ev.Source,
		Source:           string(sourceOfPath(ev.Path)),
		Account:          ev.Account,
		Model:            ev.Model,
		Stream:           ev.Stream,
		Status:           ev.Status,
		Outcome:          ev.Outcome,
		Attempts:         ev.Attempts,
		DurationMS:       ev.DurationMS,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		TotalTokens:      ev.TotalTokens,
		Credits:          ev.CreditsConsumed,
		CreditsSource:    ev.CreditsSource,
	}
}

// sourceOfPath names the upstream family a request was served by, when the
// account label does not already say so.
func sourceOfPath(path string) upstream.Source {
	switch {
	case strings.Contains(path, "responses"):
		return "responses"
	default:
		return ""
	}
}

// Credits source markers. They name where a per-turn charge came from, and an
// unsettled turn stays pending rather than reading as a charge of zero.
const (
	creditsSourceUpstream      = "upstream"
	creditsSourcePending       = "pending"
	creditsSourceSnapshotDelta = "snapshot_delta"
)

// beginSettlement opens the attribution window for the attempt about to run.
// A window left over from an earlier attempt belongs to an account that is no
// longer serving the request, so it is retired first.
func (s *Server) beginSettlement(h *recHint, u upstream.Upstream, acc *account.Account) {
	if h == nil || s.recorder == nil {
		return
	}
	h.releaseSnapshot()
	h.Snapshot = s.openSnapshot(u, acc)
}

// releaseSnapshot retires the hint's open window, if any. It is idempotent, so
// a handler can defer it once and still let settlement take ownership.
func (h *recHint) releaseSnapshot() {
	if h == nil || h.Snapshot == nil {
		return
	}
	h.Snapshot.release()
	h.Snapshot = nil
}

// settleCredits fills in the turn's credit charge without making the client
// wait for a billing lookup. Anything the response already reported wins, so
// the out-of-band path only runs when there is something to learn.
func (s *Server) settleCredits(h *recHint, u upstream.Upstream, acc *account.Account) {
	if s.recorder == nil || h == nil || h.RequestID == "" {
		h.releaseSnapshot()
		return
	}
	// A charge the response already carried wins; the window closes at once.
	if h.CreditsSource != "" || h.Credits > 0 {
		h.releaseSnapshot()
		return
	}
	snap := h.Snapshot
	h.Snapshot = nil
	turn := strings.TrimSpace(h.TurnID)
	billing, ok := u.(upstream.TurnUsage)
	if turn == "" || !ok {
		snap.release()
		return
	}
	go func() {
		defer snap.release()
		ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
		defer cancel()
		credits, source, err := billing.TurnCredits(ctx, acc, turn)
		if err == nil {
			s.recorder.Update(h.RequestID, func(ev *reqlog.Event) {
				ev.CreditsConsumed = credits
				ev.CreditsSource = source
			})
			if st := s.usage.Load(); st != nil {
				_ = st.SetCredits(h.RequestID, credits, source)
			}
			return
		}
		// The upstream has no per-turn bill for this turn. Fall back to the
		// account's own counter, but only when the difference across the turn
		// belongs to this turn alone.
		credits, source = 0, creditsSourcePending
		if snap.attributable() {
			if before, ok := snap.beforeUsed(ctx); ok {
				if after, ok := billing.UsedCredits(ctx, acc); ok && after >= before {
					credits, source = roundCredits(after-before), creditsSourceSnapshotDelta
				}
			}
		}
		s.recorder.Update(h.RequestID, func(ev *reqlog.Event) {
			ev.CreditsConsumed = credits
			ev.CreditsSource = source
		})
		if st := s.usage.Load(); st != nil {
			_ = st.SetCredits(h.RequestID, credits, source)
		}
	}()
}

// settleTimeout bounds a settlement lookup. It is generous because the client
// is not waiting on it.
const settleTimeout = 25 * time.Second

// roundCredits trims a credit amount to two decimals, the precision the usage
// surface displays.
func roundCredits(v float64) float64 { return math.Round(v*100) / 100 }

func outcomeFor(status int) string {
	if status < 400 {
		return reqlog.OutcomeSuccess
	}
	return reqlog.OutcomeHTTPError
}
