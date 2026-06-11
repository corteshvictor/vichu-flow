package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// ClaudeCodeName is the registry name of the Claude Code adapter.
const ClaudeCodeName = "claude-code"

// ClaudeCodeOptions configures how the Claude Code CLI is driven.
type ClaudeCodeOptions struct {
	// Bin is the claude executable (default "claude"). Override with
	// VICHU_CLAUDE_BIN — also how tests point at a stub.
	Bin string
	// PermissionMode is passed as --permission-mode (default "acceptEdits"):
	// the worker can edit files in the work dir, while tools that would need an
	// interactive permission prompt are auto-denied — a headless worker must
	// never hang waiting for a human.
	PermissionMode string
	// ExtraArgs are appended verbatim (escape hatch for --allowedTools etc.).
	ExtraArgs []string
}

// ClaudeCode runs workers via the Claude Code CLI in headless print mode:
//
//	claude -p --output-format stream-json --verbose [--resume <id>]
//
// stream-json events are normalized into AgentEvents; the final result event
// carries the session id (persisted for resume), cost, and token usage.
type ClaudeCode struct {
	opts ClaudeCodeOptions
}

// NewClaudeCode builds the adapter with explicit options.
func NewClaudeCode(opts ClaudeCodeOptions) *ClaudeCode {
	if opts.Bin == "" {
		opts.Bin = "claude"
	}
	if opts.PermissionMode == "" {
		opts.PermissionMode = "acceptEdits"
	}
	return &ClaudeCode{opts: opts}
}

// NewClaudeCodeFromEnv builds the adapter honoring VICHU_CLAUDE_* overrides.
func NewClaudeCodeFromEnv() *ClaudeCode {
	return NewClaudeCode(ClaudeCodeOptions{
		Bin:            os.Getenv("VICHU_CLAUDE_BIN"),
		PermissionMode: os.Getenv("VICHU_CLAUDE_PERMISSION_MODE"),
		ExtraArgs:      strings.Fields(os.Getenv("VICHU_CLAUDE_EXTRA_ARGS")),
	})
}

func (c *ClaudeCode) Name() string { return ClaudeCodeName }

// Supported claude CLI major-version range. Outside it, Probe degrades with a
// clear reason instead of failing mid-run on changed flags or output formats.
const (
	claudeMinMajor = 1
	claudeMaxMajor = 2
)

func (c *ClaudeCode) Probe(ctx context.Context) (Availability, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, c.opts.Bin, "--version").Output()
	if err != nil {
		return Availability{
			Name:      ClaudeCodeName,
			Available: false,
			Reason:    "claude CLI not found or not runnable (install Claude Code, or set VICHU_CLAUDE_BIN)",
		}, nil
	}
	version := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])

	if major, ok := parseMajor(version); ok && (major < claudeMinMajor || major > claudeMaxMajor) {
		return Availability{
			Name:    ClaudeCodeName,
			Version: version,
			Reason:  fmt.Sprintf("unsupported claude CLI version %s (supported: %d.x–%d.x)", version, claudeMinMajor, claudeMaxMajor),
		}, nil
	}

	// Non-destructive auth check: `claude auth status` reports loggedIn as
	// JSON. Older CLIs without the subcommand degrade to version-only probing.
	authOut, authErr := exec.CommandContext(ctx, c.opts.Bin, "auth", "status").Output()
	if authErr == nil {
		var st struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if json.Unmarshal(authOut, &st) == nil && !st.LoggedIn {
			return Availability{
				Name:    ClaudeCodeName,
				Version: version,
				Reason:  "claude CLI is not authenticated — run `claude` once to log in",
			}, nil
		}
	}

	return Availability{Name: ClaudeCodeName, Available: true, Version: version}, nil
}

// parseMajor extracts the leading major version from strings like
// "2.1.170 (Claude Code)". ok=false when the format is unrecognized (probing
// stays permissive rather than rejecting a CLI it can't parse).
func parseMajor(version string) (int, bool) {
	head := version
	if i := strings.IndexAny(head, " ("); i > 0 {
		head = head[:i]
	}
	parts := strings.SplitN(head, ".", 2)
	major := 0
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, false
	}
	return major, true
}

func (c *ClaudeCode) Capabilities() Caps {
	return Caps{Streaming: true, Resume: true, CostReporting: true}
}

func (c *ClaudeCode) Start(ctx context.Context, inv Invocation) (Session, error) {
	return c.launch(ctx, inv, "")
}

func (c *ClaudeCode) Resume(ctx context.Context, sessionID string, inv Invocation) (Session, error) {
	if sessionID == "" {
		return c.launch(ctx, inv, "")
	}
	return c.launch(ctx, inv, sessionID)
}

func (c *ClaudeCode) launch(ctx context.Context, inv Invocation, resumeID string) (Session, error) {
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
		return nil, fmt.Errorf("starting claude: %w", err)
	}

	// The prompt goes via stdin: prompts include the context pack and can
	// exceed OS argv limits.
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
		final, sessionID := streamEvents(stdout, sess.events)
		werr := cmd.Wait()
		sess.events <- AgentEvent{Kind: EventDone}
		close(sess.events)
		sess.finish(buildResult(final, sessionID, werr, &stderr))
	}()
	return sess, nil
}

// buildArgs assembles the claude CLI arguments for an invocation.
func (c *ClaudeCode) buildArgs(inv Invocation, resumeID string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose", // required by the CLI for stream-json in print mode
		"--permission-mode", c.opts.PermissionMode,
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	// The security policy travels into the agent's own permission system, so
	// what vichu would block is also denied inside the worker.
	if len(inv.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(inv.DisallowedTools, ","))
	}
	return append(args, c.opts.ExtraArgs...)
}

// streamEvents reads stream-json lines, forwarding normalized events and
// returning the final result line and the last session id seen.
func streamEvents(stdout io.Reader, out chan<- AgentEvent) (*claudeResult, string) {
	var final *claudeResult
	var sessionID string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool results can be large
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		evs, sid, res := decodeClaudeLine(line)
		if sid != "" {
			sessionID = sid
		}
		if res != nil {
			final = res
		}
		for _, ev := range evs {
			out <- ev
		}
	}
	return final, sessionID
}

// buildResult turns the final stream line (or its absence) into a core.Result.
func buildResult(final *claudeResult, sessionID string, werr error, stderr *bytes.Buffer) (core.Result, error) {
	if final == nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && werr != nil {
			msg = werr.Error()
		}
		return core.Result{}, fmt.Errorf("claude exited without a result event: %s", tail(msg, 2000))
	}
	result := core.Result{
		Markdown:    final.Result,
		CostUSD:     final.TotalCostUSD,
		TokensIn:    final.Usage.InputTokens,
		TokensOut:   final.Usage.OutputTokens,
		SessionID:   firstNonEmpty(final.SessionID, sessionID),
		ExitMessage: final.Subtype,
	}
	if final.IsError {
		return result, fmt.Errorf("claude reported an error result (%s): %s", final.Subtype, tail(final.Result, 2000))
	}
	return result, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tail returns the last maxBytes bytes of s.
func tail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return "…" + s[len(s)-maxBytes:]
}
