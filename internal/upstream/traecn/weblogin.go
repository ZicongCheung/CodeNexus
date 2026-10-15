package traecn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The interactive login mirrors what the desktop client does: open the
// authorization page in a browser and let it redirect back to a loopback
// address the client controls. CodeNexus therefore runs a tiny loopback
// listener per login attempt, which is why this flow needs no browser
// automation and no external runtime — and why it looks and behaves exactly
// like the WorkBuddy device flow in the console.
//
// The credential arrives in the callback query string, either as a ready token
// (userJwt) or as a refresh token that has to be exchanged.

// authPluginVersion is the IDE plugin version the authorization page records.
const authPluginVersion = "2.3.24254"

// defaultLoginTTL bounds how long an attempt's listener stays open. It matches
// the console's own authorization-session lifetime.
const defaultLoginTTL = 15 * time.Minute

// callbackPath is the loopback path the authorization page redirects to.
const callbackPath = "/authorize"

// webLoginHost owns the loopback listeners of the interactive logins.
type webLoginHost struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*webSession
	ttl      time.Duration
}

// webSession is one in-flight authorization.
type webSession struct {
	created   time.Time
	listener  net.Listener
	port      int
	machineID string
	deviceID  string

	done chan struct{}
	acc  *account.Account
	err  error
}

func newWebLoginHost(cfg Config) *webLoginHost {
	return &webLoginHost{
		cfg:      cfg,
		sessions: map[string]*webSession{},
		ttl:      defaultLoginTTL,
	}
}

// DeviceStart implements upstream.DeviceAuth.
func (h *webLoginHost) DeviceStart(_ context.Context, realm string) (*upstream.DeviceCode, error) {
	addr := "127.0.0.1:0"
	if h.cfg.WebLoginPort > 0 {
		addr = "127.0.0.1:" + strconv.Itoa(h.cfg.WebLoginPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("trae: cannot open the local callback port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	sess := &webSession{
		created:   time.Now(),
		listener:  ln,
		port:      port,
		machineID: randomHex(16),
		deviceID:  randomDigits(19),
		done:      make(chan struct{}),
	}
	state := newUUID()

	h.mu.Lock()
	h.dropExpiredLocked()
	h.sessions[state] = sess
	h.mu.Unlock()

	go h.serve(sess)
	go h.expire(sess, state)

	if realm == "" {
		realm = "cn"
	}
	return &upstream.DeviceCode{State: state, URL: h.authorizeURL(sess, state), Realm: realm}, nil
}

// DevicePoll implements upstream.DeviceAuth.
func (h *webLoginHost) DevicePoll(_ context.Context, _ string, state string) (*account.Account, bool, error) {
	h.mu.Lock()
	sess, ok := h.sessions[state]
	h.mu.Unlock()
	if !ok {
		return nil, false, fmt.Errorf("trae: authorization expired; start again")
	}
	select {
	case <-sess.done:
		h.mu.Lock()
		delete(h.sessions, state)
		h.mu.Unlock()
		if sess.err != nil {
			return nil, false, sess.err
		}
		return sess.acc, true, nil
	default:
		return nil, false, nil
	}
}

// authorizeURL builds the page URL, carrying the loopback callback and a device
// identity so the request looks like a desktop client.
func (h *webLoginHost) authorizeURL(sess *webSession, traceID string) string {
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", authPluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", h.cfg.ClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traceID)
	q.Set("auth_callback_url", fmt.Sprintf("http://127.0.0.1:%d%s", sess.port, callbackPath))
	q.Set("machine_id", sess.machineID)
	q.Set("device_id", sess.deviceID)
	q.Set("x_device_id", sess.deviceID)
	q.Set("x_machine_id", sess.machineID)
	q.Set("x_device_brand", h.cfg.DeviceBrand)
	q.Set("x_device_type", h.cfg.DeviceType)
	q.Set("x_os_version", h.cfg.OSVersion)
	q.Set("x_env", "production")
	q.Set("x_app_version", h.cfg.AppVersion)
	q.Set("x_app_type", "stable")
	q.Set("hide_saas_login", "true")

	base := strings.TrimRight(h.cfg.AuthorizeURL, "/")
	return base + "?" + q.Encode()
}

// serve answers the loopback callback until the attempt concludes.
func (h *webLoginHost) serve(sess *webSession) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		acc, err := h.parseCallback(r.URL.Query())
		if err != nil {
			h.finish(sess, nil, err)
			writeResultPage(w, http.StatusBadRequest, false, err.Error())
			return
		}
		h.finish(sess, acc, nil)
		writeResultPage(w, http.StatusOK, true, "")
	})
	_ = http.Serve(sess.listener, mux)
}

// finish records the outcome once and wakes any poller.
func (h *webLoginHost) finish(sess *webSession, acc *account.Account, err error) {
	select {
	case <-sess.done:
		return
	default:
	}
	sess.acc = acc
	sess.err = err
	close(sess.done)
	_ = sess.listener.Close()
}

// expire closes an attempt that nobody completed.
func (h *webLoginHost) expire(sess *webSession, state string) {
	t := time.NewTimer(h.ttl)
	defer t.Stop()
	select {
	case <-sess.done:
		return
	case <-t.C:
		h.finish(sess, nil, fmt.Errorf("trae: authorization timed out; start again"))
		h.mu.Lock()
		delete(h.sessions, state)
		h.mu.Unlock()
	}
}

func (h *webLoginHost) dropExpiredLocked() {
	for state, sess := range h.sessions {
		select {
		case <-sess.done:
			delete(h.sessions, state)
		default:
			if time.Since(sess.created) > h.ttl {
				delete(h.sessions, state)
				_ = sess.listener.Close()
			}
		}
	}
}

// parseCallback turns the authorization result into an account.
func (h *webLoginHost) parseCallback(q url.Values) (*account.Account, error) {
	token, refresh := parseUserJWT(q.Get("userJwt"))
	if rt := strings.TrimSpace(q.Get("refreshToken")); rt != "" && refresh == "" {
		refresh = rt
	}

	uid := parseUserInfo(q.Get("userInfo"))
	expiresAt := int64(0)

	if token == "" {
		if refresh == "" {
			return nil, fmt.Errorf("authorization response carried no credential")
		}
		// An older flow returns only a refresh token; trade it for a token now
		// so the account is usable the moment it lands in the pool.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tok, newRT, exp, err := h.exchange(ctx, refresh, uid)
		if err != nil {
			return nil, err
		}
		token, refresh, expiresAt = tok, newRT, exp
	}
	if !LooksLikeToken(token) {
		return nil, fmt.Errorf("authorization returned a malformed token")
	}
	if uid == "" {
		uid = TokenUserID(token)
	}
	if uid == "" {
		uid = token[:24]
	}
	label := shortLabel(uid)
	return &account.Account{
		Source:       account.SourceTraeCN,
		UID:          uid,
		Label:        label,
		Enabled:      true,
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
	}, nil
}

// exchange is the callback's token trade, kept separate so the loopback path
// has no Client dependency.
func (h *webLoginHost) exchange(ctx context.Context, refreshToken, userID string) (string, string, int64, error) {
	c := New(h.cfg)
	return c.exchangeToken(ctx, refreshToken, userID)
}

// parseUserJWT reads the nested credential object the page passes back. It may
// carry either a ready token, a refresh token, or both.
func parseUserJWT(raw string) (token, refresh string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if !strings.HasPrefix(raw, "{") {
		// Some flows pass the token itself.
		return raw, ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", ""
	}
	return firstString(obj, "Token", "token"), firstString(obj, "RefreshToken", "refreshToken")
}

// parseUserInfo extracts the account identity from the page's user object.
func parseUserInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return ""
	}
	return firstString(obj, "UserID", "userId", "user_id", "ID", "id")
}

// firstString returns the first non-empty string value among keys, tolerating
// numeric identities.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case json.Number:
			return v.String()
		}
	}
	return ""
}

// writeResultPage renders the page the browser lands on. It is the user's only
// feedback that the login worked, so it says so plainly.
func writeResultPage(w http.ResponseWriter, status int, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	title, body, tone := "授权成功", "CodeNexus 已收到账号，可以关闭此窗口。", "#15803d"
	if !ok {
		title, tone = "授权失败", "#b91c1c"
		body = msg
		if body == "" {
			body = "请回到 CodeNexus 控制台重试。"
		}
	}
	fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title></head>
<body style="margin:0;font-family:system-ui,-apple-system,'Segoe UI',sans-serif;background:#f6f7f9;color:#1a1d21">
<div style="max-width:26rem;margin:18vh auto;padding:1.75rem;background:#fff;border:1px solid #e3e6ea;border-radius:.75rem;text-align:center">
<h1 style="font-size:1.1rem;margin:0 0 .6rem;color:%s">%s</h1>
<p style="margin:0;font-size:.9rem;color:#6b7280;line-height:1.6">%s</p>
</div>
<script>setTimeout(function(){window.close()},1200)</script>
</body></html>`, title, tone, title, body)
}
