package api

import "encoding/json"

// Token accounting for the Anthropic count_tokens endpoint. Anthropic counts
// with a proprietary tokenizer that a relay has no access to, so the numbers
// here are estimates. They exist to let a client budget a context window, which
// an estimate satisfies; they are never used for billing. The coefficients and
// the flat per-image cost follow the reference relay so behaviour matches.

// estimateTokens approximates the token count of one run of text. Wide
// characters (CJK and the like) cost far more per character than ASCII, so the
// two are weighted differently; the result is at least one for any non-empty
// input.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	total := 0.0
	for _, r := range text {
		if r > 0x2000 {
			total += 1.5
		} else {
			total += 0.25
		}
	}
	return max(1, int(total+0.999))
}

// estimateImageTokens is the flat per-image cost. Anthropic's real cost depends
// on dimensions and model tiling; a bounded constant avoids charging the whole
// base64 payload as text and keeps the estimate in the right order of
// magnitude.
const estimateImageTokens = 1000

// anthropicInputEstimate estimates the input tokens of a raw Anthropic request.
func anthropicInputEstimate(body map[string]any) int {
	total := estimateTokens(anthropicSystemText(body["system"]))
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		total += estimateContentTokens(m["content"])
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			total += estimateTokens(jsonString(tcs))
		}
	}
	if tools := anthropicToolList(body["tools"]); len(tools) > 0 {
		total += estimateTokens(jsonString(tools))
	}
	return total
}

// estimateContentTokens estimates one message body, whether it is a bare string
// or a list of content blocks.
func estimateContentTokens(content any) int {
	switch v := content.(type) {
	case nil:
		return 0
	case string:
		return estimateTokens(v)
	case []any:
		total := 0
		for _, raw := range v {
			block, ok := raw.(map[string]any)
			if !ok {
				if s, ok := raw.(string); ok {
					total += estimateTokens(s)
				}
				continue
			}
			switch blockType(block) {
			case "text", "input_text":
				total += estimateTokens(stringField(block, "text"))
			case "image", "image_url", "input_image":
				total += estimateImageTokens
			default:
				total += estimateTokens(jsonString(block))
			}
		}
		return total
	default:
		return estimateTokens(jsonString(content))
	}
}

// jsonString renders a value as compact JSON, or "" when it cannot be encoded.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
