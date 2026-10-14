package upstream

import (
	"testing"
	"time"
)

func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	want := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, code := range want {
		if events[i]["eventCode"] != code {
			t.Errorf("event %d = %v, want %s", i, events[i]["eventCode"], code)
		}
	}
	if events[0]["conversationId"] != "conv-1" || events[0]["messageId"] != "msg-1" {
		t.Errorf("first event not joined to the conversation: %v", events[0])
	}
	// The chain only counts when the response is marked successful.
	if events[3]["isSuccessful"] != true {
		t.Errorf("chat_message_response must be successful: %v", events[3])
	}
	if events[2]["rootRequestId"] != "req-1" {
		t.Errorf("request chain not joined to the request id: %v", events[2])
	}
}

func TestMiniChatSequenceActivityID(t *testing.T) {
	plain := MiniChatSequence("conv", "")
	if _, ok := plain["activityId"]; ok {
		t.Errorf("plain conversation event must not carry an activity id")
	}
	season := MiniChatSequence("conv", "school_open_day_2026")
	if season["activityId"] != "school_open_day_2026" {
		t.Errorf("season event missing activity id: %v", season)
	}
	if plain["conversationId"] != "conv" || season["messageId"] == "" {
		t.Errorf("conversation not joined: %v / %v", plain, season)
	}
	// Each event must carry a fresh request id so the upstream does not dedupe.
	other := MiniChatSequence("conv", "")
	if plain["rootRequestId"] == other["rootRequestId"] {
		t.Errorf("two events share a request id: %v", plain["rootRequestId"])
	}
}

func TestMiniModelChatSequenceCarriesModel(t *testing.T) {
	ev := MiniModelChatSequence("conv", "glm-5.2", "GLM-5.2")
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Fatalf("model fields missing: %v", ev)
	}
	if _, ok := ev["activityId"]; ok {
		t.Errorf("model event must not carry an activity id")
	}
}

func TestExpertSequences(t *testing.T) {
	ex := MarketExpert{ID: "ex_abc", Type: "agent", Name: "专家", Title: "职称"}
	summon := DesktopExpertSummonSequence(ex)
	if len(summon) != 3 {
		t.Fatalf("summon chain = %d events, want 3", len(summon))
	}
	if summon[1]["id"] != "ex_abc" || summon[1]["mode"] != "LOCAL" {
		t.Errorf("summon click payload: %v", summon[1])
	}
	if summon[2]["eventCode"] != "expert_summoned" {
		t.Errorf("last summon event: %v", summon[2])
	}

	use := DesktopExpertActualUseEvent(ex, "conv", "0123456789abcdef0123456789abcdef")
	if use["mode"] != "craft" {
		t.Errorf("generic expert use must be craft mode: %v", use["mode"])
	}
	if use["conversationId"] != "conv" {
		t.Errorf("usage evidence not joined to the conversation")
	}
	msg, _ := use["messageId"].(string)
	if msg != "msg-89abcdef" {
		t.Errorf("message id = %q, want the last 8 of the request id", msg)
	}
	// The category defaults when the marketplace omits one.
	if use["type"] != "expert-all" {
		t.Errorf("category fallback = %v", use["type"])
	}

	local := DesktopExpertActualUseLocal(ex, "conv", "0123456789abcdef0123456789abcdef")
	if local["mode"] != "LOCAL" {
		t.Errorf("lightweight expert use must be LOCAL mode: %v", local["mode"])
	}
}

func TestTemplateAndPlaybookSequencesJoinChatChain(t *testing.T) {
	tpl := DesktopTemplateUseSequence("conv", "req", "3", "竞品分析")
	if len(tpl) != 8 {
		t.Fatalf("template group = %d events, want 8 (6 chat + 2 template)", len(tpl))
	}
	if tpl[6]["eventCode"] != "agent_task_created_with_template" || tpl[7]["eventCode"] != "template_used" {
		t.Errorf("template tail: %v / %v", tpl[6], tpl[7])
	}

	pb := DesktopPlaybookPromptSequence("conv", "req", "case-1", "案例")
	if len(pb) != 9 {
		t.Fatalf("playbook group = %d events, want 9", len(pb))
	}
	if pb[8]["eventCode"] != "playbook_prompt_send" || pb[8]["conversationId"] != "conv" {
		t.Errorf("playbook send not joined: %v", pb[8])
	}

	canvas := DesktopDesignCanvasSequence("conv", "req")
	if len(canvas) != 8 {
		t.Fatalf("canvas group = %d events, want 8", len(canvas))
	}
	if canvas[7]["eventCode"] != "wbx_design_canvas_open" {
		t.Errorf("canvas tail: %v", canvas[7])
	}
}

func TestBuddyAppSequenceIsSelfContained(t *testing.T) {
	events := DesktopBuddyAppSequence("cb_x", "应用")
	if len(events) != 5 {
		t.Fatalf("buddy app chain = %d events, want 5", len(events))
	}
	for i, ev := range events {
		if ev["buddyId"] != "cb_x" || ev["mode"] != "LOCAL" {
			t.Errorf("event %d not scoped to the app: %v", i, ev)
		}
	}
}

func TestInNightWindow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	cases := map[int]bool{23: true, 0: true, 7: true, 8: false, 12: false, 22: false}
	for hour, want := range cases {
		got := InNightWindow(time.Date(2026, 10, 15, hour, 0, 0, 0, loc))
		if got != want {
			t.Errorf("hour %d: InNightWindow = %v, want %v", hour, got, want)
		}
	}
}

func TestNewConversationIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewConversationID("test")
		if seen[id] {
			t.Fatalf("duplicate conversation id %q", id)
		}
		seen[id] = true
	}
}
