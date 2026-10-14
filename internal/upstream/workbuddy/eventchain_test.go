package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
)

// telemetryStub emulates the report, marketplace, conversation and asset hosts
// on one test server, capturing what the client sent.
type telemetryStub struct {
	mu       sync.Mutex
	desktop  []captured
	mini     []captured
	web      []captured
	appear   captured
	expertIn captured
	chatIn   captured
	chatSSE  string
}

type captured struct {
	path   string
	header http.Header
	body   string
}

func (s *telemetryStub) handler() http.Handler {
	mux := http.NewServeMux()
	capture := func(r *http.Request) captured {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		return captured{path: r.URL.Path, header: r.Header.Clone(), body: string(buf[:n])}
	}
	mux.HandleFunc("/v2/report", func(w http.ResponseWriter, r *http.Request) {
		c := capture(r)
		s.mu.Lock()
		switch {
		case r.Header.Get("X-Client-Platform") == miniProgramClientPlatform:
			s.mini = append(s.mini, c)
		case r.Header.Get("x-client-platform") == webPlatform:
			s.web = append(s.web, c)
		default:
			s.desktop = append(s.desktop, c)
		}
		s.mu.Unlock()
		writeOK(w, map[string]any{})
	})
	mux.HandleFunc("/v2/user-asset/appearance/set", func(w http.ResponseWriter, r *http.Request) {
		c := capture(r)
		s.mu.Lock()
		s.appear = c
		s.mu.Unlock()
		writeOK(w, map[string]any{})
	})
	mux.HandleFunc(marketExpertListPath, func(w http.ResponseWriter, r *http.Request) {
		c := capture(r)
		s.mu.Lock()
		s.expertIn = c
		s.mu.Unlock()
		writeOK(w, map[string]any{"experts": []map[string]any{
			{"expert_id": "ex_one", "expert_type": "agent", "display_name_zh": "研究员", "profession_zh": "行业研究员", "version": "1.0.1", "categories": []any{"expert-all"}},
			{"expert_id": "ex_two", "expert_type": "agent", "profession_zh": "只填职称", "categories": []any{}},
			{"expert_id": "", "expert_type": "agent", "display_name_zh": "缺少 id 应被丢弃"},
		}})
	})
	mux.HandleFunc(expertChatPath, func(w http.ResponseWriter, r *http.Request) {
		c := capture(r)
		s.mu.Lock()
		s.chatIn = c
		sse := s.chatSSE
		s.mu.Unlock()
		if sse == "" {
			sse = "data: {\"id\":\"msg-1\",\"choices\":[]}\n\n" +
				"data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[]}\n\n" +
				"data: [DONE]\n\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	return mux
}

func newTelemetryClient(t *testing.T, s *telemetryStub) *Client {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return New(Config{
		BaseURL:        srv.URL,
		BillingBaseURL: srv.URL,
		WebBaseURL:     srv.URL,
		TimeoutSeconds: 5,
	})
}

func TestReportDesktopEventsFingerprint(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	acc := cnAccount()
	acc.Label = "张三"

	events := []map[string]any{{"eventCode": "agent_task_created"}, {"eventCode": "chat_message_send"}}
	if err := c.ReportDesktopEvents(context.Background(), acc, events...); err != nil {
		t.Fatalf("ReportDesktopEvents: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.desktop) != 1 {
		t.Fatalf("desktop reports = %d, want 1", len(s.desktop))
	}
	got := s.desktop[0]
	if got.path != "/v2/report" {
		t.Errorf("path = %s", got.path)
	}
	if ua := got.header.Get("User-Agent"); ua != desktopTelemetryUserAgent {
		t.Errorf("ua = %q", ua)
	}
	if got.header.Get("X-Product") != "SaaS" || got.header.Get("X-User-Id") != "u-1" {
		t.Errorf("headers = %v", got.header)
	}
	var body []map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body not an array: %v (%s)", err, got.body)
	}
	if len(body) != 2 {
		t.Fatalf("events = %d, want 2", len(body))
	}
	if body[0]["extName"] != "workbuddy-desktop" || body[0]["ideName"] != "WorkBuddy" {
		t.Errorf("fingerprint missing: %v", body[0])
	}
	if body[0]["userId"] != "u-1" || body[0]["userNickname"] != "张三" {
		t.Errorf("identity missing: %v", body[0])
	}
	if body[0]["machineId"] == "" || body[0]["machineId"] == body[1]["machineId"] && body[0]["machineId"] == "" {
		t.Errorf("device id not derived: %v", body[0]["machineId"])
	}
	// The business field must survive the fingerprint merge.
	if body[1]["eventCode"] != "chat_message_send" {
		t.Errorf("event code lost: %v", body[1])
	}
}

func TestReportMiniProgramEventsFingerprint(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	if err := c.ReportMiniProgramEvents(context.Background(), cnAccount(), map[string]any{"eventCode": "chat_request_send"}); err != nil {
		t.Fatalf("ReportMiniProgramEvents: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mini) != 1 {
		t.Fatalf("mini reports = %d", len(s.mini))
	}
	got := s.mini[0]
	if got.header.Get("X-Client-Product") != miniProgramProduct || got.header.Get("X-Client-Platform") != miniProgramClientPlatform {
		t.Errorf("mini headers = %v", got.header)
	}
	if !strings.Contains(got.body, `"extName":"workbuddy-mp"`) || !strings.Contains(got.body, `"platform":"mini_program"`) {
		t.Errorf("mini fingerprint missing: %s", got.body)
	}
}

func TestReportWebEvent(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	if err := c.ReportWebEvent(context.Background(), cnAccount(), "web_element_click", "https://x/y", "el-1", "名称"); err != nil {
		t.Fatalf("ReportWebEvent: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.web) != 1 {
		t.Fatalf("web reports = %d", len(s.web))
	}
	got := s.web[0]
	if got.header.Get("x-client-platform") != webPlatform {
		t.Errorf("platform header = %q", got.header.Get("x-client-platform"))
	}
	if got.header.Get("Referer") != "https://x/y" {
		t.Errorf("referer = %q", got.header.Get("Referer"))
	}
	if !strings.Contains(got.body, `"elementId":"el-1"`) || !strings.Contains(got.body, `"os":"Win32"`) {
		t.Errorf("web body: %s", got.body)
	}
}

func TestSetAppearanceThemeBody(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	if err := c.SetAppearanceTheme(context.Background(), cnAccount(), "theme-x"); err != nil {
		t.Fatalf("SetAppearanceTheme: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appear.path != appearanceSetPath {
		t.Fatalf("path = %s", s.appear.path)
	}
	if !strings.Contains(s.appear.body, `"kind":"theme"`) || !strings.Contains(s.appear.body, `"resource_key":"theme-x"`) {
		t.Errorf("body = %s", s.appear.body)
	}
}

func TestMarketExpertsParsing(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	got, err := c.MarketExperts(context.Background(), cnAccount(), "agent")
	if err != nil {
		t.Fatalf("MarketExperts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("experts = %d, want 2 (the entry without an id is dropped)", len(got))
	}
	if got[0].ID != "ex_one" || got[0].Name != "研究员" || got[0].Category != "expert-all" {
		t.Errorf("expert[0] = %+v", got[0])
	}
	// A missing display name falls back to the profession.
	if got[1].Name != "只填职称" {
		t.Errorf("expert[1] name = %q", got[1].Name)
	}
	if got[1].Category != "" {
		t.Errorf("expert[1] category = %q, want empty", got[1].Category)
	}
	if !strings.Contains(s.expertIn.body, `"expert_type":"agent"`) {
		t.Errorf("filter not sent: %s", s.expertIn.body)
	}
}

func TestDesktopChatParsesServerRequestID(t *testing.T) {
	s := &telemetryStub{}
	c := newTelemetryClient(t, s)
	conv, req, err := c.DesktopChat(context.Background(), cnAccount(), "ex_one", "glm-5.2")
	if err != nil {
		t.Fatalf("DesktopChat: %v", err)
	}
	if req != "cmb-0123456789abcdef0123456789abcdef" {
		t.Fatalf("request id = %q", req)
	}
	if conv == "" {
		t.Fatal("conversation id must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chatIn.header.Get("X-Expert-Id") != "ex_one" {
		t.Errorf("expert header = %q", s.chatIn.header.Get("X-Expert-Id"))
	}
	if s.chatIn.header.Get("X-Conversation-ID") != conv {
		t.Errorf("conversation header = %q, want %q", s.chatIn.header.Get("X-Conversation-ID"), conv)
	}
	if !strings.Contains(s.chatIn.body, `"model":"glm-5.2"`) {
		t.Errorf("model not forwarded: %s", s.chatIn.body)
	}
}

func TestDesktopChatWithoutServerRequestIDFails(t *testing.T) {
	s := &telemetryStub{chatSSE: "data: {\"id\":\"short\"}\n\ndata: [DONE]\n\n"}
	c := newTelemetryClient(t, s)
	if _, _, err := c.DesktopChat(context.Background(), cnAccount(), "", ""); err == nil {
		t.Fatal("a stream without a server request id must fail")
	}
}

func TestDeviceIDIsStableAndScoped(t *testing.T) {
	a := cnAccount()
	if deviceID(a, "machine") != deviceID(a, "machine") {
		t.Fatal("device id must be stable")
	}
	if deviceID(a, "machine") == deviceID(a, "session") {
		t.Fatal("different salts must give different ids")
	}
	b := cnAccount()
	b.UID = "u-2"
	if deviceID(a, "machine") == deviceID(b, "machine") {
		t.Fatal("different accounts must give different ids")
	}
	if len(deviceID(a, "machine")) != 36 {
		t.Fatalf("device id length = %d, want 36", len(deviceID(a, "machine")))
	}
}

var _ = account.SourceWorkBuddy
