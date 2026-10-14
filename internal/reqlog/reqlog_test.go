package reqlog

import (
	"testing"
	"time"
)

func TestRecorderCountersAndRing(t *testing.T) {
	r := New(3, false)
	for i := 0; i < 5; i++ {
		r.Begin()
		r.Record(Event{Path: "/v1/chat/completions", Status: 200, OK: true, DurationMS: 100})
	}
	s := r.Snapshot()
	if s.Completed != 5 || s.Succeeded != 5 || s.Failed != 0 {
		t.Fatalf("counters = %+v", s)
	}
	if s.InFlight != 0 {
		t.Fatalf("in-flight = %d, want 0", s.InFlight)
	}
	if len(s.Recent) != 3 {
		t.Fatalf("recent = %d, want 3 (ring bounded)", len(s.Recent))
	}
	if s.SuccessRate != 100 {
		t.Fatalf("success rate = %v, want 100", s.SuccessRate)
	}
}

func TestRecorderNewestFirst(t *testing.T) {
	r := New(5, false)
	r.Record(Event{RequestID: "a"})
	r.Record(Event{RequestID: "b"})
	r.Record(Event{RequestID: "c"})
	got := r.Snapshot().Recent
	want := []string{"c", "b", "a"}
	if len(got) != 3 {
		t.Fatalf("recent = %d", len(got))
	}
	for i, id := range want {
		if got[i].RequestID != id {
			t.Fatalf("recent[%d] = %q, want %q", i, got[i].RequestID, id)
		}
	}
}

func TestRecorderFailureRate(t *testing.T) {
	r := New(10, false)
	r.Record(Event{OK: true, DurationMS: 200})
	r.Record(Event{OK: false, Outcome: OutcomeHTTPError, Status: 502, DurationMS: 400})
	s := r.Snapshot()
	if s.Completed != 2 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("counters = %+v", s)
	}
	if s.SuccessRate != 50 {
		t.Fatalf("success rate = %v, want 50", s.SuccessRate)
	}
	if s.AvgDurationMS != 300 {
		t.Fatalf("avg duration = %v, want 300", s.AvgDurationMS)
	}
}

func TestRate(t *testing.T) {
	got := Rate(100, 2*time.Second, 500*time.Millisecond)
	if got != 66.7 {
		t.Fatalf("rate = %v, want 66.7", got)
	}
	if Rate(0, time.Second, 0) != 0 {
		t.Fatal("rate with no tokens should be 0")
	}
	if Rate(10, time.Second, 2*time.Second) != 0 {
		t.Fatal("rate with negative generation window should be 0")
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Begin()
	r.Record(Event{})
	if s := r.Snapshot(); s.Completed != 0 {
		t.Fatalf("nil recorder snapshot = %+v", s)
	}
}
