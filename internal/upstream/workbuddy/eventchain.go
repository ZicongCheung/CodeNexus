package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// This file reports client telemetry batches. The growth tasks that say "use
// the desktop client" or "use the mini program" are lit by the same
// POST /v2/report channel the real clients use, distinguished only by which
// client fingerprint and host the batch is sent with:
//
//	desktop  → the chat host,  workbuddy-desktop 5.5.6 fingerprint
//	mini app → the billing host, workbuddy-mp 2.4.0 fingerprint
//	web      → the web host,    browser fingerprint
//
// The on-the-wire field names live in internal/upstream/events.go; this file
// only supplies the per-account device identity and the transport.

const (
	// desktopTelemetryUserAgent matches the 5.5.6 client version carried in the
	// desktop fingerprint, so the user agent and the fingerprint agree.
	desktopTelemetryUserAgent = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	// webUserAgent is a current desktop browser user agent.
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

	meteringReportPath = "/v2/report"
	appearanceSetPath  = "/v2/user-asset/appearance/set"

	miniProgramVersion = "2.4.0"
	miniProgramProduct = "workbuddy-mp"
	// miniProgramClientPlatform is the telemetry report's platform marker,
	// distinct from the "miniprogram" marker the growth endpoints require.
	miniProgramClientPlatform = "mp-weixin"
)

// nickname returns a display name for the account, falling back to the uid.
// The telemetry fingerprint carries it but the upstream does not validate it.
func nickname(acc *account.Account) string {
	if acc.Label != "" {
		return acc.Label
	}
	return acc.UID
}

// deviceID derives a stable 36-hex-character device identifier from the
// account identity, so one account always presents the same "device".
func deviceID(acc *account.Account, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + acc.UID))
	return hex.EncodeToString(sum[:18])
}

// desktopFingerprint returns the fingerprint fields injected into every
// desktop telemetry event.
func desktopFingerprint(acc *account.Account) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       acc.UID,
		"username":     nickname(acc),
		"userNickname": nickname(acc),
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    deviceID(acc, "machine"),
		"sessionId":    deviceID(acc, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// miniProgramFingerprint returns the fingerprint fields injected into every
// mini-program telemetry event.
func miniProgramFingerprint(acc *account.Account) map[string]any {
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   miniProgramVersion,
		"extName":      "workbuddy-mp",
		"extVersion":   miniProgramVersion,
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone":     "Asia/Shanghai",
		"userId":       acc.UID,
		"userNickname": nickname(acc),
	}
}

// mergeEvents folds the base fingerprint into each event, letting the event's
// own fields win.
func mergeEvents(base map[string]any, events []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}

// ReportDesktopEvents sends a desktop-fingerprint telemetry batch.
func (c *Client) ReportDesktopEvents(ctx context.Context, acc *account.Account, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("workbuddy: desktop report needs at least one event")
	}
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	h := c.taskHeaders(acc, desktopTelemetryUserAgent)
	h["Accept"] = "application/json, text/plain, */*"
	h["Content-Type"] = "application/json;charset=UTF-8"
	h["X-Domain"] = base
	h["X-Product"] = "SaaS"
	h["X-Request-ID"] = deviceID(acc, "req") + fmt.Sprintf("%06d", time.Now().UnixNano()%1e6)
	_, err := c.doTask(ctx, "POST", base+meteringReportPath, mergeEvents(desktopFingerprint(acc), events), h)
	return err
}

// ReportMiniProgramEvents sends a mini-program-fingerprint telemetry batch.
func (c *Client) ReportMiniProgramEvents(ctx context.Context, acc *account.Account, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("workbuddy: mini-program report needs at least one event")
	}
	base := strings.TrimRight(c.hostsFor(acc).billing, "/")
	h := c.taskHeaders(acc, meteringUserAgent)
	h["X-Client-Product"] = miniProgramProduct
	h["X-Client-Version"] = miniProgramVersion
	h["X-Client-Platform"] = miniProgramClientPlatform
	h["X-Platform"] = "wechatmp"
	_, err := c.doTask(ctx, "POST", base+meteringReportPath, mergeEvents(miniProgramFingerprint(acc), events), h)
	return err
}

// ReportWebEvent sends one browser-fingerprint event, which is what the web
// page-behaviour tasks key on.
func (c *Client) ReportWebEvent(ctx context.Context, acc *account.Account, eventCode, pageURL, elementID, elementName string) error {
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": webUserAgent,
		"machineId": deviceID(acc, "webmachine"), "userId": acc.UID,
		"userNickname": nickname(acc), "enterpriseId": acc.EnterpriseID,
	}
	base := strings.TrimRight(c.hostsFor(acc).web, "/")
	h := c.taskHeaders(acc, webUserAgent)
	h["Accept"] = "application/json"
	h["x-client-platform"] = webPlatform
	h["Origin"] = base
	h["Referer"] = pageURL
	_, err := c.doTask(ctx, "POST", base+meteringReportPath, []map[string]any{ev}, h)
	return err
}

// SetAppearanceTheme applies an appearance theme. On its own it does not light
// the appearance task; the caller pairs it with a skin-applied event.
func (c *Client) SetAppearanceTheme(ctx context.Context, acc *account.Account, resourceKey string) error {
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	h := c.taskHeaders(acc, desktopTelemetryUserAgent)
	h["Accept"] = "application/json, text/plain, */*"
	h["Content-Type"] = "application/json;charset=UTF-8"
	h["X-Product"] = "SaaS"
	_, err := c.doTask(ctx, "POST", base+appearanceSetPath, map[string]string{
		"kind": "theme", "resource_key": resourceKey,
	}, h)
	return err
}

// upstream's marketplace listing lives on the chat host.
const marketExpertListPath = "/portal/operation-platform/market/expert/list"

// expertChatPath is the conversation endpoint the desktop client uses. It is
// a different path from the gateway's own chat proxy, because it carries the
// desktop client's headers and answers an SSE stream.
const expertChatPath = "/v2/chat/completions"

// serverRequestID matches the server-issued conversation-request identifier:
// either 32 hex characters or the same with a "cmb-" prefix.
var serverRequestID = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// MarketExperts returns the real experts of the marketplace. expertType selects
// single experts ("agent") or expert teams ("team"); empty returns both.
func (c *Client) MarketExperts(ctx context.Context, acc *account.Account, expertType string) ([]upstream.MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	h := c.taskHeaders(acc, desktopTelemetryUserAgent)
	h["X-Domain"] = base
	h["X-Product"] = "SaaS"
	data, err := c.doTask(ctx, "POST", base+marketExpertListPath, body, h)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []struct {
			ExpertID      string `json:"expert_id"`
			ExpertType    string `json:"expert_type"`
			DisplayNameZH string `json:"display_name_zh"`
			ProfessionZH  string `json:"profession_zh"`
			Version       string `json:"version"`
			Categories    []any  `json:"categories"`
		} `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("workbuddy: expert list: %w", err)
	}
	experts := make([]upstream.MarketExpert, 0, len(out.Experts))
	for _, e := range out.Experts {
		if e.ExpertID == "" {
			continue
		}
		cat := ""
		if len(e.Categories) > 0 {
			if s, ok := e.Categories[0].(string); ok {
				cat = s
			}
		}
		name := e.DisplayNameZH
		if name == "" {
			name = e.ProfessionZH
		}
		experts = append(experts, upstream.MarketExpert{
			ID:       e.ExpertID,
			Type:     e.ExpertType,
			Name:     name,
			Title:    e.ProfessionZH,
			Version:  e.Version,
			Category: cat,
		})
	}
	return experts, nil
}

// DesktopChat sends one real desktop conversation, optionally addressed to an
// expert, and returns the conversation id plus the server-issued request id
// parsed from the response stream.
//
// The expert-usage evidence must carry the server's request id: the upstream
// discards the event when the id is invented locally. This call therefore has
// to actually run a conversation before that evidence can be reported.
func (c *Client) DesktopChat(ctx context.Context, acc *account.Account, expertID, modelID string) (conversationID, requestID string, err error) {
	if modelID == "" {
		modelID = "fast-model"
	}
	conversationID = fmt.Sprintf("codenexus-conv-%d", time.Now().UnixNano())
	body, err := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return "", "", err
	}
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+expertChatPath, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	h := c.taskHeaders(acc, desktopTelemetryUserAgent)
	h["Accept"] = "text/event-stream"
	h["X-Domain"] = base
	h["X-Product"] = "SaaS"
	h["X-Conversation-ID"] = conversationID
	h["X-Request-ID"] = fmt.Sprintf("%d", time.Now().UnixNano())
	h["X-Agent-Intent"] = "craft"
	h["X-Agent-Type"] = "main"
	h["X-IDE-Name"] = "WorkBuddy"
	h["X-IDE-Type"] = "WorkBuddy"
	h["X-IDE-Version"] = "5.5.6"
	h["x-codebuddy-request"] = "1"
	if expertID != "" {
		h["X-Expert-Id"] = expertID
	}
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", upstream.Classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", &APIError{Status: resp.StatusCode, Msg: snippet(b)}
	}

	// Scan the stream for the first server-issued request id. The stream is
	// drained either way so the connection is not left half-read.
	const maxScan = 1 << 20
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for len(buf) < maxScan {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			// Advance the search cursor past a non-matching id so the loop
			// cannot keep re-examining the same position.
			for {
				i := bytes.Index(buf[searchFrom:], []byte(`"id":"`))
				if i < 0 {
					break
				}
				abs := searchFrom + i
				rest := buf[abs+6:]
				end := bytes.IndexByte(rest, '"')
				if end <= 0 {
					searchFrom = abs + 1
					continue
				}
				id := string(rest[:end])
				if serverRequestID.MatchString(id) {
					return conversationID, id, nil
				}
				searchFrom = abs + 1
			}
		}
		if rerr != nil {
			break
		}
	}
	return "", "", fmt.Errorf("workbuddy: no server request id in chat stream")
}
