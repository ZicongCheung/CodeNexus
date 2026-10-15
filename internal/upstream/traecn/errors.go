package traecn

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// Upstream business codes that need a specific reaction. The upstream reports
// failures inside a 200 response as often as through the HTTP status, so the
// adapter maps codes rather than trusting the status alone.
const (
	// codeDeviceThrottled is the device-scoped throttling code. Retrying
	// immediately on the same device cannot succeed.
	codeDeviceThrottled = "9074"
	// codeQuotaExhausted means the account's credits are spent.
	codeQuotaExhausted = "4008"
	// codeUnauthenticated means the credential was rejected server-side, which
	// the gateway recovers from by refreshing and retrying once.
	codeUnauthenticated = "1001"
	// codeParallelLimit means the account is already serving its concurrent
	// limit; another account can take the request.
	codeParallelLimit = "solo_agent_parallel_limit"
)

// remoteErrorCode scans a raw response for a business code.
func remoteErrorCode(raw []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if code := remoteErrorCodeOf(obj); code != "" {
		return code
	}
	if nested, ok := obj["ResponseMetadata"].(map[string]any); ok {
		if e, ok := nested["Error"].(map[string]any); ok {
			return firstString(e, "Code", "code")
		}
	}
	return ""
}

// remoteErrorCodeOf reads a business code out of an already-decoded event or
// response body.
func remoteErrorCodeOf(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if code := firstString(obj, "code", "Code", "code_num", "error_code", "errno"); code != "" {
		return code
	}
	if e, ok := obj["error"].(map[string]any); ok {
		return firstString(e, "code", "Code", "type")
	}
	if e, ok := obj["data"].(map[string]any); ok {
		return firstString(e, "code", "Code")
	}
	return ""
}

// remoteError builds a classified error from the HTTP status, a business code
// and the raw body. A code wins over the status because the upstream mixes the
// two; when neither is specific the shared keyword classifier decides, and a
// wholly unrecognised in-band failure is treated as a server failure so a
// transient upstream hiccup stays retryable.
func remoteError(status int, code, raw string) *upstream.Error {
	msg := "trae: " + remoteErrorMessage(raw)
	switch strings.TrimSpace(code) {
	case codeUnauthenticated:
		return upstream.NewError(upstream.KindAuth, status, msg)
	case codeQuotaExhausted:
		return upstream.NewError(upstream.KindQuota, status, msg)
	case codeDeviceThrottled:
		return upstream.NewError(upstream.KindRateLimited, status, msg)
	case codeParallelLimit:
		return upstream.NewError(upstream.KindRateLimited, status, msg)
	case "4028":
		// Specific models reject this executor; the other surface may serve
		// the request, so it stays retryable.
		return upstream.NewError(upstream.KindServer, status, msg)
	}
	e := upstream.ClassifyHTTP(status, raw)
	if e.Message == "" {
		e.Message = msg
	} else {
		e.Message = "trae: " + e.Message
	}
	if e.Kind == upstream.KindUnknown && status == 0 {
		e.Kind = upstream.KindServer
	}
	return e
}

// remoteErrorMessage pulls the most descriptive message out of a raw payload.
func remoteErrorMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "upstream error"
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		if len(raw) > 256 {
			return raw[:256]
		}
		return raw
	}
	if msg := firstString(obj, "message", "msg", "detail", "error"); msg != "" {
		return msg
	}
	if e, ok := obj["error"].(map[string]any); ok {
		if msg := firstString(e, "message", "msg", "detail"); msg != "" {
			return msg
		}
	}
	if nested, ok := obj["ResponseMetadata"].(map[string]any); ok {
		if e, ok := nested["Error"].(map[string]any); ok {
			if msg := firstString(e, "Message", "message"); msg != "" {
				return msg
			}
		}
	}
	return "upstream error"
}

// parseAmount reads a decimal number that may arrive as a JSON number or a
// string, and truncates a fractional part rather than failing on it.
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return strconv.ParseInt(s, 10, 64)
}
