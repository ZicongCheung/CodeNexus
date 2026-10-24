package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
)

// streamLogs runs one request through the handler with the context already
// cancelled. The replay happens before the handler ever waits on the context,
// so the body is complete and deterministic by the time this returns.
func streamLogs(t *testing.T, lastEventID string) string {
	t.Helper()
	c := &Console{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/logs/stream", nil).WithContext(ctx)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := httptest.NewRecorder()
	cancel()
	c.handleLogStream(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	return rec.Body.String()
}

// dataPayloads pulls the JSON out of the event stream, in order.
func dataPayloads(t *testing.T, body string) []logx.Record {
	t.Helper()
	var out []logx.Record
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var rec logx.Record
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rec); err != nil {
			t.Fatalf("bad data line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestLogStreamReplaysHistoryWithChannelAndLevel(t *testing.T) {
	logx.Reset()
	logx.Task.Infof("checkin alice: ok")
	logx.Dialog.Warnf("chat attempt 1/3 failed: boom")

	body := streamLogs(t, "")
	got := dataPayloads(t, body)
	if len(got) != 2 {
		t.Fatalf("replayed %d records, want 2", len(got))
	}
	if got[0].Channel != logx.ChannelTask || got[0].Level != "info" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Channel != logx.ChannelDialog || got[1].Level != "warn" {
		t.Fatalf("second = %+v", got[1])
	}
	// The id must be the ring sequence, or a reconnect cannot resume.
	if !strings.Contains(body, "id: "+strconv.FormatUint(got[0].Seq, 10)+"\n") {
		t.Fatalf("event id missing from stream:\n%s", body)
	}
}

// A reconnect sends Last-Event-ID, and the client must not see the lines it
// already drew. Without this the console doubles every line on every blip.
func TestLogStreamResumesAfterLastEventID(t *testing.T) {
	logx.Reset()
	logx.Task.Infof("one")
	logx.Task.Infof("two")
	logx.Task.Infof("three")

	seen := dataPayloads(t, streamLogs(t, ""))
	if len(seen) != 3 {
		t.Fatalf("got %d records, want 3", len(seen))
	}
	body := streamLogs(t, strconv.FormatUint(seen[1].Seq, 10))
	got := dataPayloads(t, body)
	if len(got) != 1 {
		t.Fatalf("resume delivered %d records, want 1: %s", len(got), body)
	}
	if got[0].Seq != seen[2].Seq {
		t.Fatalf("resumed at %d, want %d", got[0].Seq, seen[2].Seq)
	}
}

func TestLogStreamIgnoresGarbageResumeCursor(t *testing.T) {
	logx.Reset()
	logx.Task.Infof("only line")
	// A cursor that does not parse must fall back to the full replay rather
	// than silently sending nothing.
	if got := dataPayloads(t, streamLogs(t, "not-a-number")); len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
}
