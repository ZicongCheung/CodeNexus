package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Regional service endpoints and the client identity the service expects.
const (
	serviceBaseCN     = "https://copilot.tencent.com"
	serviceBaseGlobal = "https://www.workbuddy.ai"
	originCN          = "https://www.codebuddy.cn"
	originGlobal      = "https://www.workbuddy.ai"

	// deviceUserAgent identifies the CLI-style client during the device
	// authorization handshake.
	deviceUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"
	// desktopUserAgent identifies the desktop client for API calls such as
	// token refresh.
	desktopUserAgent = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
)

type regionInfo struct{ base, origin string }

// regionFor resolves the regional service endpoints for a realm. AuthBaseURL in
// the client config overrides the base, which tests use to point at a stub.
func (c *Client) regionFor(realm string) regionInfo {
	r := regionInfo{base: serviceBaseCN, origin: originCN}
	if realm == "global" {
		r = regionInfo{base: serviceBaseGlobal, origin: originGlobal}
	}
	if c.cfg.AuthBaseURL != "" {
		r.base = c.cfg.AuthBaseURL
	}
	return r
}

// realmOf derives the region from an account's domain.
func realmOf(acc *account.Account) string {
	d := strings.ToLower(strings.TrimSpace(acc.DomainValue()))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

func (r regionInfo) headers(ua string, extra map[string]string) map[string]string {
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json, text/plain, */*",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           r.origin,
		"Referer":          r.origin + "/",
		"User-Agent":       ua,
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// envelope is the shared {code,msg,data} response wrapper of the service.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doEnvelope sends one request and unwraps the service envelope.
func (c *Client) doEnvelope(ctx context.Context, method, fullURL string, body io.Reader, headers map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstream.Classify(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("workbuddy: service returned %d", resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("workbuddy: malformed response: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("workbuddy: code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

// tokenBundle is the credential set returned by the login and refresh calls.
type tokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// DeviceStart implements upstream.DeviceAuth: it requests a device code and the
// URL the user must visit to authorize.
func (c *Client) DeviceStart(ctx context.Context, realm string) (*upstream.DeviceCode, error) {
	r := c.regionFor(realm)
	u := r.base + "/v2/plugin/auth/state?platform=CLI"
	data, err := c.doEnvelope(ctx, http.MethodPost, u, strings.NewReader("{}"), r.headers(deviceUserAgent, nil))
	if err != nil {
		return nil, err
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, errors.New("workbuddy: device start missing state or authUrl")
	}
	return &upstream.DeviceCode{State: st.State, URL: st.AuthURL, Realm: realm}, nil
}

// DevicePoll implements upstream.DeviceAuth. While the user has not finished
// authorizing it returns (nil, false, nil); on success it returns a ready
// account.
func (c *Client) DevicePoll(ctx context.Context, realm, state string) (*account.Account, bool, error) {
	r := c.regionFor(realm)
	q := url.QueryEscape(state)

	data, err := c.doEnvelope(ctx, http.MethodGet, r.base+"/v2/plugin/auth/token?state="+q, nil, r.headers(deviceUserAgent, nil))
	if err != nil {
		return nil, false, nil // authorization still pending
	}
	var tok tokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, false, nil
	}

	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if raw, err := c.doEnvelope(ctx, http.MethodGet, r.base+"/v2/plugin/login/account?state="+q, nil,
		r.headers(deviceUserAgent, map[string]string{"Authorization": "Bearer " + tok.AccessToken})); err == nil {
		_ = json.Unmarshal(raw, &acct)
	}
	if acct.UID == "" {
		return nil, false, errors.New("workbuddy: authorization finished but the account id is unavailable")
	}
	if !validUID(acct.UID) {
		return nil, false, errors.New("workbuddy: service returned an unusable account id")
	}

	label := strings.TrimSpace(acct.Nickname)
	if label == "" {
		label = acct.UID
	}
	acc := &account.Account{
		Source:  account.SourceWorkBuddy,
		UID:     acct.UID,
		Label:   label,
		Enabled: true,
	}
	acc.SetCredentials(tok.AccessToken, tok.RefreshToken, tok.Domain, expiryFrom(tok.ExpiresIn))
	return acc, true, nil
}

// Refresh implements upstream.Upstream: it renews the access token in place.
func (c *Client) Refresh(ctx context.Context, acc *account.Account) error {
	rt := acc.RefreshTokenValue()
	if rt == "" {
		return errors.New("workbuddy: account has no refresh token")
	}
	r := c.regionFor(realmOf(acc))
	u := r.base + "/v2/plugin/auth/token/refresh"
	data, err := c.doEnvelope(ctx, http.MethodPost, u, strings.NewReader("{}"),
		r.headers(desktopUserAgent, map[string]string{"X-Refresh-Token": rt}))
	if err != nil {
		return err
	}
	var tok tokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return errors.New("workbuddy: refresh returned no access token (a new login is required)")
	}
	acc.SetCredentials(tok.AccessToken, tok.RefreshToken, tok.Domain, expiryFrom(tok.ExpiresIn))
	return nil
}

// expiryFrom converts a relative lifetime into an absolute unix timestamp,
// ignoring implausible values.
func expiryFrom(expiresIn int64) int64 {
	const maxLifetime = int64(10 * 365 * 24 * 3600)
	if expiresIn <= 0 || expiresIn > maxLifetime {
		return 0
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
}

// validUID reports whether an upstream account id is safe to use in a file
// name. Ids are observed to be UUIDs, so only a conservative character set is
// accepted.
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
