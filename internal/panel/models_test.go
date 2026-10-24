package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// probeSource answers a real Chat call, so the probe endpoint can be exercised
// without an upstream. It records the request it was handed: the endpoint's job
// is to translate the console's form into a real request, and that translation
// is what these tests check.
type probeSource struct {
	upstream.Upstream
	content   string
	reasoning string
	finish    string
	tools     []upstream.ToolCall
	err       error
	perUID    map[string][]upstream.ModelSpec
	got       *upstream.ChatRequest
	account   string
}

func (s *probeSource) Source() upstream.Source { return upstream.SourceTraeCN }

func (s *probeSource) ListModels(_ context.Context, acc *account.Account) ([]upstream.ModelSpec, error) {
	if acc == nil {
		return nil, nil
	}
	return s.perUID[acc.UID], nil
}

func (s *probeSource) Chat(_ context.Context, acc *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	s.got = req
	if acc != nil {
		s.account = acc.UID
	}
	if s.err != nil {
		return nil, s.err
	}
	return &upstream.ChatResponse{
		Content:          s.content,
		ReasoningContent: s.reasoning,
		FinishReason:     s.finish,
		ToolCalls:        s.tools,
		Usage:            upstream.Usage{TotalTokens: 9},
		CreditsConsumed:  0.02,
	}, nil
}

func probeBody(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// A text probe that comes back with an answer is a pass, and the console must
// see what the model actually said so an unexpected answer is visible.
func TestModelTestPassesOnARealAnswer(t *testing.T) {
	src := &probeSource{content: "pong"}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, _ := newMaintConsole(t, &maintOnly{})
	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3", "mode": "text"}))
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["ok"] != true {
		t.Fatalf("ok = %v (%s)", out["ok"], out["error"])
	}
	if out["content"] != "pong" {
		t.Fatalf("content = %v", out["content"])
	}
	if src.got == nil || src.got.Model != "glm-5.3" {
		t.Fatalf("upstream request = %+v", src.got)
	}
	// The probe must stay cheap; a large cap would turn a connectivity check
	// into a billable generation.
	if src.got.MaxTokens == nil || *src.got.MaxTokens != 64 {
		t.Fatalf("max tokens = %v, want 64", src.got.MaxTokens)
	}
}

// A 200 with prose is not a pass in tool mode: the model ignored the tool,
// which is exactly the failure an operator is looking for.
func TestModelTestFailsWhenToolModeGetsNoToolCall(t *testing.T) {
	src := &probeSource{content: "Sure, pong!"}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, _ := newMaintConsole(t, &maintOnly{})
	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3", "mode": "tool"}))
	out := decode(t, &res)
	if out["ok"] != false {
		t.Fatalf("ok = %v, want false", out["ok"])
	}
	if out["error"] != "model answered without calling the tool" {
		t.Fatalf("error = %v", out["error"])
	}
	if src.got == nil || len(src.got.Tools) == 0 {
		t.Fatalf("tool definition not sent: %+v", src.got)
	}
}

// The upstream's own status and message are what an operator needs to see, so
// they are passed through rather than flattened into "failed".
func TestModelTestReportsUpstreamStatus(t *testing.T) {
	src := &probeSource{err: &upstream.Error{Kind: upstream.KindAuth, Status: 401, Message: "revoked"}}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, _ := newMaintConsole(t, &maintOnly{})
	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3"}))
	out := decode(t, &res)
	if out["ok"] != false {
		t.Fatalf("ok = %v", out["ok"])
	}
	if out["status"].(float64) != 401 {
		t.Fatalf("status = %v", out["status"])
	}
	if out["error"] == "" {
		t.Fatalf("error text dropped")
	}
}

// Pinning an account has to be honoured: testing "the first account" when the
// operator asked about a specific one would report a result for the wrong
// credential.
func TestModelTestUsesThePinnedAccount(t *testing.T) {
	src := &probeSource{content: "pong"}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, p := newMaintConsole(t, &maintOnly{})
	second := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Enabled: true}
	second.SetCredentials("tok2", "", "trae", 0)
	p.Add(second)

	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3", "account": "trae-cn:u-2"}))
	out := decode(t, &res)
	if out["account"] != "u-2" {
		t.Fatalf("account = %v, want u-2", out["account"])
	}
}

// With an empty pool the endpoint must answer, not crash: the console renders
// the reason in the results table.
func TestModelTestWithoutAccountsReportsWhy(t *testing.T) {
	src := &probeSource{content: "pong"}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, _, p := newConsole(t)
	for _, st := range p.Describe() {
		p.Remove(st.Key)
	}

	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3"}))
	if res.code != 200 {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	out := decode(t, &res)
	if out["ok"] != false || out["error"] != "no usable account" {
		t.Fatalf("out = %#v", out)
	}
}

// Without a pinned account the probe must find the account whose catalogue
// lists the model, not simply the first one in the pool: the pool mixes
// sources, and testing an unknown model against the wrong upstream would report
// a healthy model as broken.
func TestModelTestPicksTheAccountThatCarriesTheModel(t *testing.T) {
	src := &probeSource{
		content: "pong",
		perUID: map[string][]upstream.ModelSpec{
			"u-1": {{ID: "other-model"}},
			"u-2": {{ID: "glm-5.3"}},
		},
	}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, p := newMaintConsole(t, &maintOnly{})
	second := &account.Account{Source: account.SourceTraeCN, UID: "u-2", Enabled: true}
	second.SetCredentials("tok2", "", "trae", 0)
	p.Add(second)

	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3"}))
	out := decode(t, &res)
	if out["account"] != "u-2" {
		t.Fatalf("account = %v, want the account whose catalogue lists the model", out["account"])
	}
}

// The detail drawer shows the request beside the raw answer, so the probe must
// return the material whole: the full reply, the thinking, the tool the model
// invoked and why it stopped. A condensed line cannot answer "what did it
// actually say".
func TestModelTestReturnsRawDetailFields(t *testing.T) {
	src := &probeSource{
		content:   "pong",
		reasoning: "the user wants one word",
		finish:    "stop",
		tools: []upstream.ToolCall{{
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "relay_probe", Arguments: `{"token":"pong"}`},
		}},
	}
	upstream.Register(src)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceTraeCN) })

	c, _ := newMaintConsole(t, &maintOnly{})
	res := do(c, http.MethodPost, "/panel/api/models/test",
		probeBody(t, map[string]any{"model": "glm-5.3", "mode": "tool"}))
	out := decode(t, &res)
	if out["ok"] != true {
		t.Fatalf("ok = %v (%s)", out["ok"], out["error"])
	}
	if out["prompt"] == "" {
		t.Fatalf("prompt dropped: the drawer cannot show what was asked")
	}
	if out["reasoning"] != "the user wants one word" {
		t.Fatalf("reasoning = %v", out["reasoning"])
	}
	if out["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", out["finish_reason"])
	}
	calls, _ := out["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want the one invocation", out["tool_calls"])
	}
	first, _ := calls[0].(map[string]any)
	if first["name"] != "relay_probe" {
		t.Fatalf("tool name = %v", first["name"])
	}
	if first["arguments"] != `{"token":"pong"}` {
		t.Fatalf("tool arguments = %v, want them whole", first["arguments"])
	}
}
