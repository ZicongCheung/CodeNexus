package traecn

import (
	"context"
	"net/http"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// catalogueBody is a model list advertising a thinking-capable, max-capable
// agent-tier entry plus a bare work-tier sibling.
func catalogueBody(maxMode bool, options string) string {
	return `{"code":0,"data":{"list":[` +
		`{"function":"solo_agent_remote","models":[{"name":"glm-5.2","config_name":"glm-5.2",` +
		`"max_mode":` + boolJSON(maxMode) + `,"context_window_size":{"default":200000,"max":[1000000]},` +
		`"reasoning_effort_config":{"support_thinking":true,"options":` + options + `}}]},` +
		`{"function":"solo_work_remote","models":[{"name":"glm-5.2","config_name":"glm-5.2","max_mode":false}]}` +
		`]}}`
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func initialMessage(t *testing.T, s *stub) map[string]any {
	t.Helper()
	req := s.saw("/api/remote/v1/chat_sessions")
	if req == nil {
		t.Fatal("no session create request was made")
	}
	initial, ok := req.Body["initial_message"].(map[string]any)
	if !ok {
		t.Fatalf("initial_message missing in %v", req.Body)
	}
	return initial
}

// numEq compares numbers across the int/float64 boundary, because a value that
// went through JSON comes back as float64 while the builder produces ints.
func numEq(v any, want int) bool {
	n, ok := asInt(v)
	return ok && n == want
}

func TestEffortNormalizeAndClamp(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"low", effortLight}, {"LIGHT", effortLight}, {"minimal", effortLight},
		{"medium", effortHigh}, {"high", effortHigh}, {"default", effortHigh},
		{"xhigh", effortExtra}, {"extra-high", effortExtra}, {"max", effortExtra},
		{"nonsense", ""}, {nil, ""},
	} {
		if got := normalizeEffort(tc.in); got != tc.want {
			t.Errorf("normalizeEffort(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// A budget maps onto the same three levels.
	if got := effortFromBudget(float64(1000)); got != effortLight {
		t.Errorf("budget 1000 = %q, want light", got)
	}
	if got := effortFromBudget(float64(8000)); got != effortHigh {
		t.Errorf("budget 8000 = %q, want high", got)
	}
	if got := effortFromBudget(float64(20000)); got != effortExtra {
		t.Errorf("budget 20000 = %q, want extra_high", got)
	}

	supported := []string{effortLight, effortHigh}
	// Clamping never exceeds the requested level and never returns nothing
	// when the model supports something.
	if got := clampEffort(effortExtra, supported); got != effortHigh {
		t.Errorf("extra_high clamped to %q, want high", got)
	}
	if got := clampEffort(effortLight, supported); got != effortLight {
		t.Errorf("light clamped to %q, want light", got)
	}
	if got := clampEffort(effortHigh, []string{effortExtra}); got != effortExtra {
		t.Errorf("unsupported-low should fall up to the only level, got %q", got)
	}
	if got := clampEffort(effortHigh, nil); got != "" {
		t.Errorf("no support must yield no level, got %q", got)
	}
}

func TestApplyEffortUsesTheKeyTheEntryDeclares(t *testing.T) {
	raw := map[string]any{"reasoning_effort": "high"}
	// An entry that already carries the native key gets it updated and the
	// alias removed, so the server never has to choose between two values.
	entry := map[string]any{
		"reasoning_effort_level": "light",
		"reasoning_effort":       "stale",
		"reasoning_effort_config": map[string]any{
			"support_thinking": true, "options": []any{"light", "high"},
		},
	}
	out, level := applyEffort(entry, raw)
	if level != effortHigh {
		t.Fatalf("level = %q, want high", level)
	}
	if out["reasoning_effort_level"] != effortHigh {
		t.Errorf("reasoning_effort_level = %v", out["reasoning_effort_level"])
	}
	if _, ok := out["reasoning_effort"]; ok {
		t.Error("the stale alias must be dropped when the native key exists")
	}
	if entry["reasoning_effort_level"] != "light" {
		t.Error("applyEffort must not mutate the catalogue entry")
	}

	plain := map[string]any{
		"reasoning_effort_config": map[string]any{
			"support_thinking": true, "options": []any{"light", "extra_high"},
		},
	}
	out, level = applyEffort(plain, map[string]any{"reasoning": map[string]any{"effort": "xhigh"}})
	if level != effortExtra || out["reasoning_effort"] != effortExtra {
		t.Fatalf("out = %v level = %q, want extra_high", out, level)
	}

	// Without a thinking-capable config nothing is written.
	if _, level := applyEffort(map[string]any{}, raw); level != "" {
		t.Errorf("a model without thinking support must not get a level, got %q", level)
	}
}

func TestMaxModeNeedsTheAccountFlag(t *testing.T) {
	cfg := MaxModeConfig{Enabled: true}
	capable := map[string]any{"max_mode": true}
	plain := map[string]any{"max_mode": false}

	if !maxModeApplies(cfg, remoteAgent, "glm-5.2", capable) {
		t.Error("an enabled request on a flagged agent model should apply")
	}
	if maxModeApplies(cfg, remoteAgent, "glm-5.2", plain) {
		t.Error("a model the account does not flag must never get max mode")
	}
	if maxModeApplies(cfg, remoteAgentWork, "glm-5.2", capable) {
		t.Error("the work tier has no max profile")
	}
	if maxModeApplies(MaxModeConfig{Enabled: false}, remoteAgent, "glm-5.2", capable) {
		t.Error("disabled must not apply")
	}
	if maxModeApplies(MaxModeConfig{Enabled: true, Models: []string{"other"}}, remoteAgent, "glm-5.2", capable) {
		t.Error("an allow-list that excludes the model must not apply")
	}
	if !maxModeApplies(MaxModeConfig{Enabled: true, Models: []string{"glm-5.2"}}, remoteAgent, "glm-5.2", capable) {
		t.Error("an allow-list that includes the model should apply")
	}
}

func TestEnrichMaxModelCarriesTheWholeProfile(t *testing.T) {
	entry := map[string]any{
		"config_name":       "glm-5.2",
		"max_mode":          true,
		"prompt_max_tokens": float64(700000),
		"context_window_size": map[string]any{
			"default": float64(200000), "max": []any{float64(1000000)},
		},
	}
	out := enrichMaxModel(entry)
	if out["max_mode"] != true {
		t.Error("the enriched entry must be marked max")
	}
	if !numEq(out["prompt_max_tokens"], 700000) {
		t.Errorf("an advertised ceiling must be preserved, got %v", out["prompt_max_tokens"])
	}
	if !numEq(out["max_tokens"], maxModeOutputTokens) {
		t.Errorf("output ceiling = %v, want the documented default", out["max_tokens"])
	}
	cws, _ := out["context_window_size"].(map[string]any)
	if !numEq(cws["default"], 200000) {
		t.Errorf("dev context = %v", cws["default"])
	}
	features, _ := out["features"].(map[string]any)
	cw, _ := features["context_windows"].(map[string]any)
	data, _ := cw["data"].(map[string]any)
	if cw["enable"] != true || !numEq(data["max_context"], 1000000) {
		t.Errorf("features.context_windows = %v", features["context_windows"])
	}
	fields := maxModeWireFields(out)
	if fields["model_selection_strategy"] != "max" || !numEq(fields["mode_type"], maxModeType) {
		t.Errorf("wire fields = %v", fields)
	}
	if !numEq(fields["context_window_size"], 1000000) {
		t.Errorf("wire context = %v", fields["context_window_size"])
	}
	auto, _ := fields["model_auto_selection"].(map[string]any)
	if auto["strategy"] != "max" {
		t.Errorf("model_auto_selection = %v", fields["model_auto_selection"])
	}
}

// A catalogue that is not reachable must leave the request untouched: the
// gateway cannot verify the entitlement, so it does not claim it.
func TestCatalogueFailureLeavesTheRequestAlone(t *testing.T) {
	s := newStub(t)
	s.catalogStatus = http.StatusInternalServerError
	s.events = remoteEvents
	c := s.client(Config{MaxMode: MaxModeConfig{Enabled: true}})

	if _, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Raw:      map[string]any{"reasoning_effort": "high"},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	initial := initialMessage(t, s)
	if _, ok := initial["custom_model"]; ok {
		t.Error("no catalogue means no model profile may be attached")
	}
	if initial["model_selection_strategy"] != "manual" {
		t.Errorf("strategy = %v, want the untouched manual default", initial["model_selection_strategy"])
	}
}

func TestCatalogueAppliesClampedEffort(t *testing.T) {
	s := newStub(t)
	s.catalogStatus = http.StatusOK
	s.catalogBody = catalogueBody(false, `["light","high"]`)
	s.events = remoteEvents
	c := s.client(Config{})

	if _, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Raw:      map[string]any{"reasoning_effort": "xhigh"},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	initial := initialMessage(t, s)
	model, ok := initial["custom_model"].(map[string]any)
	if !ok {
		t.Fatalf("custom_model missing: %v", initial)
	}
	if model["reasoning_effort"] != effortHigh {
		t.Errorf("reasoning_effort = %v, want the clamped high", model["reasoning_effort"])
	}
	if initial["model_is_preset"] != true || initial["model_provider"] != "" {
		t.Errorf("manual companions = %v", initial)
	}
	if initial["model_selection_strategy"] != "manual" {
		t.Errorf("a plain effort request must stay manual, got %v", initial["model_selection_strategy"])
	}
}

func TestCatalogueAppliesMaxMode(t *testing.T) {
	s := newStub(t)
	s.catalogStatus = http.StatusOK
	s.catalogBody = catalogueBody(true, `["light","high","extra_high"]`)
	s.events = remoteEvents
	c := s.client(Config{MaxMode: MaxModeConfig{Enabled: true}})

	if _, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Raw:      map[string]any{"reasoning_effort": "extra_high"},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	initial := initialMessage(t, s)
	if initial["model_selection_strategy"] != "max" {
		t.Errorf("strategy = %v, want max", initial["model_selection_strategy"])
	}
	if !numEq(initial["mode_type"], maxModeType) {
		t.Errorf("mode_type = %v", initial["mode_type"])
	}
	if !numEq(initial["context_window_size"], 1000000) {
		t.Errorf("context_window_size = %v, want 1000000", initial["context_window_size"])
	}
	model, _ := initial["custom_model"].(map[string]any)
	if !maxCapable(model) {
		t.Errorf("custom_model = %v, want an enriched max entry", model)
	}
	// Max and effort coexist: the strength rides on the enriched entry.
	if model["reasoning_effort"] != effortExtra {
		t.Errorf("reasoning_effort = %v, want extra_high", model["reasoning_effort"])
	}
}

// The work tier omits the thinking config, so the strength is clamped against
// the agent-tier entry of the same model.
func TestWorkTierBorrowsTheAgentTierEffortConfig(t *testing.T) {
	s := newStub(t)
	s.catalogStatus = http.StatusOK
	s.catalogBody = catalogueBody(false, `["light","high"]`)
	s.events = remoteEvents
	c := s.client(Config{UpstreamMode: "work-agent"})

	if _, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Raw:      map[string]any{"thinking": map[string]any{"budget_tokens": float64(20000)}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := s.saw("/api/remote/v1/chat_sessions").Body["mode"]; got != remoteModeWork {
		t.Fatalf("mode = %v, want the work tier", got)
	}
	model, _ := initialMessage(t, s)["custom_model"].(map[string]any)
	if model == nil || model["reasoning_effort"] != effortHigh {
		t.Fatalf("custom_model = %v, want a high strength clamped from the agent tier", model)
	}
}

// ── commercial block ────────────────────────────────────────────────────────

// Trae states its price in the cached model list, not in usage records: the
// multiplier the IDE prints as "积分消耗速度 x 倍" lives under
// features.consumption_rate, and the offer in force under activity_discount.
func TestApplyFeaturesReadsCommercialBlock(t *testing.T) {
	entry := map[string]any{
		"features": map[string]any{
			"consumption_rate": map[string]any{"enable": true, "data": map[string]any{"rate": 1.83}},
			"activity_discount": map[string]any{"data": map[string]any{"current": map[string]any{
				"discount_type":           "off_peak",
				"before_consumption_rate": 2.0,
				"consumption_rate":        1.5,
				"discount":                25,
			}}},
			"context_windows": map[string]any{"data": map[string]any{
				"dev_context": 200000, "max_context": 1000000,
			}},
		},
	}
	var spec upstream.ModelSpec
	applyFeatures(&spec, entry)

	if spec.Credits != "x2" {
		t.Fatalf("credits = %q, want the listed rate the discount applies to", spec.Credits)
	}
	if spec.PromoLabel != "闲时折扣" || spec.PromoCredits != "x1.5" {
		t.Fatalf("promo = %q/%q, want 闲时折扣 x1.5", spec.PromoLabel, spec.PromoCredits)
	}
	if spec.PromoFactor == nil || *spec.PromoFactor != 0.75 {
		t.Fatalf("promo factor = %v, want 0.75", spec.PromoFactor)
	}
	// The window a request actually gets is the dev one, not the ceiling.
	if got := contextLength(entry); got != 200000 {
		t.Fatalf("context = %d, want 200000", got)
	}
}

// A zero rate is a real offer and must not read as "no data".
func TestApplyFeaturesMarksFreePromotion(t *testing.T) {
	entry := map[string]any{"features": map[string]any{
		"consumption_rate":  map[string]any{"data": map[string]any{"rate": 0.0}},
		"activity_discount": map[string]any{"data": map[string]any{"current": map[string]any{"discount_type": "limited", "before_consumption_rate": 0.5, "consumption_rate": 0.0}}},
	}}
	var spec upstream.ModelSpec
	applyFeatures(&spec, entry)
	if spec.PromoLabel != "限时特惠" || spec.PromoCredits != "0x" {
		t.Fatalf("promo = %q/%q, want 限时特惠 0x", spec.PromoLabel, spec.PromoCredits)
	}
	if spec.PromoFactor == nil || *spec.PromoFactor != 0 {
		t.Fatalf("promo factor = %v, want 0", spec.PromoFactor)
	}
}

// No offer in force means no promo fields at all — the listed rate stands.
func TestApplyFeaturesLeavesPlainRatesAlone(t *testing.T) {
	entry := map[string]any{"features": map[string]any{
		"consumption_rate":  map[string]any{"data": map[string]any{"rate": 0.06}},
		"activity_discount": map[string]any{"data": map[string]any{"current": map[string]any{"discount_type": "none"}}},
	}}
	var spec upstream.ModelSpec
	applyFeatures(&spec, entry)
	if spec.Credits != "x0.06" {
		t.Fatalf("credits = %q", spec.Credits)
	}
	if spec.PromoLabel != "" || spec.PromoCredits != "" || spec.PromoFactor != nil {
		t.Fatalf("promo leaked for a plain rate: %q/%q/%v", spec.PromoLabel, spec.PromoCredits, spec.PromoFactor)
	}
}

func TestFormatRateTrimsTrailingZeros(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1.83, "x1.83"}, {0.2, "x0.2"}, {0.06, "x0.06"}, {2, "x2"}, {0, "x0"},
	} {
		if got := formatRate(tc.in); got != tc.want {
			t.Errorf("formatRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
