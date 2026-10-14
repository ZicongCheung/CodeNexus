package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrorKind classifies an upstream failure for the pool's health machine and
// for mapping back to an HTTP status.
type ErrorKind string

const (
	// KindUnknown is an unclassified failure.
	KindUnknown ErrorKind = "unknown"
	// KindRateLimited means the account hit a short-term rate limit (429).
	KindRateLimited ErrorKind = "rate_limited"
	// KindQuota means the account has exhausted its credit/quota (402).
	KindQuota ErrorKind = "quota_exhausted"
	// KindAuth means the credential is invalid or revoked (401/403).
	KindAuth ErrorKind = "auth"
	// KindServer is an upstream 5xx failure.
	KindServer ErrorKind = "server"
	// KindTimeout is a network or context timeout.
	KindTimeout ErrorKind = "timeout"
	// KindBadRequest is a request the upstream rejected (400).
	KindBadRequest ErrorKind = "bad_request"
	// KindContentBlocked means the upstream's content filter rejected it.
	KindContentBlocked ErrorKind = "content_blocked"
	// KindModelUnavailable means the requested model is not served (404).
	KindModelUnavailable ErrorKind = "model_unavailable"
	// KindSessionDead means an established upstream session became invalid.
	KindSessionDead ErrorKind = "session_dead"
)

// Error is the normalized upstream failure returned by every adapter.
type Error struct {
	Kind       ErrorKind
	Status     int
	Message    string
	RetryAfter time.Duration
}

// Error implements error.
func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upstream: %s (status %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("upstream: %s: %s", e.Kind, e.Message)
}

// Retryable reports whether retrying the same request on another account may
// succeed. Client-caused failures (bad request, content blocked) are not.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindRateLimited, KindQuota, KindServer, KindTimeout, KindSessionDead:
		return true
	default:
		return false
	}
}

// NewError builds a classified error.
func NewError(kind ErrorKind, status int, msg string) *Error {
	return &Error{Kind: kind, Status: status, Message: msg}
}

// classifyBodyKeywords inspects a response body for well-known upstream hints.
func classifyBodyKeywords(body string) ErrorKind {
	b := strings.ToLower(body)
	switch {
	case containsAny(b, "content policy", "content_filter", "content blocked", "sensitive", "risk control", "审核"):
		return KindContentBlocked
	case containsAny(b, "insufficient", "quota", "exceeded your current", "no credit", "balance"):
		return KindQuota
	case containsAny(b, "rate limit", "too many requests", "too many users", "retry later", "slow down"):
		return KindRateLimited
	case containsAny(b, "invalid api key", "unauthorized", "invalid token", "expired token"):
		return KindAuth
	case containsAny(b, "model not found", "unsupported model", "no such model", "unknown model"):
		return KindModelUnavailable
	case containsAny(b, "context length", "too long", "maximum context"):
		return KindBadRequest
	}
	return ""
}

// ClassifyHTTP maps an HTTP status and body to a classified error. Adapters use
// it as the shared baseline and may refine the returned value.
func ClassifyHTTP(status int, body string) *Error {
	if k := classifyBodyKeywords(body); k != "" {
		return NewError(k, status, trim(body))
	}
	var kind ErrorKind
	switch {
	case status == http.StatusTooManyRequests:
		kind = KindRateLimited
	case status == http.StatusPaymentRequired:
		kind = KindQuota
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = KindAuth
	case status == http.StatusNotFound:
		kind = KindModelUnavailable
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		kind = KindTimeout
	case status == http.StatusBadRequest:
		kind = KindBadRequest
	case status >= 500:
		kind = KindServer
	default:
		kind = KindUnknown
	}
	return NewError(kind, status, trim(body))
}

// Classify converts any error returned by an adapter into a classified error.
// Errors that are already *Error pass through unchanged.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var ue *Error
	if errors.As(err, &ue) {
		return ue
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return NewError(KindTimeout, 0, err.Error())
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return NewError(KindTimeout, 0, err.Error())
	}
	return NewError(KindUnknown, 0, err.Error())
}

// IsAuth reports whether err is an authentication failure, which the gateway
// may recover from by refreshing the account's token and retrying once.
func IsAuth(err error) bool {
	e := Classify(err)
	return e != nil && e.Kind == KindAuth
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
