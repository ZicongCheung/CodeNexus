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
	"sync"
	"time"

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
	// IdleAfter, when positive, bounds how long the upstream may stay silent
	// before the answer is opened anyway. A protocol whose clients abort on a
	// missing opening (Anthropic's message_start) sets it; a protocol that can
	// simply wait leaves it zero and keeps failover open for the whole wait.
	IdleAfter time.Duration
	// Fail renders a failure in the protocol's own error shape. client
	// distinguishes a request the gateway rejected from one the pool or an
	// upstream failed; nil means the OpenAI shape.
	Fail func(w http.ResponseWriter, err error, client bool)
}

// fail renders a failure through the turn's own error shape.
func (t *turn) fail(w http.ResponseWriter, err error, client bool) {
	if t.Fail != nil {
		t.Fail(w, err, client)
		return
	}
	defaultTurnFail(w, err, client)
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
		t.fail(w, err, true)
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
			t.fail(w, aerr, false)
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
				t.fail(w, ue, false)
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
			t.fail(w, ue, false)
			return
		}
		tried[lease.Account.Key()] = true
		lastErr = ue
	}
	t.fail(w, lastErr, false)
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
	op := &streamOpener{open: func() (responder, error) { return t.Open(w) }}
	turnID := ""

	// The keepalive opens the answer if the upstream stays silent past the
	// window. It races the deltas deliberately: whichever gets there first
	// opens, and the opener makes sure only one of them does.
	var stop chan struct{}
	if t.IdleAfter > 0 {
		stop = make(chan struct{})
		go func() {
			timer := time.NewTimer(t.IdleAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				op.Keepalive()
			case <-stop:
			}
		}()
	}

	emit := func(d upstream.StreamDelta) error {
		if turnID == "" && d.TurnID != "" {
			turnID = d.TurnID
		}
		// A settlement-only delta tells us what to bill against and carries
		// nothing the client should see.
		if !renderable(d) {
			return nil
		}
		return op.Emit(d)
	}

	err := u.ChatStream(r.Context(), acc, t.Request, emit)
	if stop != nil {
		close(stop)
	}

	if err != nil {
		out, started := op.Abort()
		if !started {
			return false, turnID, err
		}
		out.Fail(err)
		return true, turnID, err
	}
	out, started := op.Finish()
	if !started {
		// Nothing was written and the answer could not be opened.
		return false, turnID, op.err()
	}
	out.End()
	return true, turnID, nil
}

// streamOpener serializes the first write of a streaming answer between the
// upstream's deltas and the optional keepalive timer, so exactly one of them
// opens the response. After Finish or Abort the attempt is sealed and a late
// keepalive can no longer write into a response the caller means to retry.
type streamOpener struct {
	open func() (responder, error)

	mu      sync.Mutex
	out     responder
	started bool
	sealed  bool
	openErr error
}

// Emit forwards a renderable delta, opening the answer first when needed.
func (o *streamOpener) Emit(d upstream.StreamDelta) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed {
		return nil
	}
	if err := o.openLocked(); err != nil {
		return err
	}
	return o.out.Delta(d)
}

// Keepalive opens the answer on the idle path. It is a no-op once a delta has
// opened it or the attempt has been sealed.
func (o *streamOpener) Keepalive() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed {
		return
	}
	if err := o.openLocked(); err != nil {
		logx.Warnf("keepalive could not open the stream: %v", err)
	}
}

// Finish concludes a successful attempt, opening an empty answer when the
// upstream produced nothing renderable. It reports whether the answer is open.
func (o *streamOpener) Finish() (responder, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.sealed && !o.started {
		_ = o.openLocked()
	}
	o.sealed = true
	return o.out, o.started
}

// Abort ends a failed attempt without opening anything, and reports whether a
// write already happened so the caller knows the failure is committed.
func (o *streamOpener) Abort() (responder, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sealed = true
	return o.out, o.started
}

// err returns the error from the last failed open, if any.
func (o *streamOpener) err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.openErr
}

// openLocked opens the answer once. The caller must hold mu.
func (o *streamOpener) openLocked() error {
	if o.started {
		return nil
	}
	out, err := o.open()
	if err != nil {
		o.openErr = err
		return err
	}
	o.out, o.started = out, true
	return nil
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
