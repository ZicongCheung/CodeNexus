package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

func TestTruncationDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		finish string
		reason string
		tools  bool
		want   bool
	}{
		{name: "length finish reason", text: "done.", finish: "length", want: true},
		{name: "max_tokens alias", text: "done.", finish: "max_tokens", want: true},
		{name: "context window alias", text: "done.", finish: "context_length_exceeded", want: true},
		{name: "clean stop", text: "All done.", finish: "stop", want: false},
		{name: "open code fence", text: "here:\n```go\nfunc x() {", finish: "stop", want: true},
		{name: "closed code fence", text: "here:\n```go\nfunc x() {}\n```\n", finish: "stop", want: false},
		{name: "unbalanced brackets", text: `{"a": [1, 2`, finish: "stop", want: true},
		{name: "trailing comma", text: "items are a, b and c,", finish: "stop", want: true},
		{name: "trailing colon", text: "the steps are:", finish: "stop", want: true},
		{name: "trailing dash", text: "the value is -", finish: "stop", want: true},
		{name: "plain sentence end", text: "The value is 3.", finish: "stop", want: false},
		{name: "tool call never continues", text: "working,", finish: "length", tools: true, want: false},
		{
			name:   "stopped after thinking",
			text:   "OK",
			finish: "stop",
			reason: strings.Repeat("thinking about the problem. ", 20),
			want:   true,
		},
		{
			name:   "short answer with little thinking is fine",
			text:   "OK",
			finish: "stop",
			reason: "brief thought",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksTruncated(tc.text, tc.finish, tc.reason, tc.tools); got != tc.want {
				t.Fatalf("looksTruncated(%q, %q) = %v, want %v", tc.text, tc.finish, got, tc.want)
			}
		})
	}

	// Reasoning-only output is the one case an empty turn is worth re-asking.
	if !looksTruncated("", "stop", "a long private trace", false) {
		t.Error("a reasoning-only turn should be continued")
	}
	if looksTruncated("", "stop", "", false) {
		t.Error("a completely empty turn belongs to the empty-response path")
	}
	if !looksReasoningOnly("", "trace", false) {
		t.Error("no visible text plus reasoning is reasoning-only")
	}
	if looksReasoningOnly("text", "trace", false) {
		t.Error("visible text means it is not reasoning-only")
	}
}

func TestContinuationSimilarity(t *testing.T) {
	if got := continuationSimilarity("abcdef", "abcdef"); got != 1 {
		t.Fatalf("identical = %v, want 1", got)
	}
	if got := continuationSimilarity("abcdef", "xyz"); got != 0 {
		t.Fatalf("unrelated = %v, want 0", got)
	}
	if got := continuationSimilarity("", "abc"); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
}

func TestContinuedMessagesReplayThePartialAnswer(t *testing.T) {
	base := []upstream.Message{{Role: "user", Content: "write a poem"}}
	out := continuedMessages(base, "Roses are red,", false)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0].Role != "user" || out[1].Role != "assistant" || out[2].Role != "user" {
		t.Fatalf("roles = %v", out)
	}
	if out[1].Content != "Roses are red," {
		t.Fatalf("the partial answer must be replayed as the assistant turn: %v", out[1])
	}
	if !strings.Contains(out[2].Content.(string), "继续") {
		t.Fatalf("the instruction must ask for the remainder: %v", out[2].Content)
	}
	if len(base) != 1 {
		t.Fatal("the caller's message slice must not be mutated")
	}

	ro := continuedMessages(base, "", true)
	if len(ro) != 2 {
		t.Fatalf("an empty partial adds no assistant turn, len = %d", len(ro))
	}
	if !strings.Contains(ro[1].Content.(string), "reasoning") {
		t.Fatalf("reasoning-only gets its own instruction: %v", ro[1].Content)
	}
}

// truncating answers twice: the first turn is cut off, the second finishes it.
type truncating struct {
	mu       sync.Mutex
	requests []*upstream.ChatRequest
	rounds   int
	streamed bool
}

func (u *truncating) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (u *truncating) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}
func (u *truncating) Refresh(context.Context, *account.Account) error { return nil }
func (u *truncating) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (u *truncating) next(req *upstream.ChatRequest) (content string, finish string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, req)
	u.rounds++
	if u.rounds == 1 {
		return "The first half", "length"
	}
	return " and the second half.", "stop"
}

func (u *truncating) seen() []*upstream.ChatRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*upstream.ChatRequest(nil), u.requests...)
}

func (u *truncating) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	content, finish := u.next(req)
	return &upstream.ChatResponse{Model: req.Model, Content: content, FinishReason: finish}, nil
}

func (u *truncating) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	content, finish := u.next(req)
	u.streamed = true
	if err := emit(upstream.StreamDelta{Content: content}); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{FinishReason: finish})
}

func TestNonStreamContinuesATruncatedAnswer(t *testing.T) {
	f := &truncating{}
	s := failoverServer(t, f, twoAccounts())

	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "The first half and the second half.") {
		t.Fatalf("the answer was not completed: %q", rec.Body.String())
	}
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("upstream turns = %d, want 2", len(reqs))
	}
	second := reqs[1].Messages
	if len(second) != 3 || second[1].Role != "assistant" {
		t.Fatalf("the continuation round must replay the partial answer: %v", second)
	}
}

func TestNonStreamStopsAtTheRoundLimit(t *testing.T) {
	f := &alwaysTruncated{}
	s := failoverServer(t, f, twoAccounts())
	s.SetAutoContinue(config.AutoContinueConfig{Enabled: true, MaxRounds: 2, TextThreshold: 200, SimilarityThreshold: 0.5})

	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.calls(); got != 3 {
		// One initial turn plus the two allowed extra rounds.
		t.Fatalf("upstream turns = %d, want 3", got)
	}
}

// alwaysTruncated never finishes, so the loop is only bounded by its cap.
type alwaysTruncated struct {
	mu sync.Mutex
	n  int
}

func (u *alwaysTruncated) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (u *alwaysTruncated) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}
func (u *alwaysTruncated) Refresh(context.Context, *account.Account) error { return nil }
func (u *alwaysTruncated) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}
func (u *alwaysTruncated) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}
func (u *alwaysTruncated) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	u.mu.Lock()
	u.n++
	round := u.n
	u.mu.Unlock()
	// Long, and it never ends on a completed sentence, so every round looks
	// truncated and the loop is only bounded by its cap. Each round differs,
	// so the repeat guard does not end it early.
	content := strings.Repeat("still working ", 30) + string(rune('a'+round)) + ","
	return &upstream.ChatResponse{Model: req.Model, Content: content, FinishReason: "stop"}, nil
}
func (u *alwaysTruncated) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	resp, err := u.Chat(context.Background(), nil, req)
	if err != nil {
		return err
	}
	if err := emit(upstream.StreamDelta{Content: resp.Content}); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{FinishReason: resp.FinishReason})
}

// Disabled means one upstream turn and nothing more.
func TestNonStreamContinueCanBeDisabled(t *testing.T) {
	f := &alwaysTruncated{}
	s := failoverServer(t, f, twoAccounts())
	s.SetAutoContinue(config.AutoContinueConfig{Enabled: false, MaxRounds: 3, TextThreshold: 200})

	postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if got := f.calls(); got != 1 {
		t.Fatalf("upstream turns = %d, want 1", got)
	}
}

func TestStreamContinuesIntoTheSameAnswer(t *testing.T) {
	f := &truncating{}
	s := failoverServer(t, f, twoAccounts())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model","stream":true}`))
	serveMakeBelieve(s, rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"The first half"`) || !strings.Contains(body, `" and the second half."`) {
		t.Fatalf("both halves must reach the client: %q", body)
	}
	// The interim finish reason must not reach the client, and the stream must
	// conclude exactly once.
	if strings.Count(body, "<<end>>") != 1 {
		t.Fatalf("want a single terminator: %q", body)
	}
	if len(f.seen()) != 2 {
		t.Fatalf("upstream turns = %d, want 2", len(f.seen()))
	}
}

// A turn that asked for a tool is never continued: the caller owns execution
// and has to receive the call as-is.
// A continuation that replays the opening it was cut on must not repeat it.
func TestContinuationDropsAReEmittedOpening(t *testing.T) {
	if got, ok := joinContinuation("```go", "```go\npackage main\n```"); !ok || got != "```go\npackage main\n```" {
		t.Fatalf("join = %q, ok = %v", got, ok)
	}
	if got, ok := joinContinuation("one two", " three four"); !ok || got != "one two three four" {
		t.Fatalf("disjoint rounds concatenate: %q, ok = %v", got, ok)
	}
	// A round that is nothing but the previous answer has no progress to show.
	if got, ok := joinContinuation("Roses are red,", "Roses are red,"); ok || got != "Roses are red," {
		t.Fatalf("a pure repeat must not advance: %q, ok = %v", got, ok)
	}
}

func TestOverlapHoldReleasesOnlyTheNewPart(t *testing.T) {
	h := &overlapHold{}
	h.start("```go")
	var out strings.Builder
	out.WriteString(h.feed("```go"))
	out.WriteString(h.feed("\npackage main\n```"))
	out.WriteString(h.finish())
	if got := out.String(); got != "\npackage main\n```" {
		t.Fatalf("streamed output = %q", got)
	}
	if h.repeated() {
		t.Fatal("a round that published content is not a repeat")
	}

	// The re-emitted opening alone, and nothing else, is a repeat.
	pure := &overlapHold{}
	pure.start("```go")
	if got := pure.feed("```go"); got != "" {
		t.Fatalf("the re-emitted opening must be withheld, got %q", got)
	}
	if got := pure.finish(); got != "" {
		t.Fatalf("a pure repeat publishes nothing, got %q", got)
	}
	if !pure.repeated() {
		t.Fatal("a round that only repeated the previous answer must be reported")
	}
}

// repeating replays the opening it was cut on, which used to reach the caller
// twice as "```go```go".
type repeating struct {
	mu     sync.Mutex
	rounds int
}

func (u *repeating) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (u *repeating) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}
func (u *repeating) Refresh(context.Context, *account.Account) error { return nil }
func (u *repeating) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}

func (u *repeating) next() (string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rounds++
	if u.rounds == 1 {
		return "```go", "length"
	}
	return "```go\npackage main\n```", "stop"
}

func (u *repeating) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	content, finish := u.next()
	return &upstream.ChatResponse{Model: req.Model, Content: content, FinishReason: finish}, nil
}

func (u *repeating) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	content, finish := u.next()
	for _, piece := range []string{content[:5], content[5:]} {
		if piece == "" {
			continue
		}
		if err := emit(upstream.StreamDelta{Content: piece}); err != nil {
			return err
		}
	}
	return emit(upstream.StreamDelta{FinishReason: finish})
}

func TestNonStreamDropsAReEmittedOpening(t *testing.T) {
	f := &repeating{}
	s := failoverServer(t, f, twoAccounts())

	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), "```go"); got != 1 {
		t.Fatalf("the fence must appear once, got %d: %s", got, rec.Body.String())
	}
}

func TestStreamDropsAReEmittedOpening(t *testing.T) {
	f := &repeating{}
	s := failoverServer(t, f, twoAccounts())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model","stream":true}`))
	serveMakeBelieve(s, rec, req)

	body := rec.Body.String()
	if got := strings.Count(body, "```go"); got != 1 {
		t.Fatalf("the fence must appear once, got %d: %q", got, body)
	}
	if !strings.Contains(body, `package main`) {
		t.Fatalf("the completed answer must reach the client: %q", body)
	}
}

func TestToolCallsAreNeverContinued(t *testing.T) {
	f := &toolCalling{}
	s := failoverServer(t, f, twoAccounts())

	rec := postChat(s, `{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := f.calls(); got != 1 {
		t.Fatalf("upstream turns = %d, want 1", got)
	}
}

type toolCalling struct {
	mu sync.Mutex
	n  int
}

func (u *toolCalling) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (u *toolCalling) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: upstream.SourceWorkBuddy}}, nil
}
func (u *toolCalling) Refresh(context.Context, *account.Account) error { return nil }
func (u *toolCalling) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}
func (u *toolCalling) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}
func (u *toolCalling) Chat(_ context.Context, _ *account.Account, req *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	u.mu.Lock()
	u.n++
	u.mu.Unlock()
	// A length finish reason would normally trigger a continuation, but the
	// turn carries a tool call so it must not.
	call := upstream.ToolCall{ID: "call-1", Type: "function"}
	call.Function.Name = "read_file"
	call.Function.Arguments = `{"path":"a"}`
	return &upstream.ChatResponse{
		Model:        req.Model,
		Content:      "let me check,",
		FinishReason: "length",
		ToolCalls:    []upstream.ToolCall{call},
	}, nil
}
func (u *toolCalling) ChatStream(_ context.Context, _ *account.Account, req *upstream.ChatRequest, emit upstream.EmitFunc) error {
	resp, _ := u.Chat(context.Background(), nil, req)
	if err := emit(upstream.StreamDelta{ToolCalls: resp.ToolCalls}); err != nil {
		return err
	}
	return emit(upstream.StreamDelta{FinishReason: resp.FinishReason})
}

// The ingress must work end to end: a TraeWork body posted to the ordinary
// chat URL is recognized and answered in TraeWork's own frames, not OpenAI's.
func TestTraeWorkIngressAnswersInItsOwnDialect(t *testing.T) {
	f := &truncating{}
	s := failoverServer(t, f, twoAccounts())
	s.SetAutoContinue(config.AutoContinueConfig{Enabled: false})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"data":{"is_custom_model":true,"config_name":"fake-model","stream":true,"messages":[{"role":"user","content":"hi"}]}}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: output", "event: done", `"response":"The first half"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %q, want it to contain %q", body, want)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("the OpenAI terminator leaked into the TraeWork dialect: %q", body)
	}
	if got := f.seen()[0].Model; got != "fake-model" {
		t.Fatalf("the config name must drive dispatch, got %q", got)
	}
}

// The connectivity probe must answer without credentials and must not present
// the relay as a preset model, or the client silently reverts to its native
// provider route.
func TestTraeWorkConnectivityIsPermissive(t *testing.T) {
	s := failoverServer(t, &truncating{}, twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/custom_model/connectivity",
		strings.NewReader(`{"data":{"config_name":"fake-model"}}`))
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	model, _ := payload["model"].(map[string]any)
	if model["is_preset"] != false {
		t.Fatalf("is_preset = %v, want false so the client keeps using the relay", model["is_preset"])
	}
	if payload["success"] != true {
		t.Fatalf("success = %v", payload["success"])
	}
}
