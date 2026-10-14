package rewrite

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The upstream model emits tool calls in a native text markup when the request
// did not declare tools. The gateway restores that markup into structured tool
// calls so the client can actually execute them.
//
//	<SEP>DSML<SEP> calls>
//	<SEP>DSML<SEP> invoke name="exec_command">
//	<SEP>DSML<SEP> parameter name="cmd" string="true">ls -la</SEP>DSML<SEP> parameter>
//	</SEP>DSML<SEP> invoke>
//	</SEP>DSML<SEP> calls>
//
// where <SEP> is two fullwidth vertical bars (U+FF5C). Anything that does not
// parse confidently is passed through verbatim: the repair layer never swallows
// bytes.
const markupSep = "\uFF5C\uFF5C"

const (
	markupOpen        = "<" + markupSep + "DSML" + markupSep + " calls>"
	markupClose       = "</" + markupSep + "DSML" + markupSep + " calls>"
	markupInvokeClose = "</" + markupSep + "DSML" + markupSep + " invoke>"
	markupParamClose  = "</" + markupSep + "DSML" + markupSep + " parameter>"
)

// markupMaxBlockBytes bounds a single block so a text that merely discusses the
// syntax cannot buffer without limit.
const markupMaxBlockBytes = 256 * 1024

var (
	markupTagPrefix = regexp.QuoteMeta("<" + markupSep + "DSML" + markupSep)
	markupInvokeRe  = regexp.MustCompile(markupTagPrefix + `\s+invoke\s+name="([^"]*)"\s*>`)
	markupParamRe   = regexp.MustCompile(markupTagPrefix + `\s+parameter\s+name="([^"]*)"(?:\s+string="(true|false)")?\s*>`)
	markupNameRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,63}$`)
)

// MarkupCall is one tool call recovered from native markup.
type MarkupCall struct {
	Name      string
	Arguments string
}

// ExtractMarkup finds the first native markup block in content. On success it
// returns the recovered calls and the surrounding text with the block removed.
// ok is false when there is no block, the block is malformed, or the calls are
// not plausible for the declared tool set.
func ExtractMarkup(content string, toolNames []string) (calls []MarkupCall, text string, ok bool) {
	start := strings.Index(content, markupOpen)
	if start < 0 {
		return nil, content, false
	}
	rel := strings.Index(content[start+len(markupOpen):], markupClose)
	if rel < 0 {
		return nil, content, false
	}
	innerStart := start + len(markupOpen)
	innerEnd := innerStart + rel

	parsed, valid := parseMarkupBlock(content[innerStart:innerEnd])
	if !valid || !plausibleCalls(parsed, toolNames) {
		return nil, content, false
	}
	rest := content[:start] + content[innerEnd+len(markupClose):]
	return parsed, strings.TrimSpace(rest), true
}

// parseMarkupBlock walks a block body that must consist solely of invoke
// elements, each holding only parameter elements.
func parseMarkupBlock(inner string) ([]MarkupCall, bool) {
	var calls []MarkupCall
	rest := inner
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		loc := markupInvokeRe.FindStringSubmatchIndex(rest)
		if loc == nil || loc[0] != 0 {
			return nil, false // stray text or a malformed open tag
		}
		name := rest[loc[2]:loc[3]]
		body := rest[loc[1]:]
		end := strings.Index(body, markupInvokeClose)
		if end < 0 {
			return nil, false
		}
		args, ok := parseMarkupParams(body[:end])
		if !ok {
			return nil, false
		}
		calls = append(calls, MarkupCall{Name: name, Arguments: string(args)})
		rest = body[end+len(markupInvokeClose):]
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// parseMarkupParams builds the JSON arguments object from an invoke body.
func parseMarkupParams(inner string) ([]byte, bool) {
	params := map[string]any{}
	rest := inner
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		loc := markupParamRe.FindStringSubmatchIndex(rest)
		if loc == nil || loc[0] != 0 {
			return nil, false
		}
		name := rest[loc[2]:loc[3]]
		stringMode := ""
		if loc[4] >= 0 {
			stringMode = rest[loc[4]:loc[5]]
		}
		body := rest[loc[1]:]
		end := strings.Index(body, markupParamClose)
		if end < 0 {
			return nil, false
		}
		raw := body[:end]
		if stringMode == "true" {
			params[name] = raw
		} else {
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				params[name] = raw
			} else {
				params[name] = v
			}
		}
		rest = body[end+len(markupParamClose):]
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, false
	}
	return b, true
}

// plausibleCalls applies the strict check when the client declared tools and a
// shape check otherwise.
func plausibleCalls(calls []MarkupCall, toolNames []string) bool {
	if len(calls) == 0 {
		return false
	}
	if len(toolNames) > 0 {
		declared := make(map[string]bool, len(toolNames))
		for _, n := range toolNames {
			declared[n] = true
		}
		for _, c := range calls {
			if !declared[c.Name] {
				return false
			}
		}
		return true
	}
	for _, c := range calls {
		if !markupNameRe.MatchString(c.Name) {
			return false
		}
	}
	return true
}

// MarkupRewriter restores native markup that arrives split across streaming
// deltas. Text is released as soon as it is unambiguous; a pending marker or a
// partial block is held back until it resolves.
type MarkupRewriter struct {
	toolNames []string
	buf       string
	inBlock   bool
	disabled  bool
}

// NewMarkupRewriter builds a rewriter for one response.
func NewMarkupRewriter(toolNames []string) *MarkupRewriter {
	return &MarkupRewriter{toolNames: toolNames}
}

// Disable stops rewriting and returns anything held back. It is called as soon
// as the upstream supplies real structured tool calls, which must win.
func (r *MarkupRewriter) Disable() string {
	r.disabled = true
	r.inBlock = false
	out := r.buf
	r.buf = ""
	return out
}

// Write consumes a content delta and returns text safe to emit now plus any
// completed tool calls.
func (r *MarkupRewriter) Write(delta string) (string, []MarkupCall) {
	if r.disabled {
		return delta, nil
	}
	r.buf += delta

	var text strings.Builder
	var calls []MarkupCall
	for {
		if r.inBlock {
			end := strings.Index(r.buf, markupClose)
			if end < 0 {
				if len(r.buf) > markupMaxBlockBytes {
					text.WriteString(r.buf) // too long to be real markup
					r.buf = ""
					r.inBlock = false
				}
				break
			}
			consumed := end + len(markupClose)
			if parsed, ok := parseMarkupBlock(r.buf[:end]); ok && plausibleCalls(parsed, r.toolNames) {
				calls = append(calls, parsed...)
			} else {
				text.WriteString(r.buf[:consumed]) // verbatim when unsure
			}
			r.buf = r.buf[consumed:]
			r.inBlock = false
			continue
		}

		i := strings.Index(r.buf, markupOpen)
		if i >= 0 {
			text.WriteString(r.buf[:i])
			r.buf = r.buf[i+len(markupOpen):]
			r.inBlock = true
			continue
		}
		hold := trailingMarkerPrefix(r.buf)
		if emit := len(r.buf) - hold; emit > 0 {
			text.WriteString(r.buf[:emit])
			r.buf = r.buf[emit:]
		}
		break
	}
	return text.String(), calls
}

// Flush releases whatever is still buffered, verbatim.
func (r *MarkupRewriter) Flush() string {
	out := r.buf
	r.buf = ""
	r.inBlock = false
	return out
}

// trailingMarkerPrefix returns the length of the longest suffix of s that is a
// proper prefix of the open marker, so it can be held back.
func trailingMarkerPrefix(s string) int {
	max := len(markupOpen) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, markupOpen[:n]) {
			return n
		}
	}
	return 0
}
