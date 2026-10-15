package upstream

// ToolRepair names one change RepairToolPairing made to a message list.
type ToolRepair struct {
	// Index is the position in the input the repair applied to.
	Index int
	// ID is the tool-call id involved when it could be read.
	ID string
	// Reason is a short, log-safe explanation.
	Reason string
}

// RepairToolPairing drops tool results that do not answer a preceding assistant
// tool call, and reports what it dropped.
//
// A conversation that crossed a protocol boundary can carry an orphan result:
// Anthropic insists every tool_result follow its tool_use, and a client that
// edited or truncated history can lose the partner. Forwarding the orphan makes
// a strict upstream reject the whole request, so the repair drops it and lets
// the turn proceed; the drop is returned rather than swallowed so the caller
// can log it.
//
// It is conservative in exactly one direction: it only drops a result it can
// prove is orphaned. When an assistant turn asked for tools but named no ids,
// or a result carries no id, there is nothing to pair against — the message is
// kept, because silently losing a tool's output is worse than a lenient
// upstream seeing it.
func RepairToolPairing(msgs []Message) ([]Message, []ToolRepair) {
	out := make([]Message, 0, len(msgs))
	var repairs []ToolRepair
	// expecting marks that the last assistant turn asked for tools; pending is
	// the set of ids still unanswered, or nil when the turn named none.
	var expecting bool
	var pending map[string]bool

	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			expecting = len(m.ToolCalls) > 0
			pending = expectedCalls(m.ToolCalls)
			out = append(out, m)
		case "tool", "function":
			id := m.ToolCallID
			switch {
			case !expecting:
				repairs = append(repairs, ToolRepair{Index: i, ID: id, Reason: "orphan tool result"})
				continue
			case pending == nil || id == "":
				// An answer expected, but nothing to match by: keep.
			case !pending[id]:
				repairs = append(repairs, ToolRepair{Index: i, ID: id, Reason: "tool result answers no pending call"})
				continue
			default:
				delete(pending, id)
			}
			out = append(out, m)
		default:
			expecting = false
			pending = nil
			out = append(out, m)
		}
	}
	return out, repairs
}

// expectedCalls lists the ids an assistant turn asked for. It returns nil when
// no call carried an id, so a client that omits ids never has its results
// dropped by mistake.
func expectedCalls(calls []ToolCall) map[string]bool {
	var ids map[string]bool
	for _, c := range calls {
		if c.ID == "" {
			continue
		}
		if ids == nil {
			ids = map[string]bool{}
		}
		ids[c.ID] = true
	}
	return ids
}
