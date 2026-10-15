package traecn

// Local-CLI executor.
//
// Some machines have a signed-in Trae CLI but no copyable token: the session
// lives inside the tool. This executor runs that CLI as the upstream for the
// whole source. It is a mode rather than an account because the CLI owns the
// identity — there is no credential to pool, rotate or bill, which is also why
// the reference implementation drops the account fields on this path.
//
// Two properties shape the code:
//
//   - Each request is a child process, so concurrency is bounded by a
//     semaphore. A machine has finite patience for concurrent coding agents.
//   - The child's tools are denied by default. A reverse proxy that can run
//     shell commands on the host is a remote code execution hole, so tools stay
//     denied unless the operator deliberately re-enables them.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// CLIMode is the upstream_mode value that selects the local-CLI executor.
const CLIMode = "cli"

// LocalCLIUID identifies the placeholder entry the cli mode needs. The pool is
// the only path a request can take to an adapter, so cli mode needs an entry to
// be dispatched to. It is not a credential: it carries no token, is never
// persisted and never refreshed, and is marked Local so the console can say so.
const LocalCLIUID = "cli-local"

// LocalCLIAccount builds that placeholder.
func LocalCLIAccount() *account.Account {
	return &account.Account{
		Source:    account.SourceTraeCN,
		UID:       LocalCLIUID,
		Label:     "本机 Trae CLI",
		Enabled:   true,
		Ephemeral: true,
		Local:     true,
	}
}

// CLIConfig tunes the local-CLI executor.
type CLIConfig struct {
	// Command overrides the executable. Empty searches the built-in names.
	Command string `json:"command,omitempty"`
	// Args replaces the built-in base arguments (default "-p").
	Args []string `json:"args,omitempty"`
	// WorkDir isolates the child. Empty means a directory under the user home.
	WorkDir string `json:"work_dir,omitempty"`
	// ModelMode is how the model is passed: default, config, flag or none.
	ModelMode string `json:"model_mode,omitempty"`
	// OutputMode is "json" (structured, the default) or "text".
	OutputMode string `json:"output_mode,omitempty"`
	// PromptStdin sends the prompt on stdin instead of as an argument.
	PromptStdin bool `json:"prompt_stdin,omitempty"`
	// MaxConcurrency caps simultaneous child processes.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// TimeoutSeconds bounds the silence between two reads from the child.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// DisableTools denies the built-in tool set. Nil means enabled.
	DisableTools *bool `json:"disable_tools,omitempty"`
	// DisallowedTools replaces the built-in deny list.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	// MaxMessages and MaxPromptChars bound the serialized transcript.
	MaxMessages    int `json:"max_messages,omitempty"`
	MaxPromptChars int `json:"max_prompt_chars,omitempty"`
}

// defaultDeniedTools are the tools a reverse proxy must not expose. They are
// named individually because the CLI takes one flag per tool.
var defaultDeniedTools = []string{
	"Read", "Bash", "Edit", "Replace", "Write", "Glob", "Grep", "Task",
}

// defaultCLICommands are the executable names to look for, in order.
var defaultCLICommands = []string{"traecli", "trae-cli", "traex", "trae"}

// Environment overrides let an operator point at a non-standard build without
// editing the config file.
const (
	cliEnvCommand = "CODENEXUS_TRAE_CLI_COMMAND"
	cliEnvArgs    = "CODENEXUS_TRAE_CLI_ARGS"
)

// cliBaseArgs is the built-in argument prefix.
var cliBaseArgs = []string{"-p"}

// normalize fills defaults and clamps the numeric bounds.
func (c CLIConfig) normalize() CLIConfig {
	if len(c.Args) == 0 {
		if env := splitCLIArgs(os.Getenv(cliEnvArgs)); len(env) > 0 {
			c.Args = env
		} else {
			c.Args = append([]string(nil), cliBaseArgs...)
		}
	}
	if strings.TrimSpace(c.ModelMode) == "" {
		c.ModelMode = "default"
	}
	if strings.TrimSpace(c.OutputMode) == "" {
		c.OutputMode = "json"
	}
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 2
	}
	if c.TimeoutSeconds < 1 {
		c.TimeoutSeconds = 300
	}
	if c.DisableTools == nil {
		enabled := true
		c.DisableTools = &enabled
	}
	if c.MaxMessages < 1 {
		c.MaxMessages = 60
	}
	if c.MaxPromptChars < 1 {
		c.MaxPromptChars = 20000
	}
	if strings.TrimSpace(c.WorkDir) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.WorkDir = filepath.Join(home, ".codenexus", "trae-cli")
		} else {
			c.WorkDir = filepath.Join(os.TempDir(), "codenexus-trae-cli")
		}
	}
	return c
}

// textMode reports whether the child should be asked for plain text.
func (c CLIConfig) textMode() bool {
	return strings.EqualFold(strings.TrimSpace(c.OutputMode), "text")
}

// deniedTools resolves the effective deny list. A request that carries a tool
// protocol always gets the built-in set as well: the caller intends to run those
// tools itself, and letting the CLI run them instead would corrupt that.
func (c CLIConfig) deniedTools(callerOwnsTools bool) []string {
	if c.DisableTools != nil && !*c.DisableTools && !callerOwnsTools {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range defaultDeniedTools {
		add(name)
	}
	for _, name := range c.DisallowedTools {
		add(name)
	}
	return out
}

// resolveCLI finds the executable. An explicit setting wins, and an explicit
// path is honoured even when it is not on PATH.
func resolveCLI(cfg CLIConfig) (string, error) {
	explicit := strings.TrimSpace(cfg.Command)
	if explicit == "" {
		explicit = strings.TrimSpace(os.Getenv(cliEnvCommand))
	}
	candidates := defaultCLICommands
	if explicit != "" {
		candidates = []string{explicit}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if looksLikePath(candidate) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("trae: no local Trae CLI found (tried %s)", strings.Join(candidates, ", "))
}

// looksLikePath reports whether a candidate names a location rather than a bare
// command. The extension test covers Windows, where "traecli.exe" is a path
// even without a separator.
func looksLikePath(candidate string) bool {
	if strings.ContainsAny(candidate, `/\`) {
		return true
	}
	switch strings.ToLower(filepath.Ext(candidate)) {
	case ".exe", ".cmd", ".bat", ".ps1", ".sh":
		return true
	}
	return false
}

// splitCLIArgs splits an argument override on whitespace.
func splitCLIArgs(raw string) []string {
	return strings.Fields(strings.TrimSpace(raw))
}

// cliRunner owns the process-level state: the resolved executable and the
// semaphore that bounds concurrency.
type cliRunner struct {
	cfg  CLIConfig
	sem  chan struct{}
	once sync.Once
	path string
	err  error
}

func newCLIRunner(cfg CLIConfig) *cliRunner {
	return &cliRunner{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrency)}
}

// command resolves and caches the executable path.
func (r *cliRunner) command() (string, error) {
	r.once.Do(func() { r.path, r.err = resolveCLI(r.cfg) })
	return r.path, r.err
}

// available reports whether a CLI can be run at all, without issuing a request.
func (r *cliRunner) available() error {
	_, err := r.command()
	return err
}

// cliInvocation is everything needed to start one child process.
type cliInvocation struct {
	command string
	args    []string
	stdin   string
}

// cliInvocation assembles the command line for one request.
func (c *Client) cliInvocation(cfg CLIConfig, command string, req *upstream.ChatRequest, prompt string, callerOwnsTools bool) cliInvocation {
	args := append([]string(nil), cfg.Args...)
	seen := map[string]bool{}
	for _, a := range args {
		seen[a] = true
	}
	push := func(flag, value string) {
		// A value-less flag is identified by its own name, so an operator who
		// already put "--json" in the base arguments does not get it twice.
		key := flag
		if value != "" {
			key = flag + "\x00" + value
		}
		if seen[key] {
			return
		}
		seen[key] = true
		args = append(args, flag)
		if value != "" {
			args = append(args, value)
		}
	}

	if cfg.textMode() {
		push("--output-format", "text")
	} else {
		push("--json", "")
	}
	if model := cliModelName(req.Model); model != "" {
		switch strings.ToLower(strings.TrimSpace(cfg.ModelMode)) {
		case "none":
		case "config":
			push("--config", "model.name="+model)
		case "flag":
			push("--model", model)
		default:
			push("-c", "default_model="+model)
		}
	}
	for _, tool := range cfg.deniedTools(callerOwnsTools) {
		push("--disallowed-tool", tool)
	}
	if cfg.PromptStdin {
		return cliInvocation{command: command, args: args, stdin: prompt}
	}
	return cliInvocation{command: command, args: append(args, prompt)}
}

// cliModelName maps a client-facing model onto the name the CLI expects. The
// CLI has its own notion of "default", so those are left for it to resolve
// rather than sent through the HTTP model mapping.
func cliModelName(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "auto", "auto-work", "solo-work":
		return ""
	}
	return ResolveConfigName(model)
}

// cliChat runs one request through the local CLI. It mirrors the remote
// surface's contract: cumulative snapshots are diffed, and only real content
// counts as output.
func (c *Client) cliChat(ctx context.Context, req *upstream.ChatRequest, write func(upstream.StreamDelta) error) (*upstream.ChatResponse, error) {
	cfg := c.cfg.CLI
	command, err := c.cli.command()
	if err != nil {
		return nil, upstream.NewError(upstream.KindServer, 503, err.Error())
	}
	prompt := cliPrompt(req, cfg)
	inv := c.cliInvocation(cfg, command, req, prompt, usesTools(req))

	out := &upstream.ChatResponse{Model: req.Model}
	st := &snapshotState{emit: write}
	var tools toolAccumulator

	onValue := func(value map[string]any) error {
		text, reasoning := cliValueText(value)
		if err := st.write(text, reasoning, out); err != nil {
			return err
		}
		if usage := cliValueUsage(value); usage != nil {
			out.Usage = *usage
		}
		if calls := tools.add(value); len(calls) > 0 {
			out.ToolCalls = append(out.ToolCalls, calls...)
			if err := write(upstream.StreamDelta{ToolCalls: calls}); err != nil {
				return err
			}
		}
		if reason := cliFinish(value); reason != "" {
			out.FinishReason = reason
		}
		return nil
	}

	if cfg.textMode() {
		err = c.cli.runText(ctx, inv, func(chunk string) error {
			out.Content += chunk
			return write(upstream.StreamDelta{Content: chunk})
		})
	} else {
		err = c.cli.run(ctx, inv, onValue)
	}
	if err != nil {
		return nil, err
	}
	if out.Content == "" && out.ReasoningContent == "" && len(out.ToolCalls) == 0 {
		return nil, upstream.NewError(upstream.KindServer, 0, "trae: local CLI returned no answer")
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}
	if out.Usage.TotalTokens > 0 {
		if err := st.emitUsage(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// cliPrompt serializes the transcript into the single string the CLI accepts.
// Messages are kept newest-first when truncating, because the tail of a
// conversation is what the next answer depends on.
func cliPrompt(req *upstream.ChatRequest, cfg CLIConfig) string {
	msgs := req.Messages
	if len(msgs) > cfg.MaxMessages {
		msgs = msgs[len(msgs)-cfg.MaxMessages:]
	}
	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(flattenContent(m.Content))
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(roleLabel(m.Role))
		b.WriteString(": ")
		b.WriteString(text)
	}
	out := b.String()
	if len(out) > cfg.MaxPromptChars {
		// Keep the end: it holds the request being answered.
		out = out[len(out)-cfg.MaxPromptChars:]
	}
	return out
}

// run executes the child and feeds every decoded JSON value to onValue. Output
// is read on the calling goroutine so a killed child cannot leak a reader.
func (r *cliRunner) run(ctx context.Context, inv cliInvocation, onValue func(map[string]any) error) error {
	cmd, stdout, stderr, release, err := r.start(ctx, inv)
	if err != nil {
		return err
	}
	defer release()
	var (
		mu      sync.Mutex
		detail  strings.Builder
		lastAt  atomic.Int64
		stalled atomic.Bool
		stop    = make(chan struct{})
	)
	lastAt.Store(time.Now().UnixNano())
	go drainStderr(stderr, &mu, &detail)
	go r.watchdog(cmd, &lastAt, &stalled, stop)

	parser := &cliJSONStream{}
	buf := make([]byte, 64<<10)
	var pending []byte
	var readErr error
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			lastAt.Store(time.Now().UnixNano())
			pending = append(pending, buf[:n]...)
			text, rest := completeUTF8Prefix(pending)
			pending = rest
			for _, value := range parser.push(text) {
				if e := onValue(value); e != nil {
					readErr = e
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				for _, value := range parser.push(string(pending)) {
					if e := onValue(value); e != nil {
						readErr = e
						break
					}
				}
				if readErr == nil {
					for _, value := range parser.flush() {
						if e := onValue(value); e != nil {
							readErr = e
							break
						}
					}
				}
			} else {
				readErr = rerr
			}
			break
		}
	}
	close(stop)
	waitErr := cmd.Wait()

	if stalled.Load() {
		return upstream.NewError(upstream.KindTimeout, 0,
			fmt.Sprintf("trae: local CLI produced no output for %s", r.timeout()))
	}
	if readErr != nil {
		return readErr
	}
	mu.Lock()
	tail := strings.TrimSpace(detail.String())
	mu.Unlock()

	// A non-zero exit is only a failure when nothing was produced: a child that
	// exits after streaming a complete answer did its job. Diagnostics on
	// stderr are then informative rather than fatal, so they are dropped.
	if waitErr != nil && !parser.seen {
		if tail != "" {
			return upstream.NewError(upstream.KindServer, 0, "trae: local CLI failed: "+tail)
		}
		return upstream.NewError(upstream.KindServer, 0, "trae: local CLI failed: "+waitErr.Error())
	}
	return nil
}

// runText executes the child in plain-text mode, where chunk boundaries follow
// the OS read size and nothing structured is available.
func (r *cliRunner) runText(ctx context.Context, inv cliInvocation, onChunk func(string) error) error {
	cmd, stdout, stderr, release, err := r.start(ctx, inv)
	if err != nil {
		return err
	}
	defer release()
	var (
		mu      sync.Mutex
		detail  strings.Builder
		lastAt  atomic.Int64
		stalled atomic.Bool
		stop    = make(chan struct{})
	)
	lastAt.Store(time.Now().UnixNano())
	go drainStderr(stderr, &mu, &detail)
	go r.watchdog(cmd, &lastAt, &stalled, stop)

	reader := bufio.NewReaderSize(stdout, 64<<10)
	var produced bool
	var readErr error
	for {
		chunk, rerr := reader.ReadString('\n')
		if chunk != "" {
			lastAt.Store(time.Now().UnixNano())
			produced = true
			if e := onChunk(chunk); e != nil {
				readErr = e
				break
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				readErr = rerr
			}
			break
		}
	}
	close(stop)
	waitErr := cmd.Wait()

	if stalled.Load() {
		return upstream.NewError(upstream.KindTimeout, 0,
			fmt.Sprintf("trae: local CLI produced no output for %s", r.timeout()))
	}
	if readErr != nil {
		return readErr
	}
	mu.Lock()
	tail := strings.TrimSpace(detail.String())
	mu.Unlock()
	if waitErr != nil && !produced {
		if tail != "" {
			return upstream.NewError(upstream.KindServer, 0, "trae: local CLI failed: "+tail)
		}
		return upstream.NewError(upstream.KindServer, 0, "trae: local CLI failed: "+waitErr.Error())
	}
	return nil
}

// start acquires a concurrency slot and launches the child. The returned
// release must be called once the process has been reaped.
func (r *cliRunner) start(ctx context.Context, inv cliInvocation) (*exec.Cmd, io.ReadCloser, io.ReadCloser, func(), error) {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, nil, nil, ctx.Err()
	}
	release := func() { <-r.sem }

	if err := os.MkdirAll(r.cfg.WorkDir, 0o700); err != nil {
		release()
		return nil, nil, nil, nil, fmt.Errorf("trae: prepare CLI work directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, inv.command, inv.args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Env = os.Environ()
	cmd.SysProcAttr = childSysProcAttr()
	// A child that exits while a grandchild holds a pipe open must not pin this
	// slot forever, so Wait is allowed to close the pipes itself.
	cmd.WaitDelay = 5 * time.Second
	if inv.stdin != "" {
		cmd.Stdin = strings.NewReader(inv.stdin)
	} else {
		cmd.Stdin = nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		release()
		return nil, nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		release()
		return nil, nil, nil, nil, fmt.Errorf("trae: start local CLI: %w", err)
	}
	return cmd, stdout, stderr, release, nil
}

// timeout is the silence budget.
func (r *cliRunner) timeout() time.Duration {
	return time.Duration(r.cfg.TimeoutSeconds) * time.Second
}

// watchdog kills a child that has gone quiet for longer than the budget. A
// steadily streaming child is never interrupted, because every read resets it.
func (r *cliRunner) watchdog(cmd *exec.Cmd, lastAt *atomic.Int64, stalled *atomic.Bool, stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	budget := r.timeout()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if time.Since(time.Unix(0, lastAt.Load())) > budget {
				stalled.Store(true)
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
		}
	}
}

// drainStderr keeps a bounded tail of the child's diagnostics for error text.
func drainStderr(stderr io.Reader, mu *sync.Mutex, detail *strings.Builder) {
	reader := bufio.NewReaderSize(stderr, 8<<10)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			mu.Lock()
			if detail.Len() < 1200 {
				detail.WriteString(line)
			}
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// cliJSONStream splits concatenated JSON values out of a stream that may also
// contain log lines. Keeping the incomplete tail is what makes it independent of
// read boundaries.
type cliJSONStream struct {
	rest string
	seen bool
}

// push consumes text and returns every complete value it now holds.
func (s *cliJSONStream) push(text string) []map[string]any {
	if s.rest != "" {
		text = s.rest + text
	}
	values, tail := splitJSONObjects(text)
	s.rest = tail
	if len(values) > 0 {
		s.seen = true
	}
	return values
}

// flush makes a final attempt on whatever is left.
func (s *cliJSONStream) flush() []map[string]any {
	values, _ := splitJSONObjects(s.rest)
	s.rest = ""
	if len(values) > 0 {
		s.seen = true
	}
	return values
}

// splitJSONObjects extracts complete JSON objects and returns the remainder.
// Scanning brackets rather than splitting on newlines is what lets it tolerate
// both pretty-printed objects and interleaved log lines.
func splitJSONObjects(text string) ([]map[string]any, string) {
	var out []map[string]any
	cursor := 0
	for cursor < len(text) {
		start := findJSONStart(text, cursor)
		if start < 0 {
			return out, ""
		}
		end := findJSONEnd(text, start)
		if end < 0 {
			return out, text[start:]
		}
		if value, ok := decodeJSONObject(text[start:end]); ok {
			out = append(out, value)
		}
		cursor = end
	}
	return out, ""
}

// findJSONStart returns the first brace or bracket at or after offset.
func findJSONStart(text string, offset int) int {
	objStart, arrStart := -1, -1
	if i := strings.IndexByte(text[offset:], '{'); i >= 0 {
		objStart = offset + i
	}
	if i := strings.IndexByte(text[offset:], '['); i >= 0 {
		arrStart = offset + i
	}
	switch {
	case objStart < 0:
		return arrStart
	case arrStart < 0:
		return objStart
	case objStart < arrStart:
		return objStart
	default:
		return arrStart
	}
}

// findJSONEnd returns the index just past the value starting at start. Strings
// and escapes are tracked so a brace inside a message is not read as structure.
func findJSONEnd(text string, start int) int {
	var stack []byte
	inString, escaped := false, false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}', ']':
			if len(stack) == 0 {
				return -1
			}
			if open := stack[len(stack)-1]; (open == '{' && ch != '}') || (open == '[' && ch != ']') {
				return -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// decodeJSONObject parses one value, reporting false when it is not an object.
func decodeJSONObject(raw string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, false
	}
	return value, true
}

// completeUTF8Prefix returns the longest prefix of b that ends on a rune
// boundary, plus the bytes that must wait for the next read. Without this a
// multi-byte character split across two reads would be corrupted.
func completeUTF8Prefix(b []byte) (string, []byte) {
	end := 0
	for end < len(b) {
		r, size := utf8.DecodeRune(b[end:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		end += size
	}
	if end == len(b) {
		return string(b), nil
	}
	// Keep the tail only when it could still become a valid sequence; anything
	// else is unrepairable and is handed on so the stream does not stall.
	if len(b)-end > utf8.UTFMax {
		return string(b), nil
	}
	return string(b[:end]), b[end:]
}

// cliValueText reads the answer and reasoning snapshots out of one value.
func cliValueText(value map[string]any) (text, reasoning string) {
	text = cliMessageText(value)
	reasoning = cliMessageReasoning(value)
	if text == "" && reasoning == "" {
		// Some shapes nest the payload one level down.
		if nested, ok := value["message"].(map[string]any); ok {
			return messageText(nested), messageReasoning(nested)
		}
	}
	return text, reasoning
}

// cliMessageText reads the answer from a CLI value, which carries it either at
// the top level or under "message".
func cliMessageText(value map[string]any) string {
	if s := messageText(value); s != "" {
		return s
	}
	if nested, ok := value["message"].(map[string]any); ok {
		if s := messageText(nested); s != "" {
			return s
		}
	}
	return ""
}

func cliMessageReasoning(value map[string]any) string {
	if s := messageReasoning(value); s != "" {
		return s
	}
	if nested, ok := value["message"].(map[string]any); ok {
		return messageReasoning(nested)
	}
	return ""
}

// cliValueUsage reads token accounting, which may sit at the top level or under
// a response-metadata block.
func cliValueUsage(value map[string]any) *upstream.Usage {
	if usage := usageOf(value); usage != nil {
		return usage
	}
	if meta, ok := value["response_meta"].(map[string]any); ok {
		return usageOf(meta)
	}
	if nested, ok := value["message"].(map[string]any); ok {
		if meta, ok := nested["response_meta"].(map[string]any); ok {
			return usageOf(meta)
		}
	}
	return nil
}

// cliFinish reads a stop reason when the child reports one. It is not the
// termination signal: a child ends the request by exiting.
func cliFinish(value map[string]any) string {
	for _, key := range []string{"finish_reason", "finishReason", "stop_reason", "stopReason"} {
		if s := firstString(value, key); s != "" {
			return normalizeFinish(s)
		}
	}
	return ""
}
