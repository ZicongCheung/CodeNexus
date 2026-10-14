package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// This file holds the pure payload builders for the client telemetry chains
// that light WorkBuddy's growth tasks. Field names and shapes are part of the
// upstream contract observed on the wire, so they are written literally; the
// values that identify a device, a session or a request are supplied by the
// adapter when it reports the batch.
//
// The builders are deliberately free of any HTTP concern so both the adapter
// and the growth executors can share them without an import cycle.

// MarketExpert is one entry of the platform's expert marketplace. The id must
// be a real marketplace id: the upstream ignores the "expert used" evidence
// when the id is invented.
type MarketExpert struct {
	ID       string
	Type     string
	Name     string
	Title    string
	Version  string
	Category string
}

// AppearanceAppliedEvent returns the event a client emits when an appearance
// theme takes effect. Applying the theme alone is not credited; this event is
// the evidence.
func AppearanceAppliedEvent(resourceKey string) map[string]any {
	return map[string]any{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": resourceKey, "vipLevel": 0, "series": "", "type": "unknown",
	}
}

// InNightWindow reports whether t falls inside the late-night counting window
// (23:00–08:00) that the night-owl task keys on. The upstream evaluates the
// window in the client's local time.
func InNightWindow(t time.Time) bool {
	h := t.Hour()
	return h >= 23 || h < 8
}

// sequenceToken returns a short random token used to make conversation and
// request identifiers unique within a batch.
func sequenceToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewConversationID returns a fresh conversation identifier for a synthesized
// telemetry chain, scoped by kind so different task families never collide.
func NewConversationID(kind string) string {
	return fmt.Sprintf("codenexus-%s-%d-%s", kind, time.Now().UnixMilli(), sequenceToken())
}

// DesktopChatSequence builds the six-event "successful desktop conversation"
// chain: agent_task_created → chat_message_send → chat_request_send →
// chat_message_response → chat_message_status → chat_request_response.
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []map[string]any {
	mk := func(code string, extra map[string]any) map[string]any {
		ev := map[string]any{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []map[string]any{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// DesktopBuddyAppSequence builds the five-event "entered a buddy app" chain:
// discover → show → enter_click → auth_confirm → bindaccount_skip. One batch
// satisfies both "entered any app" and "entered the named app" tasks.
func DesktopBuddyAppSequence(buddyID, buddyName string) []map[string]any {
	mk := func(code string, extra map[string]any) map[string]any {
		ev := map[string]any{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []map[string]any{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent builds the "scheduled task created" event.
func DesktopAutomationCreateEvent(name string) map[string]any {
	return map[string]any{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// DesktopTemplateUseSequence builds one "created a task from a template" group:
// a full chat chain joined with agent_task_created_with_template + template_used.
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		map[string]any{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		map[string]any{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// DesktopPlaybookPromptSequence builds the "sent the prompt of an inspiration
// case" group: a chat chain joined with the cta click and the dialog send.
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	withPayload := func(code string, extra map[string]any) map[string]any {
		m := map[string]any{"eventCode": code}
		for k, v := range extra {
			m[k] = v
		}
		for k, v := range payload {
			m[k] = v
		}
		return m
	}
	return append(events,
		map[string]any{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		withPayload("playbook_cta_click", map[string]any{"source": "discover", "position": 0}),
		withPayload("playbook_prompt_send", map[string]any{"conversationId": conversationID, "requestId": requestID}),
	)
}

// DesktopDesignCanvasSequence builds the "creative canvas created and opened"
// group emitted by the design tool, joined with a chat chain.
func DesktopDesignCanvasSequence(conversationID, requestID string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	tail := requestID
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return append(events,
		map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		map[string]any{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "canvas-" + tail,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// expertCategory picks the first category of an expert, defaulting to the
// catch-all bucket the client uses when the marketplace omits one.
func expertCategory(e MarketExpert) string {
	if e.Category != "" {
		return e.Category
	}
	return "expert-all"
}

// expertVersion defaults an expert's version so the payload always carries one.
func expertVersion(e MarketExpert) string {
	if e.Version != "" {
		return e.Version
	}
	return "1.0.0"
}

// DesktopExpertSummonSequence builds the "summoned an expert" chain: a page
// element click, the summon click and the summoned confirmation.
func DesktopExpertSummonSequence(e MarketExpert) []map[string]any {
	cat := expertCategory(e)
	ver := expertVersion(e)
	return []map[string]any{
		{
			"eventCode": "web_element_click", "source": e.ID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ID, "name": e.Name,
			"expertTitle": e.Title, "type": "expert-all", "position": 0,
			"expertType": e.Type, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ID, "name": e.Name,
			"expertTitle": e.Title, "type": "expert-all",
		},
	}
}

// desktopExpertActualUse builds the shared "actually used an expert" payload.
// requestID must be the server-issued id from a real conversation.
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) map[string]any {
	tail := requestID
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return map[string]any{
		"eventCode": "expert_actual_use",
		"id":        e.ID, "name": e.Name, "expertTitle": e.Title,
		"type": expertCategory(e), "expertType": e.Type, "source": "builtin", "version": expertVersion(e),
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + tail,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// DesktopExpertActualUseEvent returns the craft-mode "expert actually used"
// event, the evidence for the expert-usage tasks.
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) map[string]any {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal returns the LOCAL-mode variant required by the
// lightweight-cloud expert task.
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) map[string]any {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// MiniChatSequence builds one mini-program conversation-request event. The
// optional activityID ties the event to a seasonal campaign; an empty value
// omits the field entirely, which is what the plain conversation task expects.
func MiniChatSequence(conversationID, activityID string) map[string]any {
	rid := "codenexus-" + sequenceToken()
	tail := rid
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	ev := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + tail,
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
	if activityID != "" {
		ev["activityId"] = activityID
	}
	return ev
}

// MiniModelChatSequence is a mini-program conversation event carrying the model
// fields, which is what the "use a specific model" task keys on.
func MiniModelChatSequence(conversationID, modelID, modelName string) map[string]any {
	ev := MiniChatSequence(conversationID, "")
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniExpertUseEvent builds the mini-program "expert actually used" evidence.
// Unlike the desktop variant it carries neither conversation nor activity id,
// and it pins the mini-program client version.
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniPlaybookSequence builds the mini-program inspiration-case group: the cta
// click and the dialog send.
func MiniPlaybookSequence(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	cta := map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": "2.2.8",
	}
	for k, v := range base {
		cta[k] = v
	}
	send := map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": "codenexus-mp-pb-" + sequenceToken(),
		"extVersion":     "2.2.8",
	}
	for k, v := range base {
		send[k] = v
	}
	return []map[string]any{cta, send}
}
