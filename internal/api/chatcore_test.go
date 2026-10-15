package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// A made-up wire format, used to prove the core knows nothing about
// chat-completions: whatever the protocol does in Result and Open, the failover
// loop, settlement and request logging stay the same.
const makeBelieveWire = "/v1/make-believe"

// retryOnce fails the first attempt and succeeds afterwards, so the failover
// path is exercised without depending on a specific account.
func retryOnce() *flakyUpstream {
	return &flakyUpstream{firstErr: upstream.NewError(upstream.KindServer, 503, "first attempt fails")}
}

// parseMakeBelieveTurn is the smallest protocol that can ride the core.
func parseMakeBelieveTurn(body map[string]any) (*turn, error) {
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	return &turn{
		Name:    "make-believe",
		Model:   model,
		Stream:  stream,
		Request: &upstream.ChatRequest{Model: model, Stream: stream, Raw: body},
		Result: func(w http.ResponseWriter, resp *upstream.ChatResponse) {
			_, _ = w.Write([]byte("<<" + resp.Content + ">>"))
		},
		Open: func(w http.ResponseWriter) (responder, error) {
			return newSSEStream(w, sseDialect{
				Frame: func(d upstream.StreamDelta) string { return sseData([]byte(`"` + d.Content + `"`)) },
				End:   func() string { return "data: <<end>>\n\n" },
				Fail:  func(err error) string { return "data: <<err>>\n\n" },
			})
		},
	}, nil
}

// serveMakeBelieve wires the made-up protocol in front of the same core the real
// routes use.
func serveMakeBelieve(s *Server, w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	t, err := parseMakeBelieveTurn(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	s.runChat(w, r, t)
}

func TestCoreServesANonStreamingWireFormat(t *testing.T) {
	s := failoverServer(t, retryOnce(), twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model"}`))
	serveMakeBelieve(s, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "<<ok>>" {
		t.Fatalf("body = %q, want the protocol's own rendering", got)
	}
}

func TestCoreServesAStreamingWireFormat(t *testing.T) {
	s := failoverServer(t, retryOnce(), twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model","stream":true}`))
	serveMakeBelieve(s, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"he"`, `"llo"`, "<<end>>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body = %q, want it to contain %q", body, want)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("the chat terminator leaked into another format: %q", body)
	}
}

// A wire format that cannot open its stream at all must fail cleanly: the client
// gets a status code, never a half-written body. Retrying elsewhere would not
// help, because the answer has already been produced and thrown away.
func TestCoreReportsACleanErrorWhenTheStreamCannotOpen(t *testing.T) {
	s := failoverServer(t, retryOnce(), twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model","stream":true}`))

	body, _ := decodeBody(req)
	tr, err := parseMakeBelieveTurn(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tr.Open = func(http.ResponseWriter) (responder, error) {
		return nil, errors.New("no flusher available")
	}
	s.runChat(rec, req, tr)

	if rec.Code < 400 {
		t.Fatalf("code = %d, want an error status rather than a partial stream", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("a failed open must not leave frames behind: %q", rec.Body.String())
	}
}

// The core, not the protocol, decides what counts as content: a delta that only
// names the turn must not become a frame, or a client would receive an event for
// something the model never produced. The stream still has to conclude cleanly.
func TestCoreDoesNotRenderABookkeepingDelta(t *testing.T) {
	only := &turnIDOnly{SourceValue: upstream.SourceWorkBuddy}
	s := failoverServer(t, only, twoAccounts())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, makeBelieveWire,
		strings.NewReader(`{"model":"fake-model","stream":true}`))

	body, _ := decodeBody(req)
	tr, err := parseMakeBelieveTurn(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s.runChat(rec, req, tr)

	got := rec.Body.String()
	if strings.Contains(got, `data: ""`) {
		t.Fatalf("the turn-id delta was rendered as content: %q", got)
	}
	if !strings.Contains(got, "<<end>>") {
		t.Fatalf("the stream did not conclude: %q", got)
	}
}

// turnIDOnly streams nothing but the turn id.
type turnIDOnly struct{ SourceValue upstream.Source }

func (u *turnIDOnly) Source() upstream.Source { return u.SourceValue }
func (u *turnIDOnly) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return []upstream.ModelSpec{{ID: "fake-model", Source: u.SourceValue}}, nil
}
func (u *turnIDOnly) Chat(context.Context, *account.Account, *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	return &upstream.ChatResponse{Content: "ok", TurnID: "turn-1"}, nil
}
func (u *turnIDOnly) ChatStream(_ context.Context, _ *account.Account, _ *upstream.ChatRequest, emit upstream.EmitFunc) error {
	return emit(upstream.StreamDelta{TurnID: "turn-1"})
}
func (u *turnIDOnly) Refresh(context.Context, *account.Account) error { return nil }
func (u *turnIDOnly) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return &upstream.Probe{Healthy: true}, nil
}
