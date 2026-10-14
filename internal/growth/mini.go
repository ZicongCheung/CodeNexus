package growth

import (
	"context"
	"fmt"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The mini-program tasks are driven by the mini-program request marker for
// listing, accepting and claiming, and by mini-program telemetry for the
// evidence. The multi-conversation tasks have an anti-cheat check: events sent
// in a burst are counted and then rolled back, so the pacing in Options is not
// optional.

// miniChatTask completes a mini-program task whose evidence is one or more
// conversation events.
func miniChatTask(ctx context.Context, e *Env, code, activityID string) (string, error) {
	t, err := e.taskByCodeMini(ctx, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.Current >= targetOf(t) || t.AcceptStatus == "completed" {
		return "进度已达标，等待领奖", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !e.acceptMini(ctx, code) {
			return "接受任务未登记生效，待下次重试", nil
		}
		// An un-enrolled task reports no target; re-read to pick up the real
		// one so the outstanding count is not under-reported.
		if t2, rerr := e.taskByCodeMini(ctx, code); rerr == nil && t2 != nil {
			t = t2
		}
	}
	need := targetOf(t) - t.Current
	if need <= 0 {
		return "进度已达标，等待领奖", nil
	}
	o := e.opts()
	for i := int64(0); i < need; i++ {
		if err := e.sleep(ctx, o.MiniChatGap+e.jitter(o.MiniChatJitter)); err != nil {
			return "", err
		}
		conv := upstream.NewConversationID("mp")
		if err := e.Tasks.ReportMiniProgramEvents(ctx, e.Acc, upstream.MiniChatSequence(conv, activityID)); err != nil {
			return fmt.Sprintf("已上报 %d/%d 次后中断", i, need), nil
		}
	}
	return fmt.Sprintf("已补报 %d 次小程序对话事件", need), nil
}

// miniEventTask completes a mini-program task whose evidence is a single event
// group. When the primary shape does not light the task, the fallback shape is
// reported once as well.
func miniEventTask(ctx context.Context, e *Env, code string, primary, fallback func() error) (string, error) {
	t, err := e.taskByCodeMini(ctx, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（前置未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.Current >= targetOf(t) || t.AcceptStatus == "completed" {
		return "进度已达标，等待领奖", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !e.acceptMini(ctx, code) {
			return "接受任务未登记生效，待下次重试", nil
		}
	}
	if err := primary(); err != nil {
		return fmt.Sprintf("判据上报失败: %v", err), nil
	}
	if fallback != nil {
		if err := e.sleep(ctx, e.opts().MiniVerifyGap); err != nil {
			return "", err
		}
		if t2, rerr := e.taskByCodeMini(ctx, code); rerr == nil && t2 != nil &&
			!(t2.Claimable || t2.Claimed || t2.Current >= targetOf(t2)) {
			if err := fallback(); err != nil {
				return fmt.Sprintf("备选判据上报失败: %v", err), nil
			}
		}
	}
	return "已上报判据事件", nil
}

// miniExpertTask completes the mini-program expert task. The expert id must be
// a real marketplace id, so the marketplace is read before the task is
// enrolled: bailing out early avoids leaving the task enrolled without
// evidence.
func miniExpertTask(ctx context.Context, e *Env, code string) (string, error) {
	t, err := e.taskByCodeMini(ctx, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.Current >= targetOf(t) || t.AcceptStatus == "completed" {
		return "进度已达标，等待领奖", nil
	}
	experts, merr := e.Tasks.MarketExperts(ctx, e.Acc, "")
	if merr != nil || len(experts) == 0 {
		return "专家市场不可用，跳过以避免半程状态", nil
	}
	ex := experts[0]
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !e.acceptMini(ctx, code) {
			return "接受任务未登记生效，待下次重试", nil
		}
	}
	if err := e.Tasks.ReportMiniProgramEvents(ctx, e.Acc, upstream.MiniExpertUseEvent(ex.ID, ex.Name, ex.Type)); err != nil {
		return fmt.Sprintf("上报专家使用事件失败: %v", err), nil
	}
	return "已上报小程序专家使用事件", nil
}

// runSchoolSeason completes the campus-day task, whose evidence carries the
// campaign activity id.
func runSchoolSeason(ctx context.Context, e *Env) (string, error) {
	return miniChatTask(ctx, e, "school_season", schoolSeasonActivityID)
}

// runSequentialChat completes the first mini-program conversation task.
func runSequentialChat(ctx context.Context, e *Env) (string, error) {
	return miniChatTask(ctx, e, "Sequential_Tasks_1", "")
}

// runSequentialChat5 completes the five-conversation mini-program task.
func runSequentialChat5(ctx context.Context, e *Env) (string, error) {
	return miniChatTask(ctx, e, "Sequential_Tasks_3", "")
}

// runSequentialChat10 completes the ten-conversation mini-program task.
func runSequentialChat10(ctx context.Context, e *Env) (string, error) {
	return miniChatTask(ctx, e, "Sequential_Tasks_6", "")
}

// runSequentialExpert completes the mini-program expert task.
func runSequentialExpert(ctx context.Context, e *Env) (string, error) {
	return miniExpertTask(ctx, e, "Sequential_Tasks_2")
}

// runSequentialAutomation completes the mini-program scheduled-task task. The
// mini program has no such event, so the desktop event of the same meaning is
// used.
func runSequentialAutomation(ctx context.Context, e *Env) (string, error) {
	return miniEventTask(ctx, e, "Sequential_Tasks_4", func() error {
		return e.Tasks.ReportDesktopEvents(ctx, e.Acc, upstream.DesktopAutomationCreateEvent("codenexus 自动化"))
	}, nil)
}

// runSequentialModelChat completes the mini-program model task. The primary
// evidence is a mini-program conversation carrying the model fields; the
// fallback is the desktop model-activity report.
func runSequentialModelChat(ctx context.Context, e *Env) (string, error) {
	return miniEventTask(ctx, e, "Sequential_Tasks_5",
		func() error {
			conv := upstream.NewConversationID("mp-glm")
			return e.Tasks.ReportMiniProgramEvents(ctx, e.Acc, upstream.MiniModelChatSequence(conv, widgetModel, widgetModelName))
		},
		func() error {
			return e.Tasks.ReportActivity(ctx, e.Acc, widgetModel, widgetModelName)
		})
}

// runSequentialPlaybook completes the inspiration-feature task. The primary
// evidence is the desktop playbook group; the fallback is the mini-program
// shape.
func runSequentialPlaybook(ctx context.Context, e *Env) (string, error) {
	return miniEventTask(ctx, e, "Sequential_Tasks_7",
		func() error {
			conv := upstream.NewConversationID("pb")
			req := upstream.NewConversationID("pb-req")
			return e.Tasks.ReportDesktopEvents(ctx, e.Acc,
				upstream.DesktopPlaybookPromptSequence(conv, req, playbookCaseID, playbookCaseName)...)
		},
		func() error {
			return e.Tasks.ReportMiniProgramEvents(ctx, e.Acc,
				upstream.MiniPlaybookSequence(playbookCaseID, playbookCaseName)...)
		})
}
