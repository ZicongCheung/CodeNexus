// Package logx is a tiny leveled logger used across CodeNexus.
//
// It intentionally depends only on the standard library so the gateway stays
// dependency-free and trivially portable.
//
// Besides writing to stdout or a file, every record also lands in a bounded
// in-memory ring that the console streams to the browser over SSE. The ring is
// why the channel rides on the record instead of being parsed back out of the
// text: a console that guesses "is this a task line" from a prefix goes quietly
// wrong the first time a message is reworded.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level is a log severity.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String is the level as it appears in a log line and on the wire.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// Channel groups a record for the console's log view.
//
// The three channels answer different questions, which is why they are not a
// severity: a task failure and a chat failure both deserve attention, but an
// operator chasing one does not want the other scrolling past.
type Channel string

const (
	// ChannelSystem is process lifecycle, configuration and anything unclaimed.
	ChannelSystem Channel = "sys"
	// ChannelTask is the scheduler: check-in, growth, travel, balance sweeps.
	ChannelTask Channel = "task"
	// ChannelDialog is the request path: routing, failover, stream stalls,
	// auto-continue rounds. It deliberately carries no token accounting — the
	// usage page owns that.
	ChannelDialog Channel = "chat"
)

const (
	// ringCapacity bounds the in-memory history. A long-running process under
	// steady traffic would otherwise grow without limit for a console nobody
	// may open.
	ringCapacity = 2000
	// ReplayLines is how much history a fresh console connection receives
	// before the live stream takes over. The console keeps more than this; the
	// cap is about the first paint after a long night, not about memory.
	ReplayLines = 500
)

// Record is one log line as the console sees it.
type Record struct {
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Channel Channel   `json:"channel"`
	Text    string    `json:"text"`
}

// stream is the ring plus its live subscribers.
type stream struct {
	mu   sync.Mutex
	buf  []Record
	seq  uint64
	subs map[chan Record]struct{}
}

func newStream() *stream {
	return &stream{
		buf:  make([]Record, 0, ringCapacity),
		subs: map[chan Record]struct{}{},
	}
}

// emit appends one record, evicting the oldest once the ring is full, and fans
// it out to every subscriber.
//
// Delivery is best-effort: a subscriber that stops draining loses lines instead
// of stalling the gateway. Logging must never be able to block a request.
func (s *stream) emit(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.Seq = s.seq
	if len(s.buf) >= ringCapacity {
		copy(s.buf, s.buf[1:])
		s.buf[len(s.buf)-1] = r
	} else {
		s.buf = append(s.buf, r)
	}
	for ch := range s.subs {
		select {
		case ch <- r:
		default:
		}
	}
}

// since returns the records newer than seq, capped at the newest limit.
func (s *stream) since(seq uint64, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	for start < len(s.buf) && s.buf[start].Seq <= seq {
		start++
	}
	out := s.buf[start:]
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	dup := make([]Record, len(out))
	copy(dup, out)
	return dup
}

func (s *stream) subscribe() (<-chan Record, func()) {
	ch := make(chan Record, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		// Deleted under the same lock emit holds, so no send can race the
		// close.
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

var live = newStream()

// Recent returns up to n of the newest records, oldest first. n <= 0 returns
// the whole ring.
func Recent(n int) []Record { return live.since(0, n) }

// Since returns up to n records newer than seq. A reconnecting client passes
// the last sequence it saw, so a reconnect costs no duplicates.
func Since(seq uint64, n int) []Record { return live.since(seq, n) }

// Subscribe returns a live feed of new records and the function that stops it.
// The caller must call the returned cancel when it is done, or the ring keeps a
// channel nobody drains.
func Subscribe() (<-chan Record, func()) { return live.subscribe() }

// Reset clears the ring. It exists for tests.
func Reset() {
	live.mu.Lock()
	defer live.mu.Unlock()
	live.buf = live.buf[:0]
}

var (
	current atomic.Int32
	std     atomic.Pointer[log.Logger]
)

func init() {
	current.Store(int32(LevelInfo))
	std.Store(log.New(os.Stdout, "", log.LstdFlags))
}

// SetOutput redirects the log. The windowed build has no console to write to,
// so it points the log at a file instead; without this the operator running the
// tray build would have no way to see what the gateway is doing.
func SetOutput(w io.Writer) { std.Store(log.New(w, "", log.LstdFlags)) }

// ParseLevel maps a case-insensitive name to a Level, defaulting to info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// SetLevel sets the global minimum level.
func SetLevel(l Level) { current.Store(int32(l)) }

// Enabled reports whether messages at level l are emitted.
func Enabled(l Level) bool { return l >= Level(current.Load()) }

func logf(l Level, ch Channel, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	std.Load().Printf("[%s] %s", l.String(), msg)
	live.emit(Record{At: time.Now(), Level: l.String(), Channel: ch, Text: clip(msg)})
}

// maxRecordRunes bounds one record in the ring. Upstreams answer 401s with a
// whole HTML page; keeping that verbatim would cost memory in the ring and
// eight blank-looking rows in the console. The file on disk still has it all.
const maxRecordRunes = 2000

// clip makes one record fit on one line. The console draws a terminal, one
// record per row: a message that arrives with newlines in it would either break
// the row or render as several unrelated entries.
func clip(msg string) string {
	msg = strings.ReplaceAll(strings.TrimRight(msg, "\r\n"), "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\n", " ")
	if r := []rune(msg); len(r) > maxRecordRunes {
		return string(r[:maxRecordRunes]) + "…"
	}
	return msg
}

// Debugf logs at debug level.
func Debugf(format string, args ...any) { logf(LevelDebug, ChannelSystem, format, args...) }

// Infof logs at info level.
func Infof(format string, args ...any) { logf(LevelInfo, ChannelSystem, format, args...) }

// Warnf logs at warn level.
func Warnf(format string, args ...any) { logf(LevelWarn, ChannelSystem, format, args...) }

// Errorf logs at error level.
func Errorf(format string, args ...any) { logf(LevelError, ChannelSystem, format, args...) }

// channelLogger gives a category to a group of call sites without threading a
// channel through every function in between.
//
//	logx.Task.Infof("checkin %s: ok", key)
type channelLogger struct{ ch Channel }

// Task writes to the scheduler channel.
var Task = channelLogger{ch: ChannelTask}

// Dialog writes to the request-path channel.
var Dialog = channelLogger{ch: ChannelDialog}

func (w channelLogger) Debugf(format string, args ...any) {
	logf(LevelDebug, w.ch, format, args...)
}

func (w channelLogger) Infof(format string, args ...any) {
	logf(LevelInfo, w.ch, format, args...)
}

func (w channelLogger) Warnf(format string, args ...any) {
	logf(LevelWarn, w.ch, format, args...)
}

func (w channelLogger) Errorf(format string, args ...any) {
	logf(LevelError, w.ch, format, args...)
}
