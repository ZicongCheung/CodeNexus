package upstream

import (
	"sort"
	"strings"
)

// ToolCallTracker folds tool-call fragments into the ordinal slots they
// describe. It is the shared lower layer every wire format builds on, so none
// of them has to re-derive how a given upstream spells a call.
//
// It is deliberately tolerant. Fragments may arrive for any index and in any
// order, a call's arguments may be sent as increments (OpenAI streaming) or as
// a growing snapshot (an upstream that repeats the whole value), and either may
// repeat. Identity fields are first-wins and arguments are concatenated with a
// prefix-aware rule, so no shape double-counts:
//
//	stored="{\"ci", next="ty\":\"SF\"}"  → append "ty\":\"SF\"}"  (increment)
//	stored="ab",  next="abc"             → append "c"             (snapshot)
//	stored="ab",  next="ab"              → append ""              (duplicate)
//
// The one assumption is that an index names the same call for the whole turn,
// which every upstream observed so far respects.
type ToolCallTracker struct {
	order []int
	slots map[int]*toolSlot
}

type toolSlot struct {
	call   ToolCall
	opened bool
}

// ToolChunk is one normalized emission of a tracked call.
type ToolChunk struct {
	// Index is the ordinal slot the chunk belongs to.
	Index int
	// ID and Name are the call's identity, filled in as soon as they are known
	// and unchanged afterwards.
	ID   string
	Name string
	// Open is true on the first chunk of a slot, telling the caller to start a
	// new tool block before writing Args.
	Open bool
	// Args is the argument text added since the previous chunk for this slot.
	// It may be empty when a repeat carried nothing new, in which case Open is
	// the only thing that can make the chunk worth emitting.
	Args string
}

// NewToolCall builds a call from the pieces a tracker chunk carries. It exists
// so adapters and renderers can rebuild a call without reaching into the
// anonymous Function struct.
func NewToolCall(index int, id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function", Index: index}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// Feed folds a batch of fragments and returns the chunks worth forwarding.
// Callers that render a protocol block per call start a block on Open and
// append Args; callers that only need the finished calls call Calls instead.
func (t *ToolCallTracker) Feed(calls []ToolCall) []ToolChunk {
	var out []ToolChunk
	for _, c := range calls {
		slot := t.slot(c.Index)
		chunk := ToolChunk{Index: c.Index}
		if !slot.opened {
			slot.opened = true
			chunk.Open = true
		}
		if slot.call.Type == "" {
			slot.call.Type = firstNonBlank(c.Type, "function")
		}
		if slot.call.ID == "" {
			slot.call.ID = c.ID
		}
		if slot.call.Function.Name == "" {
			slot.call.Function.Name = c.Function.Name
		}
		chunk.ID = slot.call.ID
		chunk.Name = slot.call.Function.Name
		if delta := prefixAppend(slot.call.Function.Arguments, c.Function.Arguments); delta != "" {
			slot.call.Function.Arguments += delta
			chunk.Args = delta
		}
		if chunk.Open || chunk.Args != "" {
			out = append(out, chunk)
		}
	}
	return out
}

func (t *ToolCallTracker) slot(index int) *toolSlot {
	if t.slots == nil {
		t.slots = map[int]*toolSlot{}
	}
	if s, ok := t.slots[index]; ok {
		return s
	}
	s := &toolSlot{}
	s.call.Index = index
	t.slots[index] = s
	t.order = append(t.order, index)
	return s
}

// Calls returns the calls assembled so far, ordered by index. It is what a
// non-streaming answer or a stream summary needs.
func (t *ToolCallTracker) Calls() []ToolCall {
	idx := t.indexes()
	if len(idx) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		out = append(out, t.slots[i].call)
	}
	return out
}

// Open returns the indexes of slots that have been started, ordered by index. A
// renderer closes their protocol blocks at the end of the stream.
func (t *ToolCallTracker) Open() []int { return t.indexes() }

// indexes returns the slot indexes in ascending order. A fragment for a later
// slot may arrive first, so the appearance order is not reliable enough to
// render or close blocks in.
func (t *ToolCallTracker) indexes() []int {
	if len(t.order) == 0 {
		return nil
	}
	out := make([]int, len(t.order))
	copy(out, t.order)
	sort.Ints(out)
	return out
}

// prefixAppend returns the text next adds to prev. A value that extends prev
// contributes only its tail (a growing snapshot); anything else is an increment
// and contributes in full.
func prefixAppend(prev, next string) string {
	if next == "" || next == prev {
		return ""
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	return next
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
