package reqlog

import (
	"fmt"
	"sync"
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

// ── out-of-band settlement updates ───────────────────────────────────────────

func TestUpdateRewritesARecordedEvent(t *testing.T) {
	r := New(4, false)
	r.Record(Event{RequestID: "req-1", Path: "/v1/messages"})
	r.Update("req-1", func(e *Event) {
		e.CreditsConsumed = 1.25
		e.CreditsSource = "session_usage"
	})

	got := r.Snapshot().Recent
	if len(got) != 1 {
		t.Fatalf("recent = %d, want 1", len(got))
	}
	if got[0].CreditsConsumed != 1.25 || got[0].CreditsSource != "session_usage" {
		t.Fatalf("settlement was not applied: %+v", got[0])
	}
	if len(r.pending) != 0 {
		t.Fatalf("pending = %d, want 0 (the event was there)", len(r.pending))
	}
}

// A settlement can finish before the middleware writes the record; the mutation
// must then be applied the moment the record lands.
func TestUpdateHeldUntilTheEventArrives(t *testing.T) {
	r := New(4, false)
	r.Update("req-late", func(e *Event) {
		e.CreditsConsumed = 2
		e.CreditsSource = "session_usage"
	})
	if len(r.pending) != 1 {
		t.Fatalf("pending = %d, want 1 (held for the unreached record)", len(r.pending))
	}
	if s := r.Snapshot(); len(s.Recent) != 0 {
		t.Fatalf("nothing has been recorded yet: %d", len(s.Recent))
	}

	r.Record(Event{RequestID: "req-late", Path: "/v1/messages"})
	got := r.Snapshot().Recent
	if len(got) != 1 || got[0].CreditsConsumed != 2 {
		t.Fatalf("the held mutation was not applied: %+v", got)
	}
	if len(r.pending) != 0 {
		t.Fatalf("pending = %d, want 0 (consumed on arrival)", len(r.pending))
	}
}

func TestUpdateOnlyTouchesItsOwnEvent(t *testing.T) {
	r := New(4, false)
	r.Record(Event{RequestID: "req-1"})
	r.Record(Event{RequestID: "req-2"})
	r.Update("req-1", func(e *Event) { e.CreditsConsumed = 5 })

	recent := r.Snapshot().Recent
	if recent[0].RequestID != "req-2" || recent[0].CreditsConsumed != 0 {
		t.Fatalf("the newest event must be untouched: %+v", recent[0])
	}
	if recent[1].CreditsConsumed != 5 {
		t.Fatalf("the addressed event was not updated: %+v", recent[1])
	}
}

func TestUpdateIgnoresEmptyIDAndNilFunc(t *testing.T) {
	r := New(4, false)
	r.Record(Event{RequestID: "req-1"})
	r.Update("", func(e *Event) { e.CreditsConsumed = 9 })
	r.Update("req-1", nil)
	if len(r.pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(r.pending))
	}
	if got := r.Snapshot().Recent[0].CreditsConsumed; got != 0 {
		t.Fatalf("credits = %v, want 0", got)
	}
}

// Held mutations are bounded: a settlement whose record never arrives must not
// grow the map without limit.
func TestUpdateHeldMutationsAreBounded(t *testing.T) {
	r := New(4, false)
	for i := 0; i < maxPendingUpdates*3; i++ {
		r.Update(fmt.Sprintf("req-%d", i), func(e *Event) { e.CreditsConsumed = 1 })
	}
	if len(r.pending) > maxPendingUpdates {
		t.Fatalf("pending = %d, want at most %d", len(r.pending), maxPendingUpdates)
	}
	// Whatever survived must still apply cleanly.
	r.Record(Event{RequestID: fmt.Sprintf("req-%d", maxPendingUpdates*3-1)})
}

// An evicted event is no longer addressable, so the mutation is held instead of
// panicking on a stale slot.
func TestUpdateAfterEvictionIsHeld(t *testing.T) {
	r := New(2, false)
	r.Record(Event{RequestID: "old"})
	r.Record(Event{RequestID: "mid"})
	r.Record(Event{RequestID: "new"}) // "old" is pushed out
	r.Update("old", func(e *Event) { e.CreditsConsumed = 1 })
	if len(r.pending) != 1 {
		t.Fatalf("pending = %d, want 1 (the record was evicted)", len(r.pending))
	}
}

func TestUpdateIsSafeUnderConcurrency(t *testing.T) {
	const n = 16
	r := New(2*n, false)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			r.Begin()
			r.Record(Event{RequestID: id, OK: true})
			r.Update(id, func(e *Event) {
				e.CreditsConsumed = 0.5
				e.CreditsSource = "session_usage"
			})
		}(i)
	}
	wg.Wait()

	recent := r.Snapshot().Recent
	if len(recent) != n {
		t.Fatalf("recent = %d, want %d", len(recent), n)
	}
	for _, e := range recent {
		if e.CreditsConsumed != 0.5 || e.CreditsSource != "session_usage" {
			t.Fatalf("event %s was not updated: %+v", e.RequestID, e)
		}
	}
}
