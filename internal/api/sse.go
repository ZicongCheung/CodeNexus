package api

// Server-sent-event plumbing shared by every wire format.
//
// The status line is written lazily, on the first event. That is what keeps a
// failure before the first byte retryable on another account: the client has
// not seen a 200 yet, so the gateway is still free to answer from somewhere
// else. Everything after that point is committed, so a failure is reported
// in-band rather than by changing the status code.

import (
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// sseDialect is what one wire format contributes to the shared transport.
type sseDialect struct {
	// Frame renders one delta as the protocol's event text.
	Frame func(upstream.StreamDelta) string
	// End renders the terminator of a successful answer, if the protocol has
	// one. Anthropic messages simply stop; chat-completions sends [DONE].
	End func() string
	// Fail renders an in-band failure. It must include whatever terminator the
	// protocol expects after an error.
	Fail func(error) string
}

// sseStream writes SSE frames over a response, opening it on first use.
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	dialect sseDialect
	started bool
}

func newSSEStream(w http.ResponseWriter, dialect sseDialect) (*sseStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}
	return &sseStream{w: w, flusher: flusher, dialect: dialect}, nil
}

func (s *sseStream) begin() {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseStream) write(text string) error {
	if text == "" {
		return nil
	}
	if !s.started {
		s.started = true
		s.begin()
	}
	if _, err := io.WriteString(s.w, text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *sseStream) Delta(d upstream.StreamDelta) error { return s.write(s.dialect.Frame(d)) }

func (s *sseStream) End() {
	if s.dialect.End != nil {
		_ = s.write(s.dialect.End())
	}
}

func (s *sseStream) Fail(err error) {
	if s.dialect.Fail != nil {
		_ = s.write(s.dialect.Fail(err))
	}
}

// sseEvent renders one named SSE event. Anthropic's stream uses names; the
// OpenAI formats send anonymous data frames.
func sseEvent(name string, payload any) string {
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	if name == "" {
		return "data: " + string(b) + "\n\n"
	}
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}

// sseData renders an anonymous data frame from bytes that are already JSON.
func sseData(payload []byte) string { return "data: " + string(payload) + "\n\n" }

// wireID derives the response id a wire format reports back to its client. It
// is content-free: an upstream id is reused when it exists, and a stable hash
// of the request id is used otherwise so a client can correlate a stream with
// its own retries.
func wireID(prefix, upstreamID, requestID string) string {
	if upstreamID != "" {
		return upstreamID
	}
	h := fnv.New32a()
	_, _ = io.WriteString(h, requestID)
	return prefix + "-" + strconv.FormatUint(uint64(h.Sum32()), 10)
}
