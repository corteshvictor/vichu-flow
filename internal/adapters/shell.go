package adapters

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// ShellName is the registry name of the shell adapter.
const ShellName = "shell"

// shellResultCap bounds how much command output is kept in the worker result.
// The full output is always available to the engine via the event stream and
// gate logs; this only caps what travels as the worker's result payload.
const shellResultCap = 32 * 1024

// Shell runs an arbitrary command as a worker. It is the always-available
// baseline adapter, requiring no agent CLI, and is driven by Invocation.Command.
type Shell struct{}

// NewShell returns the shell adapter.
func NewShell() *Shell { return &Shell{} }

func (s *Shell) Name() string { return ShellName }

func (s *Shell) Probe(context.Context) (Availability, error) {
	return Availability{Name: ShellName, Available: true, Version: "builtin"}, nil
}

func (s *Shell) Capabilities() Caps {
	return Caps{Streaming: true}
}

func (s *Shell) Start(ctx context.Context, inv Invocation) (Session, error) {
	if len(inv.Command) == 0 {
		return nil, errors.New("shell adapter requires Invocation.Command")
	}

	cmd := exec.CommandContext(ctx, inv.Command[0], inv.Command[1:]...)
	cmd.Dir = inv.WorkDir
	cmd.Env = mergeEnv(inv.Env)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting command: %w", err)
	}

	sess := &cmdSession{
		events:    make(chan AgentEvent, 64),
		done:      make(chan struct{}),
		interrupt: func() error { return cmd.Process.Kill() },
	}

	var captured strings.Builder
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if captured.Len() < shellResultCap {
				captured.WriteString(line)
				captured.WriteByte('\n')
			}
			sess.events <- AgentEvent{Kind: EventText, Text: line}
		}
	}()

	go func() {
		werr := cmd.Wait() // waits for process and output copier
		_ = pw.Close()     // signal reader EOF
		<-readerDone       // let the reader drain remaining output
		sess.events <- AgentEvent{Kind: EventDone}
		close(sess.events)

		exitCode := 0
		exitMsg := "ok"
		var exitErr *exec.ExitError
		if errors.As(werr, &exitErr) {
			exitCode = exitErr.ExitCode() // -1 when killed by a signal (e.g. cancel)
			exitMsg = fmt.Sprintf("exited with code %d", exitCode)
		} else if werr != nil {
			exitMsg = werr.Error()
		}

		// A shell command has no token/cost usage, so CostReported/TokensReported stay
		// false (not applicable) — the run reports those as unknown, never a fake $0.
		result := core.Result{
			Markdown:    captured.String(),
			Data:        map[string]any{"exit_code": exitCode, "command": strings.Join(inv.Command, " ")},
			ExitMessage: exitMsg,
		}
		// A non-zero exit is a worker failure — the run must not advance on a
		// failed script — unless the invocation explicitly allows it. The result
		// is still populated so the audit trail keeps the captured output.
		var resultErr error
		if werr != nil && !inv.AllowNonZeroExit {
			resultErr = fmt.Errorf("shell worker %s", exitMsg)
		}
		sess.finish(result, resultErr)
	}()

	return sess, nil
}

func (s *Shell) Resume(context.Context, string, Invocation) (Session, error) {
	return nil, ErrResumeUnsupported
}

// mergeEnv returns the current environment with extra entries appended.
func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
