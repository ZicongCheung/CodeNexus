package api

// The chat core is protocol-neutral. It owns the things that must behave the
// same whichever wire format a client speaks: picking an account, failing over
// before the first byte, refreshing a rejected credential, recording the
// request and settling its charge.
//
// Everything the wire format decides lives behind the turn/responder pair
// below, so adding a protocol does not touch this file.

import (
	"context"
	"net/http"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/logx"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// turn is one parsed client request: what to run upstream, plus how to render
// the answer in the client's own format.
type turn struct {
	// Name labels the route family in logs.
	Name string
	// Model is the client-facing name, echoed back in the answer.
	Model string
	// Request is the neutral request handed to the adapter.
	Request *upstream.ChatRequest
	// Stream is the client's transport choice.
	Stream bool
	// Session seeds pool stickiness.
	Session string
	// Result renders a completed answer. It is never called for a streaming
	// turn.
	Result func(w http.ResponseWriter, resp *upstream.ChatResponse)
	// Open starts the streaming responder. It is called lazily, on the first
	// delta worth showing, so a failure before that can still be retried on
	// another account without the client having seen a status code.
	Open func(w http.ResponseWriter) (responder, error)
}

// responder renders a streamed answer. Delta receives every upstream delta,
// including ones that carry nothing to show; a responder must treat those as
// no-ops rather than emitting empty frames.
type responder interface {
	Delta(d upstream.StreamDelta) error
	// End concludes a successful stream.
	End()
	// Fail concludes a stream that broke after it had already started.
	Fail(err error)
}

// runChat executes one client request, failing over between accounts while
// nothing has been written yet.
func (s *Server) runChat(w http.ResponseWriter, r *http.Request, t *turn) {
	hint := hintFrom(r)
	hint.Model = t.Model
	hint.Stream = t.Stream
	// Every path out of this handler must retire the attribution window, and a
	// window that settlement took over is already cleared, so this is a no-op
	// on the normal paths and the only release on the abandoned ones.
	defer hint.releaseSnapshot()

	sources, modelName, err := s.router.Plan(t.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	t.Request.Model = modelName

	attempts := s.cfg.Pool.FailoverAttempts
	if attempts < 1 {
		attempts = 1
	}
	tried := map[string]bool{}
	refreshed := map[string]bool{}
	var lastErr *upstream.Error

	for i := 0; i < attempts; i++ {
		lease, aerr := s.pool.Acquire(r.Context(), pool.PickOptions{
			Sources:    sources,
			Model:      modelName,
			SessionKey: t.Session,
			Exclude:    tried,
		})
		if aerr != nil {
			if i > 0 && lastErr != nil {
				// Keep the upstream's own explanation when there is one.
				break
			}
			writePoolError(w, aerr)
			return
		}
		// Each source resolves the flat model name in its own vocabulary, so
		// the adapter is chosen from the lease rather than assumed.
		u, ok := upstream.Get(lease.Account.Source)
		if !ok {
			lease.Fail(upstream.NewError(upstream.KindUnknown, 0, "upstream not available: "+string(lease.Account.Source)))
			continue
		}
		hint.Attempts = i + 1
		hint.Account = accountLabel(lease.Account)
		// Open the credit window before the attempt so the reading that opens
		// it is taken from the account that is about to serve the turn.
		s.beginSettlement(hint, u, lease.Account)

		if t.Stream {
			emitted, turnID, serr := s.runStream(w, r, t, u, lease.Account)
			if serr == nil {
				lease.Succeed()
				hint.TurnID = turnID
				s.settleCredits(hint, u, lease.Account)
				return
			}
			if !emitted && s.tryRefresh(r.Context(), lease.Account, serr, refreshed) {
				emitted, turnID, serr = s.runStream(w, r, t, u, lease.Account)
				if serr == nil {
					lease.Succeed()
					hint.TurnID = turnID
					s.settleCredits(hint, u, lease.Account)
					return
				}
			}
			ue := lease.Fail(serr)
			logx.Warnf("chat stream attempt %d/%d failed: %v", i+1, attempts, serr)
			if emitted {
				// The stream already concluded in-band; the client saw the error.
				return
			}
			if !ue.Retryable() || i == attempts-1 {
				writeUpstreamError(w, ue)
				return
			}
			tried[lease.Account.Key()] = true
			lastErr = ue
			continue
		}

		resp, cerr := u.Chat(r.Context(), lease.Account, t.Request)
		if cerr == nil {
			lease.Succeed()
			finishNonStream(hint, t, w, resp)
			s.settleCredits(hint, u, lease.Account)
			return
		}
		if s.tryRefresh(r.Context(), lease.Account, cerr, refreshed) {
			if resp, cerr = u.Chat(r.Context(), lease.Account, t.Request); cerr == nil {
				lease.Succeed()
				finishNonStream(hint, t, w, resp)
				s.settleCredits(hint, u, lease.Account)
				return
			}
		}
		ue := lease.Fail(cerr)
		logx.Warnf("chat attempt %d/%d failed: %v", i+1, attempts, cerr)
		if !ue.Retryable() || i == attempts-1 {
			writeUpstreamError(w, ue)
			return
		}
		tried[lease.Account.Key()] = true
		lastErr = ue
	}
	writeUpstreamError(w, lastErr)
}

// finishNonStream records the answer's accounting and renders it.
func finishNonStream(hint *recHint, t *turn, w http.ResponseWriter, resp *upstream.ChatResponse) {
	hint.Usage = &struct{ Prompt, Completion, Total int64 }{
		Prompt:     int64(resp.Usage.PromptTokens),
		Completion: int64(resp.Usage.CompletionTokens),
		Total:      int64(resp.Usage.TotalTokens),
	}
	hint.TurnID = resp.TurnID
	hint.Credits = resp.CreditsConsumed
	hint.CreditsSource = resp.CreditsSource
	t.Result(w, resp)
}

// runStream runs one streaming attempt. It reports whether anything reached the
// client, which decides whether a retry on another account is still possible,
// and the upstream turn id when the stream carried one.
func (s *Server) runStream(w http.ResponseWriter, r *http.Request, t *turn, u upstream.Upstream, acc *account.Account) (bool, string, error) {
	var out responder
	turnID := ""
	started := false

	emit := func(d upstream.StreamDelta) error {
		if turnID == "" && d.TurnID != "" {
			turnID = d.TurnID
		}
		// A settlement-only delta tells us what to bill against and carries
		// nothing the client should see.
		if !renderable(d) {
			return nil
		}
		if !started {
			var err error
			out, err = t.Open(w)
			if err != nil {
				return err
			}
			started = true
		}
		return out.Delta(d)
	}

	if err := u.ChatStream(r.Context(), acc, t.Request, emit); err != nil {
		if !started {
			return false, turnID, err
		}
		out.Fail(err)
		return true, turnID, err
	}
	if !started {
		// The upstream produced nothing renderable but did succeed. Open now so
		// the client still receives a well-formed, empty answer.
		var err error
		out, err = t.Open(w)
		if err != nil {
			return false, turnID, err
		}
	}
	out.End()
	return true, turnID, nil
}

// renderable reports whether a delta carries anything a client should see. A
// delta that only names the turn is bookkeeping, not content.
func renderable(d upstream.StreamDelta) bool {
	return d.Content != "" || d.ReasoningContent != "" || len(d.ToolCalls) > 0 ||
		d.Usage != nil || d.FinishReason != ""
}

// tryRefresh attempts one token refresh when err looks like an auth failure and
// the account has not been refreshed yet during this request. It reports
// whether the caller should retry the same account.
func (s *Server) tryRefresh(ctx context.Context, acc *account.Account, err error, done map[string]bool) bool {
	if s.refresher == nil || !upstream.IsAuth(err) {
		return false
	}
	key := acc.Key()
	if done[key] {
		return false
	}
	done[key] = true
	if rerr := s.refresher.RefreshNow(ctx, acc); rerr != nil {
		logx.Warnf("refresh %s after auth error: %v", key, rerr)
		return false
	}
	logx.Infof("refreshed %s after auth error, retrying", key)
	return true
}
