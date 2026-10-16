// Package logx is a tiny leveled logger used across CodeNexus.
//
// It intentionally depends only on the standard library so the gateway stays
// dependency-free and trivially portable.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Level is a log severity.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

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

func logf(l Level, tag, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	std.Load().Printf("[%s] %s", tag, fmt.Sprintf(format, args...))
}

// Debugf logs at debug level.
func Debugf(format string, args ...any) { logf(LevelDebug, "debug", format, args...) }

// Infof logs at info level.
func Infof(format string, args ...any) { logf(LevelInfo, "info", format, args...) }

// Warnf logs at warn level.
func Warnf(format string, args ...any) { logf(LevelWarn, "warn", format, args...) }

// Errorf logs at error level.
func Errorf(format string, args ...any) { logf(LevelError, "error", format, args...) }
