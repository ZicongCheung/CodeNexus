package traecn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The account credential is a Cloud-IDE-JWT: a JWT issued by the web login or
// exchanged from a refresh token. Nothing about its expiry is derivable
// locally — the upstream reports it, and the refresh token is what keeps an
// account alive without another interactive login.

// TokenScheme is the Authorization scheme the upstream expects.
const TokenScheme = "Cloud-IDE-JWT"

// exchangePath renews a Cloud-IDE-JWT from a refresh token.
const exchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"

// oauthClientSecret is the placeholder the desktop client sends. The upstream
// does not validate it for this client id.
const oauthClientSecret = "-"

// ErrNoCredential reports an account that cannot authenticate at all.
var ErrNoCredential = errors.New("trae: no usable credential")

// LooksLikeToken reports whether s has the shape of a Cloud-IDE-JWT. The
// upstream rejects anything else outright, so catching it locally turns a
// confusing upstream error into a clear one.
func LooksLikeToken(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 128 {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// TokenUserID extracts the account identity from a Cloud-IDE-JWT payload. The
// upstream nests it, so several shapes are accepted.
func TokenUserID(token string) string {
	claims := tokenClaims(token)
	if claims == nil {
		return ""
	}
	for _, path := range [][]string{
		{"data", "id"},
		{"data", "user_id"},
		{"data", "userId"},
		{"userId"},
		{"user_id"},
		{"sub"},
	} {
		if v := nestedString(claims, path...); v != "" {
			return v
		}
	}
	return ""
}

// tokenClaims decodes the payload segment of a JWT. A malformed token yields
// nil rather than an error: the identity is a convenience, not a requirement.
func tokenClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// decodeBase64URL tolerates both padded and unpadded input, because the two
// appear interchangeably across clients.
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func nestedString(m map[string]any, path ...string) string {
	cur := any(m)
	for _, key := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[key]
	}
	switch v := cur.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	}
	return ""
}

// Refresh implements upstream.Upstream. An account without a refresh token
// cannot be renewed silently; the caller reports it as needing a new login.
//
// In cli mode there is nothing to renew: the local CLI holds its own session, so
// a refresh is a no-op rather than an auth failure that would disable the entry.
func (c *Client) Refresh(ctx context.Context, acc *account.Account) error {
	if acc == nil {
		return ErrNoCredential
	}
	if c.cfg.UpstreamMode == "cli" {
		return nil
	}
	rt := strings.TrimSpace(acc.RefreshTokenValue())
	if rt == "" {
		return upstream.NewError(upstream.KindAuth, 0, "trae: no refresh token; sign in again")
	}
	tok, newRT, expiresAt, err := c.exchangeToken(ctx, rt, acc.UID)
	if err != nil {
		return err
	}
	acc.SetCredentials(tok, newRT, "", expiresAt)
	return nil
}

// exchangeResult mirrors the token exchange response, tolerating the casing
// variations the upstream has shipped.
type exchangeResult struct {
	Result struct {
		Token         string `json:"Token"`
		RefreshToken  string `json:"RefreshToken"`
		TokenExpireAt any    `json:"TokenExpireAt"`
	} `json:"Result"`
	ResponseMetadata struct {
		Error struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
}

// exchangeToken trades a refresh token for a fresh Cloud-IDE-JWT.
func (c *Client) exchangeToken(ctx context.Context, refreshToken, userID string) (string, string, int64, error) {
	payload := map[string]any{
		"ClientID":     c.cfg.ClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": oauthClientSecret,
		"UserID":       userID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", 0, err
	}
	endpoint := strings.TrimRight(c.cfg.AuthBaseURL, "/") + exchangePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", 0, upstream.Classify(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", "", 0, upstream.ClassifyHTTP(resp.StatusCode, string(raw))
	}
	var out exchangeResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", 0, fmt.Errorf("trae: token exchange: %w", err)
	}
	token := strings.TrimSpace(out.Result.Token)
	if token == "" {
		msg := strings.TrimSpace(out.ResponseMetadata.Error.Message)
		if msg == "" {
			msg = "token exchange returned no token"
		}
		return "", "", 0, upstream.NewError(upstream.KindAuth, 0, "trae: "+msg)
	}
	return token, strings.TrimSpace(out.Result.RefreshToken), parseTimestamp(out.Result.TokenExpireAt), nil
}

// parseTimestamp accepts the shapes an upstream expiry field arrives in: unix
// seconds, unix milliseconds, a numeric string, or an RFC3339 string. Anything
// unrecognised becomes 0, which callers read as "unknown", never as "expired".
func parseTimestamp(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return normalizeUnix(int64(t))
	case int64:
		return normalizeUnix(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return normalizeUnix(n)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeUnix(n)
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.Unix()
			}
		}
	}
	return 0
}

// normalizeUnix converts milliseconds to seconds so callers only ever compare
// against time.Now().Unix().
func normalizeUnix(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 1e12 {
		return n / 1000
	}
	return n
}

// randomHex returns n bytes of lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// randomDigits returns a decimal string of length n.
func randomDigits(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			b.WriteByte('0')
			continue
		}
		b.WriteByte(byte('0' + v.Int64()))
	}
	return b.String()
}

// newUUID returns a random RFC 4122 version 4 identifier, used for the trace
// id the authorization page round-trips back to us.
// newHexID returns a 32-character hex id, the form the native client sends in
// both request_id and session_id.
func newHexID() string { return strings.ReplaceAll(newUUID(), "-", "") }

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return randomDigits(32)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
