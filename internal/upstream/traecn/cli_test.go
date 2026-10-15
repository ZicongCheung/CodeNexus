package traecn

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// The clause below is the standard re-exec trick: the test binary runs itself
// as the child process, so the CLI executor is exercised against a real process
// without depending on a real Trae installation.
const (
	helperEnv     = "GO_WANT_CLI_HELPER"
	helperModeEnv = "CLI_HELPER_MODE"
)

// TestCLIHelperProcess is not a test in the usual sense: when the environment
// marks it, it becomes the fake CLI and never returns to the test framework.
func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	switch os.Getenv(helperModeEnv) {
	case "json":
		// Two cumulative snapshots, a log line in between (which the splitter
		// must ignore), and a finish marker.
		fmt.Fprint(os.Stdout, `{"message":{"content":"hel`+"\n")
		fmt.Fprint(os.Stdout, `lo"}}`+"\n")
		fmt.Fprint(os.Stdout, `[info] thinking about {"not":"a message"}`+"\n")
		fmt.Fprint(os.Stdout, `{"message":{"content":"hello world"}}`+"\n")
		fmt.Fprint(os.Stdout, `{"finish_reason":"stop"}`+"\n")
	case "usage":
		fmt.Fprint(os.Stdout, `{"message":{"content":"hi"},"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n")
	case "tools":
		fmt.Fprint(os.Stdout, `{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}`+"\n")
	case "text":
		fmt.Fprint(os.Stdout, "plain one\nplain two\n")
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: no configuration found")
		os.Exit(3)
	case "fail-after-output":
		fmt.Fprint(os.Stdout, `{"message":{"content":"partial"}}`+"\n")
		os.Exit(3)
	case "silent":
		// Emit nothing and linger, so the watchdog has to intervene.
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

// newCLIClient builds an adapter pinned to the local-CLI executor, pointed at
// this test binary as the "installed" CLI.
func newCLIClient(t *testing.T, mode string, mutate func(*CLIConfig)) *Client {
	t.Helper()
	t.Setenv(helperEnv, "1")
	t.Setenv(helperModeEnv, mode)
	cfg := CLIConfig{
		Command: os.Args[0],
		// "--" stops the test binary's own flag parsing, so the arguments the
		// executor appends (--json, --disallowed-tool, …) reach the helper
		// instead of being rejected by the testing framework.
		Args:           []string{"-test.run=TestCLIHelperProcess", "--"},
		WorkDir:        t.TempDir(),
		TimeoutSeconds: 2,
		MaxConcurrency: 2,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(Config{UpstreamMode: CLIMode, CLI: cfg})
}

func TestCLIChatStreamsIncrementalDeltas(t *testing.T) {
	c := newCLIClient(t, "json", nil)

	var got strings.Builder
	err := c.ChatStream(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}},
		func(d upstream.StreamDelta) error {
			got.WriteString(d.Content)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.String() != "hello world" {
		t.Fatalf("streamed %q, want %q", got.String(), "hello world")
	}
}

func TestCLIChatAssemblesNonStreamingResponse(t *testing.T) {
	c := newCLIClient(t, "json", nil)

	resp, err := c.Chat(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello world" {
		t.Fatalf("content %q, want %q", resp.Content, "hello world")
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish %q, want stop", resp.FinishReason)
	}
}

func TestCLIChatSurfacesUsage(t *testing.T) {
	c := newCLIClient(t, "usage", nil)

	var usage *upstream.Usage
	err := c.ChatStream(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}},
		func(d upstream.StreamDelta) error {
			if d.Usage != nil {
				usage = d.Usage
			}
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("usage %+v, want total 8", usage)
	}
}

func TestCLIChatReadsTextMode(t *testing.T) {
	c := newCLIClient(t, "text", func(cfg *CLIConfig) { cfg.OutputMode = "text" })

	var got strings.Builder
	err := c.ChatStream(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}},
		func(d upstream.StreamDelta) error {
			got.WriteString(d.Content)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !strings.Contains(got.String(), "plain one") || !strings.Contains(got.String(), "plain two") {
		t.Fatalf("text mode streamed %q", got.String())
	}
}

func TestCLIChatReportsFailureWhenNothingProduced(t *testing.T) {
	c := newCLIClient(t, "fail", nil)

	_, err := c.Chat(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q does not carry the child's diagnostics", err)
	}
}

func TestCLIChatAcceptsNonZeroExitAfterOutput(t *testing.T) {
	c := newCLIClient(t, "fail-after-output", nil)

	resp, err := c.Chat(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("a child that answered and then exited non-zero must not fail: %v", err)
	}
	if resp.Content != "partial" {
		t.Fatalf("content %q, want partial", resp.Content)
	}
}

func TestCLIChatKillsSilentChild(t *testing.T) {
	c := newCLIClient(t, "silent", func(cfg *CLIConfig) { cfg.TimeoutSeconds = 1 })

	start := time.Now()
	_, err := c.Chat(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("watchdog took %s to intervene", elapsed)
	}
	if kind := upstream.Classify(err).Kind; kind != upstream.KindTimeout {
		t.Fatalf("error kind %v, want timeout", kind)
	}
}

func TestCLIChatPropagatesToolCalls(t *testing.T) {
	c := newCLIClient(t, "tools", nil)

	var calls []upstream.ToolCall
	err := c.ChatStream(context.Background(), LocalCLIAccount(),
		&upstream.ChatRequest{
			Model:    "auto",
			Messages: []upstream.Message{{Role: "user", Content: "time?"}},
			Tools:    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_time"}}},
		},
		func(d upstream.StreamDelta) error {
			calls = append(calls, d.ToolCalls...)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_time" {
		t.Fatalf("tool calls %+v", calls)
	}
}

func TestCLIResolvesExplicitPathEvenWhenNotOnPath(t *testing.T) {
	dir := t.TempDir()
	fake := dir + string(os.PathSeparator) + "traecli-fake"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	got, err := resolveCLI(CLIConfig{Command: fake})
	if err != nil {
		t.Fatalf("resolveCLI: %v", err)
	}
	if got != fake {
		t.Fatalf("resolved %q, want %q", got, fake)
	}
}

func TestCLIResolveReportsWhatItTried(t *testing.T) {
	_, err := resolveCLI(CLIConfig{Command: t.TempDir() + string(os.PathSeparator) + "absent"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error %q does not name the candidate", err)
	}
}

func TestCLIConfigNormalizeDefaults(t *testing.T) {
	t.Setenv(cliEnvArgs, "")
	cfg := CLIConfig{}.normalize()
	if len(cfg.Args) == 0 || cfg.Args[0] != "-p" {
		t.Fatalf("base args %v, want [-p]", cfg.Args)
	}
	if cfg.ModelMode != "default" || cfg.OutputMode != "json" {
		t.Fatalf("modes %q/%q", cfg.ModelMode, cfg.OutputMode)
	}
	if cfg.MaxConcurrency != 2 {
		t.Fatalf("max concurrency %d, want 2", cfg.MaxConcurrency)
	}
	if cfg.DisableTools == nil || !*cfg.DisableTools {
		t.Fatal("tools must be denied by default: the CLI can run commands on this host")
	}
	if cfg.WorkDir == "" {
		t.Fatal("work dir must be resolved")
	}
}

func TestCLIDeniedToolsForcedByCallerOwnedTools(t *testing.T) {
	enabled := false
	cfg := CLIConfig{DisableTools: &enabled}.normalize()

	if got := cfg.deniedTools(false); got != nil {
		t.Fatalf("an explicit opt-out must leave tools alone, got %v", got)
	}
	got := cfg.deniedTools(true)
	if len(got) == 0 {
		t.Fatal("a request carrying its own tools must still deny the built-in set")
	}
	for _, name := range []string{"Bash", "Write"} {
		if !containsString(got, name) {
			t.Fatalf("%q missing from %v", name, got)
		}
	}
}

func TestCLIConfigDisallowedToolsExtendDefaults(t *testing.T) {
	cfg := CLIConfig{DisallowedTools: []string{"WebFetch", "Bash"}}.normalize()
	got := cfg.deniedTools(false)

	if !containsString(got, "WebFetch") {
		t.Fatalf("extra tool missing from %v", got)
	}
	if countString(got, "Bash") != 1 {
		t.Fatalf("duplicates not collapsed: %v", got)
	}
}

func TestCLIInvocationDoesNotRepeatBaseFlags(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	cfg := CLIConfig{Args: []string{"-p", "--json"}}.normalize()

	inv := c.cliInvocation(cfg, "traecli",
		&upstream.ChatRequest{Model: "auto"}, "hello", false)
	if n := countString(inv.args, "--json"); n != 1 {
		t.Fatalf("--json appears %d times in %v", n, inv.args)
	}
	if inv.args[len(inv.args)-1] != "hello" {
		t.Fatalf("prompt must be the last argument: %v", inv.args)
	}
}

func TestCLIInvocationDeniesToolsOnceEach(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	cfg := CLIConfig{ModelMode: "flag"}.normalize()

	inv := c.cliInvocation(cfg, "traecli",
		&upstream.ChatRequest{Model: "doubao-seed-code"}, "hello", false)
	if n := countString(inv.args, "--disallowed-tool"); n != len(defaultDeniedTools) {
		t.Fatalf("deny flags %d, want %d: %v", n, len(defaultDeniedTools), inv.args)
	}
	if !containsString(inv.args, "--model") {
		t.Fatalf("model flag missing: %v", inv.args)
	}
}

func TestCLIInvocationModelModes(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	req := &upstream.ChatRequest{Model: "doubao-seed-code"}

	cases := map[string]string{
		"default": "-c",
		"config":  "--config",
		"flag":    "--model",
	}
	for mode, flag := range cases {
		inv := c.cliInvocation(CLIConfig{ModelMode: mode}.normalize(), "traecli", req, "p", false)
		if !containsString(inv.args, flag) {
			t.Fatalf("mode %s: flag %s missing from %v", mode, flag, inv.args)
		}
	}
	inv := c.cliInvocation(CLIConfig{ModelMode: "none"}.normalize(), "traecli", req, "p", false)
	for _, flag := range []string{"-c", "--model", "--config"} {
		if containsString(inv.args, flag) {
			t.Fatalf("mode none must not pass a model flag, got %v", inv.args)
		}
	}
}

func TestCLIInvocationAutoModelIsLeftToTheCLI(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	inv := c.cliInvocation(CLIConfig{}.normalize(), "traecli",
		&upstream.ChatRequest{Model: "auto"}, "p", false)
	if containsString(inv.args, "-c") || containsString(inv.args, "--model") {
		t.Fatalf("auto must be resolved by the CLI, got %v", inv.args)
	}
}

func TestCLIInvocationPromptStdin(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode})
	cfg := CLIConfig{PromptStdin: true}.normalize()

	inv := c.cliInvocation(cfg, "traecli", &upstream.ChatRequest{Model: "auto"}, "the prompt", false)
	if inv.stdin != "the prompt" {
		t.Fatalf("stdin %q", inv.stdin)
	}
	if inv.args[len(inv.args)-1] == "the prompt" {
		t.Fatalf("prompt must not also be an argument: %v", inv.args)
	}
}

func TestCLIPromptKeepsTheTailWhenTruncating(t *testing.T) {
	cfg := CLIConfig{MaxMessages: 2, MaxPromptChars: 1000}.normalize()
	req := &upstream.ChatRequest{Messages: []upstream.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
		{Role: "user", Content: "third"},
	}}
	got := cliPrompt(req, cfg)
	if strings.Contains(got, "first") {
		t.Fatalf("oldest message should have been dropped: %q", got)
	}
	if !strings.Contains(got, "third") {
		t.Fatalf("newest message must survive: %q", got)
	}
}

func TestSplitJSONObjectsToleratesLogsAndEscapes(t *testing.T) {
	stream := strings.Join([]string{
		`[warn] starting`,
		`{"a":"brace } inside","b":{"c":[1,2,{"d":"x"}]}}`,
		`garbage in between`,
		`{"e":"quote \" and backslash \\"}`,
		`{"incomplete":`,
	}, "\n")

	values, rest := splitJSONObjects(stream)
	if len(values) != 2 {
		t.Fatalf("parsed %d values, want 2: %+v", len(values), values)
	}
	if values[0]["a"] != "brace } inside" {
		t.Fatalf("string with a brace was mangled: %+v", values[0])
	}
	if values[1]["e"] != `quote " and backslash \` {
		t.Fatalf("escape handling wrong: %+v", values[1])
	}
	if !strings.HasPrefix(rest, `{"incomplete":`) {
		t.Fatalf("incomplete tail lost: %q", rest)
	}
}

func TestJSONStreamIsIndependentOfReadBoundaries(t *testing.T) {
	chunk := `{"message":{"content":"héllo"}}`

	var stream cliJSONStream
	var pending []byte
	var seen []map[string]any
	for i := 0; i < len(chunk); i++ {
		pending = append(pending, chunk[i])
		text, rest := completeUTF8Prefix(pending)
		pending = rest
		seen = append(seen, stream.push(text)...)
	}
	seen = append(seen, stream.flush()...)

	if len(seen) != 1 {
		t.Fatalf("byte-at-a-time delivery lost the object: %+v", seen)
	}
	if got := cliMessageText(seen[0]); got != "héllo" {
		t.Fatalf("content %q", got)
	}
}

func TestCompleteUTF8PrefixHoldsSplitRunes(t *testing.T) {
	full := []byte("héllo")
	// "é" is two bytes; cut it in half.
	text, rest := completeUTF8Prefix(full[:2])
	if text != "h" {
		t.Fatalf("prefix %q, want %q", text, "h")
	}
	if len(rest) != 1 {
		t.Fatalf("rest %q, want the dangling lead byte", rest)
	}
	joined, tail := completeUTF8Prefix(append(rest, full[2:]...))
	if joined != "éllo" || tail != nil {
		t.Fatalf("rejoined %q (tail %q)", joined, tail)
	}
}

func TestCLIRunnerConcurrencyIsBounded(t *testing.T) {
	cfg := CLIConfig{Command: os.Args[0], MaxConcurrency: 1}.normalize()
	r := newCLIRunner(cfg)
	if cap(r.sem) != 1 {
		t.Fatalf("semaphore capacity %d, want 1", cap(r.sem))
	}
}

func TestCLIProbeReportsMissingExecutable(t *testing.T) {
	c := New(Config{UpstreamMode: CLIMode, CLI: CLIConfig{
		Command: t.TempDir() + string(os.PathSeparator) + "absent",
	}})
	probe, err := c.Probe(context.Background(), LocalCLIAccount())
	if err != nil {
		t.Fatalf("Probe must not error: %v", err)
	}
	if probe.Healthy {
		t.Fatal("a missing executable must report unhealthy")
	}
}

func TestCLIProbeIsHealthyWithAnExecutable(t *testing.T) {
	c := newCLIClient(t, "json", nil)
	probe, err := c.Probe(context.Background(), LocalCLIAccount())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !probe.Healthy {
		t.Fatalf("probe unhealthy: %s", probe.Detail)
	}
}

func TestCLIChatRequiresNoCredential(t *testing.T) {
	c := newCLIClient(t, "json", nil)
	// A nil account is what a pool miss would hand over; the CLI path must not
	// care, because the identity lives in the local installation.
	resp, err := c.chat(context.Background(), nil,
		&upstream.ChatRequest{Model: "auto", Messages: []upstream.Message{{Role: "user", Content: "hi"}}},
		&emitTracker{})
	if err != nil {
		if upstream.Classify(err).Kind == upstream.KindAuth {
			t.Fatalf("cli mode must not demand a credential: %v", err)
		}
		t.Fatalf("chat: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatalf("response %+v", resp)
	}
}

func TestLocalCLIAccountIsEphemeralAndLocal(t *testing.T) {
	acc := LocalCLIAccount()
	if !acc.Ephemeral || !acc.Local {
		t.Fatalf("placeholder flags: ephemeral=%v local=%v", acc.Ephemeral, acc.Local)
	}
	if acc.Source != account.SourceTraeCN {
		t.Fatalf("source %q", acc.Source)
	}
}

func containsString(list []string, want string) bool { return countString(list, want) > 0 }

func countString(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
