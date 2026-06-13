package adapters

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// CodexName is the registry name of the Codex CLI adapter.
const CodexName = "codex"

// CodexOptions configures how the Codex CLI is driven.
type CodexOptions struct {
	// Bin is the codex executable (default "codex"). Override with
	// VICHU_CODEX_BIN — also how tests point at a stub.
	Bin string
	// Sandbox is passed as --sandbox (default "workspace-write"): the worker may
	// edit files in the work dir but not reach the network or paths outside it.
	// Codex's sandbox is its safety boundary; unlike claude-code it has no
	// per-tool deny list, so the engine's own mutation tracking and policy remain
	// the verified backstop (a worker that writes out of scope still blocks the
	// run after the fact).
	Sandbox string
	// ExtraArgs are appended verbatim (escape hatch for --full-auto, -c key=val …).
	ExtraArgs []string
}

// Codex runs workers via the Codex CLI in non-interactive exec mode with
// streaming JSON:
//
//	codex exec --json --sandbox workspace-write [resume <id>] -
//
// The prompt is fed on stdin; JSONL "thread events" are normalized into
// AgentEvents, and the final assistant message, thread id (for resume), and
// token usage are folded into the core.Result. Codex reports tokens but not a
// USD cost, so CostReporting is false.
type Codex struct {
	opts CodexOptions
}

// NewCodex builds the adapter with explicit options.
func NewCodex(opts CodexOptions) *Codex {
	if opts.Bin == "" {
		opts.Bin = "codex"
	}
	if opts.Sandbox == "" {
		opts.Sandbox = "workspace-write"
	}
	return &Codex{opts: opts}
}

// NewCodexFromEnv builds the adapter honoring VICHU_CODEX_* overrides.
func NewCodexFromEnv() *Codex {
	return NewCodex(CodexOptions{
		Bin:       os.Getenv("VICHU_CODEX_BIN"),
		Sandbox:   os.Getenv("VICHU_CODEX_SANDBOX"),
		ExtraArgs: strings.Fields(os.Getenv("VICHU_CODEX_EXTRA_ARGS")),
	})
}

func (c *Codex) Name() string { return CodexName }

// Supported codex CLI major-version range. Codex is pre-1.0, so 0.x is in range;
// outside it Probe degrades with a clear reason instead of failing mid-run on
// changed flags or output formats.
const (
	codexMinMajor = 0
	codexMaxMajor = 1
)

func (c *Codex) Probe(ctx context.Context) (Availability, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, c.opts.Bin, "--version").Output()
	if err != nil {
		return Availability{
			Name:      CodexName,
			Available: false,
			Reason:    "codex CLI not found or not runnable (install Codex CLI, or set VICHU_CODEX_BIN)",
		}, nil
	}
	version := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])

	if major, ok := parseMajor(stripName(version)); ok && (major < codexMinMajor || major > codexMaxMajor) {
		return Availability{
			Name:    CodexName,
			Version: version,
			Reason:  fmt.Sprintf("unsupported codex CLI version %s (supported: %d.x–%d.x)", version, codexMinMajor, codexMaxMajor),
		}, nil
	}

	if reason := c.authReason(ctx); reason != "" {
		return Availability{Name: CodexName, Version: version, Reason: reason}, nil
	}
	return Availability{Name: CodexName, Available: true, Version: version}, nil
}

// authReason returns a non-empty reason when codex is clearly unauthenticated.
// An API key in the environment authenticates non-interactively; otherwise we
// ask `codex login status`. Older CLIs without that subcommand (or any error
// running it) stay permissive — probing must not wrongly block a usable CLI.
func (c *Codex) authReason(ctx context.Context) string {
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "" {
		return ""
	}
	// `codex login status` exits non-zero BOTH when logged out and when the
	// subcommand is missing on an older CLI, so the exit code can't tell them
	// apart — only the output can. Degrade to unavailable solely on an explicit
	// "not logged in"; anything else stays permissive.
	out, _ := exec.CommandContext(ctx, c.opts.Bin, "login", "status").CombinedOutput()
	if strings.Contains(strings.ToLower(string(out)), "not logged in") {
		return "codex CLI is not authenticated — run `codex login`, or set OPENAI_API_KEY"
	}
	return ""
}

// stripName drops a leading tool name from a version string like
// "codex-cli 0.30.0" → "0.30.0", so parseMajor sees the number.
func stripName(version string) string {
	if i := strings.LastIndexByte(version, ' '); i >= 0 {
		return version[i+1:]
	}
	return version
}

func (c *Codex) Capabilities() Caps {
	return Caps{Streaming: true, Resume: true, CostReporting: false, StructuredOutput: false}
}

func (c *Codex) Start(ctx context.Context, inv Invocation) (Session, error) {
	return c.launch(ctx, inv, "")
}

func (c *Codex) Resume(ctx context.Context, sessionID string, inv Invocation) (Session, error) {
	return c.launch(ctx, inv, sessionID)
}

func (c *Codex) launch(ctx context.Context, inv Invocation, resumeID string) (Session, error) {
	cmd := exec.CommandContext(ctx, c.opts.Bin, c.buildArgs(inv, resumeID)...)
	cmd.Dir = inv.WorkDir
	cmd.Env = mergeEnv(inv.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting codex: %w", err)
	}

	// The prompt goes via stdin: prompts include the context pack and can exceed
	// OS argv limits.
	go func() {
		_, _ = io.WriteString(stdin, inv.Prompt)
		_ = stdin.Close()
	}()

	sess := &cmdSession{
		events:    make(chan AgentEvent, 64),
		done:      make(chan struct{}),
		interrupt: func() error { return cmd.Process.Kill() },
	}
	go func() {
		oc := streamCodexEvents(stdout, sess.events)
		werr := cmd.Wait()
		sess.events <- AgentEvent{Kind: EventDone}
		close(sess.events)
		sess.finish(buildCodexResult(oc, werr, &stderr))
	}()
	return sess, nil
}

// buildArgs assembles the codex CLI arguments for an invocation. The prompt is
// read from stdin ("-"). exec-level flags (--json, --sandbox, --model) MUST
// precede the `resume <id>` subcommand: `codex exec resume <id> --sandbox …`
// rejects the flags ("unexpected argument '--sandbox'"). Correct order is
// `codex exec --json --sandbox <mode> [--model <m>] resume <id> -`.
func (c *Codex) buildArgs(inv Invocation, resumeID string) []string {
	args := []string{"exec", "--json", "--sandbox", c.opts.Sandbox}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	args = append(args, c.opts.ExtraArgs...)
	if resumeID != "" {
		args = append(args, "resume", resumeID)
	}
	return append(args, "-")
}

// streamCodexEvents reads JSONL lines, forwarding normalized events and folding
// the outcome-bearing fields into a codexOutcome.
func streamCodexEvents(stdout io.Reader, out chan<- AgentEvent) codexOutcome {
	var oc codexOutcome
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool results can be large
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		for _, ev := range decodeCodexLine(line, &oc) {
			out <- ev
		}
	}
	return oc
}

// buildCodexResult turns the accumulated outcome (or its absence) into a
// core.Result. A run that produced neither a completed turn nor any assistant
// message is treated as a failure, surfacing stderr for diagnosis.
func buildCodexResult(oc codexOutcome, werr error, stderr *bytes.Buffer) (core.Result, error) {
	if !oc.completed && oc.lastMessage == "" && oc.errMsg == "" {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && werr != nil {
			msg = werr.Error()
		}
		return core.Result{}, fmt.Errorf("codex exited without a completed turn: %s", tail(msg, 2000))
	}
	exitMsg := "completed"
	if oc.errMsg != "" {
		exitMsg = oc.errMsg
	}
	result := core.Result{
		Markdown:    oc.lastMessage,
		TokensIn:    oc.tokensIn,
		TokensOut:   oc.tokensOut,
		SessionID:   oc.threadID,
		ExitMessage: exitMsg,
	}
	if oc.errMsg != "" {
		return result, fmt.Errorf("codex reported an error: %s", tail(oc.errMsg, 2000))
	}
	return result, nil
}
