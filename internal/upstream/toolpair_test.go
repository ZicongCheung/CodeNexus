package upstream

import "testing"

func assistantWithCalls(ids ...string) Message {
	m := Message{Role: "assistant"}
	for i, id := range ids {
		m.ToolCalls = append(m.ToolCalls, NewToolCall(i, id, "f", "{}"))
	}
	return m
}

func toolResult(id string) Message { return Message{Role: "tool", ToolCallID: id, Content: "ok"} }

func TestRepairDropsAnOrphanResult(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}, toolResult("ghost")}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 1 {
		t.Fatalf("messages = %d, want the orphan dropped", len(out))
	}
	if len(repairs) != 1 || repairs[0].ID != "ghost" {
		t.Fatalf("repairs = %+v", repairs)
	}
}

func TestRepairKeepsAProperlyPairedResult(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}, assistantWithCalls("call_1"), toolResult("call_1")}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 3 || len(repairs) != 0 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}

func TestRepairDropsAResultThatAnswersNothingPending(t *testing.T) {
	msgs := []Message{assistantWithCalls("call_1"), toolResult("call_2")}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 1 || len(repairs) != 1 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}

func TestRepairDropsADuplicateResult(t *testing.T) {
	msgs := []Message{assistantWithCalls("call_1"), toolResult("call_1"), toolResult("call_1")}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 2 || len(repairs) != 1 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}

func TestRepairKeepsResultsWhenTheCallNamedNoID(t *testing.T) {
	// The assistant asked for a tool but every call is id-less, so there is
	// nothing to pair by: keep the result rather than lose it.
	msgs := []Message{{Role: "assistant", ToolCalls: []ToolCall{NewToolCall(0, "", "f", "{}")}}, toolResult("")}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 2 || len(repairs) != 0 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}

func TestRepairTreatsAUserTurnAsBreakingTheChain(t *testing.T) {
	msgs := []Message{
		assistantWithCalls("call_1"),
		{Role: "user", Content: "never mind"},
		toolResult("call_1"),
	}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 2 || len(repairs) != 1 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}

func TestRepairKeepsMessagesItDoesNotUnderstand(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "be nice"}, {Role: "user", Content: "hi"}}
	out, repairs := RepairToolPairing(msgs)
	if len(out) != 2 || len(repairs) != 0 {
		t.Fatalf("out = %d, repairs = %+v", len(out), repairs)
	}
}
