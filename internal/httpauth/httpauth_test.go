package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(header, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

func TestEmptyKeyAllowsEverything(t *testing.T) {
	if !VerifyBearer(request("", ""), "") {
		t.Fatal("empty key should disable auth")
	}
}

func TestBearerToken(t *testing.T) {
	if !VerifyBearer(request("Authorization", "Bearer secret"), "secret") {
		t.Fatal("valid bearer rejected")
	}
	if VerifyBearer(request("Authorization", "Bearer wrong"), "secret") {
		t.Fatal("invalid bearer accepted")
	}
}

func TestBareAndAPIKey(t *testing.T) {
	if !VerifyBearer(request("Authorization", "secret"), "secret") {
		t.Fatal("bare token rejected")
	}
	if !VerifyBearer(request("x-api-key", "secret"), "secret") {
		t.Fatal("x-api-key rejected")
	}
}

func TestMissingToken(t *testing.T) {
	if VerifyBearer(request("", ""), "secret") {
		t.Fatal("missing token accepted")
	}
}
