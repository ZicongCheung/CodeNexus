package traecn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// checkinStub answers the check-in and entitlement APIs. It records the device
// id of every request so rotation can be observed.
type checkinStub struct {
	*httptest.Server

	mu       sync.Mutex
	statuses []map[string]any
	claims   []map[string]any
	entitle  map[string]any
	deviceID []string
	paths    []string
}

func newCheckinStub(t *testing.T) *checkinStub {
	t.Helper()
	s := &checkinStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)

		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.deviceID = append(s.deviceID, r.Header.Get("x-device-id"))
		var out map[string]any
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			out = s.pop(&s.statuses)
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			out = s.pop(&s.claims)
		case strings.HasSuffix(r.URL.Path, "/pay/ide_user_ent_usage"):
			out = s.entitle
			if out == nil {
				out = map[string]any{"code": 0}
			}
		default:
			out = map[string]any{"code": 404}
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

// pop returns the next queued response, falling back to the last one so a test
// only has to describe the interesting cases.
func (s *checkinStub) pop(queue *[]map[string]any) map[string]any {
	if len(*queue) == 0 {
		return map[string]any{"code": 0}
	}
	out := (*queue)[0]
	*queue = (*queue)[1:]
	return out
}

func (s *checkinStub) push(queue *[]map[string]any, docs ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*queue = append(*queue, docs...)
}

func (s *checkinStub) setEntitlements(doc map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entitle = doc
}

func (s *checkinStub) devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deviceID...)
}

func (s *checkinStub) sawPath(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if p == path {
			return true
		}
	}
	return false
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// newCheckinClient points the adapter at the stub with the given check-in
// options.
func newCheckinClient(t *testing.T, stub *checkinStub, mutate func(*CheckinConfig)) *Client {
	t.Helper()
	cfg := CheckinConfig{}.normalize()
	// The spacing gate would slow the tests down; each one that needs it sets
	// its own value.
	cfg.IntervalSeconds = 0
	if mutate != nil {
		mutate(&cfg)
	}
	return New(Config{
		UGBaseURL:  stub.URL,
		PayBaseURL: stub.URL,
		Checkin:    cfg,
		StateFile:  filepath.Join(t.TempDir(), "trae-checkin.json"),
	})
}

func checkinAccount(uid string) *account.Account {
	return &account.Account{
		Source:      account.SourceTraeCN,
		UID:         uid,
		Enabled:     true,
		AccessToken: fakeToken(uid),
	}
}

func TestCheckinAlreadySignedIn(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": true, "credits": 120})
	c := newCheckinClient(t, stub, nil)

	res, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.Already {
		t.Fatalf("result %+v, want Already", res)
	}
	if stub.sawPath(checkinClaimPath) {
		t.Fatal("an account already signed in must not be claimed again")
	}
}

func TestCheckinClaimsAndReportsCredit(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 0, "credits": 50})
	c := newCheckinClient(t, stub, nil)

	res, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Already {
		t.Fatal("a fresh claim must not report Already")
	}
	if res.Credit != 50 {
		t.Fatalf("credit %d, want 50", res.Credit)
	}
}

func TestCheckinIgnoresNonPositiveCredit(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 0, "credits": 0})
	c := newCheckinClient(t, stub, nil)

	res, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Credit != 0 {
		t.Fatalf("credit %d, want 0", res.Credit)
	}
}

func TestCheckinRotatesDeviceAfter9074(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims,
		map[string]any{"code": 9074, "message": "too many users"},
		map[string]any{"code": 0, "credits": 10},
	)
	c := newCheckinClient(t, stub, nil)

	res, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Credit != 10 {
		t.Fatalf("credit %d, want 10", res.Credit)
	}
	devices := stub.devices()
	// status + first claim + retry
	if len(devices) != 3 {
		t.Fatalf("saw %d requests, want 3", len(devices))
	}
	if devices[1] == devices[2] {
		t.Fatal("the retry must use a rotated device id")
	}
}

func TestCheckinBacksOffAfterSecond9074(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims,
		map[string]any{"code": 9074},
		map[string]any{"code": 9074},
	)
	c := newCheckinClient(t, stub, nil)

	_, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err == nil {
		t.Fatal("expected a rate-limited error")
	}
	ue := upstream.Classify(err)
	if ue.Kind != upstream.KindRateLimited {
		t.Fatalf("kind %v, want rate_limited", ue.Kind)
	}
	if ue.RetryAfter != 60*time.Second {
		t.Fatalf("retry after %s, want 1m (the first step of the backoff)", ue.RetryAfter)
	}
}

func TestCheckinHonoursPersistedBackoff(t *testing.T) {
	stub := newCheckinStub(t)
	c := newCheckinClient(t, stub, nil)
	acc := checkinAccount("u1")
	identity := acc.UID

	// Seed a backoff directly, then confirm nothing is sent while it is active.
	if err := c.ck.update(identity, func(r *checkinRecord) {
		r.RetryBackoff = 120
		r.RetryUpdatedAt = time.Now().Unix()
		r.Retry9074Count = 1
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := c.Checkin(context.Background(), acc)
	if err == nil {
		t.Fatal("expected the persisted backoff to be honoured")
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindRateLimited {
		t.Fatalf("kind %v, want rate_limited", kind)
	}
	if len(stub.devices()) != 0 {
		t.Fatal("a claim must not be sent while the backoff is active")
	}
}

func TestCheckinBackoffEscalatesAndCaps(t *testing.T) {
	cfg := CheckinConfig{RetryAfterSeconds: 60, MaxBackoffSeconds: 3600, BackoffExponentCap: 3}.normalize()
	want := []time.Duration{60, 120, 240, 480, 480, 480}
	for i, seconds := range want {
		if got := cfg.backoffFor(i, time.Now()); got != seconds*time.Second {
			t.Fatalf("backoff[%d] = %s, want %s", i, got, seconds*time.Second)
		}
	}
	// A ceiling below the base must not undercut the base.
	tight := CheckinConfig{RetryAfterSeconds: 60, MaxBackoffSeconds: 5}.normalize()
	if got := tight.backoffFor(4, time.Now()); got != 60*time.Second {
		t.Fatalf("clamped backoff %s, want 1m", got)
	}
}

func TestCheckin9095IsNotSuccess(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 9095, "message": "already checked in on this device"})
	c := newCheckinClient(t, stub, nil)

	res, err := c.Checkin(context.Background(), checkinAccount("u1"))
	if err == nil {
		t.Fatalf("9095 must not be reported as a successful claim: %+v", res)
	}
	if !strings.Contains(err.Error(), "9095") {
		t.Fatalf("error %q should name the code", err)
	}
	// 9095 is not a rate limit, so it must not start a backoff.
	if wait := c.ck.cooldownRemaining("u1", time.Now()); wait > 0 {
		t.Fatalf("9095 must not start a 9074 backoff, got %s", wait)
	}
}

func TestCheckinNonZeroCodeIsAnError(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 1001, "message": "token expired"})
	c := newCheckinClient(t, stub, nil)

	if _, err := c.Checkin(context.Background(), checkinAccount("u1")); err == nil {
		t.Fatal("expected an error for a non-zero business code")
	} else if !strings.Contains(err.Error(), "1001") {
		t.Fatalf("error %q should name the code", err)
	}
}

func TestCheckinRotationCanBeDisabled(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 9074})
	c := newCheckinClient(t, stub, func(cfg *CheckinConfig) {
		disabled := false
		cfg.RotateDeviceID = &disabled
	})

	if _, err := c.Checkin(context.Background(), checkinAccount("u1")); err == nil {
		t.Fatal("expected a rate-limited error")
	}
	if n := len(stub.devices()); n != 2 {
		t.Fatalf("saw %d requests, want 2 (status + one claim)", n)
	}
	if got := c.ck.generation("u1"); got != 0 {
		t.Fatalf("generation %d, want 0 when rotation is disabled", got)
	}
}

func TestCheckinPinnedDeviceIDDisablesRotation(t *testing.T) {
	stub := newCheckinStub(t)
	stub.push(&stub.statuses, map[string]any{"code": 0, "enable": true, "checked_in": false})
	stub.push(&stub.claims, map[string]any{"code": 9074})
	c := newCheckinClient(t, stub, func(cfg *CheckinConfig) {
		cfg.DeviceIDs = map[string]string{"u1": "1234567890123456"}
	})

	if _, err := c.Checkin(context.Background(), checkinAccount("u1")); err == nil {
		t.Fatal("expected a rate-limited error")
	}
	for _, id := range stub.devices() {
		if id != "1234567890123456" {
			t.Fatalf("device id %q, want the pinned value", id)
		}
	}
	if got := c.ck.generation("u1"); got != 0 {
		t.Fatal("a pinned id must not be rotated past")
	}
}

func TestCheckinDeviceIDIsStableAndAccountBound(t *testing.T) {
	c := New(Config{})
	first := c.checkinDeviceID("user-a", "uid-a")
	second := c.checkinDeviceID("user-a", "uid-a")
	if first != second {
		t.Fatalf("device id is not stable: %q vs %q", first, second)
	}
	if len(first) != 16 {
		t.Fatalf("device id %q is not 16 digits", first)
	}
	if other := c.checkinDeviceID("user-b", "uid-b"); other == first {
		t.Fatal("different identities must derive different device ids")
	}

	// Rotating the generation moves the id, and going back reproduces the
	// original: the id is a pure function of (identity, generation).
	c.ck.rotate("user-a")
	rotated := c.checkinDeviceID("user-a", "uid-a")
	if rotated == first {
		t.Fatal("a rotated generation must change the device id")
	}
	_ = c.ck.update("user-a", func(r *checkinRecord) { r.DeviceGeneration = 0 })
	if again := c.checkinDeviceID("user-a", "uid-a"); again != first {
		t.Fatalf("generation 0 must reproduce the original id: %q vs %q", again, first)
	}
}

func TestCheckinStateSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := newCheckinState(path)
	if !state.rotate("u1") {
		t.Fatal("rotate returned false")
	}
	if err := state.update("u1", func(r *checkinRecord) {
		r.RetryBackoff = 90
		r.RetryUpdatedAt = time.Now().Unix()
		r.Retry9074Count = 2
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	reloaded := newCheckinState(path)
	if got := reloaded.generation("u1"); got != 1 {
		t.Fatalf("generation %d, want 1 after reload", got)
	}
	if wait := reloaded.cooldownRemaining("u1", time.Now()); wait <= 0 {
		t.Fatal("the 9074 backoff must survive a restart")
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "retry_9074_count") {
		t.Fatalf("state file %s (err %v)", raw, err)
	}
}

func TestCheckinClaimSpacingGate(t *testing.T) {
	stub := newCheckinStub(t)
	c := newCheckinClient(t, stub, func(cfg *CheckinConfig) { cfg.IntervalSeconds = 1 })
	// Pre-arm the gate so the first claim has to wait.
	c.ck.mu.Lock()
	c.ck.claimAt = time.Now().Add(300 * time.Millisecond)
	c.ck.mu.Unlock()

	start := time.Now()
	if _, err := c.Checkin(context.Background(), checkinAccount("u1")); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("the spacing gate was skipped (elapsed %s)", elapsed)
	}
}

func TestScopeGatesCheckinByMode(t *testing.T) {
	acc := checkinAccount("u1")

	if scope := New(Config{}).Scope(acc); !scope.Checkin || !scope.Balance {
		t.Fatalf("a credentialed account should get check-in and balance: %+v", scope)
	}
	if scope := New(Config{UpstreamMode: CLIMode}).Scope(acc); scope.Checkin || scope.Balance {
		t.Fatalf("the cli mode has no account check-in: %+v", scope)
	}
	local := LocalCLIAccount()
	if scope := New(Config{}).Scope(local); scope.Checkin {
		t.Fatalf("the cli placeholder has nothing to check in: %+v", scope)
	}
	empty := &account.Account{Source: account.SourceTraeCN, UID: "no-token", Enabled: true}
	if scope := New(Config{}).Scope(empty); scope.Checkin {
		t.Fatalf("an account without a token has nothing to check in: %+v", scope)
	}
	if scope := New(Config{}).Scope(nil); scope.Checkin || scope.Balance {
		t.Fatalf("a nil account must be inert: %+v", scope)
	}
}

func TestCheckinRejectsCLIMode(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	if _, err := c.Checkin(context.Background(), checkinAccount("u1")); err == nil {
		t.Fatal("the cli mode must not attempt a check-in")
	}
}

func TestCreditsMergesEntitlementPacks(t *testing.T) {
	stub := newCheckinStub(t)
	stub.setEntitlements(map[string]any{
		"code": 0,
		"user_entitlement_pack_list": []any{
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 1000}},
				"usage":                 map[string]any{"credits_amount": 250},
			},
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 500}},
				"usage":                 map[string]any{"credits_amount": 100},
			},
			// A feature-flag pack: no credit limit, so it must not count.
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{}},
			},
		},
	})
	c := newCheckinClient(t, stub, nil)

	got, err := c.Credits(context.Background(), checkinAccount("u1"), 0)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if got.Total != 1500 || got.Remain != 1150 {
		t.Fatalf("credits %+v, want total 1500 remain 1150", got)
	}
	if got.Unlimited {
		t.Fatal("a limited grant must not report unlimited")
	}
}

func TestCreditsUnlimitedIsNotZero(t *testing.T) {
	stub := newCheckinStub(t)
	stub.setEntitlements(map[string]any{
		"code": 0,
		"user_entitlement_pack_list": []any{
			map[string]any{
				"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": -1}},
				"usage":                 map[string]any{"credits_amount": 90000},
			},
		},
	})
	c := newCheckinClient(t, stub, nil)

	got, err := c.Credits(context.Background(), checkinAccount("u1"), 0)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if !got.Unlimited {
		t.Fatalf("expected an unmetered grant, got %+v", got)
	}
}

func TestCreditsSurfaceBusinessError(t *testing.T) {
	stub := newCheckinStub(t)
	stub.setEntitlements(map[string]any{"code": 1001, "message": "token expired"})
	c := newCheckinClient(t, stub, nil)

	if _, err := c.Credits(context.Background(), checkinAccount("u1"), 0); err == nil {
		t.Fatal("expected an error for a non-zero business code")
	}
}

func TestBusinessCodeHandlesMissingAndShapes(t *testing.T) {
	if got := businessCode(map[string]any{}); got != codeOK {
		t.Fatalf("a body without a code must read as success, got %d", got)
	}
	for _, doc := range []map[string]any{{"code": float64(9074)}, {"code": "9074"}, {"code": json.Number("9074")}} {
		if got := businessCode(doc); got != codeRateLimited {
			t.Fatalf("code %v decoded as %d", doc["code"], got)
		}
	}
}
