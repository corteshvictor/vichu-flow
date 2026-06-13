package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// blockedRunEngine wires an engine whose EXPLORER (a read-only stage) writes a
// file, so the run blocks at `explore` with a completed worker that recorded a
// session — the exact precondition for resuming an agent session on re-entry.
// The shared Fake instance lets tests inspect how it was resumed.
func blockedRunEngine(t *testing.T, dir string) (*Engine, *adapters.Fake, *rt.Store) {
	t.Helper()
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	fake := adapters.NewFake(adapters.FakeScript{
		ResultText: "explored",
		Actions: map[string][]adapters.FakeAction{
			"explorer": {{Type: "write_file", Path: "sneaky.txt", Content: "x\n"}},
		},
	})
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return fake, nil })

	return New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo}), fake, store
}

// TestResumeContinuesAgentSession: a worker stage that completed and recorded a
// session, then blocked, must continue THAT agent session on resume — not start
// the agent cold.
func TestResumeContinuesAgentSession(t *testing.T) {
	dir := newTestRepo(t)
	e, fake, store := blockedRunEngine(t, dir)

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("read-only explore mutation should block, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The blocked worker completed and recorded a session id.
	ws, err := store.LoadWorkerStatus(state.RunID, "explore-01")
	if err != nil || ws.SessionID == "" {
		t.Fatalf("explore worker should have a recorded session: %+v (%v)", ws, err)
	}

	if _, err := e.Resume(context.Background(), state.RunID, ResumeOptions{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The engine must have continued the recorded agent session.
	resumed := fake.ResumedWith()
	if len(resumed) != 1 || resumed[0] != ws.SessionID {
		t.Fatalf("resume should continue session %q, got %v", ws.SessionID, resumed)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventWorkerResumed) {
		t.Fatal("expected a worker_resumed event")
	}
}

// TestResumeReconcilesInterruptedWorker: a worker left "running" by a crash must
// be reconciled to canceled on resume, with the active-worker pointer cleared —
// the audit trail must never claim a worker that is no longer alive.
func TestResumeReconcilesInterruptedWorker(t *testing.T) {
	dir := newTestRepo(t)
	e, _, store := blockedRunEngine(t, dir)

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate a crash mid-stage: flip the completed worker back to "running"
	// and point the run at it, as a killed process would have left things.
	ws, err := store.LoadWorkerStatus(state.RunID, "explore-01")
	if err != nil {
		t.Fatal(err)
	}
	ws.Status = core.WorkerRunning
	ws.FinishedAt = nil
	if err := store.SaveWorkerStatus(state.RunID, ws); err != nil {
		t.Fatal(err)
	}
	state.ActiveWorker = "explore-01"
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Resume(context.Background(), state.RunID, ResumeOptions{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The interrupted worker's audit must say canceled, not running.
	assertWorkerStatus(t, store, state.RunID, "explore-01", core.WorkerCanceled)
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventWorkerInterrupted) {
		t.Fatal("expected a worker_interrupted event")
	}
}

// TestResumeIgnoresNonCompletedWorkerSession: a worker left "running" with a
// session id (corrupt/crashed state) must NOT have its session resumed — only a
// completed worker recorded a usable session. The interrupted worker is
// reconciled and the stage restarts fresh.
func TestResumeIgnoresNonCompletedWorkerSession(t *testing.T) {
	dir := newTestRepo(t)
	e, fake, store := blockedRunEngine(t, dir)

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Corrupt the completed worker into "running" while keeping its session id.
	ws, err := store.LoadWorkerStatus(state.RunID, "explore-01")
	if err != nil {
		t.Fatal(err)
	}
	ws.Status = core.WorkerRunning
	ws.FinishedAt = nil
	if ws.SessionID == "" {
		t.Fatal("precondition: worker should carry a session id")
	}
	if err := store.SaveWorkerStatus(state.RunID, ws); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Resume(context.Background(), state.RunID, ResumeOptions{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The non-completed worker's session must not have been resumed.
	if got := fake.ResumedWith(); len(got) != 0 {
		t.Fatalf("must not resume a non-completed worker's session, got %v", got)
	}
	events, _ := store.ReadEvents(state.RunID)
	if hasEvent(events, core.EventWorkerResumed) {
		t.Fatal("no worker_resumed event should be emitted for a non-completed worker")
	}
}

// TestResumeFallsBackToStartWhenAdapterCannotResume: a stage whose adapter does
// not support resume (shell) must start fresh — and run to completion — rather
// than fail when the run is resumed.
func TestResumeFallsBackToStartWhenAdapterCannotResume(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, _ := workspace.Detect(dir)

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": failingGate()} // block after implement
	// A shell explorer (resume unsupported) that records a session id so a seed
	// exists for the stage; the engine must fall back to Start cleanly.
	cfg.Agents["explorer"] = config.AgentConfig{Provider: "shell", Command: "echo hi"}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(adapters.FakeScript{}), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("expected blocked at gate, got %s (%s)", state.Status, state.BlockedReason)
	}

	// Make the gate pass, then resume: it must progress past the shell stage
	// (fresh start, since shell can't resume) and complete.
	check := "true"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: check, Windows: "cmd /c exit 0"}}
	resumed, err := e.Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != core.StatusCompleted {
		t.Fatalf("resume should complete after the gate passes, got %s (%s)", resumed.Status, resumed.BlockedReason)
	}
}

// TestResumeLiveLockGivesActionableError: resuming a run a live process already
// holds must fail with guidance, not a raw lock error.
func TestResumeLiveLockGivesActionableError(t *testing.T) {
	dir := newTestRepo(t)
	e, _, store := blockedRunEngine(t, dir)

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hold the lock as a live owner (this process), then try to resume.
	handle, err := store.AcquireLock(state.RunID)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = handle.Release() }()

	_, err = e.Resume(context.Background(), state.RunID, ResumeOptions{})
	if err == nil {
		t.Fatal("resume over a live lock must error")
	}
	if !strings.Contains(err.Error(), "already being executed") || !strings.Contains(err.Error(), "vichu cancel") {
		t.Fatalf("error should be actionable, got %q", err.Error())
	}
}

// TestResumeReclaimsOrphanedLock: a lock left by a dead process (stale heartbeat)
// must not block resume — the engine reclaims it and runs.
func TestResumeReclaimsOrphanedLock(t *testing.T) {
	dir := newTestRepo(t)
	e, _, store := blockedRunEngine(t, dir)

	state, err := e.Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write an orphaned lock: a dead pid with an expired heartbeat (empty
	// hostname so ownerAlive falls through to the dead-pid check on every OS).
	stale := core.Lock{
		PID:         999999,
		RunID:       state.RunID,
		AcquiredAt:  time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now().Add(-time.Hour),
	}
	data, err := json.Marshal(&stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "lock.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Resume(context.Background(), state.RunID, ResumeOptions{}); err != nil {
		t.Fatalf("resume must reclaim an orphaned lock, got %v", err)
	}
}
