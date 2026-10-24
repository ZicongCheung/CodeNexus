package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Endpoint paths for the metering and growth domains.
const (
	meterResourcePathCN   = "/v2/billing/meter/get-user-resource"
	meterResourcePathIntl = "/billing/meter/get-user-resource"
	checkinPathCN         = "/v2/billing/meter/daily-checkin"
	checkinPathIntl       = "/billing/meter/daily-checkin"
	enterpriseUsagePath   = "/v2/billing/meter/get-enterprise-user-usage"
	giftPath              = "/billing/meter/claim-gift"
	compensationPath      = "/billing/meter/claim-compensation"
	reportPath            = "/v2/report"

	growthTasksPath    = "/v2/activity/growth/tasks"
	growthAcceptPath   = "/v2/activity/growth/tasks/accept"
	growthClaimPathFmt = "/activity/growth/tasks/%s/claim"

	streakPath        = "/activity/growth/streak"
	streakRedeemPath  = "/activity/growth/redeem"
	lotterySummary    = "/activity/growth/lottery/summary"
	lotteryDraw       = "/activity/growth/lottery/draw"
	heatmapPath       = "/activity/growth/heatmap"
	makeupCardUsePath = "/activity/growth/makeup-cards/use"

	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelDepartPath = "/activity/growth/buddy/travel/depart"
	travelClaimPath  = "/activity/growth/buddy/travel/claim"
	buddyInfoPath    = "/activity/growth/buddy/info"
	buddyFirstPath   = "/activity/growth/buddy/first"
	buddyAgreePath   = "/activity/growth/buddy/agreement"

	blackCatCode = "black_cat"
)

// cstZone is the upstream's reward calendar. Mainland deployments settle daily
// rewards at midnight China Standard Time and the zone has no daylight saving,
// so a fixed offset avoids depending on host tzdata.
var cstZone = time.FixedZone("CST", 8*60*60)

// packageEndLayout is the timestamp format used by credit grants.
const packageEndLayout = "2006-01-02 15:04:05"

// Scope implements upstream.Tasks.
func (c *Client) Scope(acc *account.Account) upstream.TaskScope {
	global := realmOf(acc) == "global"
	enterprise := acc != nil && acc.IsEnterprise()
	return upstream.TaskScope{
		// The mainland-only programs: international accounts have no personal
		// check-in calendar and no growth center.
		Checkin: !global && !enterprise,
		Growth:  !global && !enterprise,
		Travel:  !global && !enterprise,
		Night:   !global && !enterprise,
		// Enterprise tenants keep the metering endpoint (a different shape) and
		// activity reporting is pointless for them, so it is off.
		Balance:  true,
		Activity: !enterprise,
	}
}

// Checkin implements upstream.Tasks.
func (c *Client) Checkin(ctx context.Context, acc *account.Account) (*upstream.CheckinResult, error) {
	paths := []string{checkinPathCN}
	if realmOf(acc) == "global" {
		paths = []string{checkinPathIntl, checkinPathCN}
	}
	_, err := c.metering(ctx, acc, http.MethodPost, paths, map[string]any{})
	if err != nil {
		if IsAlreadyDone(err) {
			return &upstream.CheckinResult{Already: true}, nil
		}
		return nil, err
	}
	return &upstream.CheckinResult{}, nil
}

// Credits implements upstream.Tasks.
func (c *Client) Credits(ctx context.Context, acc *account.Account, expiringWithin time.Duration) (*upstream.Credits, error) {
	if acc != nil && acc.IsEnterprise() {
		return c.enterpriseCredits(ctx, acc, expiringWithin)
	}
	paths := []string{meterResourcePathCN}
	if realmOf(acc) == "global" {
		paths = []string{meterResourcePathIntl, meterResourcePathCN}
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format(packageEndLayout),
	}
	data, err := c.metering(ctx, acc, http.MethodPost, paths, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					PackageCode         string `json:"PackageCode"`
					SubProductCode      string `json:"SubProductCode"`
					SubProductName      string `json:"SubProductName"`
					CreateTime          int64  `json:"CreateTime"`
					DeductionEndTime    int64  `json:"DeductionEndTime"`
					ExpiredTime         string `json:"ExpiredTime"`
					PackageEndTime      string `json:"PackageEndTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("workbuddy: credit response: %w", err)
	}

	out := &upstream.Credits{Packs: make([]upstream.CreditPack, 0, len(resp.Response.Data.Accounts))}
	for _, a := range resp.Response.Data.Accounts {
		remain, used, size := packageRemainSize(a.CycleCapacityRemain, a.CycleCapacityUsed, a.CycleCapacitySize,
			a.CapacityRemain, a.CapacityUsed, a.CapacitySize)
		out.Remain += remain
		out.Total += size
		end, hasEnd := effectivePackEnd(a.DeductionEndTime, a.ExpiredTime, a.PackageEndTime, a.CycleEndTime)
		pack := upstream.CreditPack{
			Name:   a.PackageName,
			Size:   size,
			Used:   used,
			Remain: remain,
		}
		if hasEnd {
			pack.ExpiresAt = end.Unix()
			pack.ExpiresText = end.In(cstZone).Format(packageEndLayout)
		}
		// CreateTime arrives as epoch milliseconds; zero means the upstream
		// omitted it, which stays omitted rather than becoming 1970.
		if a.CreateTime > 0 {
			pack.CreatedAt = a.CreateTime / 1000
		}
		out.Packs = append(out.Packs, pack)

		if remain <= 0 {
			continue
		}
		if !hasEnd || !end.After(now) {
			continue
		}
		if out.EarliestAt.IsZero() || end.Before(out.EarliestAt) {
			out.EarliestAt = end
			out.EarliestRemaining = remain
		} else if end.Equal(out.EarliestAt) {
			out.EarliestRemaining += remain
		}
		if expiringWithin > 0 && !end.After(now.Add(expiringWithin)) {
			out.Expiring += remain
		}
	}
	return out, nil
}

// enterpriseCredits reads an enterprise tenant's allocated quota. There the
// upstream reports consumption rather than remainder, and a negative grant
// means the seat is unmetered.
func (c *Client) enterpriseCredits(ctx context.Context, acc *account.Account, expiringWithin time.Duration) (*upstream.Credits, error) {
	data, err := c.metering(ctx, acc, http.MethodPost, []string{enterpriseUsagePath}, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Credit         float64 `json:"credit"`
		UsedNum        float64 `json:"used_num"`
		LimitNum       int64   `json:"limitNum"`
		LimitNumSnake  int64   `json:"limit_num"`
		CycleEndTime   string  `json:"cycleEndTime"`
		CycleResetTime string  `json:"cycleResetTime"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("workbuddy: enterprise credit response: %w", err)
	}
	limit := resp.LimitNum
	if limit == 0 {
		limit = resp.LimitNumSnake
	}
	used := resp.Credit
	if used == 0 {
		used = resp.UsedNum
	}
	if limit < 0 {
		return &upstream.Credits{Unlimited: true, Total: -1}, nil
	}
	remain := limit - int64(used+0.5)
	if remain < 0 {
		remain = 0
	}
	out := &upstream.Credits{Remain: remain, Total: limit, Unlimited: false}
	if end, ok := parseGrantEnd(firstNonEmpty(resp.CycleEndTime, resp.CycleResetTime)); ok && end.After(time.Now()) && remain > 0 {
		out.EarliestAt = end
		out.EarliestRemaining = remain
		if expiringWithin > 0 && !end.After(time.Now().Add(expiringWithin)) {
			out.Expiring = remain
		}
	}
	return out, nil
}

// reportEvent mirrors the desktop client's chat_request_send event. The user
// id is mandatory: without it the service accepts the request and discards it.
type reportEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
	ActivityID            string `json:"activityId,omitempty"`
}

// ReportActivity implements upstream.Tasks. One report lights the consecutive
// sign-in day and unlocks the tasks that depend on activity.
func (c *Client) ReportActivity(ctx context.Context, acc *account.Account, modelID, modelName string) error {
	return c.reportActivity(ctx, acc, modelID, modelName, "")
}

// reportActivity optionally carries an activity id for the mini-program event
// shapes.
func (c *Client) reportActivity(ctx context.Context, acc *account.Account, modelID, modelName, activityID string) error {
	if modelID == "" {
		modelID, modelName = "deepseek-v4-flash", "DeepSeek V4 Flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	conv := fmt.Sprintf("codenexus-%d", now)
	ev := reportEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conv,
		RequestID:            conv,
		InputLength:          12,
		RequestModelID:       modelID,
		RequestModelName:     modelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        conv,
		ParentConversationID: conv,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               acc.UID,
		ActivityID:           activityID,
	}
	raw, err := json.Marshal([]reportEvent{ev})
	if err != nil {
		return err
	}
	billing := strings.TrimRight(c.hostsFor(acc).billing, "/")
	_, err = c.doTask(ctx, http.MethodPost, billing+reportPath, json.RawMessage(raw), c.taskHeaders(acc, meteringUserAgent))
	return err
}

// Growth implements upstream.Tasks.
func (c *Client) Growth(ctx context.Context, acc *account.Account) (*upstream.GrowthState, error) {
	data, err := c.growth(ctx, acc, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Streak struct {
			Days              int    `json:"days"`
			MonthTotalDays    int    `json:"month_total_days"`
			NextTier          string `json:"next_tier"`
			NextTierRemaining int    `json:"next_tier_remaining"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int `json:"balance"`
			Max     int `json:"max"`
		} `json:"makeup_cards"`
		Redemption struct {
			Tier7d  string `json:"tier_7d_status"`
			Tier14d string `json:"tier_14d_status"`
			Tier28d string `json:"tier_28d_status"`
			Tiers   []struct {
				Tier    string `json:"tier"`
				Days    int    `json:"days"`
				Credit  int64  `json:"credit"`
				Energy  int64  `json:"energy"`
				Cards   int    `json:"cards"`
				Chances int    `json:"chances"`
			} `json:"tiers"`
		} `json:"redemption_status"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("workbuddy: streak response: %w", err)
	}
	status := map[string]string{
		"7d":  resp.Redemption.Tier7d,
		"14d": resp.Redemption.Tier14d,
		"28d": resp.Redemption.Tier28d,
	}
	out := &upstream.GrowthState{
		StreakDays:     resp.Streak.Days,
		StreakMonth:    resp.Streak.MonthTotalDays,
		NextTier:       resp.Streak.NextTier,
		NextTierRemain: resp.Streak.NextTierRemaining,
		MakeupCards:    resp.MakeupCards.Balance,
		MakeupMax:      resp.MakeupCards.Max,
	}
	for _, t := range resp.Redemption.Tiers {
		out.Tiers = append(out.Tiers, upstream.StreakTier{
			Tier: t.Tier, Days: t.Days, Credit: t.Credit, Energy: t.Energy,
			Cards: t.Cards, Chances: t.Chances, Status: status[t.Tier],
		})
	}
	// Lottery chances live on a separate read; a failure here is not fatal for
	// the rest of the snapshot.
	if chances, enabled, err := c.lotteryChances(ctx, acc); err == nil {
		out.LotteryChances, out.LotteryEnabled = chances, enabled
	}
	return out, nil
}

func (c *Client) lotteryChances(ctx context.Context, acc *account.Account) (int, bool, error) {
	data, err := c.growth(ctx, acc, http.MethodGet, lotterySummary, nil)
	if err != nil {
		return 0, false, err
	}
	var resp struct {
		Chances int `json:"chances"`
		Module  struct {
			Enabled bool `json:"enabled"`
		} `json:"module"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, false, err
	}
	return resp.Chances, resp.Module.Enabled, nil
}

// Travel implements upstream.Tasks.
func (c *Client) Travel(ctx context.Context, acc *account.Account) (*upstream.TravelStatus, error) {
	data, err := c.growth(ctx, acc, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	out := &upstream.TravelStatus{}
	var info struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &info); err == nil {
		if trimmed := strings.TrimSpace(string(info.Buddy)); trimmed != "" && trimmed != "null" {
			var b upstream.Buddy
			if json.Unmarshal(info.Buddy, &b) == nil {
				out.Buddy = &b
			}
		}
	}
	if out.Buddy == nil {
		return out, nil
	}
	status, err := c.growth(ctx, acc, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return out, err
	}
	var st struct {
		State             string `json:"state"`
		DailyLimitReached bool   `json:"daily_limit_reached"`
		RecordID          int64  `json:"record_id"`
		RewardCredit      int64  `json:"reward_credit"`
	}
	if err := json.Unmarshal(status, &st); err != nil {
		return out, err
	}
	out.State = upstream.TravelState{
		State:             st.State,
		DailyLimitReached: st.DailyLimitReached,
		RecordID:          st.RecordID,
		RewardCredit:      st.RewardCredit,
	}
	return out, nil
}

// TravelDepart implements upstream.Tasks.
func (c *Client) TravelDepart(ctx context.Context, acc *account.Account, locationID int) error {
	_, err := c.growth(ctx, acc, http.MethodPost, travelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim implements upstream.Tasks.
func (c *Client) TravelClaim(ctx context.Context, acc *account.Account, recordID int64) (int64, error) {
	data, err := c.growth(ctx, acc, http.MethodPost, travelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// BuddyAgreement implements upstream.Tasks.
func (c *Client) BuddyAgreement(ctx context.Context, acc *account.Account) error {
	_, err := c.growth(ctx, acc, http.MethodPost, buddyAgreePath, map[string]any{"agree": true})
	return err
}

// BuddyFirst implements upstream.Tasks. The upstream rejects adoption until the
// prerequisite activity threshold is met.
func (c *Client) BuddyFirst(ctx context.Context, acc *account.Account) error {
	_, err := c.growth(ctx, acc, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// BuddyAdopt implements upstream.Tasks: agree to the terms and adopt.
func (c *Client) BuddyAdopt(ctx context.Context, acc *account.Account) error {
	if err := c.BuddyAgreement(ctx, acc); err != nil {
		return err
	}
	return c.BuddyFirst(ctx, acc)
}

// RedeemStreak implements upstream.Tasks.
func (c *Client) RedeemStreak(ctx context.Context, acc *account.Account, tier string) error {
	_, err := c.growth(ctx, acc, http.MethodPost, streakRedeemPath, map[string]any{
		"tier":         tier,
		"client_token": randomToken(),
	})
	return err
}

// DrawLottery implements upstream.Tasks.
func (c *Client) DrawLottery(ctx context.Context, acc *account.Account) (int, error) {
	if _, err := c.growth(ctx, acc, http.MethodPost, lotteryDraw, map[string]any{"client_token": randomToken()}); err != nil {
		return 0, err
	}
	chances, _, err := c.lotteryChances(ctx, acc)
	if err != nil {
		return 0, nil // the draw succeeded; a failed recount must not look like a failure
	}
	return chances, nil
}

// MissedYesterday implements upstream.Tasks.
func (c *Client) MissedYesterday(ctx context.Context, acc *account.Account) (bool, error) {
	data, err := c.growth(ctx, acc, http.MethodGet, heatmapPath, nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	want := time.Now().In(cstZone).AddDate(0, 0, -1).Format("2006-01-02")
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == want {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard implements upstream.Tasks.
func (c *Client) UseMakeupCard(ctx context.Context, acc *account.Account, date string) error {
	_, err := c.growth(ctx, acc, http.MethodPost, makeupCardUsePath, map[string]any{"target_date": date})
	return err
}

// ListTasks implements upstream.Tasks.
func (c *Client) ListTasks(ctx context.Context, acc *account.Account, miniProgram bool) ([]upstream.Task, error) {
	var (
		data json.RawMessage
		err  error
	)
	if miniProgram {
		data, err = c.growthMP(ctx, acc, http.MethodGet, growthTasksPath, nil)
	} else {
		data, err = c.growth(ctx, acc, http.MethodGet, growthTasksPath, nil)
	}
	if err != nil {
		return nil, err
	}
	return parseTasks(data, miniProgram)
}

// AcceptTasks implements upstream.Tasks. Enrolling is idempotent: progress is
// driven by upstream behaviour events, not by the acceptance itself.
func (c *Client) AcceptTasks(ctx context.Context, acc *account.Account, codes []string, miniProgram bool) error {
	if len(codes) == 0 {
		return nil
	}
	body := map[string]any{"task_codes": codes}
	if miniProgram {
		_, err := c.growthMP(ctx, acc, http.MethodPost, growthAcceptPath, body)
		return err
	}
	_, err := c.growth(ctx, acc, http.MethodPost, growthAcceptPath, body)
	return err
}

// ClaimReward implements upstream.Tasks. Claiming goes through the chat host
// for mini-program tasks and falls back to the web growth center, which
// serves the same route for the remaining shapes.
func (c *Client) ClaimReward(ctx context.Context, acc *account.Account, code string, miniProgram bool) (int64, int64, error) {
	path := fmt.Sprintf(growthClaimPathFmt, url.PathEscape(code))
	if miniProgram {
		data, err := c.growthMP(ctx, acc, http.MethodPost, path, nil)
		if err == nil {
			return parseClaim(data)
		}
		if e, ok := asAPIError(err); !ok || e.Status != http.StatusBadRequest {
			return 0, 0, err
		}
	}
	data, err := c.web(ctx, acc, http.MethodPost, path, nil)
	if err != nil {
		return 0, 0, err
	}
	return parseClaim(data)
}

// NightNeed implements upstream.Tasks.
func (c *Client) NightNeed(ctx context.Context, acc *account.Account) (int64, error) {
	tasks, err := c.ListTasks(ctx, acc, false)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.Code != blackCatCode {
			continue
		}
		if t.Claimed || (t.Target > 0 && t.Current >= t.Target) {
			return 0, nil
		}
		if t.Target > t.Current {
			return t.Target - t.Current, nil
		}
		return 0, nil
	}
	return 0, nil
}

// NightRun performs n real conversations so the late-night task progresses.
// Each turn is intentionally trivial: the task counts the interaction, not its
// content.
func (c *Client) NightRun(ctx context.Context, acc *account.Account, n int) error {
	if n <= 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		body := map[string]any{
			"model":    nightModel,
			"messages": []map[string]any{{"role": "user", "content": "1+1 equals what? Answer briefly."}},
			"stream":   true,
		}
		if err := c.drainChat(ctx, acc, body); err != nil {
			return fmt.Errorf("workbuddy: night chat %d/%d: %w", i+1, n, err)
		}
		if err := c.reportActivity(ctx, acc, nightModel, nightModel, ""); err != nil {
			return fmt.Errorf("workbuddy: night report %d/%d: %w", i+1, n, err)
		}
		if i < n-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nightChatGap):
			}
		}
	}
	return nil
}

// nightModel is the model the late-night task is defined against.
const nightModel = "glm-5.2"

// nightChatGap spaces out the night conversations to look like human pacing.
const nightChatGap = 4 * time.Second

// drainChat runs one streaming completion and discards the body. It reuses the
// streaming path so the upstream sees the same request shape as a real turn.
func (c *Client) drainChat(ctx context.Context, acc *account.Account, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v2/chat/completions", strings.NewReader(string(raw)), acc)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError("night chat", resp)
	}
	// Read a bounded prefix: the task only needs the turn to have happened.
	buf := make([]byte, 4096)
	_, _ = resp.Body.Read(buf)
	return nil
}

// ClaimGift implements upstream.Tasks. Both payouts are once-per-account; a
// missing one comes back as a business error, which callers treat as "nothing
// to collect".
func (c *Client) ClaimGift(ctx context.Context, acc *account.Account) (int64, error) {
	return c.claimCredit(ctx, acc, giftPath)
}

// ClaimCompensation collects a compensation payout when one is available.
func (c *Client) ClaimCompensation(ctx context.Context, acc *account.Account) (int64, error) {
	return c.claimCredit(ctx, acc, compensationPath)
}

func (c *Client) claimCredit(ctx context.Context, acc *account.Account, path string) (int64, error) {
	data, err := c.metering(ctx, acc, http.MethodPost, []string{path}, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

func parseTasks(data json.RawMessage, miniProgram bool) ([]upstream.Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"`
			RewardEnergy int64           `json:"reward_energy"`
			TaskType     string          `json:"task_type"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("workbuddy: task list response: %w", err)
	}
	out := make([]upstream.Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, target := t.Current, t.Target
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var p struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &p) == nil && (p.Target > 0 || p.Current > 0) {
				cur, target = p.Current, p.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, upstream.Task{
			Code:         t.TaskCode,
			Title:        t.Title,
			Description:  t.Description,
			Desc:         t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			Target:       target,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			Locked:       t.Locked,
			JumpURL:      t.JumpURL,
			Claimable:    !claimed && target > 0 && cur >= target,
			Claimed:      claimed,
			MiniProgram:  miniProgram,
		})
	}
	return out, nil
}

func parseClaim(data json.RawMessage) (int64, int64, error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if len(data) == 0 {
		return 0, 0, nil
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil
	}
	return resp.Credit, resp.Energy, nil
}

// packageRemainSize normalises one credit grant to (remain, used, size).
// Periodic grants take precedence because the non-periodic counters can lag.
func packageRemainSize(cycleRemain, cycleUsed, cycleSize, remain, used, size int64) (int64, int64, int64) {
	if cycleSize > 0 {
		r := cycleRemain
		if r < 0 {
			r = 0
		}
		if r > cycleSize {
			r = cycleSize
		}
		u := cycleSize - r
		if cycleUsed > u {
			u = cycleUsed
		}
		if u > cycleSize {
			u = cycleSize
		}
		return r, u, cycleSize
	}
	r := remain
	if r < 0 {
		r = 0
	}
	if used == 0 && size > r {
		used = size - r
	}
	if size < r {
		size = r
	}
	return r, used, size
}

// effectivePackEnd reports when a pack truly runs out. DeductionEndTime — the
// end of the spend window — is the honest answer to "when is this credit
// gone"; the grant wall-clocks are fallbacks, and CycleEndTime comes last
// because it marks the period boundary where a periodic grant resets, not an
// expiry. The expiry picture and the per-pack display must share this rule —
// they once disagreed, and a summary that says "gone Friday" above a pack
// table that says "gone next month" is worse than either alone.
func effectivePackEnd(deductionEndMS int64, expired, packageEnd, cycleEnd string) (time.Time, bool) {
	if deductionEndMS > 0 {
		return time.UnixMilli(deductionEndMS), true
	}
	for _, raw := range []string{expired, packageEnd, cycleEnd} {
		if t, ok := parseGrantEnd(raw); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseGrantEnd(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{packageEndLayout, "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, cstZone); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// randomToken produces the idempotency token the web client generates per
// action.
func randomToken() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
