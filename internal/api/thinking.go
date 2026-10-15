package api

// Reasoning travels through the core as plain text in StreamDelta.Reasoning
// (and ChatResponse.ReasoningContent), kept strictly apart from Content. What
// each wire format does with it is its own business:
//
//   - chat-completions exposes it as "reasoning_content" (the de-facto field
//     DeepSeek-style clients read);
//   - Anthropic exposes it as a "thinking" content block, which carries a
//     signature;
//   - Responses exposes it as an independent "reasoning" output item.
//
// This file owns the one piece they share that is not protocol-obvious.

import (
	"crypto/sha256"
	"encoding/base64"
)

// syntheticSignature derives a deterministic, opaque signature for a thinking
// block. The gateway's upstream is not Anthropic, so there is no genuine
// signature to relay; an Anthropic-shaped client only requires the field to be
// present and stable for the block to round-trip. The value is namespaced so it
// can never be mistaken for a real Anthropic proof, and it is a pure function
// of the text so the same answer always yields the same signature.
func syntheticSignature(thinking string) string {
	sum := sha256.Sum256([]byte("codenexus.thinking.v1\x00" + thinking))
	return "cnx-" + base64.RawStdEncoding.EncodeToString(sum[:])
}
