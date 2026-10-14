package rewrite

import (
	"strings"
	"testing"
)

const pid = "\uFF5C\uFF5C"

func markupBlock() string {
	open := "<" + pid + "DSML" + pid + " calls>"
	closeTag := "</" + pid + "DSML" + pid + " calls>"
	invoke := "<" + pid + "DSML" + pid + ` invoke name="exec_command">`
	invokeClose := "</" + pid + "DSML" + pid + " invoke>"
	param := "<" + pid + "DSML" + pid + ` parameter name="cmd" string="true">`
	paramClose := "</" + pid + "DSML" + pid + " parameter>"
	return open + "\n" + invoke + "\n" + param + "ls -la" + paramClose + "\n" + invokeClose + "\n" + closeTag
}

func TestExtractMarkupStrict(t *testing.T) {
	content := "before " + markupBlock() + " after"
	calls, text, ok := ExtractMarkup(content, []string{"exec_command"})
	if !ok {
		t.Fatal("expected markup to be recognised")
	}
	if len(calls) != 1 || calls[0].Name != "exec_command" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la") {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if strings.Contains(text, "DSML") || !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Fatalf("leftover text = %q", text)
	}
}

func TestExtractMarkupStrictRejectsUnknownTool(t *testing.T) {
	content := "x " + markupBlock()
	if _, _, ok := ExtractMarkup(content, []string{"some_other_tool"}); ok {
		t.Fatal("strict mode should reject an undeclared tool name")
	}
}

func TestExtractMarkupWeakMode(t *testing.T) {
	content := "x " + markupBlock()
	if _, _, ok := ExtractMarkup(content, nil); !ok {
		t.Fatal("weak mode should accept an identifier-shaped tool name")
	}
	placeholder := strings.Replace(markupBlock(), "exec_command", "<tool_name>", 1)
	if _, _, ok := ExtractMarkup("x "+placeholder, nil); ok {
		t.Fatal("weak mode should reject a placeholder-shaped tool name")
	}
}

func TestExtractMarkupMalformed(t *testing.T) {
	open := "<" + pid + "DSML" + pid + " calls>"
	if _, _, ok := ExtractMarkup("x "+open+" dangling", nil); ok {
		t.Fatal("unterminated block must not be repaired")
	}
}

func TestMarkupRewriterAcrossChunks(t *testing.T) {
	rw := NewMarkupRewriter([]string{"exec_command"})
	full := "hello " + markupBlock() + " world"

	var text strings.Builder
	var calls []MarkupCall
	for _, chunk := range chunkByRune(full, 4) {
		txt, got := rw.Write(chunk)
		text.WriteString(txt)
		calls = append(calls, got...)
	}
	text.WriteString(rw.Flush())

	if strings.Contains(text.String(), "DSML") {
		t.Fatalf("markup leaked into text: %q", text.String())
	}
	if !strings.Contains(text.String(), "hello") || !strings.Contains(text.String(), "world") {
		t.Fatalf("text lost: %q", text.String())
	}
	if len(calls) != 1 || calls[0].Name != "exec_command" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestMarkupRewriterDisableReleasesBuffer(t *testing.T) {
	rw := NewMarkupRewriter(nil)
	txt, _ := rw.Write("partial <" + pid + "DSML")
	if strings.Contains(txt, "DSML") {
		t.Fatalf("pending marker should be held back, got %q", txt)
	}
	released := rw.Disable()
	if !strings.Contains(released, "DSML") {
		t.Fatalf("Disable should release the buffer, got %q", released)
	}
}

func chunkByRune(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		j := i + n
		if j > len(r) {
			j = len(r)
		}
		out = append(out, string(r[i:j]))
	}
	return out
}
