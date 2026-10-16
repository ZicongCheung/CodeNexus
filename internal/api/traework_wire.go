package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// newHexRequestID creates the 32-hex-character identifier the client expects
// its session ids to look like.
func newHexRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// ── egress ──────────────────────────────────────────────────────────────────

// traeWorkCall is one tool call being assembled across fragments.
type traeWorkCall struct {
	ID   string
	Name string
	// Input is the cumulative arguments text, kept so the final frame can
	// report the whole call while each output frame reports only the suffix.
	Input string
}

// traeWorkResponder renders the private cumulative stream.
//
// Every frame repeats the whole answer so far rather than a delta, and the
// tool arguments travel as deltas inside those frames with a separate
// `tool_call_info` snapshot for the single-call case. Both shapes exist because
// different client builds read different fields.
type traeWorkResponder struct {
	stream    *sseStream
	model     string
	requestID string
	identity  map[string]string

	response      strings.Builder
	reasoning     strings.Builder
	reasoningSent string

	calls map[string]*traeWorkCall
	order []string

	usage      *upstream.Usage
	finish     string
	emitted    bool
	meaningful bool
	// failure records the terminal error so the done frame can repeat it.
	failure map[string]any
}

func newTraeWorkResponder(w http.ResponseWriter, model, requestID string, identity map[string]string) (responder, error) {
	stream, err := newSSEStream(w, sseDialect{
		Frame: func(upstream.StreamDelta) string { return "" }, // unused: the responder writes its own frames
		End:   func() string { return "" },
		Fail:  func(error) string { return "" },
	})
	if err != nil {
		return nil, err
	}
	return &traeWorkResponder{
		stream:    stream,
		model:     model,
		requestID: requestID,
		identity:  identity,
		calls:     map[string]*traeWorkCall{},
	}, nil
}

func (r *traeWorkResponder) write(event string, payload any) error {
	return r.stream.write(sseEvent(event, payload))
}

// Delta folds one upstream delta into the cumulative answer and emits an
// output frame when there is anything to report.
func (r *traeWorkResponder) Delta(d upstream.StreamDelta) error {
	text := d.Content
	reasoning := d.ReasoningContent
	var toolDeltas []map[string]any
	for _, call := range d.ToolCalls {
		if delta := r.mergeCall(call); delta != nil {
			toolDeltas = append(toolDeltas, delta)
		}
	}
	reasoningDelta := ""
	if reasoning != "" {
		r.reasoning.WriteString(reasoning)
		reasoningDelta = r.newReasoningSuffix()
	}
	if text != "" {
		r.response.WriteString(text)
	}
	if d.Usage != nil {
		r.usage = d.Usage
	}
	if d.FinishReason != "" {
		r.finish = d.FinishReason
	}
	// The terminal payload still produces a frame (it repeats the answer and
	// carries no finish reason of its own), and the very first delta always
	// opens the stream so a slow request is not read as an empty answer.
	if text == "" && reasoning == "" && len(toolDeltas) == 0 && d.FinishReason == "" && r.emitted {
		return nil
	}
	if text != "" || reasoning != "" || len(toolDeltas) > 0 {
		r.meaningful = true
	}
	return r.emitOutput(reasoningDelta, toolDeltas)
}

// mergeCall folds one tool fragment and returns the newly observed suffix.
func (r *traeWorkResponder) mergeCall(call upstream.ToolCall) map[string]any {
	// A fragment that carries neither an identity nor argument bytes says
	// nothing; the client only ever sends one of the two.
	if call.ID == "" && call.Function.Name == "" && call.Function.Arguments == "" {
		return nil
	}
	// Fragments are keyed by their stream index: OpenAI sends the id and name
	// only on the first one and then continues with index plus argument bytes,
	// so keying by id would strand every later fragment under a synthetic call.
	key := "index:" + strconv.Itoa(call.Index)
	current := r.calls[key]
	if current == nil {
		id := call.ID
		if id == "" {
			id = "call_" + strconv.Itoa(call.Index)
		}
		current = &traeWorkCall{ID: id}
		r.calls[key] = current
		r.order = append(r.order, key)
	}
	first := current.Name == "" && current.Input == ""
	if call.Function.Name != "" {
		current.Name = call.Function.Name
	}
	// A fragment with no argument bytes still counts as the call's opening
	// value: the client reads an empty object as "no arguments yet" only when
	// the field is present, so it is filled in rather than omitted.
	arg := call.Function.Arguments
	if arg == "" {
		arg = "{}"
	}
	delta := map[string]any{"index": call.Index}
	if first && call.ID != "" {
		delta["id"] = call.ID
	}
	if call.Function.Name != "" {
		delta["name"] = call.Function.Name
	}
	switch {
	case strings.HasPrefix(arg, current.Input):
		suffix := arg[len(current.Input):]
		current.Input = arg
		if suffix != "" {
			delta["input"] = suffix
		}
	case strings.HasPrefix(current.Input, arg):
		// A shorter cumulative snapshot is a duplicate frame.
	default:
		current.Input += arg
		delta["input"] = arg
	}
	if len(delta) == 1 { // only the index
		return nil
	}
	return delta
}

// emitOutput writes one cumulative output frame.
func (r *traeWorkResponder) emitOutput(reasoningDelta string, toolDeltas []map[string]any) error {
	output := map[string]any{
		"response": r.response.String(),
		"model":    r.model,
	}
	r.applyIdentity(output)
	if reasoningDelta != "" {
		// Kept separate from the visible answer: a client with a thinking
		// panel renders it, and one without ignores the field.
		output["reasoning_delta"] = reasoningDelta
	}
	if len(toolDeltas) > 0 {
		output["tool_calls"] = toolDeltas
		if len(toolDeltas) == 1 {
			only := toolDeltas[0]
			// The snapshot is only found when the fragment names the call.
			// Argument-only fragments therefore report their own delta, which
			// is what the client expects: this field is a delta for them and a
			// whole value for the fragment that names the call.
			var snapshot *traeWorkCall
			if id, _ := only["id"].(string); id != "" {
				for _, key := range r.order {
					if r.calls[key].ID == id {
						snapshot = r.calls[key]
						break
					}
				}
			}
			id, _ := only["id"].(string)
			name, _ := only["name"].(string)
			parameters, _ := only["input"].(string)
			if snapshot != nil {
				parameters = snapshot.Input
			}
			if first := r.firstCall(); first != nil {
				if id == "" {
					id = first.ID
				}
				if name == "" {
					name = first.Name
				}
			}
			output["tool_call_info"] = map[string]any{
				"tool_call_id": id,
				"name":         name,
				"parameters":   parameters,
			}
		}
	}
	r.emitted = true
	return r.write("output", output)
}

// End concludes the stream: an empty answer becomes an explicit error, then
// usage, then the terminal done frame.
func (r *traeWorkResponder) End() {
	if !r.meaningful {
		// A clean upstream completion with nothing in it is not a successful
		// answer; a silent done frame would surface as an empty response.
		failure := map[string]any{
			"code":       "empty_response",
			"message":    "Trae upstream returned no text or tool call",
			"request_id": r.requestID,
		}
		r.applyIdentity(failure)
		r.failure = map[string]any{
			"code":    "empty_response",
			"message": "Trae upstream returned no text or tool call",
		}
		_ = r.write("error", failure)
		_ = r.write("done", r.donePayload("error"))
		return
	}
	if r.usage != nil {
		// Emitted before the terminator so a client that closes immediately
		// still sees the accounting frame.
		_ = r.write("token_usage", r.usagePayload())
	}
	_ = r.write("done", r.donePayload("completed"))
}

// Fail reports an in-band failure after the stream had already started.
func (r *traeWorkResponder) Fail(err error) {
	payload := map[string]any{
		"code":       "upstream_error",
		"message":    err.Error(),
		"request_id": r.requestID,
	}
	r.applyIdentity(payload)
	r.failure = map[string]any{"code": "upstream_error", "message": err.Error()}
	_ = r.write("error", payload)
}

func (r *traeWorkResponder) donePayload(status string) map[string]any {
	final := map[string]any{
		"status":   status,
		"response": r.response.String(),
		"model":    r.model,
	}
	r.applyIdentity(final)
	if r.reasoning.Len() > 0 {
		// Reasoning is a presentation side channel: the summary travels once,
		// in the terminal frame, rather than with every token.
		final["reasoning_content"] = compactReasoning(r.reasoning.String())
	}
	if len(r.order) > 0 {
		final["tool_calls"] = r.cumulativeCalls()
	}
	if r.finish != "" {
		final["finish_reason"] = r.finish
	}
	if r.failure != nil {
		final["error"] = r.failure
	}
	return final
}

func (r *traeWorkResponder) usagePayload() map[string]any {
	prompt := r.usage.PromptTokens
	completion := r.usage.CompletionTokens
	total := r.usage.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	payload := map[string]any{
		"input_tokens":      prompt,
		"output_tokens":     completion,
		"total_tokens":      total,
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
	}
	r.applyIdentity(payload)
	return payload
}

// applyIdentity adds the credential-free model identity fields the client uses
// to keep its route choice.
func (r *traeWorkResponder) applyIdentity(payload map[string]any) {
	if payload["config_name"] == nil {
		if name := r.identity["config_name"]; name != "" {
			payload["config_name"] = name
		} else if r.model != "" {
			payload["config_name"] = r.model
		}
	}
	if name := r.identity["model_name"]; name != "" {
		if payload["model_name"] == nil {
			payload["model_name"] = name
		}
		if payload["provider_model_name"] == nil {
			payload["provider_model_name"] = name
		}
	}
	if provider := r.identity["provider"]; provider != "" && payload["provider"] == nil {
		payload["provider"] = provider
	}
}

// newReasoningSuffix returns the part of the compacted reasoning the client has
// not seen yet, so a client that appends never duplicates text.
func (r *traeWorkResponder) newReasoningSuffix() string {
	compact := compactReasoning(r.reasoning.String())
	if compact == "" {
		return ""
	}
	if strings.HasPrefix(compact, r.reasoningSent) {
		suffix := compact[len(r.reasoningSent):]
		r.reasoningSent = compact
		return suffix
	}
	// The summary was rewritten rather than extended; send the new lines.
	previous := map[string]bool{}
	for _, line := range strings.Split(r.reasoningSent, "\n") {
		if strings.TrimSpace(line) != "" {
			previous[strings.ToLower(line)] = true
		}
	}
	var fresh []string
	for _, line := range strings.Split(compact, "\n") {
		if strings.TrimSpace(line) == "" || previous[strings.ToLower(line)] {
			continue
		}
		fresh = append(fresh, line)
	}
	r.reasoningSent = compact
	if len(fresh) == 0 {
		return ""
	}
	out := strings.Join(fresh, "\n")
	if strings.HasPrefix(out, "\n") {
		return out
	}
	return "\n" + out
}

// compactReasoning bounds a reasoning trace to a few checkpoints.
//
// It mirrors the reference's intent (strip think tags, de-duplicate and bound
// the result) but not its sentence-level salience extraction: the summary is a
// presentation side channel, and an unbounded trace is the only thing that
// actually hurts the client.
func compactReasoning(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return ""
	}
	for _, tag := range []string{"<think", "<thinking"} {
		for strings.Contains(cleaned, tag) {
			start := strings.Index(cleaned, tag)
			end := strings.Index(cleaned[start:], ">")
			if end < 0 {
				break
			}
			cleaned = cleaned[:start] + cleaned[start+end+1:]
		}
	}
	cleaned = strings.NewReplacer("</think>", "", "</thinking>", "").Replace(cleaned)

	const maxLines, maxChars = 6, 800
	var kept []string
	seen := map[string]bool{}
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		kept = append(kept, line)
	}
	if len(kept) > maxLines {
		kept = kept[len(kept)-maxLines:]
	}
	out := strings.Join(kept, "\n")
	if len(out) > maxChars {
		out = out[len(out)-maxChars:]
	}
	return out
}

// cumulativeCalls renders the whole calls, the shape the done frame reports.
// The stream index travels only in the per-frame deltas, so it is left out
// here.
func (r *traeWorkResponder) cumulativeCalls() []map[string]any {
	calls := make([]map[string]any, 0, len(r.order))
	for _, key := range r.order {
		call := r.calls[key]
		calls = append(calls, map[string]any{
			"id":    call.ID,
			"name":  call.Name,
			"input": call.Input,
		})
	}
	return calls
}

// firstCall is the fallback identity for a fragment that does not name its own
// call.
func (r *traeWorkResponder) firstCall() *traeWorkCall {
	if len(r.order) == 0 {
		return nil
	}
	return r.calls[r.order[0]]
}
