package growth

import (
	"context"
	"fmt"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Fixed identifiers the upstream keys some tasks on. They are part of the
// task definitions, not of any one account.
const (
	// buddyAppID and buddyAppName identify the companion app whose visit
	// satisfies both the generic and the named "entered an app" task.
	buddyAppID   = "cb_y5Dy46tPQGGWtueMxXbe"
	buddyAppName = "企鹅教师助手"

	// libraryDocURL is the document whose intro page the library task credits.
	libraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

	// appearanceThemeKey is the theme the appearance task is keyed on.
	appearanceThemeKey = "theme-tkmw7j"

	// lighthouseExpertID is the lightweight-cloud expert of its dedicated task.
	lighthouseExpertID = "ex_2cvvUZQhDyeJ"

	// schoolSeasonActivityID ties the campus-day evidence to its campaign.
	schoolSeasonActivityID = "school_open_day_2026"

	// nightModel is the model the night-owl task credits.
	nightModel = "glm-5.2"

	// widgetModel is the model the "use a specific model" task credits.
	widgetModel     = "glm-5.2"
	widgetModelName = "GLM-5.2"

	// fastModel is the client's default model, used for the synthesized chains.
	fastModel = "fast-model"
)

// templates are the five template ids the template task credits. The upstream
// does not verify that the templates exist.
var templates = [][2]string{
	{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
	{"4", "活动策划"}, {"5", "代码评审"},
}

// playbookCaseID and playbookCaseName identify the inspiration case used by the
// playbook tasks.
const (
	playbookCaseID   = "pm-gtm-launch-plan"
	playbookCaseName = "新产品上市 GTM 发布计划一页纸"
)

// Actions returns the executor table in dependency order: an entry that
// unlocks another comes first. The order and the codes mirror the upstream's
// task catalogue.
func Actions() []Action {
	return []Action{
		{Code: "chat_5", Desc: "上报 5 条对话活跃事件（按差额补足）", Class: ClassAccount, Run: runChat5},
		{Code: "first_buddy", Desc: "上报解锁 → 同意协议 → 领养第一只伙伴", Class: ClassAccount, Run: runFirstBuddy},
		{Code: "Model_chat_GLM5.2", Desc: "接受任务 → 与 glm-5.2 真实对话一次 → 对齐模型上报", Class: ClassAccount, Run: runModelChat},
		{Code: "RichMeow_Chat", Desc: "上报桌面端完整对话事件链", Class: ClassDesktop, Run: runRichMeow},
		{Code: "Buddy_App", Desc: "上报进入伙伴应用事件链", Class: ClassDesktop, Run: runBuddyApp},
		{Code: "Buddy_App_QQ", Desc: "上报进入指定伙伴应用事件链", Class: ClassDesktop, Run: runBuddyApp},
		{Code: "automation_1", Desc: "上报定时任务创建事件", Class: ClassDesktop, Run: runAutomationCreate},
		{Code: "Library_read", Desc: "上报资料库介绍页阅读事件", Class: ClassDesktop, Run: runLibraryRead},
		{Code: "template_5", Desc: "上报 5 组模板创建事件", Class: ClassDesktop, Run: runTemplateUse},
		{Code: "playbook_prompt", Desc: "上报灵感案例发送 Prompt 事件组", Class: ClassDesktop, Run: runPlaybookPrompt},
		{Code: "create_canvas", Desc: "上报设计创意画布创建事件组", Class: ClassDesktop, Run: runCreateCanvas},
		{Code: "expert_5", Desc: "对 5 位真实专家完成召唤与使用链", Class: ClassDesktop, Run: runExpertUse},
		{Code: "Expert_team_use_3", Desc: "对 3 个专家团完成召唤与使用链", Class: ClassDesktop, Run: runExpertTeamUse},
		{Code: "Hp_Appearance", Desc: "设置主题并上报皮肤生效事件", Class: ClassDesktop, Run: runAppearance},
		{Code: "skill_1", Desc: "真实对话 + 技能加载事件", Class: ClassDesktop, Run: runSkillFresh},
		{Code: "Expert_lighthouse", Desc: "召唤并使用指定专家（真实对话 requestId）", Class: ClassDesktop, Run: runExpertLighthouse},
		{Code: "black_cat", Desc: "夜间窗口内补足 glm-5.2 对话", Class: ClassAccount, Attempt: true, Run: runBlackCat},
		{Code: "school_season", Desc: "校园日：接受 → 小程序对话上报 → 领奖", Class: ClassMiniProgram, Run: runSchoolSeason},
		{Code: "Sequential_Tasks_1", Desc: "小程序首次对话", Class: ClassMiniProgram, Run: runSequentialChat},
		{Code: "Sequential_Tasks_2", Desc: "小程序内选用专家完成对话", Class: ClassMiniProgram, Run: runSequentialExpert},
		{Code: "Sequential_Tasks_3", Desc: "小程序内完成 5 次对话", Class: ClassMiniProgram, Run: runSequentialChat5},
		{Code: "Sequential_Tasks_4", Desc: "小程序内创建定时任务", Class: ClassMiniProgram, Run: runSequentialAutomation},
		{Code: "Sequential_Tasks_5", Desc: "小程序内使用指定模型", Class: ClassMiniProgram, Run: runSequentialModelChat},
		{Code: "Sequential_Tasks_6", Desc: "小程序内完成 10 次对话", Class: ClassMiniProgram, Run: runSequentialChat10},
		{Code: "Sequential_Tasks_7", Desc: "体验灵感功能", Class: ClassMiniProgram, Run: runSequentialPlaybook},
	}
}

// ActionByCode returns the executor for a task code.
func ActionByCode(code string) (*Action, bool) {
	all := Actions()
	for i := range all {
		if all[i].Code == code {
			return &all[i], true
		}
	}
	return nil, false
}

// msgID builds the message id a telemetry chain uses for a given request id,
// matching the shape the actual-use evidence computes internally.
func msgID(requestID string) string {
	tail := requestID
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return "msg-" + tail
}

// ---------------------------------------------------------------------------
// Account-primitive executors
// ---------------------------------------------------------------------------

// runChat5 tops up the plain conversation task by reporting the outstanding
// activity events.
func runChat5(ctx context.Context, e *Env) (string, error) {
	t, err := e.taskByCode(ctx, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	o := e.opts()
	for i := int64(0); i < need; i++ {
		if err := e.Tasks.ReportActivity(ctx, e.Acc, "", ""); err != nil {
			return fmt.Sprintf("补报第 %d/%d 条后中断", i+1, need), nil
		}
		if i < need-1 {
			_ = e.sleep(ctx, o.ReportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy reports activity to unlock adoption, accepts the terms and
// adopts the first companion.
func runFirstBuddy(ctx context.Context, e *Env) (string, error) {
	o := e.opts()
	if err := e.Tasks.ReportActivity(ctx, e.Acc, "", ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	if err := e.sleep(ctx, o.ReportGap); err != nil {
		return "", err
	}
	if err := e.Tasks.BuddyAgreement(ctx, e.Acc); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := e.Tasks.BuddyFirst(ctx, e.Acc); err != nil {
		// The threshold is evaluated against the day's activity, which the
		// report above may not have credited yet.
		return "前置已上报，领养门槛尚未通过，下次重试", nil
	}
	return "已领养伙伴", nil
}

// runModelChat enrols the model task, runs a real conversation with the
// credited model and reports the matching activity event.
func runModelChat(ctx context.Context, e *Env) (string, error) {
	const code = "Model_chat_GLM5.2"
	o := e.opts()
	if err := e.Tasks.AcceptTasks(ctx, e.Acc, []string{code}, false); err != nil {
		e.logf("growth: accept %s: %v（继续走行为链路）", code, err)
	}
	if err := e.sleep(ctx, o.ReportGap); err != nil {
		return "", err
	}
	if _, _, err := e.Tasks.DesktopChat(ctx, e.Acc, "", widgetModel); err != nil {
		return fmt.Sprintf("对话请求失败: %v", err), nil
	}
	if err := e.sleep(ctx, o.ReportGap); err != nil {
		return "", err
	}
	if err := e.Tasks.ReportActivity(ctx, e.Acc, widgetModel, widgetModelName); err != nil {
		return "对话已完成，但进度上报失败", nil
	}
	return "已完成指定模型对话并上报", nil
}

// runBlackCat tops up the night-owl task inside its counting window.
func runBlackCat(ctx context.Context, e *Env) (string, error) {
	if !upstream.InNightWindow(e.now()) {
		return "当前不在 23:00–08:00 计数窗口，等待夜间排程", nil
	}
	need, err := e.Tasks.NightNeed(ctx, e.Acc)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	if err := e.Tasks.NightRun(ctx, e.Acc, int(need)); err != nil {
		return fmt.Sprintf("完成 %d 次夜间对话后中断", need), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", need), nil
}

// ---------------------------------------------------------------------------
// Desktop-telemetry executors
// ---------------------------------------------------------------------------

// runRichMeow reports a full successful desktop conversation chain.
func runRichMeow(ctx context.Context, e *Env) (string, error) {
	conv := upstream.NewConversationID("rm")
	req := upstream.NewConversationID("rm-req")
	events := upstream.DesktopChatSequence(conv, req, msgID(req), fastModel, fastModel)
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", err
	}
	return "已上报桌面端完整对话事件链", nil
}

// runBuddyApp reports the companion-app entry chain. One batch satisfies both
// the generic and the named variants of the task.
func runBuddyApp(ctx context.Context, e *Env) (string, error) {
	events := upstream.DesktopBuddyAppSequence(buddyAppID, buddyAppName)
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", err
	}
	return "已上报伙伴应用进入事件链", nil
}

// runAutomationCreate reports the scheduled-task-created event.
func runAutomationCreate(ctx context.Context, e *Env) (string, error) {
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, upstream.DesktopAutomationCreateEvent("codenexus 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead reports the library introduction page visit on the web host.
func runLibraryRead(ctx context.Context, e *Env) (string, error) {
	if err := e.Tasks.ReportWebEvent(ctx, e.Acc, "web_element_click", libraryDocURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runTemplateUse reports the five template-creation groups.
func runTemplateUse(ctx context.Context, e *Env) (string, error) {
	for i, tp := range templates {
		conv := upstream.NewConversationID(fmt.Sprintf("tpl-%d", i))
		req := upstream.NewConversationID(fmt.Sprintf("tpl-req-%d", i))
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败", i+1), nil
		}
		if err := e.sleep(ctx, 300*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "已上报 5 组模板创建事件", nil
}

// runPlaybookPrompt reports the inspiration-case prompt-send group.
func runPlaybookPrompt(ctx context.Context, e *Env) (string, error) {
	conv := upstream.NewConversationID("pb")
	req := upstream.NewConversationID("pb-req")
	events := upstream.DesktopPlaybookPromptSequence(conv, req, playbookCaseID, playbookCaseName)
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", err
	}
	return "已上报灵感案例 Prompt 发送事件组", nil
}

// runCreateCanvas reports the design-canvas creation group.
func runCreateCanvas(ctx context.Context, e *Env) (string, error) {
	conv := upstream.NewConversationID("canvas")
	req := upstream.NewConversationID("canvas-req")
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", err
	}
	return "已上报设计创意画布创建事件组", nil
}

// runAppearance applies a theme and reports the skin-applied event. The theme
// call alone does not count; the event is the evidence.
func runAppearance(ctx context.Context, e *Env) (string, error) {
	if err := e.Tasks.SetAppearanceTheme(ctx, e.Acc, appearanceThemeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	if err := e.sleep(ctx, 2*time.Second); err != nil {
		return "", err
	}
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, upstream.AppearanceAppliedEvent(appearanceThemeKey)); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runSkillFresh runs a real conversation whose response reports a tool call,
// then reports the skill-load event joined to that conversation.
func runSkillFresh(ctx context.Context, e *Env) (string, error) {
	conv, req, err := e.Tasks.DesktopChat(ctx, e.Acc, "", fastModel)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	mid := msgID(req)
	events := upstream.DesktopChatSequence(conv, req, mid, fastModel, fastModel)
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls"
		}
	}
	events = append(events, map[string]any{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": mid,
		"requestModelId": fastModel, "requestModelName": fastModel,
		"traceId": req,
	})
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", fmt.Errorf("技能加载事件: %w", err)
	}
	return "已上报真实对话与技能加载事件", nil
}

// runExpertUse completes the "used five experts" task.
func runExpertUse(ctx context.Context, e *Env) (string, error) {
	return runExpertBatch(ctx, e, "agent", 5)
}

// runExpertTeamUse completes the "used three expert teams" task.
func runExpertTeamUse(ctx context.Context, e *Env) (string, error) {
	return runExpertBatch(ctx, e, "team", 3)
}

// runExpertBatch summons and uses up to count real experts of the given type.
// Each cycle needs a real conversation so its usage evidence quotes a
// server-issued request id. A failing cycle does not stop the batch.
func runExpertBatch(ctx context.Context, e *Env, expertType string, count int) (string, error) {
	experts, err := e.Tasks.MarketExperts(ctx, e.Acc, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	o := e.opts()
	done := 0
	for i, ex := range experts {
		if done >= count {
			break
		}
		if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, upstream.DesktopExpertSummonSequence(ex)...); err != nil {
			continue
		}
		conv, req, cerr := e.Tasks.DesktopChat(ctx, e.Acc, ex.ID, "")
		if cerr != nil {
			continue
		}
		events := append(
			upstream.DesktopChatSequence(conv, req, msgID(req), fastModel, fastModel),
			upstream.DesktopExpertActualUseEvent(ex, conv, req),
		)
		if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
			continue
		}
		done++
		if i < len(experts)-1 {
			_ = e.sleep(ctx, o.ExpertSummonGap)
		}
	}
	if done == 0 {
		return "未能完成任何专家的召唤与使用链", nil
	}
	return fmt.Sprintf("已对 %d 位专家完成召唤与使用链（类型 %s）", done, expertType), nil
}

// runExpertLighthouse summons and uses the lightweight-cloud expert. Its
// evidence differs from the generic expert task in the conversation chain
// (it is marked as expert-attributed) and in the LOCAL usage mode.
func runExpertLighthouse(ctx context.Context, e *Env) (string, error) {
	lh := upstream.MarketExpert{
		ID: lighthouseExpertID, Type: "agent",
		Name: "腾讯轻量云专家", Title: "腾讯轻量云专家", Version: "1.0.2",
	}
	if experts, err := e.Tasks.MarketExperts(ctx, e.Acc, "agent"); err == nil {
		for _, ex := range experts {
			if ex.ID == lighthouseExpertID {
				lh = ex
				break
			}
		}
	}
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := e.Tasks.DesktopChat(ctx, e.Acc, lh.ID, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, msgID(req), fastModel, fastModel)
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ID
			ev["expert_name"] = lh.Name
			ev["expert_industry_id"] = ""
		}
	}
	use := upstream.DesktopExpertActualUseLocal(lh, conv, req)
	use["type"] = ""
	use["cost"] = 0
	events = append(events, use)
	if err := e.Tasks.ReportDesktopEvents(ctx, e.Acc, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报指定专家召唤与使用链", nil
}
