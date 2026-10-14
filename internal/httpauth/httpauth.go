// Package httpauth implements bearer-token verification for the gateway API.
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// VerifyBearer reports whether r carries a valid token.
//
// An empty key disables authentication and always returns true. Comparison is
// done over SHA-256 digests with a constant-time primitive so the raw secret is
// neither leaked by timing nor stored in memory during comparison.
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	token := token(r)
	if token == "" {
		return false
	}
	want := sha256.Sum256([]byte(key))
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// token extracts the caller-provided secret from either an Authorization
// header ("Bearer <token>" or a bare token) or the Anthropic-style x-api-key.
func token(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if parts := strings.SplitN(h, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
