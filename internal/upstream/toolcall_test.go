package upstream

import "testing"

// frag builds a raw fragment: an increment carries only the arguments, while a
// whole call carries id and name too.
func frag(index int, id, name, args string) ToolCall {
	tc := ToolCall{Index: index}
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func TestTrackerConcatenatesIncrementalArguments(t *testing.T) {
	var tr ToolCallTracker
	chunks := tr.Feed([]ToolCall{frag(0, "call_1", "get_weather", `{"ci`)})
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if !chunks[0].Open {
		t.Fatal("the first chunk must open the slot")
	}
	if chunks[0].ID != "call_1" || chunks[0].Name != "get_weather" {
		t.Fatalf("identity = %q/%q", chunks[0].ID, chunks[0].Name)
	}
	if chunks[0].Args != `{"ci` {
		t.Fatalf("first args = %q", chunks[0].Args)
	}

	chunks = tr.Feed([]ToolCall{frag(0, "", "", `ty":"SF"}`)})
	if len(chunks) != 1 || chunks[0].Open {
		t.Fatalf("continuation chunks = %+v", chunks)
	}
	if chunks[0].Args != `ty":"SF"}` {
		t.Fatalf("continuation args = %q", chunks[0].Args)
	}
	if got := tr.Calls()[0].Function.Arguments; got != `{"city":"SF"}` {
		t.Fatalf("assembled arguments = %q", got)
	}
}

func TestTrackerFoldsAGrowingSnapshotOnce(t *testing.T) {
	var tr ToolCallTracker
	tr.Feed([]ToolCall{frag(0, "", "f", "ab")})
	chunks := tr.Feed([]ToolCall{frag(0, "", "f", "abc")})
	if len(chunks) != 1 || chunks[0].Args != "c" {
		t.Fatalf("snapshot increment = %+v", chunks)
	}
	if got := tr.Calls()[0].Function.Arguments; got != "abc" {
		t.Fatalf("arguments = %q, want abc (no double count)", got)
	}
}

func TestTrackerIgnoresARepeatedSnapshot(t *testing.T) {
	var tr ToolCallTracker
	tr.Feed([]ToolCall{frag(0, "call_1", "f", "{}")})
	if chunks := tr.Feed([]ToolCall{frag(0, "call_1", "f", "{}")}); len(chunks) != 0 {
		t.Fatalf("a repeat must emit nothing, got %+v", chunks)
	}
	if calls := tr.Calls(); len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
}

func TestTrackerOrdersSlotsEvenWhenFragmentsArriveLate(t *testing.T) {
	var tr ToolCallTracker
	tr.Feed([]ToolCall{frag(1, "b", "second", "{}")})
	tr.Feed([]ToolCall{frag(0, "a", "first", "{}")})
	calls := tr.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Index != 0 || calls[0].ID != "a" || calls[1].Index != 1 || calls[1].ID != "b" {
		t.Fatalf("calls out of order: %+v", calls)
	}
	if open := tr.Open(); len(open) != 2 || open[0] != 0 || open[1] != 1 {
		t.Fatalf("open = %v", open)
	}
}

func TestTrackerKeepsTheFirstIdentityItSees(t *testing.T) {
	var tr ToolCallTracker
	tr.Feed([]ToolCall{frag(0, "call_1", "nameA", "")})
	tr.Feed([]ToolCall{frag(0, "call_other", "nameB", "{}")})
	call := tr.Calls()[0]
	if call.ID != "call_1" || call.Function.Name != "nameA" {
		t.Fatalf("identity was overwritten: %+v", call)
	}
	if call.Function.Arguments != "{}" {
		t.Fatalf("arguments = %q", call.Function.Arguments)
	}
}

func TestNewToolCallFillsTheType(t *testing.T) {
	tc := NewToolCall(2, "x", "y", "z")
	if tc.Index != 2 || tc.ID != "x" || tc.Type != "function" ||
		tc.Function.Name != "y" || tc.Function.Arguments != "z" {
		t.Fatalf("call = %+v", tc)
	}
}
