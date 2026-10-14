package growth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// stub implements upstream.Tasks with only the methods the executors use. The
// embedded interface is nil so an unexercised method panics loudly.
type stub struct {
	upstream.Tasks

	defaultTasks []upstream.Task
	miniTasks    []upstream.Task

	acceptedDefault []string
	acceptedMini    []string
	claimCalls      []string
	claimCredit     int64
	claimEnergy     int64
	claimErr        error

	activity  []string
	desktop   [][]map[string]any
	miniEvent [][]map[string]any
	webCalls  []string

	experts    []upstream.MarketExpert
	expertErr  error
	chatCalls  []string
	chatExpert []string
	appearance []string

	nightNeed int64
	nightRuns int

	// acceptedOnAccept flips the accept status of the matching task once the
	// enrolment is made, emulating the upstream's read-back confirmation.
	acceptedOnAccept bool
}

func (s *stub) ListTasks(_ context.Context, _ *account.Account, mini bool) ([]upstream.Task, error) {
	if mini {
		return s.miniTasks, nil
	}
	return s.defaultTasks, nil
}

func (s *stub) AcceptTasks(_ context.Context, _ *account.Account, codes []string, mini bool) error {
	if mini {
		s.acceptedMini = append(s.acceptedMini, codes...)
		if s.acceptedOnAccept {
			for i := range s.miniTasks {
				if contains(codes, s.miniTasks[i].Code) {
					s.miniTasks[i].AcceptStatus = "accepted"
				}
			}
		}
		return nil
	}
	s.acceptedDefault = append(s.acceptedDefault, codes...)
	return nil
}

func (s *stub) ClaimReward(_ context.Context, _ *account.Account, code string, _ bool) (int64, int64, error) {
	if s.claimErr != nil {
		return 0, 0, s.claimErr
	}
	s.claimCalls = append(s.claimCalls, code)
	return s.claimCredit, s.claimEnergy, nil
}

func (s *stub) ReportActivity(_ context.Context, _ *account.Account, modelID, _ string) error {
	s.activity = append(s.activity, modelID)
	return nil
}

func (s *stub) ReportDesktopEvents(_ context.Context, _ *account.Account, events ...map[string]any) error {
	s.desktop = append(s.desktop, events)
	return nil
}

func (s *stub) ReportMiniProgramEvents(_ context.Context, _ *account.Account, events ...map[string]any) error {
	s.miniEvent = append(s.miniEvent, events)
	return nil
}

func (s *stub) ReportWebEvent(_ context.Context, _ *account.Account, code, _, _, _ string) error {
	s.webCalls = append(s.webCalls, code)
	return nil
}

func (s *stub) MarketExperts(_ context.Context, _ *account.Account, _ string) ([]upstream.MarketExpert, error) {
	return s.experts, s.expertErr
}

func (s *stub) DesktopChat(_ context.Context, _ *account.Account, expertID, model string) (string, string, error) {
	s.chatCalls = append(s.chatCalls, model)
	s.chatExpert = append(s.chatExpert, expertID)
	return "conv-1", "cmb-0123456789abcdef0123456789abcdef", nil
}

func (s *stub) SetAppearanceTheme(_ context.Context, _ *account.Account, key string) error {
	s.appearance = append(s.appearance, key)
	return nil
}

func (s *stub) BuddyAgreement(context.Context, *account.Account) error { return nil }
func (s *stub) BuddyFirst(context.Context, *account.Account) error     { return nil }

func (s *stub) NightNeed(context.Context, *account.Account) (int64, error) {
	return s.nightNeed, nil
}

func (s *stub) NightRun(_ context.Context, _ *account.Account, n int) error {
	s.nightRuns += n
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// newEnv returns an Env with pacing neutralised so tests run instantly, while
// still recording the delays the executors asked for.
func newEnv(s *stub, sleeps *[]time.Duration) *Env {
	env := &Env{
		Tasks: s,
		Acc:   &account.Account{Source: account.SourceWorkBuddy, UID: "u-1", Enabled: true},
		Sleep: func(_ context.Context, d time.Duration) error {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
			return nil
		},
		Rand: func(time.Duration) time.Duration { return 0 },
	}
	env.Acc.SetCredentials("token", "refresh", "www.codebuddy.cn", 0)
	return env
}

func TestActionsTableComplete(t *testing.T) {
	all := Actions()
	if len(all) != 25 {
		t.Fatalf("actions = %d, want 25", len(all))
	}
	want := []string{
		"chat_5", "first_buddy", "Model_chat_GLM5.2", "RichMeow_Chat", "Buddy_App",
		"Buddy_App_QQ", "automation_1", "Library_read", "template_5", "playbook_prompt",
		"create_canvas", "expert_5", "Expert_team_use_3", "Hp_Appearance", "skill_1",
		"Expert_lighthouse", "black_cat", "school_season", "Sequential_Tasks_1",
		"Sequential_Tasks_2", "Sequential_Tasks_3", "Sequential_Tasks_4",
		"Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7",
	}
	seen := map[string]bool{}
	for i, a := range all {
		if a.Code != want[i] {
			t.Errorf("action %d = %s, want %s (table order is dependency order)", i, a.Code, want[i])
		}
		if seen[a.Code] {
			t.Errorf("duplicate code %s", a.Code)
		}
		seen[a.Code] = true
		if a.Run == nil {
			t.Errorf("%s has no runner", a.Code)
		}
		if a.Class == "" {
			t.Errorf("%s has no class", a.Code)
		}
	}
	for _, code := range want {
		if _, ok := ActionByCode(code); !ok {
			t.Errorf("ActionByCode(%s) not found", code)
		}
	}
}

func TestRunSkipsClaimedTask(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{{Code: "chat_5", Claimed: true}}}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "chat_5"))
	if res.Status != "skipped" {
		t.Fatalf("status = %s, want skipped", res.Status)
	}
	if len(s.activity) != 0 {
		t.Fatal("a claimed task must not produce evidence")
	}
}

func TestRunSkipsMissingTask(t *testing.T) {
	s := &stub{}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "chat_5"))
	if res.Status != "skipped" || !strings.Contains(res.Message, "没有此任务") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunClaimsWhenProgressSettles(t *testing.T) {
	s := &stub{
		defaultTasks: []upstream.Task{{Code: "chat_5", Target: 5, Current: 5, Claimable: true}},
		claimCredit:  100,
		claimEnergy:  5,
	}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "chat_5"))
	if !res.Claimed || res.Credit != 100 || res.Energy != 5 {
		t.Fatalf("res = %+v", res)
	}
	if len(s.claimCalls) != 1 || s.claimCalls[0] != "chat_5" {
		t.Fatalf("claim calls = %v", s.claimCalls)
	}
}

func TestRunReportsClaimFailureWithoutFailing(t *testing.T) {
	s := &stub{
		defaultTasks: []upstream.Task{{Code: "chat_5", Target: 5, Current: 5, Claimable: true}},
		claimErr:     errors.New("boom"),
	}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "chat_5"))
	if res.Status != "done" || res.Claimed {
		t.Fatalf("a claim failure must not fail the action: %+v", res)
	}
	if !strings.Contains(res.Message, "领奖失败") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestRunIsIdempotentOnProgressAlreadyMet(t *testing.T) {
	s := &stub{
		defaultTasks: []upstream.Task{{Code: "chat_5", Target: 5, Current: 5}},
		claimCredit:  100,
	}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "chat_5"))
	if res.Status != "done" {
		t.Fatalf("status = %s", res.Status)
	}
	if len(s.activity) != 0 {
		t.Fatalf("no evidence should be sent when the target is met: %v", s.activity)
	}
}

func TestChat5TopsUpOnlyTheDeficit(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{{Code: "chat_5", Target: 5, Current: 3}}}
	sleeps := []time.Duration{}
	Run(context.Background(), newEnv(s, &sleeps), mustAction(t, "chat_5"))
	if len(s.activity) != 2 {
		t.Fatalf("reports = %d, want 2 (5-3)", len(s.activity))
	}
	if len(sleeps) == 0 {
		t.Fatal("consecutive reports must be paced")
	}
}

func TestBlackCatOutsideWindowDoesNothing(t *testing.T) {
	s := &stub{
		nightNeed:    3,
		defaultTasks: []upstream.Task{{Code: "black_cat", Target: 1, Current: 0}},
	}
	env := newEnv(s, nil)
	// 12:00 local: outside the 23:00-08:00 window.
	env.Now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local) }
	res := Run(context.Background(), env, mustAction(t, "black_cat"))
	if res.Status != "done" || s.nightRuns != 0 {
		t.Fatalf("outside the window nothing must run: %+v runs=%d", res, s.nightRuns)
	}
}

func TestBlackCatInsideWindowTopsUp(t *testing.T) {
	s := &stub{
		nightNeed:    3,
		defaultTasks: []upstream.Task{{Code: "black_cat", Target: 1, Current: 0}},
	}
	env := newEnv(s, nil)
	env.Now = func() time.Time { return time.Date(2026, 10, 15, 23, 30, 0, 0, time.Local) }
	if res := Run(context.Background(), env, mustAction(t, "black_cat")); res.Status != "done" {
		t.Fatalf("res = %+v", res)
	}
	if s.nightRuns != 3 {
		t.Fatalf("night runs = %d, want 3", s.nightRuns)
	}
}

func TestMiniChatReportsTheOutstandingConversations(t *testing.T) {
	s := &stub{miniTasks: []upstream.Task{{
		Code: "Sequential_Tasks_1", Target: 1, Current: 0, AcceptStatus: "accepted",
	}}}
	sleeps := []time.Duration{}
	Run(context.Background(), newEnv(s, &sleeps), mustAction(t, "Sequential_Tasks_1"))
	if len(s.miniEvent) != 1 {
		t.Fatalf("mini events = %d, want 1", len(s.miniEvent))
	}
	if s.miniEvent[0][0]["eventCode"] != "chat_request_send" {
		t.Fatalf("event = %v", s.miniEvent[0][0])
	}
	// The anti-cheat gap must be applied before every event.
	if !hasDuration(sleeps, DefaultOptions().MiniChatGap) {
		t.Fatalf("mini chat gap not applied: %v", sleeps)
	}
}

func TestMiniChatEnrolsThenReadsBackTheTarget(t *testing.T) {
	s := &stub{
		acceptedOnAccept: true,
		miniTasks: []upstream.Task{{
			Code: "Sequential_Tasks_3", Target: 0, Current: 0, AcceptStatus: "not_accepted",
		}},
	}
	env := newEnv(s, nil)
	env.Opts = Options{MiniChatGap: time.Millisecond, MiniVerifyGap: time.Millisecond}
	Run(context.Background(), env, mustAction(t, "Sequential_Tasks_3"))
	if len(s.acceptedMini) != 1 || s.acceptedMini[0] != "Sequential_Tasks_3" {
		t.Fatalf("accept calls = %v", s.acceptedMini)
	}
}

func TestSchoolSeasonCarriesTheActivityID(t *testing.T) {
	s := &stub{miniTasks: []upstream.Task{{
		Code: "school_season", Target: 1, Current: 0, AcceptStatus: "accepted",
	}}}
	Run(context.Background(), newEnv(s, nil), mustAction(t, "school_season"))
	if len(s.miniEvent) != 1 {
		t.Fatalf("events = %d", len(s.miniEvent))
	}
	if s.miniEvent[0][0]["activityId"] != schoolSeasonActivityID {
		t.Fatalf("activity id = %v", s.miniEvent[0][0]["activityId"])
	}
}

func TestMiniEventFallbackFiresWhenPrimaryDoesNotLight(t *testing.T) {
	s := &stub{miniTasks: []upstream.Task{{
		Code: "Sequential_Tasks_5", Target: 1, Current: 0, AcceptStatus: "accepted",
	}}}
	env := newEnv(s, nil)
	env.Opts = Options{MiniVerifyGap: time.Millisecond}
	Run(context.Background(), env, mustAction(t, "Sequential_Tasks_5"))
	if len(s.miniEvent) != 1 {
		t.Fatalf("primary evidence = %d, want 1", len(s.miniEvent))
	}
	if len(s.activity) != 1 {
		t.Fatalf("the fallback evidence must also be reported: %v", s.activity)
	}
}

func TestMiniEventSkipsFallbackWhenAlreadyLit(t *testing.T) {
	s := &stub{miniTasks: []upstream.Task{{
		Code: "Sequential_Tasks_5", Target: 1, Current: 1, AcceptStatus: "accepted", Claimable: true,
	}}}
	env := newEnv(s, nil)
	env.Opts = Options{MiniVerifyGap: time.Millisecond}
	Run(context.Background(), env, mustAction(t, "Sequential_Tasks_5"))
	if len(s.activity) != 0 {
		t.Fatalf("no evidence should be sent when the target is met: %v", s.activity)
	}
}

func TestExpertBatchJoinsTheServerRequestID(t *testing.T) {
	s := &stub{experts: []upstream.MarketExpert{
		{ID: "ex_one", Type: "agent", Name: "A"},
		{ID: "ex_two", Type: "agent", Name: "B"},
	}, defaultTasks: []upstream.Task{{Code: "expert_5", Target: 5, Current: 0, AcceptStatus: "accepted"}}}
	Run(context.Background(), newEnv(s, nil), mustAction(t, "expert_5"))

	if len(s.chatExpert) != 2 || s.chatExpert[0] != "ex_one" {
		t.Fatalf("expert chats = %v", s.chatExpert)
	}
	// Each expert must be summoned, then used with the server's request id.
	var sawUse bool
	for _, batch := range s.desktop {
		for _, ev := range batch {
			if ev["eventCode"] != "expert_actual_use" {
				continue
			}
			sawUse = true
			if ev["requestId"] != "cmb-0123456789abcdef0123456789abcdef" {
				t.Errorf("usage evidence must quote the server request id: %v", ev["requestId"])
			}
		}
	}
	if !sawUse {
		t.Fatal("no expert usage evidence was reported")
	}
}

func TestExpertBatchWithoutMarketplaceIsAnError(t *testing.T) {
	s := &stub{
		expertErr:    errors.New("market down"),
		defaultTasks: []upstream.Task{{Code: "expert_5", Target: 5, Current: 0}},
	}
	res := Run(context.Background(), newEnv(s, nil), mustAction(t, "expert_5"))
	if res.Status != "error" {
		t.Fatalf("res = %+v", res)
	}
}

func TestAppearanceAppliesThemeThenReportsEvent(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{{Code: "Hp_Appearance", Target: 1, Current: 0}}}
	Run(context.Background(), newEnv(s, nil), mustAction(t, "Hp_Appearance"))
	if len(s.appearance) != 1 || s.appearance[0] != appearanceThemeKey {
		t.Fatalf("appearance calls = %v", s.appearance)
	}
	if len(s.desktop) != 1 || s.desktop[0][0]["eventCode"] != "appearance_skin_apply" {
		t.Fatalf("skin event not reported: %v", s.desktop)
	}
}

func TestLibraryReadUsesTheWebHost(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{{Code: "Library_read", Target: 1, Current: 0}}}
	Run(context.Background(), newEnv(s, nil), mustAction(t, "Library_read"))
	if len(s.webCalls) != 1 || s.webCalls[0] != "web_element_click" {
		t.Fatalf("web calls = %v", s.webCalls)
	}
}

func TestTemplateUseReportsFiveGroups(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{{Code: "template_5", Target: 5, Current: 0}}}
	Run(context.Background(), newEnv(s, nil), mustAction(t, "template_5"))
	if len(s.desktop) != 5 {
		t.Fatalf("template batches = %d, want 5", len(s.desktop))
	}
}

func TestRunAllSkipsEverythingForAnEmptyAccount(t *testing.T) {
	s := &stub{}
	results := RunAll(context.Background(), newEnv(s, nil), Actions())
	if len(results) != 25 {
		t.Fatalf("results = %d, want 25", len(results))
	}
	for _, r := range results {
		if r.Status != "skipped" {
			t.Fatalf("%s = %s (%s)", r.Code, r.Status, r.Message)
		}
	}
}

func TestRunAllEnrolsPendingTasks(t *testing.T) {
	s := &stub{defaultTasks: []upstream.Task{
		{Code: "a", AcceptStatus: "not_accepted"},
		{Code: "b", AcceptStatus: "accepted"},
		{Code: "c", Claimed: true},
		{Code: "d", Locked: true},
	}}
	RunAll(context.Background(), newEnv(s, nil), Actions())
	if len(s.acceptedDefault) != 1 || s.acceptedDefault[0] != "a" {
		t.Fatalf("enrolled = %v, want only the pending task", s.acceptedDefault)
	}
}

func TestAcceptAllCountsBothListings(t *testing.T) {
	s := &stub{
		defaultTasks: []upstream.Task{{Code: "a", AcceptStatus: "not_accepted"}},
		miniTasks:    []upstream.Task{{Code: "m1"}, {Code: "m2"}},
	}
	def, mini, err := AcceptAll(context.Background(), newEnv(s, nil))
	if err != nil {
		t.Fatalf("AcceptAll: %v", err)
	}
	if def != 1 || mini != 2 {
		t.Fatalf("counts = %d/%d, want 1/2", def, mini)
	}
}

func TestMiniProgramCodeRegistration(t *testing.T) {
	for _, code := range []string{"school_season", "Sequential_Tasks_1", "Sequential_Tasks_7"} {
		if !IsMiniProgram(code) {
			t.Errorf("%s must be registered as a mini-program code", code)
		}
	}
	if IsMiniProgram("chat_5") {
		t.Error("chat_5 is not mini-program-only")
	}
}

func TestProgressText(t *testing.T) {
	cases := []struct {
		task *upstream.Task
		want string
	}{
		{nil, "?"},
		{&upstream.Task{Target: 5, Current: 3}, "3/5"},
		{&upstream.Task{Claimed: true}, "claimed"},
		{&upstream.Task{AcceptStatus: "accepted"}, "accepted"},
	}
	for _, c := range cases {
		if got := ProgressText(c.task); got != c.want {
			t.Errorf("ProgressText(%+v) = %q, want %q", c.task, got, c.want)
		}
	}
}

func mustAction(t *testing.T, code string) *Action {
	t.Helper()
	a, ok := ActionByCode(code)
	if !ok {
		t.Fatalf("no action for %s", code)
	}
	return a
}

func hasDuration(list []time.Duration, want time.Duration) bool {
	for _, d := range list {
		if d >= want {
			return true
		}
	}
	return false
}
