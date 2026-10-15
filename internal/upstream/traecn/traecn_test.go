package traecn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// fakeToken builds a syntactically valid Cloud-IDE-JWT carrying an identity.
func fakeToken(uid string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"data":{"id":"` + uid + `"}}`))
	return header + "." + payload + "." + strings.Repeat("a", 96)
}

func testAccount() *account.Account {
	return &account.Account{
		Source:      account.SourceTraeCN,
		UID:         "user-1",
		Label:       "user-1",
		Enabled:     true,
		AccessToken: fakeToken("user-1"),
	}
}

// captured is one request the stub upstream saw.
type captured struct {
	Path    string
	Query   url.Values
	Method  string
	Headers http.Header
	Body    map[string]any
}

// stub is a fake upstream covering every host the adapter talks to.
type stub struct {
	*httptest.Server

	mu       sync.Mutex
	requests []captured

	sessionID string
	messageID string
	events    string

	agentStatus int
	agentBody   string

	exchangeToken  string
	exchangeBody   map[string]any
	sessionStatus  int
	eventsStatus   int
	recordExchange bool
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{
		sessionID:     "session-1",
		messageID:     "message-1",
		events:        "",
		agentStatus:   http.StatusOK,
		exchangeToken: fakeToken("exchanged-user"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/v1/chat_sessions", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		if s.sessionStatus != 0 {
			w.WriteHeader(s.sessionStatus)
			_, _ = w.Write([]byte(`{"code":4008,"message":"quota exceeded"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"chat_session_id":"` + s.sessionID + `","message_id":"` + s.messageID + `"}}`))
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		if s.eventsStatus != 0 {
			w.WriteHeader(s.eventsStatus)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(s.events))
	})
	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		if s.agentStatus != http.StatusOK {
			w.WriteHeader(s.agentStatus)
			_, _ = w.Write([]byte(`{"code":"500","message":"agent unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(s.agentBody))
	})
	mux.HandleFunc(exchangePath, func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		s.mu.Lock()
		s.recordExchange = true
		body := s.exchangeBody
		s.mu.Unlock()
		if body == nil {
			body = map[string]any{"Result": map[string]any{
				"Token":         s.exchangeToken,
				"RefreshToken":  "refresh-2",
				"TokenExpireAt": float64(1900000000000),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *stub) record(r *http.Request) {
	var body map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, captured{
		Path:    r.URL.Path,
		Query:   r.URL.Query(),
		Method:  r.Method,
		Headers: r.Header.Clone(),
		Body:    body,
	})
	s.mu.Unlock()
}

func (s *stub) saw(path string) *captured {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.requests {
		if s.requests[i].Path == path {
			return &s.requests[i]
		}
	}
	return nil
}

func (s *stub) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

func (s *stub) client(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = s.URL
	}
	if cfg.RemoteBaseURL == "" {
		cfg.RemoteBaseURL = s.URL + "/api/remote/v1"
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = s.URL
	}
	return New(cfg)
}

func collect() (upstream.EmitFunc, *[]upstream.StreamDelta) {
	var deltas []upstream.StreamDelta
	return func(d upstream.StreamDelta) error {
		deltas = append(deltas, d)
		return nil
	}, &deltas
}

// ── token helpers ────────────────────────────────────────────────────────────

func TestLooksLikeToken(t *testing.T) {
	if !LooksLikeToken(fakeToken("u")) {
		t.Fatal("a well-formed token must be accepted")
	}
	for _, bad := range []string{"", "short", "a.b", strings.Repeat("x", 200)} {
		if LooksLikeToken(bad) {
			t.Fatalf("malformed token accepted: %q", bad)
		}
	}
}

func TestTokenUserIDReadsNestedClaims(t *testing.T) {
	enc := func(claims string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(claims))
	}
	cases := []struct {
		name, payload, want string
	}{
		{"data.id", `{"data":{"id":"abc"}}`, "abc"},
		{"data.user_id", `{"data":{"user_id":"def"}}`, "def"},
		{"userId", `{"userId":"ghi"}`, "ghi"},
		{"sub", `{"sub":"jkl"}`, "jkl"},
		{"missing", `{"other":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := enc(`{"alg":"RS256"}`) + "." + enc(tc.payload) + "." + strings.Repeat("a", 96)
			if got := TokenUserID(tok); got != tc.want {
				t.Fatalf("uid = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTimestampShapes(t *testing.T) {
	want := int64(1900000000)
	for _, v := range []any{
		float64(1900000000),
		float64(1900000000000),
		"1900000000",
		"1900000000000",
	} {
		if got := parseTimestamp(v); got != want {
			t.Fatalf("parseTimestamp(%v) = %d, want %d", v, got, want)
		}
	}
	if got := parseTimestamp("2026-10-15T00:00:00Z"); got == 0 {
		t.Fatal("an RFC3339 timestamp must parse")
	}
	if got := parseTimestamp(nil); got != 0 {
		t.Fatalf("a missing expiry must be 0 (unknown), got %d", got)
	}
	if got := parseTimestamp("garbage"); got != 0 {
		t.Fatalf("unparseable input must be 0, got %d", got)
	}
}

func TestGeneratedIdentifiers(t *testing.T) {
	if got := len(randomHex(16)); got != 32 {
		t.Fatalf("randomHex(16) length = %d, want 32", got)
	}
	for _, r := range randomHex(16) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("randomHex produced a non-hex rune %q", r)
		}
	}
	if got := len(randomDigits(19)); got != 19 {
		t.Fatalf("randomDigits(19) length = %d, want 19", got)
	}
	if got := len(decimalID([]byte("material"))); got != 16 {
		t.Fatalf("decimalID length = %d, want 16", got)
	}
	if !strings.Contains(newUUID(), "-") {
		t.Fatal("newUUID must be hyphenated")
	}
}

// ── model catalog ────────────────────────────────────────────────────────────

func TestCatalogIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Catalog() {
		if m.ID == "" || m.ConfigName == "" {
			t.Fatalf("model %+v is missing an id or config name", m)
		}
		if m.ID != strings.ToLower(m.ID) {
			t.Fatalf("client id %q must be lowercase", m.ID)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate client id %q", m.ID)
		}
		seen[m.ID] = true
		if m.MaxOutput <= 0 {
			t.Fatalf("model %q has no output cap", m.ID)
		}
	}
}

func TestLookupModelAliases(t *testing.T) {
	cases := map[string]string{
		"auto":                "glm-5.2",
		"GLM-5.2":             "glm-5.2",
		"claude-sonnet-4-6":   "glm-5.2",
		"claude-haiku-4-5":    "glm-5.1",
		"gpt-4o":              "DeepSeek-V4-Pro",
		"deepseek-v4-pro":     "DeepSeek-V4-Pro",
		"DeepSeek-V4-Pro":     "DeepSeek-V4-Pro",
		"gpt-4o-mini":         "DeepSeek-V4-Flash",
		"seed-code":           "Doubao-Seed-Code",
		" qwen3.7-plus ":      "qwen-3.7-plus",
		"deepseek-v4-pro 正式版": "DeepSeek-V4-Pro-Official",
		"work":                "work",
	}
	for in, want := range cases {
		if got := ResolveConfigName(in); got != want {
			t.Fatalf("ResolveConfigName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ResolveConfigName("brand-new-model"); got != "brand-new-model" {
		t.Fatalf("an unknown model must pass through unchanged, got %q", got)
	}
}

func TestUsesToolsDetection(t *testing.T) {
	cases := []struct {
		name string
		req  *upstream.ChatRequest
		want bool
	}{
		{"plain", &upstream.ChatRequest{Messages: []upstream.Message{{Role: "user", Content: "hi"}}}, false},
		{"declared tools", &upstream.ChatRequest{Tools: []any{map[string]any{"type": "function"}}}, true},
		{"tool role history", &upstream.ChatRequest{Messages: []upstream.Message{{Role: "tool", Content: "{}"}}}, true},
		{"tool_choice only", &upstream.ChatRequest{Raw: map[string]any{"tool_choice": "auto"}}, true},
		{"structured block", &upstream.ChatRequest{Messages: []upstream.Message{{
			Role:    "assistant",
			Content: []any{map[string]any{"type": "tool_use"}},
		}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesTools(tc.req); got != tc.want {
				t.Fatalf("usesTools = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── remote surface ───────────────────────────────────────────────────────────

const remoteEvents = `event: model_config
data: {"model_name":"glm-5.2"}

: relay-keepalive

event: plan_item
data: {"content":"你好"}

event: plan_item
data: {"content":"你好，世界","reasoning_content":"思考"}

event: token_usage
data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

event: done
data: {"finish_reason":"stop"}

data: [DONE]

`

func TestRemoteChatStreamsSnapshotDeltas(t *testing.T) {
	s := newStub(t)
	s.events = remoteEvents
	c := s.client(Config{})

	emit, deltas := collect()
	req := &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
	}
	if err := c.ChatStream(context.Background(), testAccount(), req, emit); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	// A heartbeat and a metadata event must not produce a delta, because the
	// first byte is what decides whether failover is still possible.
	var text, reasoning string
	for _, d := range *deltas {
		text += d.Content
		reasoning += d.ReasoningContent
	}
	if text != "你好，世界" {
		t.Fatalf("streamed text = %q, want %q", text, "你好，世界")
	}
	if reasoning != "思考" {
		t.Fatalf("streamed reasoning = %q, want %q", reasoning, "思考")
	}
	if len(*deltas) != 3 {
		t.Fatalf("deltas = %d, want 3 (content, content, usage)", len(*deltas))
	}

	create := s.saw("/api/remote/v1/chat_sessions")
	if create == nil {
		t.Fatal("a session must be created first")
	}
	if got := create.Headers.Get("Authorization"); got != TokenScheme+" "+fakeToken("user-1") {
		t.Fatalf("Authorization = %q", got)
	}
	if got := create.Headers.Get("Origin"); got != DefaultRemoteOrigin {
		t.Fatalf("Origin = %q, want %q", got, DefaultRemoteOrigin)
	}
	msg, _ := create.Body["initial_message"].(map[string]any)
	if msg == nil {
		t.Fatal("initial_message is required")
	}
	if got, _ := msg["model_name"].(string); got != "glm-5.2" {
		t.Fatalf("model_name = %q, want glm-5.2", got)
	}
	if got, _ := msg["agent_type"].(string); got != remoteAgent {
		t.Fatalf("agent_type = %q, want %q", got, remoteAgent)
	}
	query, _ := msg["query"].(string)
	if !strings.Contains(query, "User: hi") {
		t.Fatalf("query must carry the flattened transcript, got %q", query)
	}

	events := s.saw("/api/remote/v1/chat_sessions/session-1/events")
	if events == nil {
		t.Fatal("the session's events must be read")
	}
	if got := events.Query.Get("reply_to_message_id"); got != "message-1" {
		t.Fatalf("reply_to_message_id = %q, want message-1", got)
	}
	if got := events.Headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q", got)
	}
}

func TestRemoteChatNonStreamingAccumulates(t *testing.T) {
	s := newStub(t)
	s.events = remoteEvents
	c := s.client(Config{})

	resp, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "你好，世界" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.ReasoningContent != "思考" {
		t.Fatalf("reasoning = %q", resp.ReasoningContent)
	}
	if resp.Usage.TotalTokens != 15 || resp.Usage.PromptTokens != 10 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
}

func TestRemoteSurfacesBusinessErrorCode(t *testing.T) {
	s := newStub(t)
	s.sessionStatus = http.StatusOK
	// The upstream reports quota exhaustion inside a 200 body.
	s.events = "event: error\ndata: {\"code\":4008,\"message\":\"quota exceeded\"}\n\n"
	c := s.client(Config{})

	_, err := c.Chat(context.Background(), testAccount(), &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("an error event must fail the request")
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindQuota {
		t.Fatalf("kind = %q, want %q", kind, upstream.KindQuota)
	}
}

func TestRefreshExchangesToken(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})

	acc := testAccount()
	acc.RefreshToken = "refresh-1"
	if err := c.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if acc.Token() != s.exchangeToken {
		t.Fatalf("token was not replaced")
	}
	if acc.RefreshTokenValue() != "refresh-2" {
		t.Fatalf("refresh token = %q", acc.RefreshTokenValue())
	}
	if acc.ExpiresAtValue() != 1900000000 {
		t.Fatalf("expiry = %d, want 1900000000", acc.ExpiresAtValue())
	}
	call := s.saw(exchangePath)
	if call == nil {
		t.Fatal("the exchange endpoint must be called")
	}
	if got, _ := call.Body["RefreshToken"].(string); got != "refresh-1" {
		t.Fatalf("RefreshToken = %q", got)
	}
	if got, _ := call.Body["ClientID"].(string); got != DefaultClientID {
		t.Fatalf("ClientID = %q", got)
	}
}

func TestRefreshWithoutRefreshTokenIsAuthError(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})
	acc := testAccount()
	err := c.Refresh(context.Background(), acc)
	if err == nil {
		t.Fatal("refreshing without a refresh token must fail")
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindAuth {
		t.Fatalf("kind = %q, want %q", kind, upstream.KindAuth)
	}
	if s.count(exchangePath) != 0 {
		t.Fatal("no exchange call should be made")
	}
}

// ── agent surface ────────────────────────────────────────────────────────────

const agentEvents = `data: {"response":"Hello"}

data: {"response":"Hello!"}

data: {"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}

data: {"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}

data: {"finish_reason":"tool_calls"}

data: [DONE]

`

func TestAgentChatEmitsToolCallsOnce(t *testing.T) {
	s := newStub(t)
	s.agentBody = agentEvents
	c := s.client(Config{UpstreamMode: "ide"})

	emit, deltas := collect()
	req := &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "weather?"}},
		Tools:    []any{map[string]any{"type": "function"}},
	}
	if err := c.ChatStream(context.Background(), testAccount(), req, emit); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var text string
	var calls []upstream.ToolCall
	for _, d := range *deltas {
		text += d.Content
		calls = append(calls, d.ToolCalls...)
	}
	if text != "Hello!" {
		t.Fatalf("text = %q, want Hello!", text)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (the repeat must be de-duplicated)", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("tool name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("tool id = %q", calls[0].ID)
	}

	call := s.saw("/api/agent/v3/llm_utils_chat")
	if call == nil {
		t.Fatal("the agent endpoint must be used")
	}
	if got := call.Headers.Get("X-Cloudide-Token"); got != fakeToken("user-1") {
		t.Fatalf("X-Cloudide-Token = %q", got)
	}
	if got := call.Headers.Get("x-app-id"); got != DefaultAppID {
		t.Fatalf("x-app-id = %q", got)
	}
	if got := call.Headers.Get("x-ide-version-code"); got != ideVersionCode {
		t.Fatalf("x-ide-version-code = %q", got)
	}
	if got, _ := call.Body["config_name"].(string); got != "glm-5.2" {
		t.Fatalf("config_name = %q", got)
	}
	if got, _ := call.Body["function"].(string); got != agentFunction {
		t.Fatalf("function = %q", got)
	}
	if got, _ := call.Body["stream"].(bool); !got {
		t.Fatal("the agent surface must always stream")
	}
}

func TestAgentFailureFallsBackToRemote(t *testing.T) {
	s := newStub(t)
	s.agentStatus = http.StatusInternalServerError
	s.events = remoteEvents
	c := s.client(Config{UpstreamMode: "ide"})

	emit, _ := collect()
	req := &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Tools:    []any{map[string]any{"type": "function"}},
	}
	if err := c.ChatStream(context.Background(), testAccount(), req, emit); err != nil {
		t.Fatalf("the remote fallback should have served the request: %v", err)
	}
	if s.count("/api/agent/v3/llm_utils_chat") != 1 {
		t.Fatalf("agent attempts = %d, want 1", s.count("/api/agent/v3/llm_utils_chat"))
	}
	if s.count("/api/remote/v1/chat_sessions") != 1 {
		t.Fatalf("the remote fallback must run once, saw %d sessions", s.count("/api/remote/v1/chat_sessions"))
	}
}

func TestAutoRouteSplitsByToolProtocol(t *testing.T) {
	s := newStub(t)
	s.events = remoteEvents
	s.agentBody = "data: {\"response\":\"ok\"}\n\ndata: [DONE]\n\n"
	c := s.client(Config{UpstreamMode: "auto-route", AutoRoute: true})

	plain := &upstream.ChatRequest{Model: "glm-5.2", Messages: []upstream.Message{{Role: "user", Content: "hi"}}}
	if err := c.ChatStream(context.Background(), testAccount(), plain, func(upstream.StreamDelta) error { return nil }); err != nil {
		t.Fatalf("plain chat: %v", err)
	}
	if s.count("/api/remote/v1/chat_sessions") != 1 {
		t.Fatal("plain chat must use the remote surface")
	}
	if s.count("/api/agent/v3/llm_utils_chat") != 0 {
		t.Fatal("plain chat must not touch the agent surface")
	}

	withTools := &upstream.ChatRequest{
		Model:    "glm-5.2",
		Messages: []upstream.Message{{Role: "user", Content: "hi"}},
		Tools:    []any{map[string]any{"type": "function"}},
	}
	if err := c.ChatStream(context.Background(), testAccount(), withTools, func(upstream.StreamDelta) error { return nil }); err != nil {
		t.Fatalf("tool chat: %v", err)
	}
	if s.count("/api/agent/v3/llm_utils_chat") != 1 {
		t.Fatal("a tool request must use the agent surface")
	}
}

func TestChatRejectsMissingCredential(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})
	_, err := c.Chat(context.Background(), &account.Account{Source: account.SourceTraeCN, UID: "x"}, &upstream.ChatRequest{Model: "glm-5.2"})
	if err == nil {
		t.Fatal("a credential-less account must be rejected")
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindAuth {
		t.Fatalf("kind = %q, want %q", kind, upstream.KindAuth)
	}
}

func TestProbeValidatesCredentialLocally(t *testing.T) {
	c := New(Config{})
	if p, _ := c.Probe(context.Background(), nil); p.Healthy {
		t.Fatal("a nil account must not probe healthy")
	}
	bad := &account.Account{Source: account.SourceTraeCN, UID: "x", AccessToken: "not-a-token"}
	p, err := c.Probe(context.Background(), bad)
	if p.Healthy || err == nil {
		t.Fatal("a malformed token must probe unhealthy")
	}
	good := testAccount()
	if p, err := c.Probe(context.Background(), good); !p.Healthy || err != nil {
		t.Fatalf("probe = %+v, err = %v", p, err)
	}
}

func TestListModelsReportsEveryCatalogEntry(t *testing.T) {
	c := New(Config{})
	models, err := c.ListModels(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != len(Catalog()) {
		t.Fatalf("models = %d, want %d", len(models), len(Catalog()))
	}
	for _, m := range models {
		if m.Source != upstream.SourceTraeCN {
			t.Fatalf("model %q has source %q", m.ID, m.Source)
		}
		if m.ContextLength <= 0 {
			t.Fatalf("model %q advertises no context length", m.ID)
		}
	}
}

// ── interactive login ────────────────────────────────────────────────────────

// callbackURL pulls the loopback address out of the authorization URL, which is
// how the browser would be redirected back.
func callbackURL(t *testing.T, authURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorization URL does not parse: %v", err)
	}
	cb := parsed.Query().Get("auth_callback_url")
	if cb == "" {
		t.Fatal("the authorization URL must carry auth_callback_url")
	}
	out, err := url.Parse(cb)
	if err != nil {
		t.Fatalf("callback URL does not parse: %v", err)
	}
	if !strings.HasPrefix(out.Host, "127.0.0.1:") {
		t.Fatalf("callback host = %q, want a loopback address", out.Host)
	}
	return out
}

func TestWebLoginStartBuildsDesktopShapedURL(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})

	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	parsed, _ := url.Parse(code.URL)
	if parsed.Host != "www.trae.cn" || parsed.Path != "/authorization" {
		t.Fatalf("authorization URL = %q", code.URL)
	}
	q := parsed.Query()
	for _, key := range []string{
		"login_trace_id", "auth_callback_url", "machine_id", "device_id",
		"x_device_brand", "x_device_type", "x_os_version", "x_app_version",
		"client_id", "login_channel", "auth_from", "auth_type",
	} {
		if q.Get(key) == "" {
			t.Fatalf("authorization URL is missing %q", key)
		}
	}
	if q.Get("login_trace_id") != code.State {
		t.Fatal("login_trace_id must equal the polling state")
	}
	if len(q.Get("machine_id")) != 32 {
		t.Fatalf("machine_id = %q, want 32 hex characters", q.Get("machine_id"))
	}
	if len(q.Get("device_id")) != 19 {
		t.Fatalf("device_id = %q, want 19 digits", q.Get("device_id"))
	}
	if code.Realm != "cn" {
		t.Fatalf("realm = %q", code.Realm)
	}

	// Nothing has authorized yet.
	if acc, done, err := c.DevicePoll(context.Background(), "cn", code.State); done || acc != nil || err != nil {
		t.Fatalf("a fresh attempt must be pending: acc=%v done=%v err=%v", acc, done, err)
	}
}

func TestWebLoginCompletesOnCallback(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})

	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	cb := callbackURL(t, code.URL)

	jwtPayload, _ := json.Marshal(map[string]any{"Token": fakeToken("user-9"), "RefreshToken": "rt-9"})
	userInfo, _ := json.Marshal(map[string]any{"UserID": "user-9", "Region": "cn"})
	q := url.Values{}
	q.Set("userJwt", string(jwtPayload))
	q.Set("userInfo", string(userInfo))
	cb.RawQuery = q.Encode()

	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatalf("the browser callback failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}

	acc, done, err := c.DevicePoll(context.Background(), "cn", code.State)
	if err != nil || !done {
		t.Fatalf("poll: done=%v err=%v", done, err)
	}
	if acc.Source != account.SourceTraeCN {
		t.Fatalf("source = %q", acc.Source)
	}
	if acc.UID != "user-9" {
		t.Fatalf("uid = %q, want user-9", acc.UID)
	}
	if acc.Token() != fakeToken("user-9") {
		t.Fatal("the token must be stored")
	}
	if acc.RefreshTokenValue() != "rt-9" {
		t.Fatalf("refresh token = %q", acc.RefreshTokenValue())
	}
	if !acc.Enabled {
		t.Fatal("a freshly authorized account must be enabled")
	}
	if s.count(exchangePath) != 0 {
		t.Fatal("a ready token must not be exchanged")
	}

	// The attempt is single-use.
	if _, done, _ := c.DevicePoll(context.Background(), "cn", code.State); done {
		t.Fatal("a completed attempt must not be reusable")
	}
}

func TestWebLoginExchangesRefreshOnlyCallback(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})

	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	cb := callbackURL(t, code.URL)
	cb.RawQuery = url.Values{"refreshToken": {"rt-only"}}.Encode()
	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	resp.Body.Close()

	acc, done, err := c.DevicePoll(context.Background(), "cn", code.State)
	if err != nil || !done {
		t.Fatalf("poll: done=%v err=%v", done, err)
	}
	if s.count(exchangePath) != 1 {
		t.Fatalf("exchange calls = %d, want 1", s.count(exchangePath))
	}
	if acc.Token() != s.exchangeToken {
		t.Fatal("the exchanged token must be stored")
	}
	// The exchanged token belongs to a different identity, so the uid has to
	// come from it.
	if acc.UID != "exchanged-user" {
		t.Fatalf("uid = %q, want exchanged-user", acc.UID)
	}
}

func TestWebLoginRejectsCredentiallessCallback(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})

	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	cb := callbackURL(t, code.URL)
	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if _, done, err := c.DevicePoll(context.Background(), "cn", code.State); err == nil && done {
		t.Fatal("a failed callback must not report a completed login")
	}
}

func TestWebLoginUnknownStateIsRejected(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})
	if _, _, err := c.DevicePoll(context.Background(), "cn", "nope"); err == nil {
		t.Fatal("an unknown state must be rejected")
	}
}

func TestWebLoginExpiresStaleAttempts(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{AuthorizeURL: "https://www.trae.cn/authorization"})
	c.login.ttl = 20 * time.Millisecond

	code, err := c.DeviceStart(context.Background(), "cn")
	if err != nil {
		t.Fatalf("DeviceStart: %v", err)
	}
	timer := time.Now().Add(2 * time.Second)
	for {
		acc, done, perr := c.DevicePoll(context.Background(), "cn", code.State)
		if done || perr != nil {
			if perr == nil {
				t.Fatalf("an expired attempt must report an error, got account %+v", acc)
			}
			break
		}
		if time.Now().After(timer) {
			t.Fatal("the attempt never expired")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestParseUserJWTShapes(t *testing.T) {
	if tok, rt := parseUserJWT(""); tok != "" || rt != "" {
		t.Fatal("an empty value must parse to nothing")
	}
	if tok, _ := parseUserJWT(`{"token":"t"}`); tok != "t" {
		t.Fatalf("lowercase Token = %q", tok)
	}
	if tok, rt := parseUserJWT(`{"Token":"T","RefreshToken":"R"}`); tok != "T" || rt != "R" {
		t.Fatalf("parsed %q/%q", tok, rt)
	}
	if tok, _ := parseUserJWT("raw-token"); tok != "raw-token" {
		t.Fatalf("a bare token = %q", tok)
	}
	if got := parseUserInfo(`{"UserID":123}`); got != "123" {
		t.Fatalf("numeric UserID = %q", got)
	}
	if got := parseUserInfo("not json"); got != "" {
		t.Fatalf("garbage userInfo = %q", got)
	}
}

// ── pasted credential ────────────────────────────────────────────────────────

func TestAcceptCredentialReadsIdentityFromToken(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})

	const uid = "pasted-user-abcdef123456"
	acc, err := c.AcceptCredential(fakeToken(uid), "", "", "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if acc.Source != account.SourceTraeCN {
		t.Fatalf("source = %q", acc.Source)
	}
	if acc.UID != uid {
		t.Fatalf("uid = %q", acc.UID)
	}
	if acc.Label != uid[:12] {
		t.Fatalf("label = %q", acc.Label)
	}
	if !acc.Enabled || acc.Ephemeral {
		t.Fatalf("unexpected flags: %+v", acc)
	}
	if s.count(exchangePath) != 0 {
		t.Fatal("a ready token must not be exchanged")
	}
}

func TestAcceptCredentialKeepsExplicitIdentity(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})
	acc, err := c.AcceptCredential(fakeToken("from-token"), "", "explicit-uid", "My Trae")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if acc.UID != "explicit-uid" || acc.Label != "My Trae" {
		t.Fatalf("uid/label = %q/%q", acc.UID, acc.Label)
	}
}

func TestAcceptCredentialExchangesRefreshOnlyInput(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})
	acc, err := c.AcceptCredential("", "refresh-1", "u9", "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := s.count(exchangePath); got != 1 {
		t.Fatalf("exchange calls = %d", got)
	}
	req := s.saw(exchangePath)
	if rt, _ := req.Body["RefreshToken"].(string); rt != "refresh-1" {
		t.Fatalf("refresh token sent = %v", req.Body["RefreshToken"])
	}
	if acc.AccessToken != s.exchangeToken {
		t.Fatalf("token = %q", acc.AccessToken)
	}
	if acc.RefreshToken != "refresh-2" {
		t.Fatalf("rotated refresh = %q", acc.RefreshToken)
	}
	if acc.ExpiresAt <= 0 {
		t.Fatal("expected expiry from the exchange")
	}
}

func TestAcceptCredentialRejectsUnusableInput(t *testing.T) {
	s := newStub(t)
	c := s.client(Config{})
	if _, err := c.AcceptCredential("", "", "", ""); err == nil {
		t.Fatal("an empty credential must be rejected")
	}
	if _, err := c.AcceptCredential("not-a-jwt", "", "", ""); err == nil {
		t.Fatal("a malformed token must be rejected")
	}
}
