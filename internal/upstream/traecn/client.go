// Package traecn implements the Trae CN upstream adapter.
//
// The upstream exposes two very different chat surfaces, and the adapter keeps
// both:
//
//   - The remote surface (a coding-agent session: create it, then read its
//     event stream) is the plain-chat path. It is the most compatible and the
//     least surprising for ordinary conversation.
//   - The agent surface (a single streaming completion that accepts a native
//     tools array) is the tool-calling path. A remote session owns a workspace
//     and may satisfy file or shell tools itself, which silently breaks tools
//     the caller meant to execute, so anything carrying a tool protocol is
//     routed here.
//
// Credentials are Cloud-IDE-JWT tokens; see auth.go.
package traecn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Built-in endpoints. The gateway talks to the same hosts the desktop client
// does; every one of them is overridable for tests and for regional variants.
const (
	DefaultBaseURL       = "https://trae-api-cn.mchost.guru"
	DefaultRemoteBaseURL = "https://trae-api-cn.mchost.guru/api/remote/v1"
	DefaultAuthBaseURL   = "https://api.trae.cn"
	DefaultUGBaseURL     = "https://api.trae.cn"
	DefaultPayBaseURL    = "https://api.trae.cn"
	DefaultUsageBaseURL  = "https://api5-normal.mchost.guru"
	DefaultAuthorizeURL  = "https://www.trae.cn/authorization"
	DefaultClientID      = "ono9krqynydwx5"
	DefaultAppID         = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	DefaultIDEVersion    = "3.3.67"
	DefaultAppVersion    = "3.3.65"
	DefaultDeviceBrand   = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	DefaultDeviceType    = "windows"
	DefaultOSVersion     = "Windows 10 Pro"
)

// DefaultRemoteOrigin is the web origin the remote surface expects.
const DefaultRemoteOrigin = "https://solo.trae.cn"

// DefaultRemoteUserAgent identifies a normal browser session on the remote
// surface. The desktop client sends an empty agent on the agent surface.
const DefaultRemoteUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

// ideVersionCode travels with the agent surface. A stale code is rejected, so
// it is a named constant rather than an inline literal.
const ideVersionCode = "20260401"

// Config configures the adapter.
type Config struct {
	BaseURL        string
	RemoteBaseURL  string
	AuthBaseURL    string
	AuthorizeURL   string
	ClientID       string
	UserAgent      string
	TimeoutSeconds int
	MaxTokens      int
	WebLoginPort   int
	DeviceBrand    string
	DeviceType     string
	OSVersion      string
	AppVersion     string
	IDEVersion     string
	AppID          string
	// UpstreamMode pins the executor: remote, solo, ide, work-agent, cli or
	// auto-route. Anything unknown falls back to auto-route.
	UpstreamMode string
	// AutoRoute splits requests by whether they carry a tool protocol.
	AutoRoute bool
	// LocalDataDirs overrides where the desktop client's store is looked for.
	// Empty means derive the platform defaults.
	LocalDataDirs []string
	// CLI tunes the local-CLI executor. Only used when UpstreamMode is "cli".
	CLI CLIConfig
	// UGBaseURL serves the daily check-in API.
	UGBaseURL string
	// PayBaseURL serves the entitlement-usage API.
	PayBaseURL string
	// UsageBaseURL serves the per-turn commercial usage API.
	UsageBaseURL string
	// Checkin tunes the daily check-in and its risk-control handling.
	Checkin CheckinConfig
	// DebugUpstream dumps every raw upstream event into the debug log. It is a
	// troubleshooting aid for protocol drift, never a default.
	DebugUpstream bool
	// StateFile persists the check-in device generation and 9074 backoff. It is
	// what keeps a rate-limited device id from being reused after a restart.
	// Empty keeps the state in memory only.
	StateFile string
}

// Client is the Trae CN upstream adapter.
type Client struct {
	cfg    Config
	http   *http.Client
	stream *http.Client
	login  *webLoginHost
	cli    *cliRunner
	ck     *checkinState
}

// New builds an adapter, applying defaults for zero-valued fields.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.RemoteBaseURL == "" {
		cfg.RemoteBaseURL = DefaultRemoteBaseURL
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = DefaultAuthBaseURL
	}
	if cfg.UGBaseURL == "" {
		cfg.UGBaseURL = DefaultUGBaseURL
	}
	if cfg.PayBaseURL == "" {
		cfg.PayBaseURL = DefaultPayBaseURL
	}
	if cfg.UsageBaseURL == "" {
		cfg.UsageBaseURL = DefaultUsageBaseURL
	}
	if cfg.AuthorizeURL == "" {
		cfg.AuthorizeURL = DefaultAuthorizeURL
	}
	if cfg.ClientID == "" {
		cfg.ClientID = DefaultClientID
	}
	if cfg.AppID == "" {
		cfg.AppID = DefaultAppID
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultRemoteUserAgent
	}
	if cfg.DeviceBrand == "" {
		cfg.DeviceBrand = DefaultDeviceBrand
	}
	if cfg.DeviceType == "" {
		cfg.DeviceType = DefaultDeviceType
	}
	if cfg.OSVersion == "" {
		cfg.OSVersion = DefaultOSVersion
	}
	if cfg.AppVersion == "" {
		cfg.AppVersion = DefaultAppVersion
	}
	if cfg.IDEVersion == "" {
		cfg.IDEVersion = DefaultIDEVersion
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 120
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 64000
	}
	cfg.CLI = cfg.CLI.normalize()
	cfg.Checkin = cfg.Checkin.normalize()
	switch cfg.UpstreamMode {
	case "remote", "solo", "ide", "work-agent", "cli", "auto-route":
	default:
		cfg.UpstreamMode = "auto-route"
	}
	if cfg.UpstreamMode == "auto-route" {
		cfg.AutoRoute = true
	}

	c := &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		// A streamed answer legitimately runs for minutes, so only a per-request
		// context bounds it; a loopback first-byte timeout catches a hung
		// connection instead of the whole-request timeout.
		stream: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
	c.login = newWebLoginHost(cfg)
	c.cli = newCLIRunner(cfg.CLI)
	c.ck = newCheckinState(cfg.StateFile)
	return c
}

// Source implements upstream.Upstream.
func (c *Client) Source() upstream.Source { return upstream.SourceTraeCN }

// DeviceStart implements upstream.DeviceAuth.
func (c *Client) DeviceStart(ctx context.Context, realm string) (*upstream.DeviceCode, error) {
	return c.login.DeviceStart(ctx, realm)
}

// DevicePoll implements upstream.DeviceAuth.
func (c *Client) DevicePoll(ctx context.Context, realm, state string) (*account.Account, bool, error) {
	return c.login.DevicePoll(ctx, realm, state)
}

// AcceptCredential implements upstream.CredentialAcceptor. It lets a user paste
// a Cloud-IDE-JWT (and optionally a refresh token) straight into the console.
// A token is optional when a refresh token is supplied: the refresh is traded
// for a live token immediately so the account is usable the moment it lands.
func (c *Client) AcceptCredential(token, refreshToken, uid, label string) (*account.Account, error) {
	token = strings.TrimSpace(token)
	refreshToken = strings.TrimSpace(refreshToken)
	uid = strings.TrimSpace(uid)
	label = strings.TrimSpace(label)
	expiresAt := int64(0)

	if token == "" {
		if refreshToken == "" {
			return nil, errors.New("provide a Cloud-IDE-JWT token or a refresh token")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tok, newRT, exp, err := c.exchangeToken(ctx, refreshToken, uid)
		if err != nil {
			return nil, err
		}
		token, refreshToken, expiresAt = tok, newRT, exp
	}
	if !LooksLikeToken(token) {
		return nil, errors.New("credential does not look like a Cloud-IDE-JWT")
	}
	if uid == "" {
		uid = TokenUserID(token)
	}
	if uid == "" {
		uid = token[:24]
	}
	if label == "" {
		label = shortLabel(uid)
	}
	return &account.Account{
		Source:       account.SourceTraeCN,
		UID:          uid,
		Label:        label,
		Enabled:      true,
		AccessToken:  token,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// shortLabel trims an identity into a readable console label. The web-login
// callback uses the same rule, so both entry points stay consistent.
func shortLabel(uid string) string {
	if len(uid) > 12 {
		return uid[:12]
	}
	return uid
}

// ListModels implements upstream.Upstream. The catalog is static on purpose: it
// has to answer while every account is cooling, and a stable list keeps
// cross-source model routing deterministic. A name that is not listed still
// passes through to the upstream unchanged.
func (c *Client) ListModels(_ context.Context, _ *account.Account) ([]upstream.ModelSpec, error) {
	models := Catalog()
	out := make([]upstream.ModelSpec, 0, len(models))
	for _, m := range models {
		out = append(out, upstream.ModelSpec{
			ID:            m.ID,
			Source:        upstream.SourceTraeCN,
			ContextLength: m.ContextLen,
			Capabilities:  m.Capabilities,
		})
	}
	return out, nil
}

// Probe implements upstream.Upstream. The credential is validated locally
// because there is no cheap upstream endpoint that both confirms the token and
// reports credit; a revoked token surfaces as an auth error on the next real
// request, which the gateway already recovers from by refreshing.
//
// In cli mode there is no credential to inspect: the session belongs to the
// local installation, so health means "an executable can be found".
func (c *Client) Probe(_ context.Context, acc *account.Account) (*upstream.Probe, error) {
	if c.cfg.UpstreamMode == "cli" {
		path, err := c.cli.command()
		if err != nil {
			return &upstream.Probe{Healthy: false, Detail: err.Error()}, nil
		}
		return &upstream.Probe{Healthy: true, Detail: "trae cli: " + path}, nil
	}
	if acc == nil || strings.TrimSpace(acc.Token()) == "" {
		return &upstream.Probe{Healthy: false, Detail: "trae: no credential"}, nil
	}
	if !LooksLikeToken(acc.Token()) {
		return &upstream.Probe{
			Healthy: false,
			Detail:  "trae: malformed credential",
		}, upstream.NewError(upstream.KindAuth, 0, "trae: malformed credential; sign in again")
	}
	return &upstream.Probe{Healthy: true, Detail: "trae"}, nil
}

// endpoint joins a path onto the configured model gateway.
func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + path
}

// remoteEndpoint joins a path onto the remote chat base.
func (c *Client) remoteEndpoint(path string) string {
	return strings.TrimRight(c.cfg.RemoteBaseURL, "/") + path
}

// deviceIdentity is the pseudo-hardware a request claims to come from. It is
// derived from the account so the upstream keeps seeing one installation
// instead of a new device on every request, which is what its risk controls
// look at.
type deviceIdentity struct {
	MachineID string
	DeviceID  string
	LocalID   string
}

// identity derives the stable device identity of an account.
func (c *Client) identity(acc *account.Account) deviceIdentity {
	uid := ""
	if acc != nil {
		uid = acc.UID
	}
	dev := sha256.Sum256([]byte("codenexus/trae/device\x00" + uid))
	mach := sha256.Sum256([]byte("codenexus/trae/machine\x00" + uid))
	return deviceIdentity{
		MachineID: hex.EncodeToString(mach[:16]),
		DeviceID:  decimalID(dev[:]),
		LocalID:   "aha-" + hex.EncodeToString(dev[:16]),
	}
}

// decimalID renders 16 decimal digits from hash material, matching the numeric
// device id the desktop client reports.
func decimalID(material []byte) string {
	n := new(big.Int).SetBytes(material)
	mod := new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil)
	n.Mod(n, mod)
	s := n.String()
	if len(s) < 16 {
		s = strings.Repeat("0", 16-len(s)) + s
	}
	return s
}

// setRemoteHeaders applies the headers the remote chat surface expects.
func (c *Client) setRemoteHeaders(req *http.Request, acc *account.Account, sse bool) {
	tok := ""
	uid := ""
	if acc != nil {
		tok = acc.Token()
		uid = acc.UID
	}
	req.Header.Set("Authorization", TokenScheme+" "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trae-Client-Type", "web")
	req.Header.Set("X-Preferenced-Language", "zh-CN")
	req.Header.Set("x-user-region", "CN")
	req.Header.Set("Origin", DefaultRemoteOrigin)
	req.Header.Set("Referer", DefaultRemoteOrigin+"/")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if uid != "" {
		req.Header.Set("x-uid", uid)
	}
	if sse {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Connection", "keep-alive")
	} else {
		req.Header.Set("Accept", "application/json")
	}
}

// setAgentHeaders applies the headers the agent surface expects. It carries
// three equivalent token headers because different upstream hosts read
// different ones.
func (c *Client) setAgentHeaders(req *http.Request, acc *account.Account) {
	tok := ""
	uid := ""
	if acc != nil {
		tok = acc.Token()
		uid = acc.UID
	}
	id := c.identity(acc)
	req.Header.Set("Authorization", TokenScheme+" "+tok)
	req.Header.Set("X-Cloudide-Token", tok)
	req.Header.Set("x-ide-token", tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("x-app-id", c.cfg.AppID)
	req.Header.Set("x-ide-version", c.cfg.IDEVersion)
	req.Header.Set("x-ide-version-code", ideVersionCode)
	req.Header.Set("x-ide-version-type", "stable")
	req.Header.Set("x-device-id", id.DeviceID)
	req.Header.Set("x-machine-id", id.MachineID)
	req.Header.Set("x-request-id", newUUID())
	req.Header.Set("x-device-cpu", "AMD")
	req.Header.Set("x-device-brand", c.cfg.DeviceBrand)
	req.Header.Set("x-device-type", c.cfg.DeviceType)
	req.Header.Set("x-os-version", c.cfg.OSVersion)
	req.Header.Set("x-system-type", "Windows")
	if uid != "" {
		req.Header.Set("x-uid", uid)
	}
	// The desktop client sends an empty agent here; matching it is the point.
	req.Header.Set("User-Agent", "")
}

// mode resolves which executor serves a request. A pinned mode always wins; the
// cli mode is pinned by definition, since a local installation cannot serve only
// part of a source's traffic.
func (c *Client) mode(req *upstream.ChatRequest) string {
	if c.cfg.UpstreamMode != "auto-route" {
		return c.cfg.UpstreamMode
	}
	if usesTools(req) {
		return "ide"
	}
	return "remote"
}

// usesTools reports whether the request carries a tool protocol. Only a tool
// protocol forces the agent surface: a remote session owns a workspace and may
// run file or shell tools itself, so caller-owned tools would silently never
// come back as calls.
func usesTools(req *upstream.ChatRequest) bool {
	if req == nil {
		return false
	}
	if len(req.Tools) > 0 {
		return true
	}
	if raw := req.Raw; raw != nil {
		for _, key := range []string{"tool_choice", "parallel_tool_calls", "functions", "function_call"} {
			if v, ok := raw[key]; ok && v != nil {
				return true
			}
		}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool", "function":
			return true
		}
		if mm, ok := m.Content.([]any); ok {
			for _, block := range mm {
				bm, ok := block.(map[string]any)
				if !ok {
					continue
				}
				switch bm["type"] {
				case "tool_use", "tool_call", "function_call", "tool_result":
					return true
				}
			}
		}
	}
	return false
}
