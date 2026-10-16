package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// handleTraeWorkChat serves the custom-model ingress on its dedicated paths.
func (s *Server) handleTraeWorkChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		traeWorkFail(w, errorString("method not allowed"), true, "auto", "", nil)
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		traeWorkFail(w, err, true, "auto", "", nil)
		return
	}
	s.serveTraeWork(w, r, body)
}

// handleTraeWorkConnectivity answers the probe the client runs before it will
// offer the relay as a custom model.
//
// The critical field is is_preset=false: a client that reads the advertised
// model as a preset silently switches back to its own native provider route,
// which looks exactly like "the relay does not work".
func (s *Server) handleTraeWorkConnectivity(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Method == http.MethodPost {
		if decoded, err := decodeBody(r); err == nil {
			body = decoded
		}
	}
	in, err := parseTraeWorkInbound(body, r.Header, r.URL.Path)
	if err != nil || in == nil {
		in = &traeWorkInbound{Model: "auto", ModelName: "auto"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(traeWorkConnectivity(in))
}

// serveTraeWork normalizes an already-read body and runs it through the shared
// core.
func (s *Server) serveTraeWork(w http.ResponseWriter, r *http.Request, body map[string]any) {
	in, err := parseTraeWorkInbound(body, r.Header, r.URL.Path)
	if err != nil {
		traeWorkFail(w, err, true, "auto", "", nil)
		return
	}
	if len(in.Messages) == 0 {
		traeWorkFail(w, errorString("messages or query is required"), true, in.Model, in.RequestID, nil)
		return
	}
	s.runChat(w, r, traeWorkTurn(in))
}

// traeWorkTurn maps the normalized request onto the neutral core.
func traeWorkTurn(in *traeWorkInbound) *turn {
	identity := traeWorkIdentity(in)
	req := &upstream.ChatRequest{
		Model:    in.Model,
		Messages: in.Messages,
		Stream:   in.Stream,
		Raw:      in.Options,
	}
	if tools, ok := in.Options["tools"].([]any); ok {
		req.Tools = tools
	}
	return &turn{
		Name:    "traework",
		Model:   in.Model,
		Stream:  in.Stream,
		Session: in.SessionID,
		Request: req,
		Result: func(w http.ResponseWriter, resp *upstream.ChatResponse) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(traeWorkCompletion(resp, in.Model, identity))
		},
		Open: func(w http.ResponseWriter) (responder, error) {
			return newTraeWorkResponder(w, in.Model, in.RequestID, identity)
		},
		Fail: func(w http.ResponseWriter, err error, client bool) {
			traeWorkFail(w, err, client, in.Model, in.RequestID, identity)
		},
	}
}

// traeWorkIdentity is the credential-free model identity echoed back so the
// client keeps its route choice. The values come from the request: the relay
// never invents a provider it did not receive.
func traeWorkIdentity(in *traeWorkInbound) map[string]string {
	identity := map[string]string{}
	if in.ConfigName != "" {
		identity["config_name"] = in.ConfigName
	}
	name := in.ModelName
	if name == "" {
		name = in.ConfigName
	}
	if name != "" {
		identity["model_name"] = name
	}
	if in.Provider != "" {
		identity["provider"] = in.Provider
	}
	if in.Display != "" {
		identity["display_name"] = in.Display
	}
	return identity
}

// traeWorkCompletion renders the non-streaming completion.
func traeWorkCompletion(resp *upstream.ChatResponse, model string, identity map[string]string) map[string]any {
	result := map[string]any{
		"response": resp.Content,
		"model":    model,
	}
	finish := resp.FinishReason
	if finish == "" {
		if len(resp.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	result["finish_reason"] = finish
	for k, v := range identity {
		if _, exists := result[k]; !exists {
			result[k] = v
		}
	}
	if resp.ReasoningContent != "" {
		result["reasoning_content"] = compactReasoning(resp.ReasoningContent)
	}
	if len(resp.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			// No stream index here: it exists only to group fragments.
			calls = append(calls, map[string]any{
				"id":    call.ID,
				"name":  call.Function.Name,
				"input": call.Function.Arguments,
			})
		}
		result["tool_calls"] = calls
		if len(calls) == 1 {
			params := calls[0]["input"]
			if s, _ := params.(string); strings.TrimSpace(s) == "" {
				params = "{}"
			}
			result["tool_call_info"] = map[string]any{
				"tool_call_id": calls[0]["id"],
				"name":         calls[0]["name"],
				"parameters":   params,
			}
		}
	}
	if u := resp.Usage; u.TotalTokens != 0 || u.PromptTokens != 0 || u.CompletionTokens != 0 {
		total := u.TotalTokens
		if total == 0 {
			total = u.PromptTokens + u.CompletionTokens
		}
		result["usage"] = map[string]any{
			"input_tokens":      u.PromptTokens,
			"output_tokens":     u.CompletionTokens,
			"total_tokens":      total,
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
		}
	}
	return result
}

// traeWorkFail renders a failure in the dialect's own envelope: the client
// reads `error.message` and never a bare status code.
func traeWorkFail(w http.ResponseWriter, err error, client bool, model, requestID string, identity map[string]string) {
	status := http.StatusBadGateway
	errType := "api_error"
	if client {
		status = http.StatusBadRequest
		errType = "invalid_request_error"
	}
	safe := map[string]any{"message": err.Error(), "type": errType}
	if requestID != "" {
		safe["request_id"] = requestID
	}
	payload := map[string]any{
		"response": "",
		"status":   "error",
		"model":    model,
		"error":    safe,
	}
	for k, v := range identity {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// traeWorkConnectivity builds the permissive model descriptor the client
// accepts as a custom base URL.
func traeWorkConnectivity(in *traeWorkInbound) map[string]any {
	configName := in.ConfigName
	if configName == "" {
		configName = "auto"
	}
	rawName := in.ModelName
	if rawName == "" {
		rawName = configName
	}
	display := in.Display
	if display == "" {
		display = configName
	}
	provider := in.Provider
	if provider == "" {
		provider = "trae-cn-relay"
	}
	modelItem := map[string]any{
		"id":           configName,
		"name":         configName,
		"model":        configName,
		"model_name":   rawName,
		"config_name":  configName,
		"display_name": display,
		"provider":     provider,
		// The flag that keeps the client pointed at this relay.
		"is_preset":          false,
		"is_custom_base_url": true,
		"custom_model_id":    nil,
		"config_source":      1,
		"context_window_size": map[string]any{
			"default": 200000,
			"max":     []any{1000000},
		},
		"prompt_max_tokens": 936000,
		"max_tokens":        64000,
		"features": map[string]any{
			"reasoning":  map[string]any{"enable": true},
			"tool_calls": map[string]any{"enable": true},
			"context_windows": map[string]any{
				"enable": true,
				"data": map[string]any{
					"dev_context": 200000,
					"max_context": 1000000,
				},
			},
		},
	}
	return map[string]any{
		"success":      true,
		"ok":           true,
		"code":         0,
		"message":      "ok",
		"provider":     provider,
		"config_name":  configName,
		"model_name":   rawName,
		"display_name": display,
		"model":        modelItem,
		"models":       []any{modelItem},
		"data": map[string]any{
			"success": true,
			"code":    0,
			"model":   modelItem,
			"models":  []any{modelItem},
		},
	}
}

// errorString is a tiny error type for the shapes built inside a handler.
type errorString string

func (e errorString) Error() string { return string(e) }
