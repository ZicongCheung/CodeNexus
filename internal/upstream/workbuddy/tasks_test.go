package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// taskStub emulates the metering, growth and web hosts on one test server.
type taskStub struct {
	mu           sync.Mutex
	checkinMsg   string
	checkinCode  int
	claimStatus  int
	reportBodies []string
	accepts      []string
}

func (s *taskStub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		code, msg := s.checkinCode, s.checkinMsg
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg})
	})
	mux.HandleFunc("/v2/billing/meter/get-user-resource", func(w http.ResponseWriter, r *http.Request) {
		soon := time.Now().In(cstZone).Add(24 * time.Hour).Format(packageEndLayout)
		later := time.Now().In(cstZone).Add(30 * 24 * time.Hour).Format(packageEndLayout)
		writeOK(w, map[string]any{"Response": map[string]any{"Data": map[string]any{"Accounts": []map[string]any{
			{"CycleCapacitySize": 1000, "CycleCapacityRemain": 600, "CycleCapacityUsed": 400, "CycleEndTime": soon},
			{"CycleCapacitySize": 50, "CycleCapacityRemain": 50, "CycleCapacityUsed": 0, "CycleEndTime": later},
		}}}})
	})
	mux.HandleFunc("/v2/billing/meter/get-enterprise-user-usage", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{
			"credit": 812.45, "limitNum": 2000,
			"cycleEndTime": time.Now().In(cstZone).Add(48 * time.Hour).Format(packageEndLayout),
		})
	})
	mux.HandleFunc("/v2/report", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		s.mu.Lock()
		s.reportBodies = append(s.reportBodies, string(buf[:n]))
		s.mu.Unlock()
		writeOK(w, map[string]any{})
	})
	mux.HandleFunc("/activity/growth/streak", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{
			"streak":       map[string]any{"days": 7, "month_total_days": 9, "next_tier": "14d", "next_tier_remaining": 7},
			"makeup_cards": map[string]any{"balance": 1, "max": 3},
			"redemption_status": map[string]any{
				"tier_7d_status": "unclaimed", "tier_14d_status": "locked", "tier_28d_status": "claimed",
				"tiers": []map[string]any{{"tier": "7d", "days": 7, "credit": 100, "energy": 5, "cards": 1, "chances": 2}},
			},
		})
	})
	mux.HandleFunc("/activity/growth/lottery/summary", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"chances": 2, "module": map[string]any{"enabled": true}})
	})
	mux.HandleFunc("/activity/growth/lottery/draw", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"prize": "credit"})
	})
	mux.HandleFunc("/activity/growth/buddy/info", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"buddy": map[string]any{"id": 7, "name": "Mochi"}})
	})
	mux.HandleFunc("/activity/growth/buddy/travel/status", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"state": "arrived", "record_id": 42, "reward_credit": 30})
	})
	mux.HandleFunc("/v2/activity/growth/tasks", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"tasks": []map[string]any{
			{"task_code": "chat_5", "reward_credit": 100, "target": 5, "progress": map[string]any{"current": 5, "target": 5}, "accept_status": "accepted"},
			{"task_code": "black_cat", "target": 3, "current": 1, "accept_status": "accepted"},
			{"task_code": "done_one", "target": 1, "current": 1, "accept_status": "claimed"},
		}})
	})
	mux.HandleFunc("/v2/activity/growth/tasks/accept", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TaskCodes []string `json:"task_codes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.accepts = append(s.accepts, body.TaskCodes...)
		s.mu.Unlock()
		writeOK(w, map[string]any{})
	})
	mux.HandleFunc("/activity/growth/tasks/clean_me/claim", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status := s.claimStatus
		s.mu.Unlock()
		// The mini-program route rejects this task shape; the web host serves it.
		if status != 0 && r.Header.Get("X-Client-Platform") == miniProgramPlatform {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "msg": "bad request"})
			return
		}
		writeOK(w, map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
	})
	return mux
}

func newTaskClient(t *testing.T, s *taskStub) *Client {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return New(Config{
		BaseURL:        srv.URL,
		BillingBaseURL: srv.URL,
		WebBaseURL:     srv.URL,
		TimeoutSeconds: 5,
	})
}

func cnAccount() *account.Account {
	a := &account.Account{Source: account.SourceWorkBuddy, UID: "u-1", Enabled: true}
	a.SetCredentials("token-1", "refresh-1", "www.codebuddy.cn", 0)
	return a
}

func TestScopeGating(t *testing.T) {
	c := newTaskClient(t, &taskStub{})

	cn := c.Scope(cnAccount())
	if !cn.Checkin || !cn.Growth || !cn.Travel || !cn.Night || !cn.Activity || !cn.Balance {
		t.Fatalf("cn scope = %+v", cn)
	}

	global := &account.Account{Source: account.SourceWorkBuddy, UID: "u-2"}
	global.SetCredentials("t", "", "www.workbuddy.ai", 0)
	g := c.Scope(global)
	if g.Checkin || g.Growth || g.Travel || g.Night {
		t.Fatalf("global account must not have the mainland programs: %+v", g)
	}
	if !g.Activity || !g.Balance {
		t.Fatalf("global account keeps activity and balance: %+v", g)
	}

	ent := cnAccount()
	ent.EnterpriseID = "ent-1"
	e := c.Scope(ent)
	if e.Checkin || e.Growth || e.Travel || e.Night || e.Activity {
		t.Fatalf("enterprise account has no personal programs: %+v", e)
	}
	if !e.Balance {
		t.Fatal("enterprise account still reports quota")
	}
}

func TestCheckinAndIdempotentRepeat(t *testing.T) {
	s := &taskStub{}
	c := newTaskClient(t, s)
	acc := cnAccount()

	res, err := c.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Already {
		t.Fatal("first check-in must not report Already")
	}

	s.mu.Lock()
	s.checkinCode, s.checkinMsg = 10001, "今天已签到"
	s.mu.Unlock()

	res, err = c.Checkin(context.Background(), acc)
	if err != nil {
		t.Fatalf("repeat checkin should be idempotent: %v", err)
	}
	if !res.Already {
		t.Fatal("repeat check-in must report Already")
	}
}

func TestCreditsAggregatesAndBuckets(t *testing.T) {
	c := newTaskClient(t, &taskStub{})
	got, err := c.Credits(context.Background(), cnAccount(), 48*time.Hour)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if got.Remain != 650 || got.Total != 1050 {
		t.Fatalf("credits = %+v, want remain=650 total=1050", got)
	}
	if got.Expiring != 600 {
		t.Fatalf("expiring = %d, want 600 (only the 24h batch)", got.Expiring)
	}
	if got.EarliestAt.IsZero() || got.EarliestRemaining != 600 {
		t.Fatalf("earliest = %v/%d", got.EarliestAt, got.EarliestRemaining)
	}
}

// A single balance hides when the credits actually lapse — the whole question
// a credit view has to answer. The upstream itemises the grant and the client
// must carry the packs through, not sum them away: "not itemised" is a claim
// about the upstream, and printing it while holding the itemisation would lie.
func TestCreditsCarriesPerPackDetail(t *testing.T) {
	c := newTaskClient(t, &taskStub{})
	got, err := c.Credits(context.Background(), cnAccount(), 48*time.Hour)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if len(got.Packs) != 2 {
		t.Fatalf("packs = %d, want 2", len(got.Packs))
	}
	first := got.Packs[0]
	if first.Remain != 600 || first.Used != 400 || first.Size != 1000 {
		t.Fatalf("pack[0] = %+v, want 600/400/1000", first)
	}
	second := got.Packs[1]
	if second.Remain != 50 || second.Used != 0 || second.Size != 50 {
		t.Fatalf("pack[1] = %+v, want 50/0/50", second)
	}
	if first.ExpiresAt == 0 || second.ExpiresAt == 0 {
		t.Fatalf("packs lost their expiry: %+v / %+v", first, second)
	}
	// The summary and the itemisation must agree on the earliest lapse.
	if !got.EarliestAt.Equal(time.Unix(first.ExpiresAt, 0)) {
		t.Fatalf("earliest %v disagrees with pack expiry %v", got.EarliestAt, time.Unix(first.ExpiresAt, 0))
	}
}

// DeductionEndTime — the end of the spend window — outranks CycleEndTime,
// which is the period boundary where a periodic grant resets, not an expiry.
// Using the boundary instead of the window reports credits as expiring that
// are merely resetting.
func TestCreditsExpiryPrefersSpendWindow(t *testing.T) {
	window := time.Now().Add(10 * 24 * time.Hour)
	boundary := time.Now().Add(24 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"Response": map[string]any{"Data": map[string]any{"Accounts": []map[string]any{
			{
				"CycleCapacitySize": 1000, "CycleCapacityRemain": 600, "CycleCapacityUsed": 400,
				"CycleEndTime":     boundary.In(cstZone).Format(packageEndLayout),
				"DeductionEndTime": window.UnixMilli(),
				"PackageName":      "国内运营裂变包",
				"PackageCode":      "TCACA_code_007",
				"CreateTime":       window.Add(-48 * time.Hour).UnixMilli(),
			},
		}}}})
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, BillingBaseURL: srv.URL, WebBaseURL: srv.URL, TimeoutSeconds: 5})

	got, err := c.Credits(context.Background(), cnAccount(), 48*time.Hour)
	if err != nil {
		t.Fatalf("Credits: %v", err)
	}
	if len(got.Packs) != 1 {
		t.Fatalf("packs = %d, want 1", len(got.Packs))
	}
	pack := got.Packs[0]
	if pack.Name != "国内运营裂变包" {
		t.Fatalf("pack name = %q", pack.Name)
	}
	if got.EarliestAt.Unix() != window.Unix() {
		t.Fatalf("earliest = %v, want the spend window %v (not the cycle boundary)", got.EarliestAt, window)
	}
	if got.Expiring != 0 {
		t.Fatalf("expiring = %d, want 0: the spend window is outside the 48h request", got.Expiring)
	}
	if pack.CreatedAt == 0 {
		t.Fatal("pack lost its grant time")
	}
}

func TestEnterpriseCredits(t *testing.T) {
	c := newTaskClient(t, &taskStub{})
	acc := cnAccount()
	acc.EnterpriseID = "ent-1"

	got, err := c.Credits(context.Background(), acc, 72*time.Hour)
	if err != nil {
		t.Fatalf("enterprise Credits: %v", err)
	}
	if got.Remain != 1188 || got.Total != 2000 {
		t.Fatalf("enterprise credits = %+v, want remain=1188 total=2000", got)
	}
	if got.Expiring != 1188 {
		t.Fatalf("enterprise expiring = %d, want 1188", got.Expiring)
	}
}

func TestReportActivityCarriesUserID(t *testing.T) {
	s := &taskStub{}
	c := newTaskClient(t, s)
	if err := c.ReportActivity(context.Background(), cnAccount(), "", ""); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reportBodies) != 1 {
		t.Fatalf("reports = %d", len(s.reportBodies))
	}
	if !strings.Contains(s.reportBodies[0], `"userId":"u-1"`) {
		t.Fatalf("report must carry the user id, got %s", s.reportBodies[0])
	}
	if !strings.Contains(s.reportBodies[0], `"eventCode":"chat_request_send"`) {
		t.Fatalf("report must carry the event code, got %s", s.reportBodies[0])
	}
}

func TestGrowthSnapshot(t *testing.T) {
	c := newTaskClient(t, &taskStub{})
	g, err := c.Growth(context.Background(), cnAccount())
	if err != nil {
		t.Fatalf("Growth: %v", err)
	}
	if g.StreakDays != 7 || g.StreakMonth != 9 || g.MakeupCards != 1 {
		t.Fatalf("growth = %+v", g)
	}
	if len(g.Tiers) != 1 || g.Tiers[0].Tier != "7d" || g.Tiers[0].Status != "unclaimed" || g.Tiers[0].Chances != 2 {
		t.Fatalf("tiers = %+v", g.Tiers)
	}
	if g.LotteryChances != 2 || !g.LotteryEnabled {
		t.Fatalf("lottery = %d/%v", g.LotteryChances, g.LotteryEnabled)
	}
}

func TestTravelAndTaskCatalog(t *testing.T) {
	stub := &taskStub{}
	c := newTaskClient(t, stub)
	acc := cnAccount()

	tr, err := c.Travel(context.Background(), acc)
	if err != nil {
		t.Fatalf("Travel: %v", err)
	}
	if tr.Buddy == nil || tr.Buddy.Name != "Mochi" {
		t.Fatalf("buddy = %+v", tr.Buddy)
	}
	if tr.State.State != "arrived" || tr.State.RecordID != 42 || tr.State.RewardCredit != 30 {
		t.Fatalf("travel state = %+v", tr.State)
	}

	tasks, err := c.ListTasks(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	byCode := map[string]upstream.Task{}
	for _, tk := range tasks {
		byCode[tk.Code] = tk
	}
	if !byCode["chat_5"].Claimable || byCode["chat_5"].Current != 5 || byCode["chat_5"].Credit != 100 {
		t.Fatalf("chat_5 = %+v", byCode["chat_5"])
	}
	if byCode["done_one"].Claimable || !byCode["done_one"].Claimed {
		t.Fatalf("done_one = %+v", byCode["done_one"])
	}

	need, err := c.NightNeed(context.Background(), acc)
	if err != nil {
		t.Fatalf("NightNeed: %v", err)
	}
	if need != 2 {
		t.Fatalf("night need = %d, want 2", need)
	}

	if err := c.AcceptTasks(context.Background(), acc, []string{"chat_5", "template_5"}, false); err != nil {
		t.Fatalf("AcceptTasks: %v", err)
	}
	stub.mu.Lock()
	accepts := stub.accepts
	stub.mu.Unlock()
	if len(accepts) != 2 {
		t.Fatalf("accepted = %v", accepts)
	}
}

func TestClaimRewardFallsBackToWebHost(t *testing.T) {
	stub := &taskStub{claimStatus: http.StatusBadRequest}
	c := newTaskClient(t, stub)

	credit, energy, err := c.ClaimReward(context.Background(), cnAccount(), "clean_me", true)
	if err != nil {
		t.Fatalf("ClaimReward: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Fatalf("claim = %d/%d", credit, energy)
	}
}

func TestAPIErrorPredicates(t *testing.T) {
	if !IsAlreadyDone(&APIError{Code: 1, Msg: "today already checked in"}) {
		t.Fatal("IsAlreadyDone missed an already marker")
	}
	if !IsNotFound(&APIError{Status: 404}) {
		t.Fatal("IsNotFound missed a 404")
	}
	if !IsNotCompleted(&APIError{Status: 400, Msg: "task not completed"}) {
		t.Fatal("IsNotCompleted missed a marker")
	}
	if !IsUnsupported(&APIError{Status: 400, Msg: "enterprise account not supported"}) {
		t.Fatal("IsUnsupported missed a marker")
	}
	if !IsPrerequisite(&APIError{Status: 400, Msg: "prerequisite not met"}) {
		t.Fatal("IsPrerequisite missed a marker")
	}
	if IsAlreadyDone(&APIError{Status: 500}) {
		t.Fatal("final failure must not look idempotent")
	}
}

func TestParseGrantEndAcceptsMultipleLayouts(t *testing.T) {
	if _, ok := parseGrantEnd("2026-12-31 23:59:59"); !ok {
		t.Fatal("space-separated layout not parsed")
	}
	if _, ok := parseGrantEnd("2026-12-31T23:59:59+08:00"); !ok {
		t.Fatal("RFC3339 layout not parsed")
	}
	if _, ok := parseGrantEnd(""); ok {
		t.Fatal("empty timestamp must not parse")
	}
	if _, ok := parseGrantEnd("not-a-date"); ok {
		t.Fatal("garbage must not parse")
	}
}
