package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// hostFirstEngine wires an engine over a filesystem workspace (no git) with no
// agents — in host-first mode the HOST runs the subagent; the kernel only owns
// the verified state. A passing `test` gate lets the verify stage close.
func hostFirstEngine(t *testing.T) (*Engine, *rt.Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := rt.Open(dir)
	prov, err := workspace.OpenFilesystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	check := "test -f feature.go"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: check, Windows: check}}
	e := New(Options{Store: store, Registry: adapters.NewRegistry(), Config: cfg, Repo: prov})
	return e, store, dir
}

// TestHostFirstDriveToCompletion drives a full `quick` run from the host side —
// no engine loop, no agents — purely through the transactional commands:
// run start → (explore worker) → stage close → (implement worker writes) →
// stage close → (verify gate) → completed. The implement worker's change must be
// audited; the run must complete on the real gate.
func TestHostFirstDriveToCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, dir := hostFirstEngine(t)

	state, err := e.StartRun("add a feature", "quick", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	runID := state.RunID

	// explore (read-only) — the host's researcher touches nothing.
	mustWorker(t, e, runID, "explore", "explorer", "" /*no file*/, dir)
	mustStageClose(t, e, runID, "explore")

	// implement — the host's subagent writes the feature.
	mustWorker(t, e, runID, "implement", "implementer", "feature.go", dir)
	if !mutationRecorded(store, runID, "feature.go") {
		t.Fatal("implement worker's change must be audited in mutations.json")
	}
	mustStageClose(t, e, runID, "implement")

	// verify — the kernel runs the real `test -f feature.go` gate; it passes.
	if reason, err := e.StageClose(runID, "verify", ""); err != nil || reason != "" {
		t.Fatalf("verify stage close: reason=%q err=%v", reason, err)
	}

	final, _ := store.LoadState(runID)
	if final.Status != core.StatusCompleted {
		t.Fatalf("host-first drive should complete, got %s (%s)", final.Status, final.BlockedReason)
	}
}

// mustWorker runs one host-first worker: start, optionally write a file (the
// "agent"), then complete — asserting it does not block.
func mustWorker(t *testing.T, e *Engine, runID, stage, role, writeFile, dir string) {
	t.Helper()
	wid, blockReason, err := e.WorkerStart(runID, stage, role, "")
	if err != nil {
		t.Fatalf("WorkerStart(%s): %v", stage, err)
	}
	if blockReason != "" {
		t.Fatalf("WorkerStart(%s) unexpectedly blocked: %s", stage, blockReason)
	}
	if writeFile != "" {
		if err := os.WriteFile(filepath.Join(dir, writeFile), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if reason, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "done"}); err != nil || reason != "" {
		t.Fatalf("WorkerComplete(%s): reason=%q err=%v", stage, reason, err)
	}
}

func mustStageClose(t *testing.T, e *Engine, runID, stage string) {
	t.Helper()
	if reason, err := e.StageClose(runID, stage, ""); err != nil || reason != "" {
		t.Fatalf("StageClose(%s): reason=%q err=%v", stage, reason, err)
	}
}

// driveToReview drives a host-first `review` run up to the review stage:
// explore (read-only) → implement (writes feature.go) → at review.
func driveToReview(t *testing.T, e *Engine, dir string) (*rt.Store, string) {
	t.Helper()
	store := e.store
	state, err := e.StartRun("feat", "review", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", dir)
	mustStageClose(t, e, runID, "explore")
	mustWorker(t, e, runID, "implement", "implementer", "feature.go", dir)
	mustStageClose(t, e, runID, "implement") // → review
	if st, _ := store.LoadState(runID); st.CurrentStage != "review" {
		t.Fatalf("expected to be at review, got %q", st.CurrentStage)
	}
	return store, runID
}

// reviewVerdict runs the reviewer worker and records a verdict, returning the
// block reason (if any).
func reviewVerdict(t *testing.T, e *Engine, runID, verdict string) string {
	t.Helper()
	wid, blocked, err := e.WorkerStart(runID, "review", "reviewer", "")
	if err != nil || blocked != "" {
		t.Fatalf("review WorkerStart: blocked=%q err=%v", blocked, err)
	}
	reason, err := e.ReviewComplete(runID, wid, "", WorkerOutcome{Result: verdict})
	if err != nil {
		t.Fatalf("ReviewComplete: %v", err)
	}
	return reason
}

// TestHostFirstReviewApprovedCompletes: an `approved` verdict advances review →
// verify, and closing verify (real gate) completes the run — host-first.
func TestHostFirstReviewApprovedCompletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, _, dir := hostFirstEngine(t)
	store, runID := driveToReview(t, e, dir)

	if reason := reviewVerdict(t, e, runID, `{"status":"approved","summary":"lgtm"}`); reason != "" {
		t.Fatalf("approved verdict should not block: %s", reason)
	}
	if st, _ := store.LoadState(runID); st.CurrentStage != "verify" {
		t.Fatalf("approved review must advance to verify, got %q", st.CurrentStage)
	}
	mustStageClose(t, e, runID, "verify")
	if final, _ := store.LoadState(runID); final.Status != core.StatusCompleted {
		t.Fatalf("host-first review run should complete, got %s (%s)", final.Status, final.BlockedReason)
	}
}

// TestHostFirstReviewNeedsFixesLoops: a `needs_fixes` verdict loops review → fix.
func TestHostFirstReviewNeedsFixesLoops(t *testing.T) {
	e, _, dir := hostFirstEngine(t)
	store, runID := driveToReview(t, e, dir)

	reason := reviewVerdict(t, e, runID, `{"status":"needs_fixes","summary":"x","findings":[{"severity":"major","file":"feature.go","message":"add tests"}]}`)
	if reason != "" {
		t.Fatalf("needs_fixes within budget should not block: %s", reason)
	}
	if st, _ := store.LoadState(runID); st.CurrentStage != "fix" {
		t.Fatalf("needs_fixes must loop to fix, got %q", st.CurrentStage)
	}
}

// TestHostFirstReviewBlockedStops: a `blocked` verdict stops the run for a human.
func TestHostFirstReviewBlockedStops(t *testing.T) {
	e, _, dir := hostFirstEngine(t)
	store, runID := driveToReview(t, e, dir)

	reason := reviewVerdict(t, e, runID, `{"status":"blocked","summary":"task is unsafe"}`)
	if reason == "" {
		t.Fatal("a blocked verdict must block the run")
	}
	if final, _ := store.LoadState(runID); final.Status != core.StatusBlocked {
		t.Fatalf("blocked verdict must block the run, got %s", final.Status)
	}
}

// TestHostFirstReadOnlyWorkerBlocksButStillAudits: a read-only stage (explore)
// whose worker mutates must block the run — but the audit (mutations.json) MUST
// still be written. Auditing what the agent touched is non-negotiable.
func TestHostFirstReadOnlyWorkerBlocksButStillAudits(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, err := e.StartRun("explore the code", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "") // explore is read-only and is the current stage
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sneaky.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockReason, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "explored and wrote a file"})
	if err != nil {
		t.Fatal(err)
	}
	if blockReason == "" {
		t.Fatal("a read-only worker that mutated must block the run")
	}
	if !mutationRecorded(store, runID, "sneaky.txt") {
		t.Fatal("the audit must be written even when the run blocks")
	}
	if final, _ := store.LoadState(runID); final.Status != core.StatusBlocked {
		t.Fatalf("run should be blocked, got %s", final.Status)
	}
}

// TestHostFirstWorkerStartRespectsBudget: host-first must still cut runaway spend
// — once the agent-invocation budget is exhausted, `worker start` blocks the run
// instead of opening another worker.
func TestHostFirstWorkerStartRespectsBudget(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	e.cfg.Budgets.Run.MaxAgentInvocations = 1

	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, blockReason, err := e.WorkerStart(runID, "explore", "explorer", "")
	if err != nil || blockReason != "" {
		t.Fatalf("first worker start: block=%q err=%v", blockReason, err)
	}
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatal(err)
	}

	// Budget exhausted (1 of 1 used) → the next start blocks the run and returns a
	// reason with NO worker id (the host must not launch a subagent).
	gotID, gotReason, err := e.WorkerStart(runID, "explore", "explorer", "")
	if err != nil {
		t.Fatalf("over-budget worker start should block, not error: %v", err)
	}
	if gotID != "" || gotReason == "" {
		t.Fatalf("over-budget start must return empty id + a block reason, got id=%q reason=%q", gotID, gotReason)
	}
	if final, _ := store.LoadState(runID); final.Status != core.StatusBlocked {
		t.Fatalf("run should be blocked on the agent-invocation budget, got %s", final.Status)
	}
}

// TestHostFirstOpIDIsIdempotent: a retry with the same --op-id returns the same
// result WITHOUT re-applying — so a host that loses the response can safely retry.
// Without an op-id, the safety guards still reject a double-apply.
func TestHostFirstOpIDIsIdempotent(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// worker start: a retry with the same op-id returns the SAME worker id and
	// does NOT open a second worker or double-count the invocation.
	wid1, _, err := e.WorkerStart(runID, "explore", "explorer", "op-start-1")
	if err != nil {
		t.Fatal(err)
	}
	wid2, _, err := e.WorkerStart(runID, "explore", "explorer", "op-start-1")
	if err != nil {
		t.Fatalf("retried worker start must not error: %v", err)
	}
	if wid1 != wid2 {
		t.Fatalf("idempotent worker start must return the same id, got %q then %q", wid1, wid2)
	}
	if st, _ := store.LoadState(runID); st.Budgets.AgentInvocations != 1 {
		t.Fatalf("retry must not double-count invocations, got %d", st.Budgets.AgentInvocations)
	}

	// worker complete: a retry with the same op-id returns the cached result, NOT
	// the "already completed" error.
	if _, err := e.WorkerComplete(runID, wid1, "op-done-1", WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid1, "op-done-1", WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatalf("retried worker complete with the same op-id must succeed (cached): %v", err)
	}
	// But WITHOUT an op-id, the guard still rejects a double-complete — op-id is
	// what makes the retry safe.
	if _, err := e.WorkerComplete(runID, wid1, "", WorkerOutcome{Result: "ok"}); err == nil {
		t.Fatal("worker complete without an op-id must reject an already-done worker")
	}
}

// TestWorkerCompleteRecoversWhenOpRecordWriteFails: the durability edge — the
// worker close fully applies (status=done, mutations written) but writing the op
// record fails. A retry with the SAME op-id must RECOVER from the applied state
// and succeed, not error with "not the active worker".
func TestWorkerCompleteRecoversWhenOpRecordWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based denial is unreliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	e, store, dir := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = dir

	// Force the op-record write to fail: make runs/<id>/operations a read-only
	// FILE so MkdirAll/write under it fails, but the worker writes still succeed.
	opsPath := filepath.Join(store.RunDir(runID), "operations")
	if err := os.WriteFile(opsPath, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}

	_, cerr := e.WorkerComplete(runID, wid, "op-rec-fail", WorkerOutcome{Result: "done"})
	if cerr == nil {
		t.Fatal("worker complete should error when the op record cannot be written")
	}
	// The worker DID apply (status persisted, mutations) — only the op record failed.
	if ws, _ := store.LoadWorkerStatus(runID, wid); ws.Status != core.WorkerDone {
		t.Fatalf("worker should be applied (done), got %s", ws.Status)
	}

	// Recover: remove the blocker and retry with the SAME op-id — must succeed.
	if err := os.Remove(opsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "op-rec-fail", WorkerOutcome{Result: "done"}); err != nil {
		t.Fatalf("retry with the same op-id must recover, got: %v", err)
	}
	if st, _ := store.LoadState(runID); st.ActiveWorker != "" {
		t.Fatalf("after recovery the worker must be finished, got active=%q", st.ActiveWorker)
	}
}

// TestRunStartRetryRematerializesIncompleteRun: a reserved run start that wrote
// state.json but is missing an auditable input (here, config.snapshot.yaml) is
// INCOMPLETE — a retry with the same op-id must re-materialize it, not return a
// half-built run.
func TestRunStartRetryRematerializesIncompleteRun(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "op-incomplete")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	// Simulate a crash that left the run without its config snapshot.
	if err := os.Remove(store.ConfigSnapshotPath(runID)); err != nil {
		t.Fatal(err)
	}
	if store.ConfigSnapshotExists(runID) {
		t.Fatal("precondition: config snapshot should be gone")
	}

	// Retry with the same op-id → same run, but now fully materialized.
	st, err := e.StartRun("task", "quick", "op-incomplete")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st.RunID != runID {
		t.Fatalf("retry must map to the same run, got %s vs %s", st.RunID, runID)
	}
	if !store.ConfigSnapshotExists(runID) {
		t.Fatal("retry must re-materialize the missing config snapshot before returning success")
	}
}

// TestWorkerStartRecoversWhenOpRecordWriteFails: worker start opens the worker but
// writing its op record fails. A retry with the SAME op-id must RECOVER (return
// the same worker id) instead of erroring with "worker already active".
func TestWorkerStartRecoversWhenOpRecordWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based denial is unreliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Force the op-record write to fail: make runs/<id>/operations a read-only file.
	opsPath := filepath.Join(store.RunDir(runID), "operations")
	if err := os.WriteFile(opsPath, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	_, _, serr := e.WorkerStart(runID, "explore", "explorer", "ws-fail")
	if serr == nil {
		t.Fatal("worker start should error when the op record cannot be written")
	}
	// The worker WAS opened (active) — only the op record failed.
	if st, _ := store.LoadState(runID); st.ActiveWorker == "" {
		t.Fatal("the worker should have been opened (active)")
	}

	// Recover: remove the blocker and retry with the SAME op-id — same worker id.
	if err := os.Remove(opsPath); err != nil {
		t.Fatal(err)
	}
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "ws-fail")
	if err != nil {
		t.Fatalf("retry with the same op-id must recover: %v", err)
	}
	st, _ := store.LoadState(runID)
	if wid != st.ActiveWorker {
		t.Fatalf("recovery must return the active worker id, got %q vs active %q", wid, st.ActiveWorker)
	}
	// No second worker created — the invocation counter stays at 1.
	if st.Budgets.AgentInvocations != 1 {
		t.Fatalf("recovery must not open a second worker, invocations=%d", st.Budgets.AgentInvocations)
	}
}

// TestStartRunOpIDReservation: run start with the same op-id returns the SAME run
// (atomic reservation), and the workflow is normalized so "" and the default name
// ("quick") are the same operation, while a genuinely different workflow errors.
func TestStartRunOpIDReservation(t *testing.T) {
	e, store, _ := hostFirstEngine(t)

	s1, err := e.StartRun("task", "", "op1")
	if err != nil {
		t.Fatal(err)
	}
	// Same op-id, default workflow spelled out → same run (normalized fingerprint).
	s2, err := e.StartRun("task", "quick", "op1")
	if err != nil {
		t.Fatalf("normalized retry must not error: %v", err)
	}
	if s1.RunID != s2.RunID {
		t.Fatalf("same op-id must map to the same run, got %s then %s", s1.RunID, s2.RunID)
	}
	if runs, _ := store.ListRuns(); len(runs) != 1 {
		t.Fatalf("reservation must create exactly ONE run, got %d", len(runs))
	}
	// Same op-id, a genuinely different workflow → rejected.
	if _, err := e.StartRun("task", "review", "op1"); err == nil {
		t.Fatal("reusing an op-id for a different workflow must error")
	}
}

// TestHostFirstOpIDBoundToOperation: an op-id is bound to its command+args. Reusing
// it for a DIFFERENT operation must error, not return the wrong cached result.
func TestHostFirstOpIDBoundToOperation(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Record op "shared" as a worker start.
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "shared")
	if err != nil {
		t.Fatal(err)
	}

	// Reusing "shared" for worker COMPLETE (different kind) must be rejected — not
	// silently return the cached worker-start result (which would leave the worker
	// active while the CLI claims completion).
	if _, err := e.WorkerComplete(runID, wid, "shared", WorkerOutcome{Result: "ok"}); err == nil {
		t.Fatal("reusing an op-id across different operations must error")
	}

	// Reusing it for the same kind but DIFFERENT args (another stage/role) is also
	// a different operation → rejected.
	if _, _, err := e.WorkerStart(runID, "implement", "implementer", "shared"); err == nil {
		t.Fatal("reusing an op-id for the same kind but different args must error")
	}
}

// TestHostFirstStrictWriteFailsLoudly: if a must-succeed write fails mid-command,
// the command must return an ERROR and NOT record the op as completed — a host
// never gets "success" while the runtime is left corrupt. We force the failure by
// making the worker dir unwritable after `worker start`.
func TestHostFirstStrictWriteFailsLoudly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based denial is unreliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	e, store, dir := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "")
	if err != nil {
		t.Fatal(err)
	}
	// Make the worker dir read-only so writing result/status fails.
	wdir := store.WorkerDir(runID, wid)
	if err := os.Chmod(wdir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(wdir, 0o755) }()
	_ = dir

	_, cerr := e.WorkerComplete(runID, wid, "op-fail", WorkerOutcome{Result: "result that cannot be written"})
	if cerr == nil {
		t.Fatal("worker complete must FAIL when a critical write fails, not report success")
	}
	// The op must NOT be recorded as completed — so a retry (after the dir is
	// writable again) can re-apply rather than returning a phantom success.
	if _, ok := e.cachedOp(runID, "op-fail"); ok {
		t.Fatal("a failed operation must not be recorded as completed")
	}
	// The run must still be at the worker (ActiveWorker intact) — we failed fast
	// BEFORE finishing the worker, so the retry is recoverable.
	if st, _ := store.LoadState(runID); st.ActiveWorker != wid {
		t.Fatalf("a failed-fast complete must leave the worker active for retry, got %q", st.ActiveWorker)
	}

	// RECOVER: make the dir writable again and retry with the SAME op-id — it must
	// now succeed and finish the worker cleanly.
	if err := os.Chmod(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "op-fail", WorkerOutcome{Result: "result that cannot be written"}); err != nil {
		t.Fatalf("retry after the failure must recover and succeed: %v", err)
	}
	if st, _ := store.LoadState(runID); st.ActiveWorker != "" {
		t.Fatalf("the recovered complete must finish the worker, got active=%q", st.ActiveWorker)
	}
}

// TestWorkerStartEnforcesWorkflow: the kernel decides the stage and role — the
// host cannot skip to a different stage, use the wrong role, or open two workers.
func TestWorkerStartEnforcesWorkflow(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "") // current stage = explore
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	if _, _, err := e.WorkerStart(runID, "implement", "implementer", ""); err == nil {
		t.Fatal("must reject a stage that is not the current one")
	}
	if _, _, err := e.WorkerStart(runID, "explore", "implementer", ""); err == nil {
		t.Fatal("must reject a role that does not match the stage")
	}
	if _, _, err := e.WorkerStart(runID, "verify", "explorer", ""); err == nil {
		t.Fatal("must reject closing onto a gate stage as a worker")
	}
	// A valid start, then a second start must be refused (one active worker).
	if _, _, err := e.WorkerStart(runID, "explore", "explorer", ""); err != nil {
		t.Fatalf("valid worker start failed: %v", err)
	}
	if _, _, err := e.WorkerStart(runID, "explore", "explorer", ""); err == nil {
		t.Fatal("must reject a second worker while one is active")
	}
}

// TestWorkerUsageUnknownWhenHostDoesNotReport: a native host that does not expose
// usage closes its worker WITHOUT either reported flag — the run's tokens/cost stay
// honestly "unknown", not a fake zero. Cost and tokens are independent: a host can
// report tokens but not cost (codex), and that must keep cost unknown.
func TestWorkerUsageUnknownWhenHostDoesNotReport(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Close the explore worker with no usage — the host did not surface it.
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	st, _ := store.LoadState(runID)
	if st.Budgets.TokensReported || st.Budgets.CostReported {
		t.Fatal("a host that did not report usage must leave both flags false")
	}
	if st.Budgets.AgentInvocations != 1 {
		t.Fatalf("invocations are always counted, got %d", st.Budgets.AgentInvocations)
	}

	// Close the next worker reporting ONLY tokens (codex-style) — tokens accrue and
	// become known; cost stays unknown.
	wid2, _, err := e.WorkerStart(runID, "explore", "explorer", "") // still on explore
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid2, "", WorkerOutcome{
		Result: "ok", TokensReported: true, TokensIn: 100, TokensOut: 50,
		CostUSD: 0.5, // present but NOT reported — must be ignored
	}); err != nil {
		t.Fatal(err)
	}
	st, _ = store.LoadState(runID)
	if !st.Budgets.TokensReported {
		t.Fatal("a host that reported tokens must flip TokensReported true")
	}
	if st.Budgets.CostReported {
		t.Fatal("cost was not reported — CostReported must stay false")
	}
	if st.Budgets.TokensTotalSpent() != 150 {
		t.Fatalf("reported tokens must accrue, got %d", st.Budgets.TokensTotalSpent())
	}
	if st.Budgets.CostUSDSpent != 0 {
		t.Fatalf("unreported cost must not accrue, got %v", st.Budgets.CostUSDSpent)
	}
}
