package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// newTestRepo initializes a git repo with one commit and returns its path.
func newTestRepo(t *testing.T) string {
	t.Helper()
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "init")
	return dir
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", msg}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// testEngine wires an engine with a fake adapter whose implementer writes a
// source file plus a passing test script, and a test gate that runs it.
func testEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	// A test gate that passes by checking the file the implementer wrote exists.
	// Windows needs `cmd /c` because `if exist` is a cmd.exe builtin, not a binary.
	checkCmd := "test -f src/feature.txt"
	if runtime.GOOS == "windows" {
		checkCmd = "cmd /c if exist src\\feature.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: checkCmd, Windows: checkCmd}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			ResultText: "did the work",
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
			},
		}), nil
	})

	return New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})
}

func TestQuickRunEndToEnd(t *testing.T) {
	dir := newTestRepo(t)
	e := testEngine(t, dir)

	state, err := e.Start(context.Background(), "add a feature file", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (blocked: %s)", state.Status, state.BlockedReason)
	}

	store := rt.Open(dir)

	// The implementer's mutation must be recorded as verified evidence.
	if !mutationRecorded(store, state.RunID, "src/feature.txt") {
		t.Fatal("expected src/feature.txt in a worker's mutations.json")
	}

	// The gate evidence must exist and have passed.
	verdict := filepath.Join(store.GateDir(state.RunID, "verify", 1), "verdict.json")
	if _, err := os.Stat(verdict); err != nil {
		t.Fatalf("expected gate verdict.json: %v", err)
	}

	// The event timeline must record creation and completion.
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventRunCreated) || !hasEvent(events, core.EventRunCompleted) {
		t.Fatal("timeline missing run_created/run_completed")
	}
}

// TestRequireGatesBlocksUnverifiedRun: with workflow.requireGates on, a run whose
// verify stage wanted gates but found none configured must BLOCK — it must not
// report "completed" having verified nothing.
func TestRequireGatesBlocksUnverifiedRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	required := true
	cfg.Workflow.RequireGates = &required
	cfg.Commands = nil // no test/lint/typecheck configured → verify has nothing to run

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "nothing was verified") {
		t.Fatalf("requireGates must block an unverified run, got %s (%s)", state.Status, state.BlockedReason)
	}
}

// TestRunOnRepoWithNoCommits: VichuFlow must run on a freshly `git init`'d repo
// with NO commits (unborn branch) — the user shouldn't need an initial commit.
func TestRunOnRepoWithNoCommits(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = nil // keep this about the workspace, not gates
	noGates := false
	cfg.Workflow.RequireGates = &noGates // this test is about the workspace, not verification

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "feature.txt", Content: "hi\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("a run on a repo with no commits should complete, got %s (%s)", state.Status, state.BlockedReason)
	}
	if !mutationRecorded(store, state.RunID, "feature.txt") {
		t.Fatal("the worker's mutation must be audited on an unborn branch")
	}
}

func TestGateFailureBlocksRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": failingGate()}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })

	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})
	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("failing gate should block the run, got %s", state.Status)
	}
}

func TestDriftIgnoresOwnChangesButCatchesExternal(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": failingGate()} // block after implement

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "x\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("expected blocked at gate, got %s", state.Status)
	}

	snap, err := store.LoadWorkspace(state.RunID)
	if err != nil {
		t.Fatal(err)
	}

	// The run's own mutation (src/feature.txt) must NOT count as drift.
	if drift, reason := e.checkDrift(state.RunID, snap); drift {
		t.Fatalf("run's own change should not be drift, got %q", reason)
	}

	// An external change to an unrelated file MUST be detected as drift.
	if err := os.WriteFile(filepath.Join(dir, "external.txt"), []byte("oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, reason := e.checkDrift(state.RunID, snap)
	if !drift {
		t.Fatal("external change should be detected as drift")
	}
	if reason == "" {
		t.Fatal("drift should carry a reason")
	}
}

// failingGate returns a gate command that exits non-zero on every platform
// using real executables (`false`) or `cmd /c` (builtins need the shell).
func failingGate() config.OSCommand {
	return config.OSCommand{Unix: "false", Windows: "cmd /c exit 1"}
}

func TestCancelInterruptsActiveRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	// Fast explorer, then an implementer that would run for 60s if not killed.
	sleepCmd := "sleep 60"
	if runtime.GOOS == "windows" {
		sleepCmd = "ping -n 61 127.0.0.1"
	}
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "fake"}
	cfg.Agents["implementer"] = config.AgentConfig{Provider: "shell", Command: sleepCmd}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	done := make(chan *core.State, 1)
	go func() {
		st, err := e.Start(context.Background(), "task", "quick")
		if err != nil {
			t.Errorf("Start: %v", err)
		}
		done <- st
	}()

	// Wait until the slow implementer is the active worker.
	runID := waitFor(t, 15*time.Second, func() (string, bool) {
		id, _ := store.LatestRun()
		if id == "" {
			return "", false
		}
		st, err := store.LoadState(id)
		return id, err == nil && st.CurrentStage == "implement" && st.ActiveWorker != ""
	})

	// Cancel from "another process": write canceled to disk, as `vichu cancel` does.
	st, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	st.Status = core.StatusCanceled
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}

	select {
	case final := <-done:
		if final.Status != core.StatusCanceled {
			t.Fatalf("engine must honor external cancel, got %s", final.Status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("engine did not stop after cancel — worker ran to completion")
	}

	// The canceled status must survive on disk (not be overwritten).
	finalDisk, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	if finalDisk.Status != core.StatusCanceled {
		t.Fatalf("disk state overwritten after cancel: %s", finalDisk.Status)
	}

	// The interrupted worker's audit must say canceled — not done.
	assertWorkerStatus(t, store, runID, "implement-02", core.WorkerCanceled)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() (string, bool)) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id, ok := cond(); ok {
			return id
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
	return ""
}

func TestDriftDetectsExternalEditToWorkerFile(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": failingGate()} // block after implement

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "feature.txt", Content: "v1\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("expected blocked at gate, got %s", state.Status)
	}
	snap, err := store.LoadWorkspace(state.RunID)
	if err != nil {
		t.Fatal(err)
	}

	// Same path the worker touched, same name — but different CONTENT.
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, reason := e.checkDrift(state.RunID, snap)
	if !drift {
		t.Fatal("external edit to a worker-touched file must be drift")
	}
	if reason == "" {
		t.Fatal("drift must carry a reason")
	}

	// And Resume must block the run with workspace_drift instead of continuing.
	resumed, err := e.Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != core.StatusBlocked || !strings.Contains(resumed.BlockedReason, "workspace_drift") {
		t.Fatalf("resume over drift must block with workspace_drift, got %s (%s)", resumed.Status, resumed.BlockedReason)
	}
}

func TestSensitiveMutationBlocksRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default() // sensitiveMutations defaults to block
	cfg.Workspace.RequireCleanTree = "allow"

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: ".github/workflows/evil.yml", Content: "on: push\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "sensitive") {
		t.Fatalf("CI config mutation must block, got %s (%s)", state.Status, state.BlockedReason)
	}
}

func TestReadOnlyStageBlocksOnMutation(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				// The EXPLORER (read-only stage) writes a file — must block.
				"explorer": {{Type: "write_file", Path: "sneaky.txt", Content: "x\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "read-only") {
		t.Fatalf("mutation in read-only stage must block, got %s (%s)", state.Status, state.BlockedReason)
	}
}

func TestBudgetExhaustionBlocksRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Budgets.Run.MaxAgentInvocations = 1 // explore consumes it; implement must block

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "budget") {
		t.Fatalf("exhausted budget must block with reason, got %s (%s)", state.Status, state.BlockedReason)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventBudgetExceeded) {
		t.Fatal("budget_exceeded event missing from timeline")
	}
}

// TestAgentInvocationBudgetGatesOnlyAgentStarts: the agent-invocation budget
// must gate only the START of an agent — not gates or completion. A budget of N
// allows exactly N agents (verify/done still run); the (N+1)th agent is blocked
// before it starts.
func TestAgentInvocationBudgetGatesOnlyAgentStarts(t *testing.T) {
	t.Run("exactly N completes", func(t *testing.T) {
		dir := newTestRepo(t)
		e := testEngine(t, dir)
		e.cfg.Budgets.Run.MaxAgentInvocations = 2 // explore + implement = 2 agents

		state, err := e.Start(context.Background(), "add a feature file", "quick")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		// The 2 agents were used exactly; the verify gate and done stage must still
		// run — the budget never starts a 3rd agent, but it must not block a gate.
		if state.Status != core.StatusCompleted {
			t.Fatalf("maxAgentInvocations=2 must let quick (2 agents) complete, got %s (%s)", state.Status, state.BlockedReason)
		}
	})

	t.Run("blocks before the over-budget agent", func(t *testing.T) {
		dir := newTestRepo(t)
		store := rt.Open(dir)
		e := testEngine(t, dir)
		e.cfg.Budgets.Run.MaxAgentInvocations = 1 // explore consumes it; implement must not start

		state, err := e.Start(context.Background(), "add a feature file", "quick")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "agent invocation budget") {
			t.Fatalf("must block before implement, got %s (%s)", state.Status, state.BlockedReason)
		}
		// The implement agent must NEVER have started — only explore ran.
		workers, _ := store.ListWorkers(state.RunID)
		if len(workers) != 1 || workers[0] != "explore-01" {
			t.Fatalf("only explore should have run, got workers %v", workers)
		}
	})
}

func TestBlockedFixAcceptChangesCompletes(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	// Gate requires fixed.txt, which the worker does NOT create → blocked.
	check := "test -f fixed.txt"
	if runtime.GOOS == "windows" {
		check = "cmd /c if exist fixed.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: check, Windows: check}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			ResultText: "summary of my work",
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "attempt.txt", Content: "tried\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("expected blocked at gate, got %s", state.Status)
	}

	// Evidence persisted while blocked: stage summaries and the gate excerpt.
	if store.StageSummary(state.RunID, "implement") == "" {
		t.Fatal("summaries/implement.md not persisted")
	}
	if _, err := os.Stat(filepath.Join(store.GateDir(state.RunID, "verify", 1), "excerpt.txt")); err != nil {
		t.Fatalf("gate excerpt.txt not persisted: %v", err)
	}

	// The user fixes the problem by hand (external change).
	if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("manual fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plain resume must refuse (drift)...
	blocked, err := e.Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != core.StatusBlocked || !strings.Contains(blocked.BlockedReason, "workspace_drift") {
		t.Fatalf("plain resume over manual fix must block, got %s (%s)", blocked.Status, blocked.BlockedReason)
	}

	// ...and --accept-changes must re-baseline and complete.
	final, err := e.Resume(context.Background(), state.RunID, ResumeOptions{AcceptChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != core.StatusCompleted {
		t.Fatalf("accept-changes resume should complete, got %s (%s)", final.Status, final.BlockedReason)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, "workspace_rebaselined") {
		t.Fatal("workspace_rebaselined event missing")
	}
}

// TestFailedWorkerStillAuditsMutations: a worker that modifies files and THEN
// fails must still leave a mutations.json — a failed run is no excuse to skip
// the audit of what the agent touched.
func TestFailedWorkerStillAuditsMutations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	// The implementer writes a file and THEN exits non-zero.
	cfg.Agents["implementer"] = config.AgentConfig{Provider: "shell", Command: "sh -c 'echo x > touched.txt; exit 1'"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusFailed {
		t.Fatalf("failing worker must fail the run, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The file the worker wrote before failing must exist on disk...
	if _, err := os.Stat(filepath.Join(dir, "touched.txt")); err != nil {
		t.Fatalf("worker's file should exist: %v", err)
	}
	// ...and it MUST be audited in a mutations.json despite the failure.
	if !mutationRecorded(store, state.RunID, "touched.txt") {
		t.Fatal("a failed worker's mutation must still be audited in mutations.json")
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventMutationTracked) {
		t.Fatal("a failed worker must still emit mutation_tracked")
	}
}

// TestFailedWorkerStillCountsBudget: a worker that reports real cost/tokens and
// THEN fails must still have that spend aggregated into the run budget — status
// must not under-report what was actually burned.
func TestFailedWorkerStillCountsBudget(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	reg := adapters.NewRegistry()
	// The explorer reports cost/tokens AND fails (Result returns an error).
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{CostUSD: 5, TokensIn: 100, TokensOut: 50, ResultErr: "boom"}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusFailed {
		t.Fatalf("worker error must fail the run, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The failed worker's spend must count toward the run budget...
	if state.Budgets.CostUSDSpent != 5 || state.Budgets.TokensTotalSpent() != 150 {
		t.Fatalf("failed worker spend must aggregate: cost=%v tokens=%d", state.Budgets.CostUSDSpent, state.Budgets.TokensTotalSpent())
	}
	// ...and that must be persisted, not just in memory.
	disk, err := store.LoadState(state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Budgets.CostUSDSpent != 5 || disk.Budgets.TokensTotalSpent() != 150 {
		t.Fatalf("state.json must persist the failed worker's spend, got cost=%v tokens=%d", disk.Budgets.CostUSDSpent, disk.Budgets.TokensTotalSpent())
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, "token_usage") {
		t.Fatal("token_usage must be emitted even for a failed worker")
	}
}

func TestShellWorkerNonZeroExitFailsRun(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	failWorker := "false"
	if runtime.GOOS == "windows" {
		failWorker = "cmd /c exit 7"
	}
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "shell", Command: failWorker}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusFailed {
		t.Fatalf("failing shell worker must fail the run, got %s", state.Status)
	}
	assertWorkerStatus(t, store, state.RunID, "explore-01", core.WorkerFailed)
	// A failed run clears its transients: no active worker, no next action.
	assertNoActiveWorker(t, store, state.RunID)
}

func TestShellWorkerAllowNonZeroExit(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	failWorker := "false"
	if runtime.GOOS == "windows" {
		failWorker = "cmd /c exit 7"
	}
	cfg.Agents["default"] = config.AgentConfig{Provider: "shell", Command: failWorker, AllowNonZeroExit: true}
	noGates := false
	cfg.Workflow.RequireGates = &noGates // this test is about allowNonZeroExit, not gates

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("allowNonZeroExit worker should not fail the run, got %s (%s)", state.Status, state.BlockedReason)
	}
}

func TestPolicyBlocksDangerousWorkerCommand(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default() // requireConfirmationFor defaults include git_push
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "shell", Command: "git push origin main"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "policy") {
		t.Fatalf("git push worker must be policy-blocked BEFORE running, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The command must never have executed: no worker result exists.
	if _, err := store.LoadMutationReport(state.RunID, "explore-01"); err == nil {
		t.Fatal("blocked worker should not have produced mutations")
	}
}

func TestPolicyBlocksDangerousGateCommand(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "rm -rf build", Windows: "rm -rf build"}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "confirmation") {
		t.Fatalf("destructive gate command must be policy-blocked, got %s (%s)", state.Status, state.BlockedReason)
	}
}

// TestPolicyBlocksGitGlobalOptionBypass is the reviewer's git-global-option
// bypass, end to end: `git -C . clean -fd build` must block before running and
// leave the directory intact.
func TestPolicyBlocksGitGlobalOptionBypass(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	// An untracked file that `git clean` would delete if the policy missed it.
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "sentinel.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "git -C . clean -fd build", Windows: "git -C . clean -fd build"}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("git clean via -C must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "sentinel.txt")); err != nil {
		t.Fatalf("git clean must never have run: %v", err)
	}
}

// TestGateMutationTrackingBlocksInterpreter is the interpreter-mutation
// backstop: even if a destructive command slipped past classification, a gate
// that deletes a tracked file is caught after the fact and blocks the run.
func TestGateMutationTrackingBlocksInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	// A TRACKED file the gate will delete.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "add tracked")

	cfg := config.Default() // gateMutations defaults to block
	cfg.Workspace.RequireCleanTree = "allow"
	// `rm` directly would be classified; use a plain shell builtin redirect that
	// the classifier does not flag, to exercise the tracking backstop.
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "sh -c 'rm tracked.txt'"}}
	// Allow the wrapper so we reach execution and rely on the tracking backstop.
	cfg.Security.RequireConfirmationFor = []string{"git_push", "package_install"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "gate") {
		t.Fatalf("gate that deletes a tracked file must block via tracking, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The gate's mutation must be recorded for the audit trail.
	if _, err := os.Stat(filepath.Join(store.GateDir(state.RunID, "verify", 1), "mutations.json")); err != nil {
		t.Fatalf("gate mutations.json not persisted: %v", err)
	}
}

// TestGateRollsBackCleanTrackedFile covers rollback of a tracked-AND-CLEAN file
// (not in the dirty backup): the gate deletes it, the run blocks, and it must be
// restored from HEAD.
func TestGateRollsBackCleanTrackedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	// A tracked, committed, CLEAN file (not dirty when the gate runs).
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, "add committed")

	cfg := config.Default() // gateMutations defaults to block
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "sh -c 'rm committed.txt'"}}
	cfg.Security.RequireConfirmationFor = []string{"git_push", "package_install"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("gate deleting a tracked file must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The clean tracked file must be restored from HEAD, not left deleted.
	restored, err := os.ReadFile(filepath.Join(dir, "committed.txt"))
	if err != nil {
		t.Fatalf("clean tracked file must be rolled back from HEAD: %v", err)
	}
	if string(restored) != "committed work\n" {
		t.Fatalf("rolled-back content wrong: %q", restored)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, "gate_rolled_back") {
		t.Fatal("expected gate_rolled_back event")
	}
}

// TestGateDeletingUntrackedFileBlocks covers the reviewer's untracked-deletion
// gap: a gate that deletes an untracked file (real user work) must be detected
// by gate mutation tracking and block the run.
func TestGateDeletingUntrackedFileBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	// An UNTRACKED file representing real user work in the worktree.
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "sentinel.txt"), []byte("user work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default() // gateMutations defaults to block
	cfg.Workspace.RequireCleanTree = "allow"
	// `rm build/sentinel.txt` (no -rf) is not classified, so it runs and the
	// tracking backstop must catch the untracked deletion.
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "sh -c 'rm build/sentinel.txt'"}}
	cfg.Security.RequireConfirmationFor = []string{"git_push", "package_install"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "gate") {
		t.Fatalf("gate deleting an untracked file must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	if _, err := os.Stat(filepath.Join(store.GateDir(state.RunID, "verify", 1), "mutations.json")); err != nil {
		t.Fatalf("gate mutations.json not persisted: %v", err)
	}
	// The deleted user work must be ROLLED BACK — blocking is not enough.
	restored, err := os.ReadFile(filepath.Join(dir, "build", "sentinel.txt"))
	if err != nil {
		t.Fatalf("gate's deletion of user work must be rolled back: %v", err)
	}
	if string(restored) != "user work\n" {
		t.Fatalf("rolled-back content wrong: %q", restored)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, "gate_rolled_back") {
		t.Fatal("expected gate_rolled_back event")
	}
}

// TestPolicyBlocksWrappedGateCommandKeepsFiles is the reviewer's combined-flag
// bypass, end to end: a gate `sh -ec 'rm -rf build'` must block BEFORE running
// and leave the directory intact.
func TestPolicyBlocksWrappedGateCommandKeepsFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -ec")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	// A real directory the wrapped rm would delete if the policy missed it.
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "artifact"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "sh -ec 'rm -rf build'"}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("wrapped destructive gate must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "artifact")); err != nil {
		t.Fatalf("build/ must be intact — the rm must never have run: %v", err)
	}
}

// sleepWorker returns a shell command that sleeps ~5s on every platform.
func sleepWorker() string {
	if runtime.GOOS == "windows" {
		return "ping -n 6 127.0.0.1"
	}
	return "sleep 5"
}

// TestWallClockBudgetKillsLastWorker is the reviewer's exact repro: a tiny
// maxWallClock with a slow FINAL worker and no gates. The run must block, not
// complete — the deadline kills the worker mid-flight.
func TestWallClockBudgetKillsLastWorker(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Budgets.Run.MaxWallClock = config.Duration(1 * time.Second)
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "fake"}
	cfg.Agents["implementer"] = config.AgentConfig{Provider: "shell", Command: sleepWorker()}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	start := time.Now()
	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "wall-clock") {
		t.Fatalf("over-budget run must block with wall-clock reason, got %s (%s)", state.Status, state.BlockedReason)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("deadline should have killed the worker (~1s), took %s", elapsed)
	}
	// The killed worker's audit says canceled, never done.
	assertWorkerStatus(t, store, state.RunID, "implement-02", core.WorkerCanceled)
	// A blocked run must not point at an active worker (observable state truth).
	assertNoActiveWorker(t, store, state.RunID)
}

// assertNoActiveWorker checks both the returned state's invariant and the
// persisted state.json: a non-active run never advertises an active worker.
func assertNoActiveWorker(t *testing.T, store *rt.Store, runID string) {
	t.Helper()
	disk, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	if disk.ActiveWorker != "" {
		t.Fatalf("state.json has stale active_worker %q on a %s run", disk.ActiveWorker, disk.Status)
	}
	if disk.Status == core.StatusFailed && disk.NextAction != "" {
		t.Fatalf("failed run should have no next_action, got %q", disk.NextAction)
	}
}

func TestCostBudgetBlocksBeforeCompletion(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Budgets.Run.MaxCostUSD = 1.0

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{CostUSD: 5.0}), nil // each worker costs $5
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "cost") {
		t.Fatalf("over-cost run must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	if state.Status == core.StatusCompleted {
		t.Fatal("run completed despite exceeding cost budget")
	}
}

func TestTokenBudgetBlocksAndAccumulates(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Budgets.Run.MaxTotalTokens = 250 // explore (150) ok; +implement (150) = 300 > 250

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{TokensIn: 100, TokensOut: 50}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "token") {
		t.Fatalf("over-token run must block, got %s (%s)", state.Status, state.BlockedReason)
	}
	// Tokens must aggregate across workers (the central multi-agent accounting).
	if state.Budgets.TokensTotalSpent() < 250 {
		t.Fatalf("tokens should have accumulated across workers, got %d", state.Budgets.TokensTotalSpent())
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, "token_usage") {
		t.Fatal("token_usage event missing from timeline")
	}
}

func TestStageWallClockBudget(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Budgets.Stage = map[string]config.StageBudget{
		"implement": {MaxWallClock: config.Duration(1 * time.Second)},
	}
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "fake"}
	cfg.Agents["implementer"] = config.AgentConfig{Provider: "shell", Command: sleepWorker()}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "wall-clock") {
		t.Fatalf("stage wall-clock budget must block, got %s (%s)", state.Status, state.BlockedReason)
	}
}

func TestStageIterationBudget(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": failingGate()}
	cfg.Budgets.Stage = map[string]config.StageBudget{
		"verify": {MaxIterations: 1},
	}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	// First pass: verify runs (iteration 1) and blocks on the failing gate.
	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("expected gate block, got %s", state.Status)
	}

	// Resume re-enters verify → iteration 2 > budget 1 → blocked by iterations
	// BEFORE re-running the gate.
	resumed, err := e.Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != core.StatusBlocked || !strings.Contains(resumed.BlockedReason, "iteration") {
		t.Fatalf("second entry must exceed iteration budget, got %s (%s)", resumed.Status, resumed.BlockedReason)
	}
}

func assertWorkerStatus(t *testing.T, store *rt.Store, runID, workerID string, want core.WorkerState) {
	t.Helper()
	var ws core.WorkerStatus
	data, err := os.ReadFile(filepath.Join(store.WorkerDir(runID, workerID), "status.json"))
	if err != nil {
		t.Fatalf("reading worker status: %v", err)
	}
	if err := json.Unmarshal(data, &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Status != want {
		t.Fatalf("worker %s status = %q, want %q", workerID, ws.Status, want)
	}
}

// TestAdvanceStageTransitionIsConsistent: a stage transition is persisted in a
// single write, so the on-disk state never shows a stage that is BOTH done and
// current_stage — the inconsistency that would re-run a completed stage on a
// resume after a crash mid-transition.
func TestAdvanceStageTransitionIsConsistent(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)
	e := New(Options{Store: store, Registry: adapters.NewRegistry(), Config: config.Default(), Repo: repo})

	state := &core.State{
		RunID:        "run-x",
		Status:       core.StatusActive,
		Stages:       map[string]core.StageStatus{"implement": core.StageActive, "verify": core.StagePending},
		CurrentStage: "implement",
		Iterations:   map[string]int{},
	}
	if err := store.CreateRun(state); err != nil {
		t.Fatal(err)
	}

	if ok := e.advanceStage(state, workflows.Stage{Name: "implement", Kind: workflows.KindWorker, Next: "verify"}); !ok {
		t.Fatal("advanceStage should succeed")
	}

	disk, err := store.LoadState("run-x")
	if err != nil {
		t.Fatal(err)
	}
	if disk.Stages["implement"] != core.StageDone {
		t.Fatalf("implement should be done, got %s", disk.Stages["implement"])
	}
	if disk.CurrentStage != "verify" {
		t.Fatalf("current_stage should be verify, got %s", disk.CurrentStage)
	}
	if disk.Stages[disk.CurrentStage] == core.StageDone {
		t.Fatal("current_stage must never already be marked done (crash inconsistency)")
	}
}

func TestMutationPolicyVerdict(t *testing.T) {
	sec := config.Default().Security
	roStage := workflows.Stage{Name: "explore", ReadOnly: true}
	rwStage := workflows.Stage{Name: "implement"}

	if v := mutationPolicyVerdict(roStage, []core.Mutation{{Path: "a.txt"}}, sec); v == "" {
		t.Error("read-only stage with mutations must block")
	}
	if v := mutationPolicyVerdict(rwStage, []core.Mutation{{Path: "vichu.yaml", Sensitive: true}}, sec); v == "" {
		t.Error("sensitive mutation must block by default")
	}
	sec.SensitiveMutations = "warn"
	if v := mutationPolicyVerdict(rwStage, []core.Mutation{{Path: "vichu.yaml", Sensitive: true}}, sec); v != "" {
		t.Error("sensitive mutation with warn policy must not block")
	}
	if v := mutationPolicyVerdict(rwStage, []core.Mutation{{Path: "docs/x.md", OutOfScope: true}}, sec); v != "" {
		t.Error("out-of-scope defaults to warn — must not block")
	}
	sec.OutOfScopeMutations = "block"
	if v := mutationPolicyVerdict(rwStage, []core.Mutation{{Path: "docs/x.md", OutOfScope: true}}, sec); v == "" {
		t.Error("out-of-scope with block policy must block")
	}
}

func hasEvent(events []core.Event, name string) bool {
	for _, ev := range events {
		if ev.Event == name {
			return true
		}
	}
	return false
}

// mutationRecorded reports whether any worker's mutations.json includes path.
func mutationRecorded(store *rt.Store, runID, path string) bool {
	workers, _ := store.ListWorkers(runID)
	for _, w := range workers {
		r, err := store.LoadMutationReport(runID, w)
		if err != nil {
			continue
		}
		for _, m := range r.Mutations {
			if m.Path == path {
				return true
			}
		}
	}
	return false
}
