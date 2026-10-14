// Package reqlog keeps a small in-memory record of recent gateway requests for
// the console, plus a one-line summary on standard output.
//
// Records are deliberately narrow: timing, token accounting, which account and
// source served the request, and how it ended. Prompts, responses, message
// content and credentials are never recorded here.
package reqlog

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
)

// Outcome classifies how a request ended.
const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// NewRequestID returns a random, user-free identifier for one request.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// Event is one finished request.
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	Source    string    `json:"source,omitempty"`
	// Account is a display label. It never carries a full credential or token.
	Account string `json:"account,omitempty"`
	Model   string `json:"model,omitempty"`
	Stream  bool   `json:"stream,omitempty"`

	Status   int    `json:"status"`
	OK       bool   `json:"ok"`
	Outcome  string `json:"outcome"`
	Attempts int    `json:"attempts,omitempty"`

	DurationMS int64 `json:"duration_ms"`
	TTFBMS     int64 `json:"ttfb_ms,omitempty"`

	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
	// TokensPerSecond is the generation rate after the first byte.
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`
}

// Snapshot is the console view of the recorder.
type Snapshot struct {
	StartedAt     time.Time `json:"started_at"`
	InFlight      int64     `json:"in_flight"`
	Completed     int64     `json:"completed"`
	Succeeded     int64     `json:"succeeded"`
	Failed        int64     `json:"failed"`
	SuccessRate   float64   `json:"success_rate"`
	AvgDurationMS float64   `json:"avg_duration_ms"`
	AvgTTFBMS     float64   `json:"avg_ttfb_ms"`
	Recent        []Event   `json:"recent"`
}

// Recorder accumulates request statistics and the most recent events. It is
// safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	limit  int
	stdout bool
	start  time.Time

	ring []Event
	next int
	size int

	inFlight    int64
	completed   int64
	succeeded   int64
	failed      int64
	sumDuration int64
	sumTTFB     int64
	ttfbCount   int64
}

// New builds a Recorder holding up to limit recent events. A limit of zero
// falls back to 100.
func New(limit int, stdout bool) *Recorder {
	if limit <= 0 {
		limit = 100
	}
	return &Recorder{limit: limit, stdout: stdout, start: time.Now(), ring: make([]Event, limit)}
}

// Begin marks a request as in flight.
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record finishes a request: it stores the event, updates the counters and
// optionally mirrors a summary line to the process log.
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		e.Outcome = OutcomeSuccess
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	} else {
		r.failed++
	}
	r.sumDuration += e.DurationMS
	if e.TTFBMS > 0 {
		r.sumTTFB += e.TTFBMS
		r.ttfbCount++
	}
	r.ring[r.next] = e
	r.next = (r.next + 1) % r.limit
	if r.size < r.limit {
		r.size++
	}
	out := r.stdout
	r.mu.Unlock()

	if out {
		label := e.Account
		if label == "" {
			label = "-"
		}
		logx.Infof("req %s %s model=%s account=%s status=%d outcome=%s dur=%dms ttfb=%dms tok=%d%s",
			e.RequestID, e.Path, orDash(e.Model), label, e.Status, e.Outcome, e.DurationMS, e.TTFBMS, e.TotalTokens, rateSuffix(e.TokensPerSecond))
	}
}

// Snapshot returns the counters and the recent events, newest first.
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	s := Snapshot{
		StartedAt: r.start,
		InFlight:  r.inFlight,
		Completed: r.completed,
		Succeeded: r.succeeded,
		Failed:    r.failed,
		Recent:    make([]Event, 0, r.size),
	}
	if r.completed > 0 {
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.AvgDurationMS = round1(float64(r.sumDuration) / float64(r.completed))
	}
	if r.ttfbCount > 0 {
		s.AvgTTFBMS = round1(float64(r.sumTTFB) / float64(r.ttfbCount))
	}
	for i := 0; i < r.size; i++ {
		idx := (r.next - 1 - i + r.limit*2) % r.limit
		s.Recent = append(s.Recent, r.ring[idx])
	}
	return s
}

// Rate computes the generation rate from a duration and first-byte latency.
func Rate(completionTokens int64, duration, ttfb time.Duration) float64 {
	gen := duration - ttfb
	if completionTokens <= 0 || gen <= 0 {
		return 0
	}
	return round1(float64(completionTokens) / gen.Seconds())
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func rateSuffix(v float64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf(" rate=%.1ftok/s", v)
}
