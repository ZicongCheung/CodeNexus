package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/reqlog"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// billingUpstream is a healthy upstream that also reports a per-turn charge.
// It records the turn ids it was asked to settle so a test can tell whether the
// out-of-band lookup ran at all.
type billingUpstream struct {
	credits float64
	source  string
	err     error
	turnID  string

	// used scripts the account's consumption counter, handed out one reading
	// per call. usedErr makes the counter unreadable.
	used    []float64
	usedErr bool

	mu      sync.Mutex
	turns   []string
	usedIdx int
	reads   int
	// beforeSeen is closed once the first counter reading has been taken, so a
	// turn can be made to wait for the window that opened before it.
	beforeSeen chan struct{}
	armed      bool
}

func (b *billingUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }

func (b *billingUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}

// awaitWindow blocks until the opening counter reading has landed. Without it a
// scripted counter could be consumed out of order by a settlement that raced
// ahead of the window, and the test would be flaky rather than wrong.
func (b *billingUpstream) awaitWindow() {
	b.mu.Lock()
	armed := b.armed
	wait := b.beforeSeen
	b.mu.Unlock()
	if !armed || wait == nil {
		return
	}
	select {
	case <-wait:
	case <-time.After(2 * time.Second):
	}
}

func (b *billingUpstream) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	b.awaitWindow()
	return &upstream.ChatResponse{
		ID: "1", Model: req.Model, Content: "ok", FinishReason: "stop", TurnID: b.turnID,
	}, nil
}

func (b *billingUpstream) ChatStream(_ context.Context, _ *account.Account, _ *upstream.ChatRequest, emit upstream.EmitFunc) error {
	b.awaitWindow()
	// Announce the turn id before any content, the way the Trae adapter does.
	if err := emit(upstream.StreamDelta{TurnID: b.turnID}); err != nil {
		return err
	}
	if err := emit(upstream.StreamDelta{Content: "ok"}); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{FinishReason: "stop"})
}

func (b *billingUpstream) Refresh(context.Context, *account.Account) error { return nil }

func (b *billingUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (b *billingUpstream) TurnCredits(_ context.Context, _ *account.Account, turnID string) (float64, string, error) {
	b.mu.Lock()
	b.turns = append(b.turns, turnID)
	b.mu.Unlock()
	if b.err != nil {
		return 0, "", b.err
	}
	return b.credits, b.source, nil
}

// UsedCredits pops the next scripted reading of the account's consumption
// counter. With nothing scripted it reports the counter as unreadable, so a
// test that does not script one stays on the per-turn path.
func (b *billingUpstream) UsedCredits(_ context.Context, _ *account.Account) (float64, bool) {
	b.mu.Lock()
	b.reads++
	first := b.reads == 1
	if b.usedErr || b.usedIdx >= len(b.used) {
		wait := b.beforeSeen
		armed := b.armed
		b.mu.Unlock()
		if first && armed && wait != nil {
			close(wait)
		}
		return 0, false
	}
	value := b.used[b.usedIdx]
	b.usedIdx++
	wait := b.beforeSeen
	armed := b.armed
	b.mu.Unlock()
	if first && armed && wait != nil {
		close(wait)
	}
	return value, true
}

// scriptCounter arms a counter sequence and returns a channel that the fake's
// own turn will wait on, making the opening reading the first one taken.
func (b *billingUpstream) scriptCounter(values ...float64) {
	b.used = values
	b.beforeSeen = make(chan struct{})
	b.armed = true
}

func (b *billingUpstream) counterReads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

func (b *billingUpstream) settled() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.turns...)
}

// billingServer installs a recorder and the billing upstream, so a settlement
// can be observed end to end.
func billingServer(t *testing.T, u upstream.Upstream, rec *reqlog.Recorder) *Server {
	t.Helper()
	upstream.Register(u)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })

	p := pool.New(pool.Options{StickyEnabled: false})
	p.Replace([]*account.Account{{Source: account.SourceWorkBuddy, UID: "one", Enabled: true}})
	cfg := config.Default()
	cfg.APIKey = ""
	return NewServer(&cfg, p, nil, WithRecorder(rec))
}

// waitForCharge polls until the record for id carries a settlement decision.
func waitForCharge(t *testing.T, rec *reqlog.Recorder, id string) reqlog.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range rec.Snapshot().Recent {
			if e.RequestID == id && e.CreditsSource != "" {
				return e
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the record for %s never settled: %+v", id, rec.Snapshot().Recent)
	return reqlog.Event{}
}

// ── tier one: a charge the response already reported ─────────────────────────

func TestRecordAppliesATierOneCharge(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))

	h := s.record("chat", func(w http.ResponseWriter, r *http.Request) {
		hint := hintFrom(r)
		hint.Model = "m"
		hint.Credits = 2.5
		hint.CreditsSource = creditsSourceUpstream
		w.WriteHeader(http.StatusOK)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	recent := rec.Snapshot().Recent
	if len(recent) != 1 {
		t.Fatalf("recent = %d, want 1", len(recent))
	}
	if recent[0].CreditsConsumed != 2.5 || recent[0].CreditsSource != creditsSourceUpstream {
		t.Fatalf("tier-one charge was not recorded: %+v", recent[0])
	}
}

func TestRecordInfersTheUpstreamSource(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))

	h := s.record("chat", func(w http.ResponseWriter, r *http.Request) {
		hintFrom(r).Credits = 1
		w.WriteHeader(http.StatusOK)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	got := rec.Snapshot().Recent[0]
	if got.CreditsSource != creditsSourceUpstream || got.CreditsConsumed != 1 {
		t.Fatalf("charge = %+v", got)
	}
}

// ── settlement ───────────────────────────────────────────────────────────────

func TestSettleCreditsFillsTheRecord(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	u := &billingUpstream{credits: 1.5, source: "session_usage"}

	// The middleware has already written the record; the settlement lands on it.
	rec.Record(reqlog.Event{RequestID: "req-a", Path: "/v1/chat/completions", OK: true})
	s.settleCredits(&recHint{RequestID: "req-a", TurnID: "turn-1"}, u, &account.Account{UID: "one"})

	got := waitForCharge(t, rec, "req-a")
	if got.CreditsConsumed != 1.5 || got.CreditsSource != "session_usage" {
		t.Fatalf("settled charge = %+v", got)
	}
	if turns := u.settled(); len(turns) != 1 || turns[0] != "turn-1" {
		t.Fatalf("settled turns = %v, want [turn-1]", turns)
	}
}

func TestSettleCreditsMarksAnUnsettledTurnPending(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	u := &billingUpstream{err: context.DeadlineExceeded}

	rec.Record(reqlog.Event{RequestID: "req-b", OK: true})
	s.settleCredits(&recHint{RequestID: "req-b", TurnID: "turn-2"}, u, &account.Account{UID: "one"})

	got := waitForCharge(t, rec, "req-b")
	if got.CreditsSource != creditsSourcePending {
		t.Fatalf("source = %q, want %q", got.CreditsSource, creditsSourcePending)
	}
	// A pending turn is not a free one: it must not read as a settled zero.
	if got.CreditsConsumed != 0 {
		t.Fatalf("credits = %v, want 0", got.CreditsConsumed)
	}
}

func TestSettleCreditsSkipsWhenTheResponseAlreadyCharged(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	u := &billingUpstream{credits: 9, source: "session_usage"}

	s.settleCredits(&recHint{
		RequestID: "req-c", TurnID: "turn-3",
		Credits: 2, CreditsSource: creditsSourceUpstream,
	}, u, &account.Account{UID: "one"})

	if turns := u.settled(); len(turns) != 0 {
		t.Fatalf("a known charge must not be looked up again: %v", turns)
	}
}

func TestSettleCreditsSkipsWhatItCannotSettle(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	u := &billingUpstream{credits: 1}

	cases := []struct {
		name  string
		hint  *recHint
		serve *Server
	}{
		{"no turn id", &recHint{RequestID: "req-d"}, s},
		{"no request id", &recHint{TurnID: "turn-4"}, s},
		{"nil hint", nil, s},
		{"no recorder", &recHint{RequestID: "req-e", TurnID: "turn-5"}, NewServer(&cfg, pool.New(pool.Options{}), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.serve.settleCredits(tc.hint, u, &account.Account{UID: "one"})
		})
	}
	if turns := u.settled(); len(turns) != 0 {
		t.Fatalf("nothing should have been looked up: %v", turns)
	}
}

// An upstream without a per-turn billing surface must be left alone rather than
// having a settlement invented for it.
func TestSettleCreditsSkipsANonBillingUpstream(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))

	rec.Record(reqlog.Event{RequestID: "req-f", OK: true})
	s.settleCredits(&recHint{RequestID: "req-f", TurnID: "turn-6"}, &flakyUpstream{}, &account.Account{UID: "one"})

	got := rec.Snapshot().Recent[0]
	if got.CreditsSource != "" || got.CreditsConsumed != 0 {
		t.Fatalf("a non-billing upstream left a charge: %+v", got)
	}
}

// ── end to end ───────────────────────────────────────────────────────────────

func TestChatSettlesTheTurnNonStreaming(t *testing.T) {
	rec := reqlog.New(8, false)
	u := &billingUpstream{credits: 3.25, source: "session_usage", turnID: "turn-ns"}
	s := billingServer(t, u, rec)

	httpRec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", httpRec.Code, httpRec.Body.String())
	}
	// The client must not have waited for the billing lookup: the response is
	// already complete when the record is still uncharged.
	events := rec.Snapshot().Recent
	if len(events) != 1 {
		t.Fatalf("recent = %d, want 1", len(events))
	}
	got := waitForCharge(t, rec, events[0].RequestID)
	if got.CreditsConsumed != 3.25 || got.CreditsSource != "session_usage" {
		t.Fatalf("settled charge = %+v", got)
	}
	if turns := u.settled(); len(turns) != 1 || turns[0] != "turn-ns" {
		t.Fatalf("settled turns = %v", turns)
	}
}

func TestChatStreamSettlesTheTurnAndHidesTheIdDelta(t *testing.T) {
	rec := reqlog.New(8, false)
	u := &billingUpstream{credits: 0.5, source: "session_usage", turnID: "turn-stream"}
	s := billingServer(t, u, rec)

	httpRec := postChat(s, `{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", httpRec.Code, httpRec.Body.String())
	}
	body := httpRec.Body.String()
	if !strings.Contains(body, `"content":"ok"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body = %q", body)
	}
	// The id-only delta is the gateway's business, not the client's: it must
	// not add a chunk the model never produced. Two chunks are expected, the
	// content and the finish; the third data line is [DONE].
	chunks := strings.Count(body, `"object":"chat.completion.chunk"`)
	if chunks != 2 {
		t.Fatalf("chunks = %d, want 2 (content, finish); body = %q", chunks, body)
	}

	events := rec.Snapshot().Recent
	if len(events) != 1 {
		t.Fatalf("recent = %d, want 1", len(events))
	}
	if !events[0].Stream {
		t.Fatalf("the event must be marked as a stream: %+v", events[0])
	}
	got := waitForCharge(t, rec, events[0].RequestID)
	if got.CreditsConsumed != 0.5 || got.CreditsSource != "session_usage" {
		t.Fatalf("settled charge = %+v", got)
	}
}

// A settlement that finishes before the middleware records the request must
// still land, which is what makes it safe to run it out of band.
func TestSettlementCanArriveBeforeTheRecord(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	u := &billingUpstream{credits: 7, source: "session_usage"}

	hint := &recHint{RequestID: "req-early", TurnID: "turn-7"}
	s.settleCredits(hint, u, &account.Account{UID: "one"})
	// Give the goroutine a moment to reach the recorder first.
	time.Sleep(20 * time.Millisecond)
	rec.Record(reqlog.Event{RequestID: "req-early", OK: true, Path: "/v1/chat/completions"})

	got := waitForCharge(t, rec, "req-early")
	if got.CreditsConsumed != 7 || got.CreditsSource != "session_usage" {
		t.Fatalf("settled charge = %+v", got)
	}
}

// ── tier three: a charge inferred from the account's own counter ─────────────

// seedWindow opens a credit window and pre-fills its opening reading, so the
// fallback can be exercised without racing a background read.
func seedWindow(s *Server, acc *account.Account, before float64, readable bool) *creditSnapshot {
	snap := &creditSnapshot{
		acc:    acc,
		gate:   s.gate,
		ctx:    context.Background(),
		cancel: func() {},
		before: make(chan usedReading, 1),
	}
	snap.usable = s.gate.begin(acc)
	snap.before <- usedReading{used: before, ok: readable}
	return snap
}

// settleWithWindow runs one settlement whose per-turn bill is unavailable and
// whose window opened at before.
func settleWithWindow(t *testing.T, before, after float64, readable bool) reqlog.Event {
	t.Helper()
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "one"}
	u := &billingUpstream{err: context.DeadlineExceeded, used: []float64{after}, usedErr: !readable}

	rec.Record(reqlog.Event{RequestID: "req-t3", OK: true})
	s.settleCredits(&recHint{
		RequestID: "req-t3",
		TurnID:    "turn-t3",
		Snapshot:  seedWindow(s, acc, before, readable),
	}, u, acc)
	return waitForCharge(t, rec, "req-t3")
}

func TestSettleCreditsInfersTheChargeFromTheCounter(t *testing.T) {
	got := settleWithWindow(t, 10, 13, true)
	if got.CreditsSource != creditsSourceSnapshotDelta || got.CreditsConsumed != 3 {
		t.Fatalf("inferred charge = %+v, want 3 from snapshot_delta", got)
	}
}

// A counter that did not move is a real answer: the turn was free, which is not
// the same as a turn nobody could bill.
func TestSettleCreditsAcceptsAnUnchangedCounter(t *testing.T) {
	got := settleWithWindow(t, 10, 10, true)
	if got.CreditsSource != creditsSourceSnapshotDelta || got.CreditsConsumed != 0 {
		t.Fatalf("charge = %+v, want a settled zero from snapshot_delta", got)
	}
}

// A counter that went backwards cannot describe this turn, so it is discarded
// rather than reported as a negative charge.
func TestSettleCreditsRejectsAShrinkingCounter(t *testing.T) {
	got := settleWithWindow(t, 13, 10, true)
	if got.CreditsSource != creditsSourcePending || got.CreditsConsumed != 0 {
		t.Fatalf("charge = %+v, want pending", got)
	}
}

func TestSettleCreditsReportsPendingWhenTheCounterIsUnreadable(t *testing.T) {
	got := settleWithWindow(t, 10, 13, false)
	if got.CreditsSource != creditsSourcePending || got.CreditsConsumed != 0 {
		t.Fatalf("charge = %+v, want pending", got)
	}
}

// A window that began while the account already had a turn in flight describes
// a mixed counter, so it must not be subtracted from.
func TestSettleCreditsRejectsAWindowThatBeganWithCompany(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "one"}
	u := &billingUpstream{err: context.DeadlineExceeded, used: []float64{13}}

	// Another turn is already running on this account.
	if !s.gate.begin(acc) {
		t.Fatal("the first turn must be attributable")
	}
	snap := seedWindow(s, acc, 10, true)
	if snap.usable {
		t.Fatal("a window opened beside another turn must not be attributable")
	}
	defer s.gate.end(acc)

	rec.Record(reqlog.Event{RequestID: "req-mixed", OK: true})
	s.settleCredits(&recHint{RequestID: "req-mixed", TurnID: "turn-mixed", Snapshot: snap}, u, acc)

	got := waitForCharge(t, rec, "req-mixed")
	if got.CreditsSource != creditsSourcePending || got.CreditsConsumed != 0 {
		t.Fatalf("charge = %+v, want pending", got)
	}
}

// A window that was alone when it opened is no longer usable if a second turn
// started meanwhile, because the closing reading is shared by then.
func TestSettleCreditsRejectsAWindowThatLostItsAloneStatus(t *testing.T) {
	rec := reqlog.New(8, false)
	cfg := config.Default()
	s := NewServer(&cfg, pool.New(pool.Options{}), nil, WithRecorder(rec))
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "one"}
	u := &billingUpstream{err: context.DeadlineExceeded, used: []float64{13}}

	snap := seedWindow(s, acc, 10, true)
	// A second turn lands while the first is still settling.
	s.gate.begin(acc)
	defer s.gate.end(acc)

	rec.Record(reqlog.Event{RequestID: "req-late", OK: true})
	s.settleCredits(&recHint{RequestID: "req-late", TurnID: "turn-late", Snapshot: snap}, u, acc)

	got := waitForCharge(t, rec, "req-late")
	if got.CreditsSource != creditsSourcePending {
		t.Fatalf("charge = %+v, want pending once the account stopped being sole", got)
	}
}

func TestSnapshotGateOnlyTrustsASoleTurn(t *testing.T) {
	g := newSnapshotGate()
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "one"}

	if !g.begin(acc) {
		t.Fatal("the first turn on an idle account must be attributable")
	}
	if g.begin(acc) {
		t.Fatal("a second concurrent turn must not be attributable")
	}
	if g.safe(acc) {
		t.Fatal("an account with two turns in flight is not attributable")
	}
	g.end(acc)
	if g.safe(acc) {
		t.Fatal("the account still has a turn in flight")
	}
	g.end(acc)
	if !g.safe(acc) {
		t.Fatal("an idle account must be attributable again")
	}
	if !g.begin(acc) {
		t.Fatal("a fresh sole turn must be attributable")
	}
	g.end(acc)
	if len(g.active) != 0 || len(g.unsafe) != 0 {
		t.Fatalf("the gate leaked state: active=%v unsafe=%v", g.active, g.unsafe)
	}
}

// The window is opened for the account that serves the turn and released once
// settlement is done, so a later turn on the same account is attributable again.
func TestChatInfersTheChargeFromTheCounterEndToEnd(t *testing.T) {
	rec := reqlog.New(8, false)
	u := &billingUpstream{err: context.DeadlineExceeded, turnID: "turn-t3"}
	u.scriptCounter(10, 13)
	s := billingServer(t, u, rec)

	httpRec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", httpRec.Code, httpRec.Body.String())
	}
	events := rec.Snapshot().Recent
	if len(events) != 1 {
		t.Fatalf("recent = %d, want 1", len(events))
	}
	got := waitForCharge(t, rec, events[0].RequestID)
	if got.CreditsSource != creditsSourceSnapshotDelta || got.CreditsConsumed != 3 {
		t.Fatalf("charge = %+v, want 3 from snapshot_delta", got)
	}
	if reads := u.counterReads(); reads < 2 {
		t.Fatalf("counter reads = %d, want the window plus the closing reading", reads)
	}
	// The window must not outlive the request.
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	if len(s.gate.active) != 0 || len(s.gate.unsafe) != 0 {
		t.Fatalf("the window outlived the request: active=%v unsafe=%v", s.gate.active, s.gate.unsafe)
	}
}
