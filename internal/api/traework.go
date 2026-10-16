package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// TraeWork's "custom model" ingress.
//
// The desktop client can be pointed at an OpenAI-compatible base URL as a
// custom model. It then speaks its own raw dialect: a differently shaped
// request body, and — more importantly — a private streaming contract built on
// cumulative `event: output` frames. Answering it with plain OpenAI SSE would
// not fail loudly; the client would silently fall back to its native provider
// or stop rendering mid-answer, which is worse than not supporting it at all.
//
// So this file is a wire format, not a second pipeline: the request is
// normalized into the same neutral ChatRequest every other entry point builds,
// the shared chat core runs it (account selection, failover, settlement and
// logging unchanged), and the result is rendered back into TraeWork's frames.
// Tools are never executed here — the desktop client owns the tool host and
// only needs the call relayed.

// ── request detection ───────────────────────────────────────────────────────

// traeWorkRawAliases are the paths the client posts its raw chat to. The
// ordinary /v1/chat/completions URL is handled by detection instead, because a
// custom model may use it and hijacking every call would break real clients.
var traeWorkRawAliases = []string{
	"/api/ide/v2/llm_raw_chat",
	"/api/agent/v3/llm_raw_chat_custom_model",
	"/api/agent/v3/custom_model_proxy/chat",
	"/v1/traework/chat",
}

// traeWorkConnectivityAliases are the paths the client probes before it will
// offer the model at all.
var traeWorkConnectivityAliases = []string{
	"/api/agent/v3/custom_model_connectivity_check",
	"/api/ide/v1/custom_model_connectivity_check",
	"/v1/custom_model/connectivity",
}

// normalizedPath lowercases and canonicalizes a request path for comparison.
func normalizedPath(p string) string {
	p = "/" + strings.TrimLeft(strings.TrimSpace(p), "/")
	return strings.ToLower(strings.TrimRight(p, "/"))
}

func pathIn(list []string, path string) bool {
	want := normalizedPath(path)
	for _, item := range list {
		if normalizedPath(item) == want {
			return true
		}
	}
	return false
}

// isTraeWorkRawPath reports a dedicated raw-chat alias.
func isTraeWorkRawPath(path string) bool { return pathIn(traeWorkRawAliases, path) }

// isTraeWorkConnectivityPath reports the connectivity probe.
func isTraeWorkConnectivityPath(path string) bool { return pathIn(traeWorkConnectivityAliases, path) }

// looksLikeTraeWork decides whether a request belongs to this dialect.
//
// The test is deliberately three-layered (path, headers, body) because the
// client hides its user agent behind an Electron network layer on some builds
// and posts to the plain chat URL on others. Every body-level signal requires
// an actual custom-model identity, so ordinary OpenAI callers that merely
// happen to send agent metadata are left alone.
func looksLikeTraeWork(path string, h http.Header, body map[string]any) bool {
	lower := strings.ToLower(path)
	if isTraeWorkRawPath(path) || strings.Contains(lower, "custom_model_connectivity_check") {
		return true
	}
	blob := strings.ToLower(strings.Join([]string{
		h.Get("User-Agent"),
		h.Get("X-Trae-Client-Type"),
		h.Get("X-Trae-Client"),
		h.Get("X-Trae-Protocol"),
		h.Get("X-Bridge-Transport"),
		h.Get("X-Custom-Model"),
		h.Get("X-Trae-Work"),
	}, " "))
	for _, marker := range []string{"traework", "trae-work", "aha", "custom-model"} {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	// Native raw headers are a strong signal even when the user agent is
	// hidden by an Electron network layer.
	if h.Get("Extra") != "" && h.Get("X-App-Id") != "" && h.Get("X-Ide-Function") != "" {
		return true
	}
	for _, layer := range bodyLayers(body) {
		if truthyAny(layer["is_custom_model"], layer["isCustomModel"]) {
			return true
		}
		if cm, ok := firstMap(layer, "custom_model", "customModel"); ok && len(cm) > 0 {
			return true
		}
		if s, ok := layer["custom_model"].(string); ok && strings.TrimSpace(s) != "" {
			return true
		}
		if info, ok := firstMap(layer, "model_info", "modelInfo"); ok {
			if _, found := info["custom_model_id"]; found {
				return true
			}
			if _, found := info["is_custom_base_url"]; found {
				return true
			}
			if _, found := info["customModelId"]; found {
				return true
			}
			if _, found := info["isCustomBaseUrl"]; found {
				return true
			}
		}
	}
	return false
}

// ── inbound normalization ──────────────────────────────────────────────────

// nestedBodyKeys are the wrappers the client nests its payload inside; older
// and newer builds differ in which one they use.
var nestedBodyKeys = []string{"data", "params", "request", "payload", "body", "input"}

var traeWorkMessageKeys = []string{
	"messages", "chat_messages", "chatMessages", "message_history", "messageHistory", "history",
}

var traeWorkSessionKeys = []string{
	"session_id", "sessionId", "conversation_id", "conversationId", "chat_session_id",
	"chatSessionId", "agent_loop_id", "agentLoopId", "connect_session_id", "connectSessionId",
}

// traeWorkOptionKeys are the fields copied from the request into the neutral
// options. Copying the raw values is what lets a thinking strength or a tool
// protocol survive the translation.
var traeWorkOptionKeys = []string{
	"tools", "tool_choice", "toolChoice", "parallel_tool_calls", "parallelToolCalls",
	"thinking", "reasoning_effort", "reasoningEffort", "reasoning",
	"max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "response_format",
	"client_context", "clientContext", "workspace_context", "workspaceContext",
	"render_context", "renderContext", "environment_context", "environmentContext",
	"workspace_folder", "workspacePath", "agent_type", "agentType",
	"model_selection_strategy", "model_auto_selection", "custom_model", "customModel",
	"model_config_source", "model_is_preset", "model_provider", "provider",
	"config_source", "configSource",
}

// bodyLayers returns the nested payload maps, outer first.
func bodyLayers(body map[string]any) []map[string]any {
	if body == nil {
		return nil
	}
	var layers []map[string]any
	pending := []map[string]any{body}
	seen := map[uintptr]bool{}
	for len(pending) > 0 && len(layers) < 8 {
		current := pending[0]
		pending = pending[1:]
		key := mapKey(current)
		if seen[key] {
			continue
		}
		seen[key] = true
		layers = append(layers, current)
		for _, k := range nestedBodyKeys {
			if nested, ok := current[k].(map[string]any); ok {
				pending = append(pending, nested)
			}
		}
	}
	return layers
}

// mapKey is a stable identity for a map within one call, standing in for the
// reference's id()-based cycle guard.
func mapKey(m map[string]any) uintptr {
	if m == nil {
		return 0
	}
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	var h uintptr
	for _, c := range b {
		h = h*31 + uintptr(c)
	}
	return h
}

// firstLayerValue returns the first non-empty value, searching outer first.
func firstLayerValue(layers []map[string]any, keys ...string) any {
	for _, layer := range layers {
		for _, key := range keys {
			if v, ok := layer[key]; ok && !emptyValue(v) {
				return v
			}
		}
	}
	return nil
}

// deepestLayerValue returns the innermost non-empty value, which is what the
// model identity has to come from: an outer wrapper may carry a display alias
// while the inner payload carries the real config id.
func deepestLayerValue(layers []map[string]any, keys ...string) any {
	for i := len(layers) - 1; i >= 0; i-- {
		for _, key := range keys {
			if v, ok := layers[i][key]; ok && !emptyValue(v) {
				return v
			}
		}
	}
	return nil
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// customModelMapping finds the custom-model object, innermost first.
func customModelMapping(layers []map[string]any) map[string]any {
	for i := len(layers) - 1; i >= 0; i-- {
		if m, ok := firstMap(layers[i],
			"custom_model", "customModel", "model_info", "modelInfo", "model_config", "modelConfig"); ok {
			return m
		}
	}
	return nil
}

func firstMap(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if v, ok := m[key].(map[string]any); ok {
			return v, true
		}
	}
	return nil, false
}

func truthyAny(values ...any) bool {
	for _, v := range values {
		if truthyValue(v) {
			return true
		}
	}
	return false
}

func truthyValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		}
	case float64:
		return t != 0
	}
	return false
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// traeWorkInbound is the normalized request.
type traeWorkInbound struct {
	Messages   []upstream.Message
	Model      string
	Stream     bool
	SessionID  string
	RequestID  string
	Options    map[string]any
	ConfigName string
	ModelName  string
	Display    string
	Provider   string
}

// parseTraeWorkInbound turns a raw TraeWork body into the neutral request.
func parseTraeWorkInbound(body map[string]any, h http.Header, path string) (*traeWorkInbound, error) {
	layers := bodyLayers(body)
	custom := customModelMapping(layers)

	extra := map[string]any{}
	extraValue := firstLayerValue(layers, "Extra", "extra", "extra_info", "extraInfo")
	if emptyValue(extraValue) {
		extraValue = h.Get("Extra")
	}
	if !emptyValue(extraValue) {
		extra = parseRawExtra(anyString(extraValue))
	}

	messages := normalizeInboundMessages(firstLayerValue(layers, traeWorkMessageKeys...))
	if len(messages) == 0 {
		for _, key := range []string{"message_content", "messageContent", "query", "prompt", "text", "input_text", "content"} {
			candidate := firstLayerValue(layers, key)
			if emptyValue(candidate) {
				continue
			}
			if key == "query" || key == "content" {
				messages = queryToMessages(candidate)
			} else {
				messages = normalizeInboundMessages(candidate)
			}
			if len(messages) > 0 {
				break
			}
		}
	}

	// A raw request carries both the stable catalogue id (config_name) and the
	// provider-facing id (model_name). Dispatch must use the former: a value
	// like ``glm-5.2__dev`` is not a public model and would be remapped.
	modelValue := deepestLayerValue(layers, "config_name", "configName", "ai_model_name", "aiModelName")
	if emptyValue(modelValue) {
		modelValue = firstLayerValue(layers, "model")
	}
	if emptyValue(modelValue) && custom != nil {
		modelValue = firstNonEmpty(custom["config_name"], custom["configName"])
	}
	if emptyValue(modelValue) {
		modelValue = firstNonEmpty(extra["config_name"])
	}
	rawModelName := ""
	if custom != nil {
		rawModelName = anyString(firstNonEmpty(custom["raw_model_name"], custom["rawModelName"],
			custom["model_name"], custom["modelName"]))
	}
	if rawModelName == "" {
		rawModelName = anyString(deepestLayerValue(layers, "raw_model_name", "rawModelName", "model_name", "modelName"))
	}
	if emptyValue(modelValue) {
		modelValue = firstNonEmpty(rawModelName, extra["model_name"])
	}
	model := strings.TrimSpace(anyString(modelValue))
	if model == "" {
		model = "auto"
	}
	if rawModelName == "" {
		rawModelName = model
	}

	sessionID := strings.TrimSpace(anyString(firstLayerValue(layers, traeWorkSessionKeys...)))
	if sessionID == "" {
		sessionID = anyString(firstNonEmpty(extra["session_id"], extra["agent_loop_id"]))
	}
	if sessionID == "" {
		sessionID = firstNonEmptyString(h.Get("X-Session-Id"), h.Get("X-Conversation-Id"), h.Get("X-Chat-Session-Id"))
	}
	if sessionID == "" {
		sessionID = newHexRequestID()
	}
	requestID := strings.TrimSpace(anyString(firstLayerValue(layers, "request_id", "requestId", "x_request_id")))
	if requestID == "" {
		requestID = firstNonEmptyString(h.Get("X-Request-Id"), h.Get("X-Trae-Request-Id"))
	}
	if requestID == "" {
		requestID = sessionID
	}

	options := map[string]any{}
	// Inner payload values win, but every layer is copied so wrappers from
	// different client versions stay usable.
	for i := len(layers) - 1; i >= 0; i-- {
		for _, key := range traeWorkOptionKeys {
			if v, ok := layers[i][key]; ok && v != nil {
				if _, exists := options[key]; !exists {
					options[key] = v
				}
			}
		}
	}
	if custom != nil {
		options["custom_model"] = custom
	}
	options["session_id"] = sessionID
	options["_traework_custom_model"] = true

	streamValue := firstLayerValue(layers, "stream", "is_stream", "isStream")
	stream := true
	if streamValue != nil {
		switch t := streamValue.(type) {
		case bool:
			stream = t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "1", "true", "yes", "on":
				stream = true
			case "0", "false", "no", "off":
				stream = false
			}
		default:
			stream = truthyValue(streamValue)
		}
	}

	return &traeWorkInbound{
		Messages:   messages,
		Model:      model,
		Stream:     stream,
		SessionID:  sessionID,
		RequestID:  requestID,
		Options:    options,
		ConfigName: model,
		ModelName:  rawModelName,
		Display:    anyString(firstNonEmpty(customField(custom, "display_name", "displayName"), extra["display_name"])),
		Provider:   anyString(firstNonEmpty(customField(custom, "provider"), extra["provider"])),
	}, nil
}

func customField(m map[string]any, keys ...string) any {
	if m == nil {
		return nil
	}
	return firstNonEmpty(keysToValues(m, keys)...)
}

func keysToValues(m map[string]any, keys []string) []any {
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, m[key])
	}
	return out
}

func firstNonEmpty(values ...any) any {
	for _, v := range values {
		if !emptyValue(v) {
			return v
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizeInboundMessages accepts the several shapes the client uses for its
// history and produces neutral messages.
func normalizeInboundMessages(value any) []upstream.Message {
	switch t := value.(type) {
	case map[string]any:
		if nested, ok := t["messages"]; ok && !emptyValue(nested) {
			return normalizeInboundMessages(nested)
		}
		value = []any{t}
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []upstream.Message{{Role: "user", Content: t}}
	}
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []upstream.Message
	for _, item := range list {
		switch t := item.(type) {
		case string:
			out = append(out, upstream.Message{Role: "user", Content: t})
			continue
		case map[string]any:
			msg := upstream.Message{
				Role:    normalizeRendererRole(firstNonEmpty(t["role"], t["speaker"], t["role_type"])),
				Content: t["content"],
			}
			if emptyValue(msg.Content) {
				for _, key := range []string{"parts", "text", "message", "prompt", "value", "input"} {
					if candidate, ok := t[key]; ok && !emptyValue(candidate) {
						msg.Content = candidate
						break
					}
				}
			}
			if msg.Content == nil {
				continue
			}
			out = append(out, msg)
		}
	}
	return out
}

// normalizeRendererRole folds the client's numeric and alias roles onto the
// neutral vocabulary.
func normalizeRendererRole(value any) string {
	switch t := value.(type) {
	case bool:
		return "user"
	case float64:
		switch int(t) {
		case 0:
			return "system"
		case 2:
			return "assistant"
		case 3:
			return "tool"
		default:
			return "user"
		}
	}
	text := strings.ToLower(strings.TrimSpace(anyString(value)))
	switch text {
	case "", "1":
		return "user"
	case "0":
		return "system"
	case "2", "ai":
		return "assistant"
	case "3":
		return "tool"
	case "human":
		return "user"
	}
	switch text {
	case "system", "developer", "user", "assistant", "tool", "function":
		return text
	}
	return "user"
}

// queryToMessages converts the client's flattened query/content wrappers.
func queryToMessages(value any) []upstream.Message {
	switch t := value.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []upstream.Message{{Role: "user", Content: t}}
	case []any:
		var parts []any
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				parts = append(parts, item)
				continue
			}
			data := m
			if nested, ok := m["data"].(map[string]any); ok {
				data = nested
			}
			eventType := strings.ToLower(anyString(firstNonEmpty(m["type"], data["type"])))
			if eventType == "" {
				eventType = "text"
			}
			switch eventType {
			case "text", "input_text", "output_text":
				text := firstNonEmpty(data["content"], data["text"], data["value"])
				if !emptyValue(text) {
					parts = append(parts, map[string]any{"type": "text", "text": anyString(text)})
				}
			case "tool_use", "tool_result", "thinking", "image_url", "data":
				parts = append(parts, data)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return []upstream.Message{{Role: "user", Content: parts}}
	}
	return normalizeInboundMessages(value)
}

// parseRawExtra decodes the client's `Extra` header, which is base64 JSON on
// some builds and plain JSON on others. A malformed value is ignored: it only
// carries hints, never the request itself.
func parseRawExtra(value string) map[string]any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	candidates := []string{value}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		candidates = append(candidates, string(decoded))
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		candidates = append(candidates, string(decoded))
	}
	for _, candidate := range candidates {
		var out map[string]any
		if err := json.Unmarshal([]byte(candidate), &out); err == nil && len(out) > 0 {
			return out
		}
	}
	return nil
}
