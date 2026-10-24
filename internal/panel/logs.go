package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
)

// heartbeatInterval keeps an idle connection from being reaped by anything
// between the browser and this process. It is a comment line, so it never shows
// up as a log entry.
const heartbeatInterval = 20 * time.Second

// handleLogStream streams the process log to the console over SSE.
//
// Server-sent events rather than polling: a console that asks every five
// seconds makes the operator wait out the interval to find out whether the
// thing they just clicked did anything — and that first second is the only
// moment the log really matters. The connection is page-lifetime, so opening
// and closing the dialog does not churn it.
//
// Reconnects are cheap and duplicate-free: the SSE id carries the ring sequence
// number, and the browser replays it as Last-Event-ID, so a dropped connection
// resumes exactly where it left off.
func (c *Console) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "该服务不支持流式响应")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Buffering proxies would turn a live stream into a batch; ask them not to.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe before replaying the backlog. The other order loses whatever
	// is emitted between the snapshot and the subscription, which is exactly
	// the line the operator is waiting for.
	feed, cancel := logx.Subscribe()
	defer cancel()

	last := lastEventID(r)
	for _, rec := range logx.Since(last, logx.ReplayLines) {
		if !writeLogRecord(w, rec) {
			return
		}
		last = rec.Seq
	}
	flusher.Flush()

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case rec, open := <-feed:
			if !open {
				return
			}
			if rec.Seq <= last {
				continue
			}
			if !writeLogRecord(w, rec) {
				return
			}
			last = rec.Seq
			flusher.Flush()
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeLogRecord emits one SSE event. The id is the ring sequence, which is the
// only ordering the client can rely on after a reconnect.
func writeLogRecord(w http.ResponseWriter, rec logx.Record) bool {
	b, err := json.Marshal(rec)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", rec.Seq, b); err != nil {
		return false
	}
	return true
}

// lastEventID reads the resume cursor EventSource sends after a reconnect.
func lastEventID(r *http.Request) uint64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
