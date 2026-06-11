// Package gates runs verification commands (test, lint, typecheck) and records
// their verified results. This is the heart of "the runtime does not trust the
// agent": VichuFlow runs these commands itself, captures exit code and output,
// and only that verdict — never agent-authored markdown — authorizes a stage
// transition.
package gates

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
)

// Spec is a fully resolved gate command (OS-specific selection already done by
// config). Command[0] is the executable, the rest are arguments.
type Spec struct {
	Name    string
	Command []string
	Dir     string
	Env     map[string]string
}

// Runner executes gates and persists their evidence into a run.
type Runner struct {
	store *runtime.Store
}

// NewRunner returns a gate runner that writes evidence through store.
func NewRunner(store *runtime.Store) *Runner { return &Runner{store: store} }

// Run executes a gate, streaming all output to the gate's output.log and
// writing command.json and verdict.json. The returned verdict's Passed field is
// the authoritative signal for the engine.
func (r *Runner) Run(ctx context.Context, runID, stage string, n int, spec Spec) (*core.GateVerdict, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("gate requires a command")
	}

	cmdRecord := &core.GateCommand{
		Name:    spec.Name,
		Command: spec.Command[0],
		Args:    spec.Command[1:],
		Dir:     spec.Dir,
	}
	if err := r.store.SaveGateCommand(runID, stage, n, cmdRecord); err != nil {
		return nil, err
	}

	outPath := r.store.GateOutputPath(runID, stage, n)
	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnv(spec.Env)
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	finish := time.Now()
	_ = out.Close()

	exitCode, passed := classify(runErr)

	var size int64
	if info, statErr := os.Stat(outPath); statErr == nil {
		size = info.Size()
	}

	verdict := &core.GateVerdict{
		Name:        spec.Name,
		Command:     strings.Join(spec.Command, " "),
		ExitCode:    exitCode,
		Passed:      passed,
		DurationMS:  finish.Sub(start).Milliseconds(),
		OutputPath:  outPath,
		OutputBytes: size,
		StartedAt:   start.UTC(),
		FinishedAt:  finish.UTC(),
	}
	if err := r.store.SaveGateVerdict(runID, stage, n, verdict); err != nil {
		return nil, err
	}
	return verdict, nil
}

// classify turns a command's run error into an exit code and pass/fail. A
// command that fails to start (e.g. missing binary) is a non-pass with code -1.
func classify(runErr error) (exitCode int, passed bool) {
	if runErr == nil {
		return 0, true
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return ee.ExitCode(), false
	}
	return -1, false
}

func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
