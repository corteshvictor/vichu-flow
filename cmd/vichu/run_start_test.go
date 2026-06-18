package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
)

// TestRunStartCreatesButDoesNotExecute: `vichu run start` is the host-first
// lifecycle entry — it materializes a run and stops, leaving it for the host (or
// the transactional commands) to drive. No stage runs, no worker executes.
func TestRunStartCreatesButDoesNotExecute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	if err := cmdRunStart([]string{"do a thing"}); err != nil {
		t.Fatalf("cmdRunStart: %v", err)
	}

	store := runtime.Open(dir)
	runID, err := store.LatestRun()
	if err != nil || runID == "" {
		t.Fatalf("run not created: %v", err)
	}
	state, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusActive {
		t.Fatalf("run start should leave the run active, got %s (%s)", state.Status, state.BlockedReason)
	}
	if state.CurrentStage != "explore" {
		t.Fatalf("quick workflow starts at explore, got %q", state.CurrentStage)
	}
	if state.Task != "do a thing" {
		t.Fatalf("task not recorded, got %q", state.Task)
	}
	// The defining property: nothing was EXECUTED — no worker ran.
	if workers, _ := store.ListWorkers(runID); len(workers) != 0 {
		t.Fatalf("run start must not execute any worker, got %v", workers)
	}

	// A second `run start` makes a distinct run (no lock held from the first).
	if err := cmdRunStart([]string{"another"}); err != nil {
		t.Fatalf("second run start: %v", err)
	}
	if runs, _ := store.ListRuns(); len(runs) != 2 {
		t.Fatalf("want 2 distinct runs, got %d", len(runs))
	}
}

// TestRunStartNeedsTask: a run start with no task is a usage error.
func TestRunStartNeedsTask(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdRunStart(nil); err == nil {
		t.Fatal("run start with no task must error")
	}
}

// TestRunStartOpIDIsIdempotent: the host pack's first command is
// `run start --op-id <uuid>`. A retry with the same op-id must map to the SAME
// run (not create a duplicate); a different task with the same op-id must error.
func TestRunStartOpIDIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	store := runtime.Open(dir)

	if err := cmdRunStart([]string{"--op-id", "abc", "--task", "build it"}); err != nil {
		t.Fatalf("run start: %v", err)
	}
	if err := cmdRunStart([]string{"--op-id", "abc", "--task", "build it"}); err != nil {
		t.Fatalf("retry with same op-id must succeed: %v", err)
	}
	if runs, _ := store.ListRuns(); len(runs) != 1 {
		t.Fatalf("idempotent run start must create ONE run, got %d", len(runs))
	}
	// Same op-id, different task → rejected.
	if err := cmdRunStart([]string{"--op-id", "abc", "--task", "something else"}); err == nil {
		t.Fatal("reusing an op-id for a different task must error")
	}
	// The literal command the host pack's skill runs must parse.
	if err := cmdRunStart([]string{"--workflow", "quick", "--op-id", "xyz", "--json", "skill task"}); err != nil {
		t.Fatalf("the host pack's literal run-start command must work: %v", err)
	}
}

// TestRunResumeDoesNotExecute: host-first `run resume` must reopen/validate the
// run and report state WITHOUT executing any stage — the host drives execution.
// (A run created by `run start` has 0 workers; resume must keep it at 0.)
func TestRunResumeDoesNotExecute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdRunStart([]string{"a task"}); err != nil {
		t.Fatal(err)
	}
	store := runtime.Open(dir)
	runID, _ := store.LatestRun()

	if err := cmdRunResume([]string{"--run", runID}); err != nil {
		t.Fatalf("run resume: %v", err)
	}
	// The defining property: resume did NOT run the loop — no worker executed.
	if workers, _ := store.ListWorkers(runID); len(workers) != 0 {
		t.Fatalf("host-first run resume must not execute workers, got %v", workers)
	}
	if st, _ := store.LoadState(runID); st.Status != core.StatusActive {
		t.Fatalf("resumed run should be active, got %s", st.Status)
	}

	// The deprecated `vichu resume` alias must ALSO be reopen-only (not headless).
	if err := cmdResume([]string{runID}); err != nil {
		t.Fatalf("resume alias: %v", err)
	}
	if workers, _ := store.ListWorkers(runID); len(workers) != 0 {
		t.Fatalf("`vichu resume` alias must not execute workers either, got %v", workers)
	}
}

// TestObserveIsReadOnly: `vichu observe` must not modify the runtime — it is a
// read-only view safe to run against a live run.
func TestObserveIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdRunStart([]string{"a task"}); err != nil {
		t.Fatal(err)
	}
	store := runtime.Open(dir)
	runID, _ := store.LatestRun()
	statePath := filepath.Join(store.RunDir(runID), "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdObserve([]string{runID}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	after, _ := os.ReadFile(statePath)
	if string(before) != string(after) {
		t.Fatal("observe must not modify state.json")
	}
}
