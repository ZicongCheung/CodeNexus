package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Regional hosts for the task endpoints. The gateway's chat base serves the
// growth domain; metering and the web growth center are separate hosts on the
// mainland deployment but collapse into one on the international service.
const (
	billingHostCN = "https://www.codebuddy.cn"
	webHostCN     = "https://www.workbuddy.cn"

	// meteringUserAgent identifies a desktop build to the metering endpoints,
	// which expect a single-segment product UA.
	meteringUserAgent = "WorkBuddy/5.5.4"
	// miniProgramPlatform is the client-platform marker required by the
	// mini-program-only tasks for listing, accepting and claiming.
	miniProgramPlatform = "miniprogram"
	// webPlatform marks requests made by the web growth center.
	webPlatform = "web"
)

// APIError is an upstream business failure: the transport succeeded but the
// response envelope carried a non-zero code. Status is the HTTP status and
// Code the business code, both zero when the request never completed.
type APIError struct {
	Status int
	Code   int
	Msg    string
}

// Error implements error.
func (e *APIError) Error() string {
	switch {
	case e.Status != 0 && e.Msg != "":
		return fmt.Sprintf("workbuddy: http %d: %s", e.Status, e.Msg)
	case e.Status != 0:
		return fmt.Sprintf("workbuddy: http %d", e.Status)
	case e.Msg != "":
		return fmt.Sprintf("workbuddy: code %d: %s", e.Code, e.Msg)
	default:
		return "workbuddy: request failed"
	}
}

// IsAuth reports whether the failure means the credential was rejected, so the
// caller can refresh and retry.
func (e *APIError) IsAuth() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(e.Msg), "token")
}

func asAPIError(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsUnauthorized reports whether err is an authentication failure.
func IsUnauthorized(err error) bool {
	if e, ok := asAPIError(err); ok {
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	}
	return upstream.IsAuth(err)
}

// IsNotFound reports whether err means the endpoint or resource is absent.
// Callers use it to fall back between regional path variants.
func IsNotFound(err error) bool {
	e, ok := asAPIError(err)
	return ok && e.Status == http.StatusNotFound
}

// IsAlreadyDone reports whether err is the upstream's idempotent rejection of a
// repeat action (for example checking in twice on the same day).
func IsAlreadyDone(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(e.Msg)
	for _, marker := range []string{"already", "repeat", "已签到", "重复"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsNotCompleted reports whether err says the task's progress is not yet
// sufficient to claim its reward.
func IsNotCompleted(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(e.Msg)
	for _, marker := range []string{"not completed", "未完成", "not reached"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsUnsupported reports whether err says the account type does not have the
// requested feature (enterprise tenants have no personal growth program).
func IsUnsupported(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(e.Msg)
	for _, marker := range []string{"not supported", "不支持", "only available for personal", "not_authorized", "not authorized"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsPrerequisite reports whether err says a prerequisite task is still missing.
// It is expected while a chained task list unlocks day by day.
func IsPrerequisite(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(e.Msg)
	return strings.Contains(msg, "prerequisite") || strings.Contains(msg, "task not completed yet")
}

// taskHosts is the per-account endpoint host set.
type taskHosts struct {
	chat    string
	billing string
	web     string
}

// hostsFor resolves the endpoint hosts for an account's realm, honouring test
// overrides.
func (c *Client) hostsFor(acc *account.Account) taskHosts {
	h := taskHosts{chat: c.cfg.BaseURL, billing: billingHostCN, web: webHostCN}
	if realmOf(acc) == "global" {
		h = taskHosts{chat: serviceBaseGlobal, billing: serviceBaseGlobal, web: serviceBaseGlobal}
	}
	if c.cfg.BillingBaseURL != "" {
		h.billing = c.cfg.BillingBaseURL
	}
	if c.cfg.WebBaseURL != "" {
		h.web = c.cfg.WebBaseURL
	}
	return h
}

// taskHeaders builds the header set shared by the metering and growth calls.
func (c *Client) taskHeaders(acc *account.Account, ua string) map[string]string {
	lang := "zh-CN"
	if realmOf(acc) == "global" {
		lang = "en-US"
	}
	h := map[string]string{
		"Accept":              "application/json",
		"Content-Type":        "application/json",
		"User-Agent":          ua,
		"Accept-Language":     lang,
		"X-Requested-With":    "XMLHttpRequest",
		"X-CodeBuddy-Request": "1",
	}
	if tok := acc.Token(); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	if acc.UID != "" {
		h["X-User-Id"] = acc.UID
	}
	if acc.EnterpriseID != "" {
		h["X-Enterprise-Id"] = acc.EnterpriseID
		h["X-Tenant-Id"] = acc.EnterpriseID
	} else {
		h["X-No-Enterprise-Id"] = "1"
	}
	if d := acc.DomainValue(); d != "" {
		h["X-Domain"] = d
	}
	return h
}

// doTask performs one task request and unwraps the {code,msg,data} envelope.
// Transport failures are classified through the shared error taxonomy so the
// caller can tell timeouts from rejections.
func (c *Client) doTask(ctx context.Context, method, url string, body any, hdrs map[string]string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range hdrs {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Msg: snippet(raw)}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("workbuddy: malformed response: %w", err)
	}
	if env.Code != 0 {
		return nil, &APIError{Status: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	}
	return env.Data, nil
}

// metering calls the billing host, trying each path variant in order. The
// international service moved these endpoints under a /v2 prefix, so a 404
// falls through to the next candidate.
func (c *Client) metering(ctx context.Context, acc *account.Account, method string, paths []string, body any) (json.RawMessage, error) {
	h := c.taskHeaders(acc, meteringUserAgent)
	base := strings.TrimRight(c.hostsFor(acc).billing, "/")
	var lastErr error
	for i, p := range paths {
		data, err := c.doTask(ctx, method, base+p, body, h)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !IsNotFound(err) || i == len(paths)-1 {
			return nil, err
		}
	}
	return nil, lastErr
}

// growth calls the growth domain, which lives under the chat host.
func (c *Client) growth(ctx context.Context, acc *account.Account, method, path string, body any) (json.RawMessage, error) {
	h := c.taskHeaders(acc, c.cfg.UserAgent)
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	return c.doTask(ctx, method, base+path, body, h)
}

// growthMP is growth with the mini-program platform marker, which is required
// to list, accept and claim the mini-program-only tasks.
func (c *Client) growthMP(ctx context.Context, acc *account.Account, method, path string, body any) (json.RawMessage, error) {
	h := c.taskHeaders(acc, c.cfg.UserAgent)
	h["X-Client-Platform"] = miniProgramPlatform
	base := strings.TrimRight(c.hostsFor(acc).chat, "/")
	return c.doTask(ctx, method, base+path, body, h)
}

// web calls the web growth center, which hosts the reward-claim endpoint for
// some task shapes that the chat host rejects.
func (c *Client) web(ctx context.Context, acc *account.Account, method, path string, body any) (json.RawMessage, error) {
	h := c.taskHeaders(acc, c.cfg.UserAgent)
	h["X-Client-Platform"] = webPlatform
	if realmOf(acc) != "global" {
		h["Origin"] = webHostCN
		h["Referer"] = webHostCN + "/profile/growth-center"
	}
	base := strings.TrimRight(c.hostsFor(acc).web, "/")
	return c.doTask(ctx, method, base+path, body, h)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
