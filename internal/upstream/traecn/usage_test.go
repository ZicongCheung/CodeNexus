package traecn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// usageStub answers the per-turn commercial usage endpoint. Responses are
// queued so a test can model "not settled yet, then settled"; the last one is
// repeated so a single-entry queue also describes the steady state.
type usageStub struct {
	*httptest.Server

	mu        sync.Mutex
	responses []map[string]any
	requests  []captured
	status    int
}

func newUsageStub(t *testing.T) *usageStub {
	t.Helper()
	s := &usageStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		s.mu.Lock()
		out := map[string]any{"code": 0}
		if len(s.responses) > 0 {
			out = s.responses[0]
			if len(s.responses) > 1 {
				s.responses = s.responses[1:]
			}
		}
		status := s.status
		s.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *usageStub) record(r *http.Request) {
	var body map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, captured{
		Path:    r.URL.Path,
		Query:   r.URL.Query(),
		Method:  r.Method,
		Headers: r.Header.Clone(),
		Body:    body,
	})
	s.mu.Unlock()
}

func (s *usageStub) queue(docs ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, docs...)
}

func (s *usageStub) seen() []captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]captured(nil), s.requests...)
}

func (s *usageStub) client() *Client {
	return New(Config{UsageBaseURL: s.URL})
}

// fastDelays shrinks the polling schedule so the "not settled" path does not
// cost the schedule's real seconds. Both knobs are package state, so they are
// restored on cleanup.
func fastDelays(t *testing.T, delays ...time.Duration) {
	t.Helper()
	previousDelays := sessionUsageDelays
	previousSettle := sessionUsageSettle
	sessionUsageDelays = delays
	sessionUsageSettle = 0
	t.Cleanup(func() {
		sessionUsageDelays = previousDelays
		sessionUsageSettle = previousSettle
	})
}

// TestMain removes the settle window for the whole package: it is wall-clock
// pacing, and no test should pay it unless it is testing it.
func TestMain(m *testing.M) {
	sessionUsageSettle = 0
	os.Exit(m.Run())
}

// ── TurnCredits ──────────────────────────────────────────────────────────────

func TestTurnCreditsReadsSettledCharge(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"credits_float": 1.5}})

	credits, source, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if credits != 1.5 {
		t.Fatalf("credits = %v, want 1.5", credits)
	}
	if source != CreditsSourceSessionUsage {
		t.Fatalf("source = %q, want %q", source, CreditsSourceSessionUsage)
	}

	seen := s.seen()
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	if seen[0].Path != sessionUsagePath {
		t.Fatalf("path = %q, want %q", seen[0].Path, sessionUsagePath)
	}
	if got := seen[0].Headers.Get("Authorization"); got != TokenScheme+" "+fakeToken("user-1") {
		t.Fatalf("Authorization = %q", got)
	}
	if got, _ := seen[0].Body["session_id"].(string); got != "message-1" {
		t.Fatalf("session_id = %q, want message-1", got)
	}
}

// A settled zero is a value, not an absence: the upstream charged nothing for
// the turn, and reporting it as pending would make the caller retry forever.
func TestTurnCreditsTreatsZeroAsSettled(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"credits_consumed": 0}})

	credits, source, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("a settled zero must not be an error: %v", err)
	}
	if credits != 0 || source != CreditsSourceSessionUsage {
		t.Fatalf("credits = %v source = %q", credits, source)
	}
}

func TestTurnCreditsPollsUntilSettled(t *testing.T) {
	fastDelays(t, 0, 0, 0)
	s := newUsageStub(t)
	s.queue(
		map[string]any{"code": 0, "data": map[string]any{"session_id": "message-1"}},
		map[string]any{"code": 0, "data": map[string]any{"credits_float": 2.75}},
	)

	credits, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if credits != 2.75 {
		t.Fatalf("credits = %v, want 2.75", credits)
	}
	if n := len(s.seen()); n != 2 {
		t.Fatalf("requests = %d, want 2 (the unsetttled probe, then the settled read)", n)
	}
}

// Only "no field at all" is pending. Every attempt is made before giving up,
// and the failure is reported rather than read as a free turn.
func TestTurnCreditsReportsPendingWhenNeverSettled(t *testing.T) {
	fastDelays(t, 0, 0, 0)
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"session_id": "message-1"}})

	_, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err == nil {
		t.Fatal("an unsettled turn must be reported")
	}
	if n := len(s.seen()); n != 3 {
		t.Fatalf("requests = %d, want 3 (every attempt)", n)
	}
}

func TestTurnCreditsDiscardsNegativeCharge(t *testing.T) {
	fastDelays(t, 0)
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"credits_float": -5}})

	if _, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1"); err == nil {
		t.Fatal("a negative charge must not settle the turn")
	}
}

func TestTurnCreditsSurfacesBusinessError(t *testing.T) {
	fastDelays(t, 0)
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 4008, "message": "quota exceeded"})

	_, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err == nil {
		t.Fatal("a business error must fail the lookup")
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindServer {
		t.Fatalf("kind = %q, want %q", kind, upstream.KindServer)
	}
	if !strings.Contains(err.Error(), "4008") {
		t.Fatalf("the error must name the code: %v", err)
	}
	// A business error is terminal: polling again cannot change the answer.
	if n := len(s.seen()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

func TestTurnCreditsRejectsEmptyTurnID(t *testing.T) {
	s := newUsageStub(t)
	if _, _, err := s.client().TurnCredits(context.Background(), testAccount(), "  "); err == nil {
		t.Fatal("an empty turn id must be rejected")
	}
	if n := len(s.seen()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestTurnCreditsRejectsMissingCredential(t *testing.T) {
	s := newUsageStub(t)
	acc := &account.Account{Source: account.SourceTraeCN, UID: "user-1", Enabled: true}
	if _, _, err := s.client().TurnCredits(context.Background(), acc, "message-1"); err == nil {
		t.Fatal("a charge cannot be read without a credential")
	}
	if n := len(s.seen()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestTurnCreditsRejectsCLIMode(t *testing.T) {
	s := newUsageStub(t)
	c := New(Config{UsageBaseURL: s.URL, UpstreamMode: CLIMode})
	if _, _, err := c.TurnCredits(context.Background(), testAccount(), "message-1"); err == nil {
		t.Fatal("the local CLI has no per-turn charge to settle")
	}
	if n := len(s.seen()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestTurnCreditsHonoursContextCancellation(t *testing.T) {
	// A long delay models the slow path; the context must win.
	fastDelays(t, time.Hour)
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"session_id": "message-1"}})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := s.client().TurnCredits(ctx, testAccount(), "message-1")
	if err == nil {
		t.Fatal("a cancelled context must stop the poll")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the poll waited %s instead of honouring the context", elapsed)
	}
	if n := len(s.seen()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestTurnCreditsSurfacesHTTPFailure(t *testing.T) {
	fastDelays(t, 0)
	s := newUsageStub(t)
	s.status = http.StatusInternalServerError

	if _, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1"); err == nil {
		t.Fatal("an HTTP failure must be reported")
	}
}

// ── container / key priority ─────────────────────────────────────────────────

func TestTurnCreditsPrefersTheBillingContainer(t *testing.T) {
	// The preferred container must win over an unrelated number that a generic
	// walk would reach later.
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{
		"user_usage_group_by_session": map[string]any{"credits_float": 1.5},
		"some_other_metadata":         map[string]any{"credits_float": 999},
	}})

	credits, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if credits != 1.5 {
		t.Fatalf("credits = %v, want the billing container's 1.5", credits)
	}
}

func TestTurnCreditsFallsBackToAGenericWalk(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{
		"unexpected_wrapper": map[string]any{"credits_consumed": 2.25},
	}})

	credits, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if credits != 2.25 {
		t.Fatalf("credits = %v, want 2.25", credits)
	}
}

func TestTurnCreditsReadsATopLevelCharge(t *testing.T) {
	// Some deployments answer without the data envelope.
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "credits_float": 4})

	credits, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1")
	if err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if credits != 4 {
		t.Fatalf("credits = %v, want 4", credits)
	}
}

// ── field helpers ────────────────────────────────────────────────────────────

// The fixtures use the numeric shapes a JSON decode can produce: the endpoint
// is always read through encoding/json, so an untyped int literal would only
// describe a caller that does not exist.
func TestFindSessionCreditKeyPriority(t *testing.T) {
	cases := []struct {
		name string
		doc  any
		want float64
		ok   bool
	}{
		{"credits_float wins", map[string]any{"credits_float": 1.0, "amountFloat": 9.0}, 1, true},
		{"creditsFloat before snake siblings", map[string]any{"creditsFloat": 2.0, "credits_consumed": 3.0}, 2, true},
		{"credits_consumed", map[string]any{"credits_consumed": 3.0}, 3, true},
		{"consumed_credits", map[string]any{"consumed_credits": 4.0}, 4, true},
		{"credit_cost", map[string]any{"credit_cost": 5.0}, 5, true},
		{"credits_cost", map[string]any{"credits_cost": 6.0}, 6, true},
		{"amount_float", map[string]any{"amount_float": 7.0}, 7, true},
		{"amountFloat", map[string]any{"amountFloat": 8.0}, 8, true},
		{"an integer shape", map[string]any{"credits_float": json.Number("9")}, 9, true},
		{"an int64 shape", map[string]any{"credits_float": int64(10)}, 10, true},
		{"a boolean is not a charge", map[string]any{"credits_float": true, "amount_float": 8.0}, 8, true},
		{"a boolean alone is not a charge", map[string]any{"credits_float": true}, 0, false},
		{"a negative is discarded", map[string]any{"credits_float": -1.0}, 0, false},
		{"an unrelated number is not a charge", map[string]any{"total_tokens": 12.0}, 0, false},
		{"nested container", map[string]any{"usage": map[string]any{"credits_float": 1.5}}, 1.5, true},
		{"array wrapper", map[string]any{"cost": []any{map[string]any{"credits_float": 2.5}}}, 2.5, true},
		{"string amount", map[string]any{"credits_consumed": "3.5"}, 3.5, true},
		{"unparseable string", map[string]any{"credits_consumed": "many"}, 0, false},
		{"a settled zero", map[string]any{"credits_float": 0.0}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := findSessionCredit(tc.doc, 0)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (value %v)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("value = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindSessionCreditBoundsDepth(t *testing.T) {
	// Seven wrappers put the charge past the walk limit; the walk must stop
	// rather than chase an arbitrarily deep document.
	var deep any = map[string]any{"credits_float": 1.0}
	for i := 0; i < 8; i++ {
		deep = map[string]any{"wrapper": deep}
	}
	if _, ok := findSessionCredit(deep, 0); ok {
		t.Fatal("the walk must be depth bounded")
	}
}

func TestCreditFloatShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
		ok   bool
	}{
		{"float64", float64(1.5), 1.5, true},
		{"int64", int64(2), 2, true},
		{"json number", json.Number("3.25"), 3.25, true},
		{"trimmed string", " 4.5 ", 4.5, true},
		{"empty string", "", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
		{"list", []any{1}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := creditFloat(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("value = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWaitCtxImmediateAndCancelled(t *testing.T) {
	if !waitCtx(context.Background(), 0) {
		t.Fatal("a zero delay must return at once")
	}
	if !waitCtx(context.Background(), -time.Second) {
		t.Fatal("a negative delay must return at once")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitCtx(ctx, 0) {
		t.Fatal("a finished context must report false")
	}
	if waitCtx(ctx, time.Hour) {
		t.Fatal("a finished context must report false even with a delay")
	}
}

// The poll opens after a short settle window: the upstream bills a moment after
// the answer ends, so an immediate first attempt is nearly always too early.
func TestTurnCreditsWaitsOutTheSettleWindow(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 0, "data": map[string]any{"credits_float": 2}})
	previous := sessionUsageSettle
	sessionUsageSettle = 60 * time.Millisecond
	t.Cleanup(func() { sessionUsageSettle = previous })

	start := time.Now()
	if _, _, err := s.client().TurnCredits(context.Background(), testAccount(), "message-1"); err != nil {
		t.Fatalf("TurnCredits: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("the first poll opened after %s, want it to respect the settle window", elapsed)
	}
	if seen := s.seen(); len(seen) != 1 {
		t.Fatalf("requests = %d, want exactly one poll", len(seen))
	}
}

// ── UsedCredits ──────────────────────────────────────────────────────────────

// entitlementClient points the pay and check-in hosts at one stub, which is
// what a single-recorded-fixture test needs.
func entitlementClient(s *usageStub) *Client {
	return New(Config{PayBaseURL: s.URL, UGBaseURL: s.URL})
}

// entitlementBody builds one pack with a grant and a consumed amount.
func entitlementBody(limit, amount int) map[string]any {
	return map[string]any{
		"code": 0,
		"user_entitlement_pack_list": []any{
			map[string]any{
				"entitlement_base_info": map[string]any{
					"quota": map[string]any{"credits_limit": limit},
				},
				"usage": map[string]any{"credits_amount": amount},
			},
		},
	}
}

func TestUsedCreditsReadsTheConsumptionCounter(t *testing.T) {
	s := newUsageStub(t)
	s.queue(entitlementBody(1000, 350))

	used, ok := entitlementClient(s).UsedCredits(context.Background(), testAccount())
	if !ok || used != 350 {
		t.Fatalf("used = %v ok = %v, want 350", used, ok)
	}
	seen := s.seen()
	if len(seen) != 1 || seen[0].Path != entitlementPath {
		t.Fatalf("requests = %+v", seen)
	}
	// All packs, the same scope the balance refresh reads.
	if got, _ := seen[0].Body["req_source"].(float64); got != 0 {
		t.Fatalf("req_source = %v, want 0", got)
	}
}

// Consumption is summed across packs, so a pack the account has not touched
// still contributes its grant without hiding what the others used.
func TestUsedCreditsSumsAcrossPacks(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{
		"code": 0,
		"user_entitlement_pack_list": []any{
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 1000}},
				"usage":                 map[string]any{"credits_amount": 200},
			},
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 500}},
				"usage":                 map[string]any{"credits_amount": 150},
			},
		},
	})

	used, ok := entitlementClient(s).UsedCredits(context.Background(), testAccount())
	if !ok || used != 350 {
		t.Fatalf("used = %v ok = %v, want 350", used, ok)
	}
}

// An unmetered grant has no meaningful counter, so it must not look like a
// readable zero.
func TestUsedCreditsReportsUnlimitedAsUnreadable(t *testing.T) {
	s := newUsageStub(t)
	s.queue(entitlementBody(-1, 350))

	if _, ok := entitlementClient(s).UsedCredits(context.Background(), testAccount()); ok {
		t.Fatal("an unmetered account has no consumable counter to read")
	}
}

// A counter that could not be read is not a counter of zero: reporting zero
// would let the caller settle a turn as free.
func TestUsedCreditsReportsUnreadableRatherThanZero(t *testing.T) {
	s := newUsageStub(t)
	s.queue(map[string]any{"code": 4003, "message": "nope"})

	if _, ok := entitlementClient(s).UsedCredits(context.Background(), testAccount()); ok {
		t.Fatal("a failed lookup must not read as a settled zero")
	}
}

func TestUsedCreditsRejectsAnAccountWithoutACredential(t *testing.T) {
	s := newUsageStub(t)
	s.queue(entitlementBody(1000, 350))

	if _, ok := entitlementClient(s).UsedCredits(context.Background(), &account.Account{}); ok {
		t.Fatal("an account with no credential has no counter to read")
	}
	if seen := s.seen(); len(seen) != 0 {
		t.Fatalf("nothing should have been sent: %+v", seen)
	}
}
