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

// TestHeadlessNeverReportsCompletedWithoutPersisting: a headless run whose terminal-state
// write fails must return an ERROR and leave the on-disk state non-completed — never print
// "completed" over a state.json it could not write. In host-first this was already fatal
// (strict scope); headless degraded criticalWrite to a warning and lied about the outcome.
func TestHeadlessNeverReportsCompletedWithoutPersisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses chmod to make the run dir unwritable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	dir := newTestRepo(t)
	e := testEngine(t, dir)
	// A gate that passes but makes the run directory read-only, so the subsequent
	// completion write fails — exactly the reviewer's "removed write perms mid-run".
	e.cfg.Commands = map[string]config.OSCommand{
		"test": {Unix: "sh -c 'chmod -R 0500 .vichu/runs/$(ls -t .vichu/runs | head -1); exit 0'"},
	}

	state, err := e.Start(context.Background(), "task", "quick")
	if err == nil {
		t.Fatal("a run whose terminal write failed must return an error, not nil")
	}
	if state != nil && state.Status == core.StatusCompleted {
		// The in-memory state may read completed, but the CLI keys off the returned error;
		// what must never happen is the caller seeing (completed, nil).
		if err == nil {
			t.Fatal("returned completed with no error")
		}
	}
	// Make the tree writable again (the gate chmod'd it recursively) so the DURABLE state can
	// be read and t.TempDir cleanup can remove it.
	runDir := rt.Open(dir).RunDir(firstRunID(t, dir))
	_ = filepath.WalkDir(runDir, func(p string, _ os.DirEntry, _ error) error {
		_ = os.Chmod(p, 0o700)
		return nil
	})
	persisted, lerr := rt.Open(dir).LoadState(firstRunID(t, dir))
	if lerr != nil {
		t.Fatalf("load persisted state: %v", lerr)
	}
	if persisted.Status == core.StatusCompleted {
		t.Fatal("state.json says completed, but the completion write had failed — the kernel lied")
	}
}

// firstRunID returns the id of the only run in the store (test helper).
func firstRunID(t *testing.T, dir string) string {
	t.Helper()
	ids, err := rt.Open(dir).ListRuns()
	if err != nil || len(ids) == 0 {
		t.Fatalf("no runs found: %v", err)
	}
	return ids[0]
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

// TestReadOnlyStageIgnoresHostLocalState: the coding HOST rewrites its own
// machine-local state mid-run — Claude Code persists an approved permission to
// .claude/settings.local.json the moment the user says "yes" to a command. That is the
// host's bookkeeping, not the agent's work on the code, so it must not BLOCK a
// read-only stage; otherwise every explore/propose/plan fails the instant a user
// approves anything, for a file the agent never touched.
//
// This is the GIT provider, where the audit sees what git sees: many users (and Claude
// Code's own setup guidance) gitignore settings.local.json, and a gitignored file is
// outside the audit — the same as any .env. On the FILESYSTEM provider we do record it,
// flagged host_bookkeeping: see TestHostLocalStateIsRecordedNotHidden.
func TestReadOnlyStageIgnoresHostLocalState(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	check := "test -f src/feature.txt"
	if runtime.GOOS == "windows" {
		check = "cmd /c if exist src\\feature.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: check, Windows: check}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				// The host writes its permission allowlist during the READ-ONLY stage.
				"explorer": {{Type: "write_file", Path: ".claude/settings.local.json",
					Content: "{\"permissions\":{\"allow\":[\"Bash(go version *)\"]}}\n"}},
				"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("host-local state written during a read-only stage must not block the run, got %s (%s)",
			state.Status, state.BlockedReason)
	}
	// Whether git surfaces it depends on the user's ignore rules; what must ALWAYS hold
	// is that if it is recorded, it is recorded as the HOST's bookkeeping — never
	// attributed to the worker.
	if m, ok := findMutation(store, state.RunID, ".claude/settings.local.json"); ok && !m.HostBookkeeping {
		t.Fatal("the host's own settings.local.json must never be attributed to the worker")
	}
}

// findMutation returns the recorded mutation for a path across the run's workers.
func findMutation(store *rt.Store, runID, path string) (core.Mutation, bool) {
	workers, _ := store.ListWorkers(runID)
	for _, w := range workers {
		r, err := store.LoadMutationReport(runID, w)
		if err != nil {
			continue
		}
		for _, m := range r.Mutations {
			if m.Path == path {
				return m, true
			}
		}
	}
	return core.Mutation{}, false
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

// TestPolicyBlockedCommandConsumesNoBudget: the invocation counter must not be spent by a worker
// that never reached the adapter. A shell command the policy blocks (git push, default config)
// blocks the run BEFORE dispatch; if it had already bumped agent_invocations, a `resume` after
// fixing the config would find the budget gone though no provider was ever called.
func TestPolicyBlockedCommandConsumesNoBudget(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "shell", Command: "git push origin main"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("a policy-blocked command must block the run, got %s (%s)", state.Status, state.BlockedReason)
	}
	// Neither the returned state nor the PERSISTED one (what resume reads) may show a consumed slot.
	if state.Budgets.AgentInvocations != 0 {
		t.Fatalf("policy-blocked worker consumed a budget slot in memory: agent_invocations=%d", state.Budgets.AgentInvocations)
	}
	persisted, err := store.LoadState(state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Budgets.AgentInvocations != 0 {
		t.Fatalf("policy-blocked worker consumed a budget slot on disk: agent_invocations=%d", persisted.Budgets.AgentInvocations)
	}
	if workers, _ := store.ListWorkers(state.RunID); len(workers) != 0 {
		t.Fatalf("a policy-blocked worker must not create a worker record, got %v", workers)
	}
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

// TestReadOnlyStageBlocksOnAnIgnoredFile: being gitignored is NOT a license to mutate.
// `Derived` was briefly treated as "the project declared this disposable", which exempted
// it from the read-only check — but an ignored path can just as easily be a private note, a
// credential or a certificate, and a global gitignore can make it one the project never
// mentioned. A read-only worker that touched anything blocks. The only exemption is the
// host's own bookkeeping, which the agent did not write.
func TestReadOnlyStageBlocksOnAnIgnoredFile(t *testing.T) {
	sec := config.Default().Security
	roStage := workflows.Stage{Name: "explore", ReadOnly: true}

	ignored := core.Mutation{Path: "private.notes", Kind: core.MutationModified, Derived: true}
	if v := mutationPolicyVerdict(roStage, []core.Mutation{ignored}, sec); v == "" {
		t.Error("a read-only worker that overwrote an ignored file must block — ignored is not disposable")
	}
	hostState := core.Mutation{Path: ".claude/settings.local.json", HostBookkeeping: true}
	if v := mutationPolicyVerdict(roStage, []core.Mutation{hostState}, sec); v != "" {
		t.Errorf("the host's own permission file is the one exemption, got %q", v)
	}
}

// TestGateOutputAllowed: a gate may only rewrite a pre-existing file the project has
// EXPLICITLY declared as a gate output. Inferring it from "the file is gitignored" is what
// let a gate overwrite a private note and still reach `completed`.
func TestGateOutputAllowed(t *testing.T) {
	coverage := core.Mutation{Path: "coverage.out", Kind: core.MutationModified, Derived: true}
	notes := core.Mutation{Path: "private.notes", Kind: core.MutationModified, Derived: true}
	env := core.Mutation{Path: ".env", Kind: core.MutationModified, Derived: true, Sensitive: true}

	if gateOutputAllowed(coverage, nil) {
		t.Error("nothing is disposable until the project says so — an empty allowlist allows nothing")
	}
	if !gateOutputAllowed(coverage, []string{"coverage.out"}) {
		t.Error("a declared gate output must be allowed")
	}
	if gateOutputAllowed(notes, []string{"coverage.out"}) {
		t.Error("an ignored file that is NOT declared must still block")
	}
	if gateOutputAllowed(env, []string{".env", "*"}) {
		t.Error("a sensitive path can never be allowlisted, however hard you try")
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

// TestTransitionEmitsNoEventWhenStateDidNotPersist is the deterministic regression for the
// "phantom transition" bug: advanceStage saved state and then UNCONDITIONALLY emitted
// stage_completed + stage_transition, so a failed state write left the public audit trail
// claiming a transition the authoritative state.json never recorded. Persist → check → emit.
func TestTransitionEmitsNoEventWhenStateDidNotPersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses chmod to make the run dir unwritable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	dir := newTestRepo(t)
	e := testEngine(t, dir)
	store := rt.Open(dir)

	state := &core.State{
		RunID: "run-1", Status: core.StatusActive, CurrentStage: "explore",
		Stages: map[string]core.StageStatus{"explore": core.StageActive, "implement": core.StagePending},
	}
	if err := store.CreateRun(state); err != nil {
		t.Fatal(err)
	}
	// The transition write must land under a strict scope for the guard to see the failure.
	e.strict = &strictScope{}

	// Make the run directory unwritable, so the transition's SaveState fails.
	runDir := store.RunDir("run-1")
	if err := os.Chmod(runDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runDir, 0o700) })

	stage := workflows.Stage{Name: "explore", Kind: workflows.KindWorker, Next: "implement"}
	if e.advanceStage(state, stage) {
		t.Fatal("advanceStage must report failure when the transition state did not persist")
	}

	// The audit trail must NOT claim the transition happened.
	_ = os.Chmod(runDir, 0o700)
	events, _ := store.ReadEvents("run-1")
	if hasEvent(events, core.EventStageTransition) {
		t.Fatal("a stage_transition event was written for a transition that never persisted — the kernel lied")
	}
	if hasEvent(events, core.EventStageCompleted) {
		t.Fatal("a stage_completed event was written for a stage whose transition never persisted")
	}
	// And the persisted state still says explore.
	persisted, err := store.LoadState("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CurrentStage != "explore" {
		t.Fatalf("persisted stage moved to %q despite the write failing", persisted.CurrentStage)
	}
}

// TestGateDoesNotRunWhenStageStartEventFails: a gate command can have real, non-idempotent
// effects, so it must not execute if the event announcing it could not be persisted — a retry
// would re-run the whole operation. Reproduces the reviewer's events.ndjson-symlink case.
func TestGateDoesNotRunWhenStageStartEventFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a symlink to make the event write fail")
	}
	dir := newTestRepo(t)
	e := testEngine(t, dir)
	e.strict = &strictScope{}
	store := rt.Open(dir)

	// A gate that leaves an observable marker if it runs.
	marker := filepath.Join(dir, "gate-ran.txt")
	e.cfg.Commands = map[string]config.OSCommand{
		"test": {Unix: "sh -c 'echo ran > gate-ran.txt'", Windows: "cmd /c echo ran> gate-ran.txt"},
	}

	state := &core.State{
		RunID: "run-1", Status: core.StatusActive, CurrentStage: "verify",
		Stages: map[string]core.StageStatus{"verify": core.StageActive},
	}
	if err := store.CreateRun(state); err != nil {
		t.Fatal(err)
	}
	// Plant events.ndjson as a symlink escaping the runtime root, so the append is refused.
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIG\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.RunDir("run-1"), "events.ndjson")); err != nil {
		t.Fatal(err)
	}

	stage := workflows.Stage{Name: "verify", Kind: workflows.KindGate, Gates: []string{"test"}}
	advance, err := e.runGateStage(context.Background(), state, nil, stage)

	if advance {
		t.Fatal("the stage must not advance when the gate could not be safely started")
	}
	if err == nil {
		t.Fatal("a failed gate-start event must surface as an error")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the gate command RAN even though its start event could not be persisted")
	}
}

// TestWorkerAdapterNotInvokedWhenStartEventFails: the adapter must not launch if the
// worker_started event could not be persisted — the guard that used to be checked only on the
// state save is now also checked on the EVENT, so a dispatched agent always has a durable
// record it began. The fake implementer writes src/feature.txt if it runs; its absence proves
// the adapter never ran.
func TestWorkerAdapterNotInvokedWhenStartEventFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a symlink to make the event write fail")
	}
	dir := newTestRepo(t)
	e := testEngine(t, dir)
	e.strict = &strictScope{}
	store := rt.Open(dir)

	state := &core.State{
		RunID: "run-1", Status: core.StatusActive, CurrentStage: "implement",
		Stages: map[string]core.StageStatus{"implement": core.StageActive}, Iterations: map[string]int{},
	}
	if err := store.CreateRun(state); err != nil {
		t.Fatal(err)
	}
	// events.ndjson → a symlink escaping the runtime root, so worker_started cannot persist.
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIG\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.RunDir("run-1"), "events.ndjson")); err != nil {
		t.Fatal(err)
	}

	rs := &runState{wf: nil, pack: "", startedAt: time.Now()}
	stage := workflows.Stage{Name: "implement", Kind: workflows.KindWorker, Role: "implementer", Next: "verify"}
	advance, err := e.runWorkerStage(context.Background(), state, rs, stage)

	if advance {
		t.Fatal("the stage must not advance when the worker could not be safely started")
	}
	if err == nil {
		t.Fatal("a failed worker_started event must surface as an error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "src", "feature.txt")); statErr == nil {
		t.Fatal("the adapter RAN (wrote its file) even though worker_started could not be persisted")
	}
}

// TestHeadlessBlocksOnNegativeUsage: a headless adapter reporting negative tokens must block
// the run — accruing them unchecked used to drive the run UNDER its own budget and still
// reach `completed`. Host-first validated; headless did not.
func TestHeadlessBlocksOnNegativeUsage(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = nil
	noGates := false
	cfg.Workflow.RequireGates = &noGates

	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{TokensIn: -100, TokensOut: -100}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("negative usage must block the run, got %s", state.Status)
	}
	if state.Budgets.TokensInSpent < 0 {
		t.Fatalf("negative tokens were accrued into the budget: %d", state.Budgets.TokensInSpent)
	}
	// The worker must be left with a TERMINAL status (not running), or resume mis-classifies it
	// as interrupted and cancels a worker that actually finished.
	ws, werr := store.LoadWorkerStatus(state.RunID, "explore-01")
	if werr != nil {
		t.Fatalf("load worker status: %v", werr)
	}
	if ws.Status == core.WorkerRunning || ws.FinishedAt == nil {
		t.Fatalf("worker left non-terminal after an invalid-usage block: status=%s finished_at=%v", ws.Status, ws.FinishedAt)
	}
}
