package upstream

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{http.StatusTooManyRequests, KindRateLimited},
		{http.StatusPaymentRequired, KindQuota},
		{http.StatusUnauthorized, KindAuth},
		{http.StatusForbidden, KindAuth},
		{http.StatusNotFound, KindModelUnavailable},
		{http.StatusGatewayTimeout, KindTimeout},
		{http.StatusRequestTimeout, KindTimeout},
		{http.StatusInternalServerError, KindServer},
		{http.StatusBadRequest, KindBadRequest},
	}
	for _, c := range cases {
		if got := ClassifyHTTP(c.status, "").Kind; got != c.want {
			t.Errorf("status %d: got %s want %s", c.status, got, c.want)
		}
	}
}

func TestClassifyBodyKeywordsWin(t *testing.T) {
	if got := ClassifyHTTP(http.StatusBadRequest, "content policy violation").Kind; got != KindContentBlocked {
		t.Errorf("got %s want content blocked", got)
	}
	if got := ClassifyHTTP(http.StatusOK, "insufficient balance").Kind; got != KindQuota {
		t.Errorf("got %s want quota", got)
	}
	if got := ClassifyHTTP(http.StatusInternalServerError, "too many users, retry later").Kind; got != KindRateLimited {
		t.Errorf("got %s want rate limited", got)
	}
}

func TestRetryable(t *testing.T) {
	retryable := []ErrorKind{KindRateLimited, KindQuota, KindServer, KindTimeout, KindSessionDead}
	for _, k := range retryable {
		if !NewError(k, 0, "").Retryable() {
			t.Errorf("%s should be retryable", k)
		}
	}
	for _, k := range []ErrorKind{KindBadRequest, KindContentBlocked, KindAuth, KindModelUnavailable} {
		if NewError(k, 0, "").Retryable() {
			t.Errorf("%s should not be retryable", k)
		}
	}
}

func TestClassifyWrapsErrors(t *testing.T) {
	base := NewError(KindQuota, 402, "no credit")
	if got := Classify(base); got != base {
		t.Fatal("classified error should pass through")
	}
	if got := Classify(context.DeadlineExceeded); got.Kind != KindTimeout {
		t.Errorf("deadline: got %s", got.Kind)
	}
	wrapped := errors.Join(errors.New("x"), base)
	if got := Classify(wrapped); got.Kind != KindQuota {
		t.Errorf("wrapped: got %s", got.Kind)
	}
	if Classify(nil) != nil {
		t.Fatal("nil should stay nil")
	}
}
