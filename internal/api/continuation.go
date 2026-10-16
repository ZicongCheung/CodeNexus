package api

import (
	"regexp"
	"strings"

	"github.com/ZicongCheung/CodeNexus/internal/config"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// SetAutoContinue swaps the truncation-continuation settings. The console calls
// it after a save, so a tuning change applies to the next request.
func (s *Server) SetAutoContinue(cfg config.AutoContinueConfig) {
	s.autoCont.Store(&cfg)
}

// autoContinue reads the current settings.
func (s *Server) autoContinue() config.AutoContinueConfig {
	if p := s.autoCont.Load(); p != nil {
		return *p
	}
	return config.AutoContinueConfig{}
}

// A relayed turn can stop mid-answer: the output budget runs out, the provider
// disconnects, or the model spends the whole turn thinking and emits a
// fragment. Handing the caller that half-written answer is worse than asking
// the model to finish, so a truncated turn is re-issued and the remainder is
// streamed into the same public response.
//
// The cost of a wrong guess is a whole extra billable turn, so the signals are
// deliberately mixed: an explicit length finish reason, a structurally
// unclosed answer (open code fence, unbalanced brackets), a tail that ends on
// a character no finished sentence ends on, or a long reasoning trace followed
// by a fragment. And a round that merely repeats the previous one ends the
// loop rather than duplicating the answer.

// lengthFinishReasons are every alias an upstream or a translator uses for
// "the model ran out of output budget". Missing one would hand the caller a
// half answer.
var lengthFinishReasons = map[string]bool{
	"length":                        true,
	"max_tokens":                    true,
	"max_output_tokens":             true,
	"max_token":                     true,
	"token_limit":                   true,
	"model_context_window_exceeded": true,
	"context_window_exceeded":       true,
	"context_length_exceeded":       true,
}

// truncatedEndings prove the model stopped mid-token or mid-structure.
var truncatedEndings = []*regexp.Regexp{
	regexp.MustCompile(`,\s*$`),
	regexp.MustCompile(`[|｜]\s*$`),
	regexp.MustCompile(`\.\.\.\s*$`),
	regexp.MustCompile(`\\\s*$`),
	regexp.MustCompile(`//\s*$`),
	regexp.MustCompile(`#\s*$`),
	regexp.MustCompile(`[-–—]\s*$`),
	regexp.MustCompile(`[:：]\s*$`),
}

var sentenceEnd = regexp.MustCompile("[.!?。！？\\]\\)\\}`\"'\u201d\u2019]\\s*$")

const (
	// fragmentTextLimit and the reasoning ratio bound the "spent the turn
	// thinking" case.
	fragmentTextLimit        = 40
	fragmentReasoning        = 200
	fragmentRatio            = 4
	truncationTailSize       = 100
	continueInstruction      = "请继续输出，从你中断的地方继续，不要重复已经写过的内容，不要重新开始。"
	reasoningOnlyInstruction = "Your previous response contained only thinking/reasoning with no actual " +
		"output. Provide the actual response now - either text content or a tool call."
)

// looksTruncated reports whether a finished turn was cut off mid-answer.
//
// A turn that produced tool calls is never truncated: the caller owns
// execution and has to receive the call as-is.
func looksTruncated(text, finishReason, reasoning string, hasToolCalls bool) bool {
	if hasToolCalls {
		return false
	}
	if lengthFinishReasons[strings.ToLower(strings.TrimSpace(finishReason))] {
		return true
	}
	if strings.TrimSpace(text) == "" {
		// Nothing public was produced. Reasoning-only output is the one case
		// worth re-asking; a completely empty turn is the empty-response path's
		// business, not this one's.
		return strings.TrimSpace(reasoning) != ""
	}
	if unclosedCodeFence(text) {
		return true
	}
	if unbalancedBrackets(tailOf(text, truncationTailSize)) {
		return true
	}
	tail := strings.TrimRight(tailOf(text, truncationTailSize), " \t\r\n")
	for _, pattern := range truncatedEndings {
		if pattern.MatchString(tail) {
			return true
		}
	}
	return stoppedAfterThinking(text, reasoning)
}

// looksReasoningOnly reports a turn that only produced reasoning.
func looksReasoningOnly(text, reasoning string, hasToolCalls bool) bool {
	if hasToolCalls {
		return false
	}
	return strings.TrimSpace(text) == "" && strings.TrimSpace(reasoning) != ""
}

// stoppedAfterThinking catches a long reasoning trace followed by a sentence
// fragment: the model burned the turn inside the thinking channel and emitted
// only a token or two of answer.
func stoppedAfterThinking(text, reasoning string) bool {
	visible := strings.TrimSpace(text)
	thinking := strings.TrimSpace(reasoning)
	if visible == "" || len([]rune(visible)) > fragmentTextLimit {
		return false
	}
	if len([]rune(thinking)) < fragmentReasoning {
		return false
	}
	if len([]rune(thinking)) < len([]rune(visible))*fragmentRatio {
		return false
	}
	return !sentenceEnd.MatchString(visible)
}

// unclosedCodeFence reports an odd number of triple backticks.
func unclosedCodeFence(text string) bool {
	return strings.Count(text, "```")%2 == 1
}

// unbalancedBrackets reports a code-shaped tail that is missing closers.
//
// A plain count difference needs a small tolerance (prose and tables carry
// stray brackets), but a tail that ends inside an open group is unambiguous:
// nothing after it could close the group.
func unbalancedBrackets(tail string) bool {
	opens, closes := 0, 0
	for _, r := range tail {
		switch r {
		case '[', '{', '(':
			opens++
		case ']', '}', ')':
			closes++
		}
	}
	if opens > closes+2 {
		return true
	}
	// Walk the tail and require every closer to match the innermost opener, so
	// `{a: [1, 2}` is not mistaken for balanced output.
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	for _, r := range tail {
		switch r {
		case '[', '{', '(':
			stack = append(stack, r)
		case ']', '}', ')':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return true
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) > 0
}

// continuationSimilarity is the character-position overlap of two short
// answers, used to stop a repeat loop.
func continuationSimilarity(left, right string) float64 {
	if left == "" || right == "" {
		return 0
	}
	a, b := []rune(left), []rune(right)
	overlap := len(a)
	if len(b) < overlap {
		overlap = len(b)
	}
	if overlap <= 0 {
		return 0
	}
	same := 0
	for i := 0; i < overlap; i++ {
		if a[i] == b[i] {
			same++
		}
	}
	return float64(same) / float64(overlap)
}

// continuedMessages builds the conversation for one continuation round: the
// original request plus the partial answer replayed as the assistant's own
// turn, followed by a user turn that only asks for the remainder.
func continuedMessages(messages []upstream.Message, partial string, reasoningOnly bool) []upstream.Message {
	out := make([]upstream.Message, 0, len(messages)+2)
	out = append(out, messages...)
	if strings.TrimSpace(partial) != "" {
		out = append(out, upstream.Message{Role: "assistant", Content: partial})
	}
	instruction := continueInstruction
	if reasoningOnly {
		instruction = reasoningOnlyInstruction
	}
	return append(out, upstream.Message{Role: "user", Content: instruction})
}

// tailOf returns at most n trailing bytes of s, adjusted to a rune boundary.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8Start(s[cut]) {
		cut++
	}
	return s[cut:]
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
