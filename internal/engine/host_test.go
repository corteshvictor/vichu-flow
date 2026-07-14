package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

	state, tok, err := e.StartRun("add a feature", "quick", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	runID := state.RunID

	// explore (read-only) — the host's researcher touches nothing.
	mustWorker(t, e, hostRun{runID, tok}, "explore", "explorer", "" /*no file*/, dir)
	mustStageClose(t, e, hostRun{runID, tok}, "explore")

	// implement — the host's subagent writes the feature.
	mustWorker(t, e, hostRun{runID, tok}, "implement", "implementer", "feature.go", dir)
	if !mutationRecorded(store, runID, "feature.go") {
		t.Fatal("implement worker's change must be audited in mutations.json")
	}
	mustStageClose(t, e, hostRun{runID, tok}, "implement")

	// verify — the kernel runs the real `test -f feature.go` gate; it passes.
	if reason, err := e.StageClose(runID, "verify", "", tok); err != nil || reason != "" {
		t.Fatalf("verify stage close: reason=%q err=%v", reason, err)
	}

	final, _ := store.LoadState(runID)
	if final.Status != core.StatusCompleted {
		t.Fatalf("host-first drive should complete, got %s (%s)", final.Status, final.BlockedReason)
	}
}

// hostRun is what a host needs to drive a run: its id, and the driver token that authorizes
// changing it. They always travel together — the token is meaningless without the run, and
// the run cannot be driven without the token — so the helpers take them as one thing.
type hostRun struct {
	id  string
	tok string
}

// mustWorker runs one host-first worker: start, optionally write a file (the
// "agent"), then complete — asserting it does not block.
func mustWorker(t *testing.T, e *Engine, run hostRun, stage, role, writeFile, dir string) {
	t.Helper()
	runID, tok := run.id, run.tok
	wid, blockReason, err := e.WorkerStart(runID, stage, role, "", tok)
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
	if reason, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "done"}); err != nil || reason != "" {
		t.Fatalf("WorkerComplete(%s): reason=%q err=%v", stage, reason, err)
	}
}

func mustStageClose(t *testing.T, e *Engine, run hostRun, stage string) {
	t.Helper()
	if reason, err := e.StageClose(run.id, stage, "", run.tok); err != nil || reason != "" {
		t.Fatalf("StageClose(%s): reason=%q err=%v", stage, reason, err)
	}
}

// driveToReview drives a host-first `review` run up to the review stage:
// explore (read-only) → implement (writes feature.go) → at review.
func driveToReview(t *testing.T, e *Engine, dir string) (*rt.Store, string, string) {
	t.Helper()
	store := e.store
	state, tok, err := e.StartRun("feat", "review", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, hostRun{runID, tok}, "explore", "explorer", "", dir)
	mustStageClose(t, e, hostRun{runID, tok}, "explore")
	mustWorker(t, e, hostRun{runID, tok}, "implement", "implementer", "feature.go", dir)
	mustStageClose(t, e, hostRun{runID, tok}, "implement") // → review
	if st, _ := store.LoadState(runID); st.CurrentStage != "review" {
		t.Fatalf("expected to be at review, got %q", st.CurrentStage)
	}
	return store, runID, tok
}

// reviewVerdict runs the reviewer worker and records a verdict, returning the
// block reason (if any).
func reviewVerdict(t *testing.T, e *Engine, run hostRun, verdict string) string {
	t.Helper()
	wid, blocked, err := e.WorkerStart(run.id, "review", "reviewer", "", run.tok)
	if err != nil || blocked != "" {
		t.Fatalf("review WorkerStart: blocked=%q err=%v", blocked, err)
	}
	reason, err := e.ReviewComplete(run.id, wid, "", run.tok, WorkerOutcome{Result: verdict})
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
	store, runID, tok := driveToReview(t, e, dir)

	if reason := reviewVerdict(t, e, hostRun{runID, tok}, `{"status":"approved","summary":"lgtm"}`); reason != "" {
		t.Fatalf("approved verdict should not block: %s", reason)
	}
	if st, _ := store.LoadState(runID); st.CurrentStage != "verify" {
		t.Fatalf("approved review must advance to verify, got %q", st.CurrentStage)
	}
	mustStageClose(t, e, hostRun{runID, tok}, "verify")
	if final, _ := store.LoadState(runID); final.Status != core.StatusCompleted {
		t.Fatalf("host-first review run should complete, got %s (%s)", final.Status, final.BlockedReason)
	}
}

// TestHostFirstReviewNeedsFixesLoops: a `needs_fixes` verdict loops review → fix.
func TestHostFirstReviewNeedsFixesLoops(t *testing.T) {
	e, _, dir := hostFirstEngine(t)
	store, runID, tok := driveToReview(t, e, dir)

	reason := reviewVerdict(t, e, hostRun{runID, tok}, `{"status":"needs_fixes","summary":"x","findings":[{"severity":"major","file":"feature.go","message":"add tests"}]}`)
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
	store, runID, tok := driveToReview(t, e, dir)

	reason := reviewVerdict(t, e, hostRun{runID, tok}, `{"status":"blocked","summary":"task is unsafe"}`)
	if reason == "" {
		t.Fatal("a blocked verdict must block the run")
	}
	if final, _ := store.LoadState(runID); final.Status != core.StatusBlocked {
		t.Fatalf("blocked verdict must block the run, got %s", final.Status)
	}
}

// TestHostFirstMalformedVerdictIsRejectedAndReviewerStaysOpen: in host-first the
// VERDICT ENVELOPE IS BUILT BY THE HOST, so a malformed one is a protocol error,
// not review evidence: `review complete` must fail, change nothing on disk, and
// leave the reviewer OPEN so the host can retry with a well-formed verdict. (The
// headless runner is the opposite case — there the AGENT produced the garbage, so
// it is evidence and it blocks: see TestReviewInvalidVerdictBlocks.)
//
// Regression: the kernel used to close the reviewer FIRST and block after, which
// stranded a terminal reviewer behind a blocked run — the corrected verdict then
// had no worker to attach to.
func TestHostFirstMalformedVerdictIsRejectedAndReviewerStaysOpen(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	_, runID, tok := driveToReview(t, e, dir)

	wid, _, err := e.WorkerStart(runID, "review", "reviewer", "", tok)
	if err != nil {
		t.Fatalf("review WorkerStart: %v", err)
	}
	// The shape the orchestrator actually got wrong in the field: `verdict` instead
	// of the required `status` key.
	if _, err := e.ReviewComplete(runID, wid, "op-bad", tok, WorkerOutcome{
		Result: `{"verdict":"approved","summary":"lgtm"}`,
	}); err == nil {
		t.Fatal("a malformed verdict envelope must be REJECTED, not recorded")
	}

	st, _ := store.LoadState(runID)
	if st.Status != core.StatusActive {
		t.Fatalf("rejecting the envelope must not block the run, got %s (%s)", st.Status, st.BlockedReason)
	}
	if st.ActiveWorker != wid {
		t.Fatalf("the reviewer must stay OPEN for the retry, active worker is %q", st.ActiveWorker)
	}

	// The host fixes the JSON and retries the SAME reviewer — this must now work.
	if reason, err := e.ReviewComplete(runID, wid, "op-good", tok, WorkerOutcome{
		Result: `{"status":"approved","summary":"lgtm"}`,
	}); err != nil || reason != "" {
		t.Fatalf("corrected verdict on the same reviewer: reason=%q err=%v", reason, err)
	}
	if st, _ := store.LoadState(runID); st.CurrentStage != "verify" {
		t.Fatalf("the corrected verdict must branch the run to verify, got %q", st.CurrentStage)
	}
}

// TestCompleteOnClosedWorkerFailsLoudly: a NEW op-id against an already-closed
// worker is not a lost response — it is a new operation on a terminal worker. It
// must fail. Silently answering "recorded" while discarding the host's evidence is
// the worst failure this kernel can have: it is a false success claim.
func TestCompleteOnClosedWorkerFailsLoudly(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("explore the code", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "op-close", tok, WorkerOutcome{Result: "done"}); err != nil {
		t.Fatalf("WorkerComplete: %v", err)
	}
	// Same op-id → the retry of a lost response, recovered idempotently.
	if _, err := e.WorkerComplete(runID, wid, "op-close", tok, WorkerOutcome{Result: "done"}); err != nil {
		t.Fatalf("retrying the SAME op-id must recover, not fail: %v", err)
	}
	// Different op-id → a new operation on a terminal worker. Must fail.
	if _, err := e.WorkerComplete(runID, wid, "op-other", tok, WorkerOutcome{Result: "different result"}); err == nil {
		t.Fatal("a fresh op-id on an already-closed worker must fail, not report a phantom success")
	}
}

// TestHostFirstReadOnlyWorkerBlocksButStillAudits: a read-only stage (explore)
// whose worker mutates must block the run — but the audit (mutations.json) MUST
// still be written. Auditing what the agent touched is non-negotiable.
func TestHostFirstReadOnlyWorkerBlocksButStillAudits(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("explore the code", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok) // explore is read-only and is the current stage
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sneaky.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockReason, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "explored and wrote a file"})
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

	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, blockReason, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil || blockReason != "" {
		t.Fatalf("first worker start: block=%q err=%v", blockReason, err)
	}
	if _, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatal(err)
	}

	// Budget exhausted (1 of 1 used) → the next start blocks the run and returns a
	// reason with NO worker id (the host must not launch a subagent).
	gotID, gotReason, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
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

// TestAcceptedChangeIsNotChargedToTheNextWorker: `run resume --accept-changes` says
// "this change is mine, not the agent's". Re-baselining must therefore make it
// invisible to whoever runs NEXT — otherwise the accepted change reappears in the next
// worker's mutations.json, and the audit accuses that worker of a write it never made.
// An audit that misattributes is worse than no audit: it is evidence you cannot trust.
func TestAcceptedChangeIsNotChargedToTheNextWorker(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// The explore worker (read-only) writes a file → the run blocks on the violation.
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accepted.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, _ := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "oops"}); reason == "" {
		t.Fatal("a read-only worker that mutated must block the run")
	}
	// The human looks at it and says: keep it. Resume ROTATES the driver token — a leaked
	// or lost capability dies here — so the orchestrator drives on with the new one.
	_, tok, err = e.ReopenRun(runID, ResumeOptions{AcceptChanges: true})
	if err != nil {
		t.Fatalf("resume --accept-changes: %v", err)
	}
	mustStageClose(t, e, hostRun{runID, tok}, "explore")

	// The next worker touches ONLY its own file.
	wid2, _, err := e.WorkerStart(runID, "implement", "implementer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, err := e.WorkerComplete(runID, wid2, "", tok, WorkerOutcome{Result: "done"}); err != nil || reason != "" {
		t.Fatalf("implement worker: reason=%q err=%v", reason, err)
	}

	report, err := store.LoadMutationReport(runID, wid2)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range report.Mutations {
		if m.Path == "accepted.go" {
			t.Fatalf("the accepted change was charged to worker %s, which never touched it: %+v", wid2, report.Mutations)
		}
	}
	if !mutationRecorded(store, runID, "feature.go") {
		t.Fatal("the worker's own change must still be audited")
	}
}

// TestHostLocalStateIsRecordedNotHidden: the host rewrites .claude/settings.local.json
// mid-run (Claude Code persists an approved permission the moment the user says "yes"),
// on a file the agent never touched. So it must not BLOCK a read-only stage.
//
// But it must still be RECORDED. That file IS the host's permission allowlist: an agent
// that wrote to it would be granting itself tools. Exempting it from the policy is a
// decision we can defend; claiming the mutation never happened is not. It lands in
// mutations.json with its hash, flagged host_bookkeeping.
func TestHostLocalStateIsRecordedNotHidden(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("explore the code", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"),
		[]byte(`{"permissions":{"allow":["Bash(ls)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Read-only stage: it must NOT block. The agent touched nothing.
	if reason, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "explored"}); err != nil || reason != "" {
		t.Fatalf("the host's own bookkeeping must not block a read-only stage: reason=%q err=%v", reason, err)
	}
	report, err := store.LoadMutationReport(runID, wid)
	if err != nil {
		t.Fatal(err)
	}
	var found *core.Mutation
	for i := range report.Mutations {
		if report.Mutations[i].Path == ".claude/settings.local.json" {
			found = &report.Mutations[i]
		}
	}
	if found == nil {
		t.Fatal("the change must still be RECORDED — not blocking on the host's allowlist is not the same as pretending it never changed")
	}
	if !found.HostBookkeeping {
		t.Fatal("it must be flagged host_bookkeeping — that flag is what exempts it from the policy")
	}
	if found.Hash == "" {
		t.Fatal("it must carry its content hash, like any other piece of evidence")
	}
}

// TestRetryDoesNotDuplicateEvents: a retry RECOVERS by replaying the operation, so it
// re-emits every event the first attempt already wrote. events.ndjson is the public audit
// trail — the file a human reads to know what happened — and a duplicated mutation_tracked
// makes it ambiguous whether the worker was audited once or twice. "A retry does not
// duplicate effects" has to include the record of those effects.
func TestRetryDoesNotDuplicateEvents(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "touched.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The operation commits, then the process dies before its op record lands.
	out := WorkerOutcome{Result: "done"}
	crashAfterCommit(t, e, runID, wid, "op-dup", "explore is read-only but the worker modified touched.go", out)
	if _, err := e.WorkerComplete(runID, wid, "op-dup", tok, out); err != nil {
		t.Fatal(err)
	}
	first := countEvents(t, store, runID)

	// The host never saw the response and retries with the SAME op-id.
	if _, err := e.WorkerComplete(runID, wid, "op-dup", tok, out); err != nil {
		t.Fatal(err)
	}
	for name, n := range countEvents(t, store, runID) {
		if n != first[name] {
			t.Fatalf("the retry duplicated the %q event (%d → %d) — events.ndjson is the audit trail", name, first[name], n)
		}
	}
}

func countEvents(t *testing.T, store *rt.Store, runID string) map[string]int {
	t.Helper()
	events, err := store.ReadEvents(runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, ev := range events {
		out[ev.Event]++
	}
	return out
}

// TestHostLocalStateBlockStopsTheRun: `security.hostLocalState: block` is the opt-in for
// people who have pre-authorized every command their agents need, so that file should
// never move mid-run. For them, a change to the host's permission allowlist during a
// worker means something they did not expect wrote it — and the kernel cannot tell whether
// that was the host or the agent granting itself tools. So it stops and asks.
//
// The DEFAULT stays warn (see TestHostLocalStateIsRecordedNotHidden): blocking by default
// would kill a run the first time a user clicks "approve", which is the bug that started
// all of this.
func TestHostLocalStateBlockStopsTheRun(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	e.cfg.Security.HostLocalState = "block"

	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent grants itself every bash command.
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"),
		[]byte(`{"permissions":{"allow":["Bash(*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reason, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "explored"})
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("with hostLocalState: block, a change to the host's permission file must stop the run")
	}
	if final, _ := store.LoadState(runID); final.Status != core.StatusBlocked {
		t.Fatalf("the run must be blocked, got %s", final.Status)
	}
}

// TestBudgetPoisoningIsRejected: the budget is what stops a runaway agent, so a host
// must not be able to disable it by reporting nonsense usage. A NEGATIVE cost or token
// count SUBTRACTS from what the run has spent. A NaN cost makes every `spent >= max`
// comparison false — NaN compares false against everything — so the cap stops existing,
// silently. Both must be rejected BEFORE anything is written.
func TestBudgetPoisoningIsRejected(t *testing.T) {
	poison := map[string]WorkerOutcome{
		"negative cost":     {Result: "x", CostUSD: -1000, CostReported: true},
		"NaN cost":          {Result: "x", CostUSD: math.NaN(), CostReported: true},
		"infinite cost":     {Result: "x", CostUSD: math.Inf(1), CostReported: true},
		"negative tokens":   {Result: "x", TokensIn: -5_000_000, TokensReported: true},
		"negative out only": {Result: "x", TokensOut: -1, TokensReported: true},
	}
	for name, out := range poison {
		t.Run(name, func(t *testing.T) {
			e, store, _ := hostFirstEngine(t)
			state, tok, err := e.StartRun("task", "quick", "")
			if err != nil {
				t.Fatal(err)
			}
			wid, _, err := e.WorkerStart(state.RunID, "explore", "explorer", "", tok)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.WorkerComplete(state.RunID, wid, "op-poison", tok, out); err == nil {
				t.Fatal("poisoned usage must be rejected — it would disable the run's budget")
			}
			assertBudgetUnpoisoned(t, store, state.RunID, wid)
		})
	}
}

// assertBudgetUnpoisoned checks a rejected usage report reached neither the budget nor
// the worker: the numbers stay sane, and the worker stays OPEN for a corrected retry.
func assertBudgetUnpoisoned(t *testing.T, store *rt.Store, runID, wid string) {
	t.Helper()
	st, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	b := st.Budgets
	if b.CostUSDSpent < 0 || b.TokensInSpent < 0 || b.TokensOutSpent < 0 {
		t.Fatalf("the rejected usage still reached the budget: %+v", b)
	}
	if math.IsNaN(b.CostUSDSpent) || math.IsInf(b.CostUSDSpent, 0) {
		t.Fatalf("a non-finite cost reached the budget (%v) — every cap comparison against it is false", b.CostUSDSpent)
	}
	if st.ActiveWorker != wid {
		t.Fatalf("a rejected call must leave the worker open for a corrected retry, active=%q", st.ActiveWorker)
	}
}

// TestTokenBudgetSaturatesInsteadOfWrapping: signed overflow in Go wraps to a large
// NEGATIVE number, which would reset a run's spend to below zero — the one number a
// runaway agent must never be able to reset. Saturating keeps `spent >= max` true.
func TestTokenBudgetSaturatesInsteadOfWrapping(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.LoadState(state.RunID)
	st.Budgets.TokensInSpent = math.MaxInt - 10
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	wid, _, err := e.WorkerStart(state.RunID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(state.RunID, wid, "", tok, WorkerOutcome{
		Result: "x", TokensIn: 1000, TokensReported: true,
	}); err != nil {
		t.Fatal(err)
	}
	if final, _ := store.LoadState(state.RunID); final.Budgets.TokensInSpent < 0 {
		t.Fatalf("the token budget wrapped negative (%d) — a runaway agent could reset its own cap", final.Budgets.TokensInSpent)
	}
}

// TestRejectedArtifactWritesNothing: a call the kernel REJECTS must leave the runtime
// exactly as it found it (I1/I2). The old code saved result.md first and validated
// artifacts inside the write loop — so a batch with one good and one bad artifact left
// the result and (depending on Go's random map order) an arbitrary subset on disk.
func TestRejectedArtifactWritesNothing(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, hostRun{runID, tok}, "explore", "explorer", "", dir)
	mustStageClose(t, e, hostRun{runID, tok}, "explore") // → propose

	wid, _, err := e.WorkerStart(runID, "propose", "proposer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	// One artifact this stage MAY produce, one it may not. The batch must be rejected
	// whole — `plan` is the plan stage's evidence, never the proposer's.
	_, cerr := e.WorkerComplete(runID, wid, "op-mixed", tok, WorkerOutcome{
		Result:    "my proposal",
		Artifacts: map[string]string{"proposal": "# Proposal", "plan": "# Plan (not mine to write)"},
	})
	if cerr == nil {
		t.Fatal("an artifact this stage cannot produce must be rejected")
	}
	for _, name := range []string{"proposal", "plan"} {
		if _, err := store.LoadArtifactMeta(runID, name); err == nil {
			t.Fatalf("the rejected call still wrote the %q artifact — a rejected call must write NOTHING", name)
		}
	}
	if st, _ := store.LoadState(runID); st.ActiveWorker != wid {
		t.Fatalf("a rejected call must leave the worker open, active=%q", st.ActiveWorker)
	}
}

// TestOpIDIsBoundToItsPayload: an op-id names ONE operation, and an operation is its
// payload — not just its target. Reusing an op-id with DIFFERENT evidence is not a
// retry: answering "already done" to it would silently discard what the host just sent.
func TestOpIDIsBoundToItsPayload(t *testing.T) {
	e, _, dir := hostFirstEngine(t)
	store, runID, tok := driveToReview(t, e, dir)

	wid, _, err := e.WorkerStart(runID, "review", "reviewer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReviewComplete(runID, wid, "op-v", tok, WorkerOutcome{
		Result: `{"status":"approved","summary":"lgtm"}`,
	}); err != nil {
		t.Fatal(err)
	}
	// Same op-id, a DIFFERENT verdict. This must not be mistaken for a lost response.
	if _, err := e.ReviewComplete(runID, wid, "op-v", tok, WorkerOutcome{
		Result: `{"status":"needs_fixes","summary":"actually no"}`,
	}); err == nil {
		t.Fatal("the same op-id with different evidence must be rejected, not answered 'already done'")
	}
	// And the run kept the branch its REAL verdict produced.
	if st, _ := store.LoadState(runID); st.CurrentStage != "verify" {
		t.Fatalf("the run must keep the branch from the recorded verdict, got %q", st.CurrentStage)
	}
}

// crashAfterCommit simulates the process dying in the window between a worker-close
// operation's COMMIT POINT (the worker's terminal status is durably written, carrying
// the op-id and the outcome it decided) and its later steps (applying that outcome to
// the run state, writing the op record). It writes only what the commit writes.
func crashAfterCommit(t *testing.T, e *Engine, runID, workerID, opID, blockReason string, out WorkerOutcome) {
	t.Helper()
	ws, err := e.store.LoadWorkerStatus(runID, workerID)
	if err != nil {
		t.Fatal(err)
	}
	fin := time.Now().UTC()
	ws.Status, ws.FinishedAt = core.WorkerDone, &fin
	// The commit stamps the op-id, the payload fingerprint AND the outcome in one write —
	// that is exactly what makes the recovery verifiable. Simulating it without the
	// fingerprint would be simulating a different (older) kernel.
	ws.CloseOpID, ws.CloseFingerprint, ws.CloseBlockReason = opID, opFingerprint(runID, workerID, out.payloadHash()), blockReason
	if err := e.store.SaveWorkerStatus(runID, ws); err != nil {
		t.Fatal(err)
	}
}

// TestRetryAfterCrashBlocksOnTheRecordedViolation: the phantom-success case, and the
// reason the worker's status doubles as an operation journal.
//
// A read-only worker mutated the tree, so `worker complete` decided to BLOCK the run.
// The process then dies after the worker is marked done but before that block reaches
// state.json. The retry (same op-id) must apply the recorded block — not look at a
// run that is still `active`, conclude "nothing to do", and report success. Treating
// "the worker is done" as "the operation is done" loses a security block to a crash.
func TestRetryAfterCrashBlocksOnTheRecordedViolation(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("explore the code", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snuck-in.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := WorkerOutcome{Result: "done"}
	crashAfterCommit(t, e, runID, wid, "op-crash", "explore is read-only but the worker modified snuck-in.go", out)

	reason, err := e.WorkerComplete(runID, wid, "op-crash", tok, out)
	if err != nil {
		t.Fatalf("the retry must resume the committed operation: %v", err)
	}
	if reason == "" {
		t.Fatal("the retry reported success for an operation that had decided to BLOCK — a phantom success")
	}
	if st, _ := store.LoadState(runID); st.Status != core.StatusBlocked {
		t.Fatalf("the recorded violation must reach the run state, got %s (%s)", st.Status, st.BlockedReason)
	}
}

// TestRetryAfterCrashAppliesTheReviewBranch: same window, review side. The verdict is
// persisted and the reviewer committed, but the process dies before the run branches.
// The retry must recompute the branch FROM THE PERSISTED VERDICT and advance — not
// report "recorded" and leave the run parked at review forever.
func TestRetryAfterCrashAppliesTheReviewBranch(t *testing.T) {
	e, _, dir := hostFirstEngine(t)
	store, runID, tok := driveToReview(t, e, dir)

	wid, _, err := e.WorkerStart(runID, "review", "reviewer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	verdict := `{"status":"approved","summary":"lgtm"}`
	st, _ := store.LoadState(runID)
	stage, _ := e.stageOf(st, "review")
	if verr := e.persistReviewVerdict(st, stage, core.Result{Markdown: verdict}); verr != nil {
		t.Fatal(verr)
	}
	out := WorkerOutcome{Result: verdict}
	crashAfterCommit(t, e, runID, wid, "op-crash", "", out)

	if reason, err := e.ReviewComplete(runID, wid, "op-crash", tok, out); err != nil || reason != "" {
		t.Fatalf("the retry must resume the committed review: reason=%q err=%v", reason, err)
	}
	if final, _ := store.LoadState(runID); final.CurrentStage != "verify" {
		t.Fatalf("the retry must branch from the persisted verdict, got stage %q", final.CurrentStage)
	}
}

// TestHostFirstOpIDIsIdempotent: a retry with the same --op-id returns the same
// result WITHOUT re-applying — so a host that loses the response can safely retry.
// Without an op-id, the safety guards still reject a double-apply.
func TestHostFirstOpIDIsIdempotent(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// worker start: a retry with the same op-id returns the SAME worker id and
	// does NOT open a second worker or double-count the invocation.
	wid1, _, err := e.WorkerStart(runID, "explore", "explorer", "op-start-1", tok)
	if err != nil {
		t.Fatal(err)
	}
	wid2, _, err := e.WorkerStart(runID, "explore", "explorer", "op-start-1", tok)
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
	if _, err := e.WorkerComplete(runID, wid1, "op-done-1", tok, WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid1, "op-done-1", tok, WorkerOutcome{Result: "ok"}); err != nil {
		t.Fatalf("retried worker complete with the same op-id must succeed (cached): %v", err)
	}
	// But WITHOUT an op-id, the guard still rejects a double-complete — op-id is
	// what makes the retry safe.
	if _, err := e.WorkerComplete(runID, wid1, "", tok, WorkerOutcome{Result: "ok"}); err == nil {
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
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	_ = dir

	// Force the op-record WRITE to fail while leaving the READ intact: a read-only DIRECTORY.
	// Looking for a record inside it still returns "not there" (which is the truth); creating
	// one fails. (It used to be a read-only FILE, which also broke the read — and the engine
	// now fails closed on an unreadable record, correctly: an op record we cannot read is what
	// stops an op-id being reused for a different operation.)
	opsPath := filepath.Join(store.RunDir(runID), "operations")
	if err := os.MkdirAll(opsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(opsPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(opsPath, 0o755) })

	_, cerr := e.WorkerComplete(runID, wid, "op-rec-fail", tok, WorkerOutcome{Result: "done"})
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
	if _, err := e.WorkerComplete(runID, wid, "op-rec-fail", tok, WorkerOutcome{Result: "done"}); err != nil {
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
	state, _, err := e.StartRun("task", "quick", "op-incomplete")
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
	st, _, err := e.StartRun("task", "quick", "op-incomplete")
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
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Force the op-record WRITE to fail while the READ still works — see the note in
	// TestWorkerCompleteRecoversWhenOpRecordWriteFails.
	opsPath := filepath.Join(store.RunDir(runID), "operations")
	if err := os.MkdirAll(opsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(opsPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(opsPath, 0o755) })
	_, _, serr := e.WorkerStart(runID, "explore", "explorer", "ws-fail", tok)
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
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "ws-fail", tok)
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

	s1, _, err := e.StartRun("task", "", "op1")
	if err != nil {
		t.Fatal(err)
	}
	// Same op-id, default workflow spelled out → same run (normalized fingerprint).
	s2, _, err := e.StartRun("task", "quick", "op1")
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
	if _, _, err := e.StartRun("task", "review", "op1"); err == nil {
		t.Fatal("reusing an op-id for a different workflow must error")
	}
}

// TestHostFirstOpIDBoundToOperation: an op-id is bound to its command+args. Reusing
// it for a DIFFERENT operation must error, not return the wrong cached result.
func TestHostFirstOpIDBoundToOperation(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Record op "shared" as a worker start.
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "shared", tok)
	if err != nil {
		t.Fatal(err)
	}

	// Reusing "shared" for worker COMPLETE (different kind) must be rejected — not
	// silently return the cached worker-start result (which would leave the worker
	// active while the CLI claims completion).
	if _, err := e.WorkerComplete(runID, wid, "shared", tok, WorkerOutcome{Result: "ok"}); err == nil {
		t.Fatal("reusing an op-id across different operations must error")
	}

	// Reusing it for the same kind but DIFFERENT args (another stage/role) is also
	// a different operation → rejected.
	if _, _, err := e.WorkerStart(runID, "implement", "implementer", "shared", tok); err == nil {
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
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
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

	_, cerr := e.WorkerComplete(runID, wid, "op-fail", tok, WorkerOutcome{Result: "result that cannot be written"})
	if cerr == nil {
		t.Fatal("worker complete must FAIL when a critical write fails, not report success")
	}
	// The op must NOT be recorded as completed — so a retry (after the dir is
	// writable again) can re-apply rather than returning a phantom success.
	if _, ok, _ := e.cachedOp(runID, "op-fail"); ok {
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
	if _, err := e.WorkerComplete(runID, wid, "op-fail", tok, WorkerOutcome{Result: "result that cannot be written"}); err != nil {
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
	state, tok, err := e.StartRun("task", "quick", "") // current stage = explore
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	if _, _, err := e.WorkerStart(runID, "implement", "implementer", "", tok); err == nil {
		t.Fatal("must reject a stage that is not the current one")
	}
	if _, _, err := e.WorkerStart(runID, "explore", "implementer", "", tok); err == nil {
		t.Fatal("must reject a role that does not match the stage")
	}
	if _, _, err := e.WorkerStart(runID, "verify", "explorer", "", tok); err == nil {
		t.Fatal("must reject closing onto a gate stage as a worker")
	}
	// A valid start, then a second start must be refused (one active worker).
	if _, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok); err != nil {
		t.Fatalf("valid worker start failed: %v", err)
	}
	if _, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok); err == nil {
		t.Fatal("must reject a second worker while one is active")
	}
}

// TestWorkerUsageUnknownWhenHostDoesNotReport: a native host that does not expose
// usage closes its worker WITHOUT either reported flag — the run's tokens/cost stay
// honestly "unknown", not a fake zero. Cost and tokens are independent: a host can
// report tokens but not cost (codex), and that must keep cost unknown.
func TestWorkerUsageUnknownWhenHostDoesNotReport(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// Close the explore worker with no usage — the host did not surface it.
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "ok"}); err != nil {
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
	wid2, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok) // still on explore
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid2, "", tok, WorkerOutcome{
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

// TestClosedWorkerIsNeverReAudited: the recovery path must not become a way to rewrite
// history. A worker closed by an older build carries a close_op_id but NO fingerprint, so
// resumeIfCommitted cannot verify the payload matches — and it must not assume it does.
// Before this guard, such a retry fell through to the audit path and RE-AUDITED the closed
// worker, overwriting its evidence with whatever payload the caller now sent.
func TestClosedWorkerIsNeverReAudited(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	_ = dir

	// A worker as an older build would leave it: done, still the active worker (the crash
	// window), with an op-id but no fingerprint.
	ws, err := store.LoadWorkerStatus(runID, wid)
	if err != nil {
		t.Fatal(err)
	}
	fin := time.Now().UTC()
	ws.Status, ws.FinishedAt, ws.CloseOpID = core.WorkerDone, &fin, "op-legacy"
	ws.CloseFingerprint = "" // the field did not exist yet
	if err := store.SaveWorkerStatus(runID, ws); err != nil {
		t.Fatal(err)
	}

	// Same op-id, DIFFERENT evidence. It must be refused, not quietly re-audited.
	if _, err := e.WorkerComplete(runID, wid, "op-legacy", tok, WorkerOutcome{
		Result: "completely different evidence",
	}); err == nil {
		t.Fatal("a closed worker must never be re-audited — its evidence has already been decided on")
	}
	if after, _ := store.LoadWorkerStatus(runID, wid); after.CloseFingerprint != "" {
		t.Fatal("the refused call still stamped a fingerprint on the closed worker")
	}
}

// TestUpgradeDoesNotLoseASecurityBlock: a run from a build with no operation journal could
// crash after the worker was marked `done` and its mutations.json written, but BEFORE the
// resulting block reached state.json. Such a worker is not `running`, so reconciliation
// skipped it, cleared ActiveWorker and let the run carry on — silently dropping a
// read-only violation that its own evidence still proves, on the very upgrade the docs
// promise is safe.
func TestUpgradeDoesNotLoseASecurityBlock(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	// A read-only worker mutated the tree; the audit landed on disk.
	if err := os.WriteFile(filepath.Join(dir, "snuck-in.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, _ := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "oops"}); reason == "" {
		t.Fatal("a read-only worker that mutated must block")
	}
	// Now rewind state.json to what an OLD build would have left after crashing between
	// "worker done" and "state applied": the block never reached the run, and the worker
	// carries no journal.
	ws, err := store.LoadWorkerStatus(runID, wid)
	if err != nil {
		t.Fatal(err)
	}
	ws.CloseOpID, ws.CloseFingerprint, ws.CloseBlockReason = "", "", "" // the fields did not exist
	if err := store.SaveWorkerStatus(runID, ws); err != nil {
		t.Fatal(err)
	}
	st, _ := store.LoadState(runID)
	st.Status, st.BlockedReason, st.ActiveWorker = core.StatusActive, "", wid
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}

	// The user upgrades and resumes.
	if _, _, err := e.ReopenRun(runID, ResumeOptions{}); err != nil {
		t.Fatalf("ReopenRun: %v", err)
	}
	final, _ := store.LoadState(runID)
	if final.Status != core.StatusBlocked {
		t.Fatalf("resume dropped the security block its own mutations.json still proves: status=%s reason=%q", final.Status, final.BlockedReason)
	}
}

// TestASubagentCannotDriveTheRun is the whole reason the driver token exists.
//
// A coding host's permission rules are SESSION-WIDE. The pack pre-authorizes
// `Bash(vichu worker complete:*)` so the ORCHESTRATOR can drive the run — and thereby
// authorizes it for every subagent in that session with Bash, including the implementer,
// which needs Bash to run the project's tests.
//
// Without a capability, that implementer can close its OWN worker and then keep editing
// files: mutation tracking ended at the close, so the later changes never reach
// mutations.json and can never block the run. The agent has stepped around the one thing
// this kernel is for. The permission layer cannot tell the two callers apart. The kernel
// can — if driving requires something the subagent does not have.
func TestASubagentCannotDriveTheRun(t *testing.T) {
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}

	// The subagent has Bash and every pre-authorized `vichu` command — but not the token.
	for name, call := range map[string]func() error{
		"worker complete": func() error {
			_, err := e.WorkerComplete(runID, wid, "", "", WorkerOutcome{Result: "closing myself"})
			return err
		},
		"stage close": func() error {
			_, err := e.StageClose(runID, "explore", "", "")
			return err
		},
		"worker start": func() error {
			_, _, err := e.WorkerStart(runID, "explore", "explorer", "", "")
			return err
		},
		"a guessed token": func() error {
			_, err := e.WorkerComplete(runID, wid, "", "deadbeef", WorkerOutcome{Result: "x"})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Fatalf("a caller without the driver token must not be able to run %q", name)
		}
	}

	// The token is never written under .vichu/ — only its hash — so a subagent that can
	// READ the runtime still cannot drive the run.
	st, _ := store.LoadState(runID)
	if st.DriverTokenHash == "" {
		t.Fatal("the run must carry the token's hash")
	}
	if st.DriverTokenHash == tok {
		t.Fatal("state.json must hold the HASH, never the token itself")
	}

	// The orchestrator, which does hold it, drives normally — and the audit still sees
	// everything the worker touched.
	if err := os.WriteFile(filepath.Join(dir, "touched.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, _ := e.WorkerComplete(runID, wid, "", tok, WorkerOutcome{Result: "done"}); reason == "" {
		t.Fatal("the read-only worker's mutation must still block — the audit is intact")
	}
}

// TestResumeNeverHandsOutATokenItDidNotPersist: rotation must LAND before the token is
// returned. `e.saveState` only warns outside a host-first operation, and resume is not one
// — so minting, warning, and returning anyway produced a 64-character secret that could not
// drive the run, while state.json still carried the OLD hash. The kernel handing out a key
// to nothing is I6 broken as plainly as it gets.
func TestResumeNeverHandsOutATokenItDidNotPersist(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("relies on POSIX directory permissions denying a write")
	}
	e, store, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	before, _ := store.LoadState(runID)

	// Make the run directory un-writable: SaveState's temp+rename cannot land.
	runDir := store.RunDir(runID)
	if err := os.Chmod(runDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, newTok, rerr := e.ReopenRun(runID, ResumeOptions{})
	if err := os.Chmod(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rerr == nil {
		t.Fatal("a rotation that cannot be persisted must FAIL, not report success")
	}
	if newTok != "" {
		t.Fatalf("a failed rotation must return NO token, got one of %d chars", len(newTok))
	}

	// The previous token is still the valid one — the honest, recoverable outcome.
	after, _ := store.LoadState(runID)
	if after.DriverTokenHash != before.DriverTokenHash {
		t.Fatal("a failed rotation must leave the old hash in place")
	}
	if _, _, err := e.WorkerStart(runID, "explore", "explorer", "", tok); err != nil {
		t.Fatalf("the caller's original token must still work after a failed rotation: %v", err)
	}
	_ = dir
}

// TestAFailedEventWriteFailsTheOperation: events.ndjson is the public audit trail. An
// operation that reports success while its evidence never reached the log is the kernel
// lying about what happened — `emit` used to degrade that to a warning, so `worker start`
// exited 0 with an active worker and no `worker_started` event to show for it.
func TestAFailedEventWriteFailsTheOperation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replaces a file with a directory")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	// Make events.ndjson un-appendable: replace it with a directory.
	events := filepath.Join(store.RunDir(state.RunID), "events.ndjson")
	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(events, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := e.WorkerStart(state.RunID, "explore", "explorer", "op-ev", tok); err == nil {
		t.Fatal("an operation whose event cannot be written must FAIL, not report success")
	}
}

// TestACorruptOpRecordFailsClosed: the operation record is what carries an op-id's kind and
// fingerprint, and therefore what stops the SAME id being reused for a DIFFERENT operation.
// `cachedOp` used to swallow the read error, so "no record" and "a record I cannot read"
// became the same thing: corrupt the record of a `worker.start` and its op-id was free again —
// a `worker.complete` reusing it was accepted, closed the worker, and exited 0.
//
// Corruption silently removed the guard. Now it fails closed.
func TestACorruptOpRecordFailsClosed(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	wid, _, err := e.WorkerStart(runID, "explore", "explorer", "worker-a", tok)
	if err != nil {
		t.Fatal(err)
	}
	// The record of `worker-a` (a worker.start) is corrupted.
	rec := filepath.Join(store.RunDir(runID), "operations", "worker-a.json")
	if err := os.WriteFile(rec, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reusing that op-id for a DIFFERENT operation must be refused, not accepted.
	if _, err := e.WorkerComplete(runID, wid, "worker-a", tok, WorkerOutcome{Result: "done"}); err == nil {
		t.Fatal("a corrupt op record must not let its op-id be reused for another operation")
	}
	if ws, _ := store.LoadWorkerStatus(runID, wid); ws.Status != core.WorkerRunning {
		t.Fatalf("the refused call must not have closed the worker, got %s", ws.Status)
	}
	if st, _ := store.LoadState(runID); st.ActiveWorker != wid {
		t.Fatalf("the refused call must leave the run untouched, active=%q", st.ActiveWorker)
	}
}

// TestFrozenConfigBeatsMidRunTampering: an agent that rewrites vichu.yaml mid-run must not
// change the rule that judges it. The run starts with a FAILING gate (frozen in the
// snapshot); the agent flips the live config to a passing gate; verify must still run the
// FROZEN failing gate and block — not complete on the tampered one.
func TestFrozenConfigBeatsMidRunTampering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell gates")
	}
	e, store, dir := hostFirstEngine(t)
	// The run starts with a gate that always fails; that is what the snapshot freezes.
	e.cfg.Commands = map[string]config.OSCommand{"test": {Unix: "false", Windows: "cmd /c exit 1"}}

	state, tok, err := e.StartRun("add a feature", "quick", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	runID := state.RunID

	// The agent rewrites the LIVE config to a passing gate (and disables the sensitive-file
	// block, for good measure) — the exact mid-run tampering the freeze must defeat.
	e.cfg.Commands = map[string]config.OSCommand{"test": {Unix: "true", Windows: "cmd /c exit 0"}}
	e.cfg.Security.SensitiveMutations = "warn"

	mustWorker(t, e, hostRun{runID, tok}, "explore", "explorer", "", dir)
	mustStageClose(t, e, hostRun{runID, tok}, "explore")
	mustWorker(t, e, hostRun{runID, tok}, "implement", "implementer", "feature.go", dir)
	mustStageClose(t, e, hostRun{runID, tok}, "implement")

	// verify: the frozen `false` gate must run and block, NOT the live `true` one.
	reason, err := e.StageClose(runID, "verify", "", tok)
	if err != nil {
		t.Fatalf("verify stage close: %v", err)
	}
	if reason == "" {
		t.Fatal("verify passed using the agent's tampered gate — the frozen failing gate was ignored")
	}
	final, _ := store.LoadState(runID)
	if final.Status == core.StatusCompleted {
		t.Fatal("run reached completed using a config the agent rewrote mid-run")
	}
}

// TestStageCloseOutOfOrderRejectsWithoutMutating: closing a worker stage that never had a
// `worker start` is a host PROTOCOL error — it must be REJECTED (error), not turned into a
// blocked run. It must write nothing: no state change, no run_blocked event, no op record.
func TestStageCloseOutOfOrderRejectsWithoutMutating(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	runID := state.RunID

	before, _ := store.LoadState(runID)
	evBefore, _ := store.ReadEvents(runID)

	// stage close explore, with NO worker start/complete.
	if reason, cerr := e.StageClose(runID, "explore", "", tok); cerr == nil {
		t.Fatalf("out-of-order stage close must return an error, got reason=%q nil err", reason)
	}

	after, _ := store.LoadState(runID)
	if after.Status != core.StatusActive || after.Status != before.Status {
		t.Fatalf("state was mutated by a rejected stage close: %s → %s", before.Status, after.Status)
	}
	evAfter, _ := store.ReadEvents(runID)
	if len(evAfter) != len(evBefore) {
		t.Fatalf("a rejected stage close wrote %d event(s)", len(evAfter)-len(evBefore))
	}
	for _, ev := range evAfter {
		if ev.Event == core.EventRunBlocked {
			t.Fatal("a rejected protocol error must not emit run_blocked")
		}
	}
}

// TestNativeWallClockBudgetBlocks (ronda 18): a native (host-first) run has no headless loop, so
// its wall-clock must be accrued durably from CreatedAt at each transactional command — otherwise
// maxWallClock never fires and a run can run for hours under a 2h budget.
func TestNativeWallClockBudgetBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	e.cfg.Budgets.Run.MaxWallClock = config.Duration(time.Second)

	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	// Backdate creation so the elapsed wall-clock exceeds the 1s budget (no sleep).
	st, _ := store.LoadState(state.RunID)
	st.CreatedAt = time.Now().Add(-10 * time.Second)
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}

	_, blockReason, err := e.WorkerStart(state.RunID, "explore", "explorer", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if blockReason == "" {
		t.Fatal("worker start must block when the native wall-clock budget is exhausted")
	}
	final, _ := store.LoadState(state.RunID)
	if final.Budgets.WallClockSpentSeconds == 0 {
		t.Fatal("native wall-clock must be recorded (was 0)")
	}
}

// TestReopenRunFailsWhenResumeEventCannotPersist (ronda 18): `run resume` must not report success
// (and hand out a fresh driver token) if run_resumed never reached the audit. events.ndjson turned
// into a directory makes the append fail.
func TestReopenRunFailsWhenResumeEventCannotPersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, _, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	// Make the audit log unwritable: remove it and put a directory in its place.
	ev := filepath.Join(store.RunDir(state.RunID), "events.ndjson")
	if err := os.Remove(ev); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ev, 0o755); err != nil {
		t.Fatal(err)
	}

	_, tok, rerr := e.ReopenRun(state.RunID, ResumeOptions{})
	if rerr == nil {
		t.Fatal("ReopenRun reported success though run_resumed could not be appended")
	}
	if tok != "" {
		t.Fatal("ReopenRun handed out a driver token despite the resume event failing")
	}
}

// TestStageCloseRejectsForeignOpID (ronda 19): once a stage is closed, only the op-id that closed
// it may recover the result. A DIFFERENT op-id closing an already-closed stage is out-of-order and
// must be rejected — not silently succeed and mint a bogus op record.
func TestStageCloseRejectsForeignOpID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, _, dir := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	run := hostRun{state.RunID, tok}
	mustWorker(t, e, run, "explore", "explorer", "", dir)
	if _, err := e.StageClose(run.id, "explore", "close-original", tok); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// A brand-new op-id trying to close the already-closed 'explore' must be rejected.
	if _, err := e.StageClose(run.id, "explore", "close-brand-new", tok); err == nil {
		t.Fatal("a foreign op-id closed an already-closed stage and got a success it never earned")
	}
}

// TestStageCloseBlocksWhenWallClockExhausted (ronda 19): a run that ran past maxWallClock while the
// agent worked must NOT reach the terminal stage and report completed — stage close reconciles the
// native wall-clock and blocks before advancing.
func TestStageCloseBlocksWhenWallClockExhausted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, dir := hostFirstEngine(t)
	e.cfg.Budgets.Run.MaxWallClock = config.Duration(time.Second)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	run := hostRun{state.RunID, tok}
	mustWorker(t, e, run, "explore", "explorer", "", dir)
	// The agent "took" more than the budget: backdate creation past the 1s limit.
	st, _ := store.LoadState(run.id)
	st.CreatedAt = time.Now().Add(-10 * time.Second)
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}

	reason, err := e.StageClose(run.id, "explore", "", tok)
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("stage close must block when the wall-clock budget is exhausted, not advance")
	}
	final, _ := store.LoadState(run.id)
	if final.Status == core.StatusCompleted {
		t.Fatal("run completed despite exceeding maxWallClock")
	}
}

// TestResumeFailureKeepsPreviousTokenValid (ronda 19): when resume fails to record run_resumed, it
// must NOT have rotated the token — the message promises "your previous token is still valid", so
// that must be TRUE. The old token still authenticates once storage is fixed.
func TestResumeFailureKeepsPreviousTokenValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok0, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	// Break the audit log so run_resumed cannot be appended — but keep its valid contents so we
	// can restore them (the run stays materialized; only the write is blocked).
	ev := filepath.Join(store.RunDir(state.RunID), "events.ndjson")
	orig, err := os.ReadFile(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ev); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ev, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, tok, rerr := e.ReopenRun(state.RunID, ResumeOptions{}); rerr == nil || tok != "" {
		t.Fatalf("resume must fail without a new token, got tok=%q err=%v", tok, rerr)
	}
	// Storage recovers (the valid log is restored); the ORIGINAL token must still drive the run —
	// a failed resume must not have rotated it.
	if err := os.RemoveAll(ev); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ev, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "", tok0); werr != nil {
		t.Fatalf("the previous token was invalidated by a failed resume: %v", werr)
	}
}

// TestWorkerStartBlocksOnCorruptAuditWithoutOpID (ronda 21): the audit log is validated on EVERY
// host command, not just when an op-id is present. A corrupt log must block the mutation.
func TestWorkerStartBlocksOnCorruptAuditWithoutOpID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "events.ndjson"), []byte("not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "", tok); werr == nil {
		t.Fatal("worker start (no op-id) proceeded on a corrupt audit log")
	}
}

// TestWorkerStartBlocksOnDeletedAudit (ronda 21): a materialized run always has a log (run_created);
// a missing one is corruption, not "zero events" — mutating would lose run_created.
func TestWorkerStartBlocksOnDeletedAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.RunDir(state.RunID), "events.ndjson")); err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "op1", tok); werr == nil {
		t.Fatal("worker start proceeded on a deleted audit log")
	}
}

// TestStageCloseRejectsWrongStageEvenWhenOverBudget (ronda 22, self-audit of r19): an out-of-order
// close (wrong stage) must be REJECTED (I1), not turned into a run BLOCK just because the budget is
// also exhausted. Validation runs before the budget reconciliation.
func TestStageCloseRejectsWrongStageEvenWhenOverBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	e.cfg.Budgets.Run.MaxWallClock = config.Duration(time.Second)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.LoadState(state.RunID)
	st.CreatedAt = time.Now().Add(-10 * time.Second) // budget exhausted
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	// Current stage is explore; closing 'implement' is out of order.
	if _, cerr := e.StageClose(state.RunID, "implement", "", tok); cerr == nil {
		t.Fatal("closing the wrong stage must be rejected, not accepted")
	}
	final, _ := store.LoadState(state.RunID)
	if final.Status == core.StatusBlocked {
		t.Fatal("an invalid stage close must NOT block the run (I1: reject != block)")
	}
}

// TestWorkerStartBlocksOnEmptyObjectAudit (ronda 22): `{}` parses as JSON but is not an event —
// it has no ts/run/event, so ValidateEventLog must reject it (syntax is not integrity).
func TestWorkerStartBlocksOnEmptyObjectAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "events.ndjson"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "", tok); werr == nil {
		t.Fatal("worker start proceeded on a `{}` audit (no ts/run/event)")
	}
}

// TestCachedRetryFailsOnCorruptAudit (ronda 22): the audit is validated BEFORE the op-id cache, so a
// retry cannot recover a cached "success" on top of a corrupt log.
func TestCachedRetryFailsOnCorruptAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, tok, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "ws1", tok); werr != nil {
		t.Fatal(werr) // first attempt records the op
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "events.ndjson"), []byte("not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, werr := e.WorkerStart(state.RunID, "explore", "explorer", "ws1", tok); werr == nil {
		t.Fatal("a cached retry returned success on a corrupt audit")
	}
}

// TestReopenRunBlocksOnCorruptAudit (ronda 23): resume must validate the audit before rotating the
// token or appending run_resumed — a corrupt log fails closed.
func TestReopenRunBlocksOnCorruptAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, _, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "events.ndjson"), []byte("{not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, tok, rerr := e.ReopenRun(state.RunID, ResumeOptions{}); rerr == nil || tok != "" {
		t.Fatalf("resume must fail on a corrupt audit, got tok=%q err=%v", tok, rerr)
	}
}

// TestReopenRunValidatesTerminalAudit (ronda 24): ReopenRun used to return "done" for a TERMINAL run
// before taking the lock or checking the audit — so a completed run whose history is corrupt was
// reported as a clean finish. Reporting a run as finished leans on a history the kernel must be able
// to read; validate under the lock BEFORE the terminal short-circuit.
func TestReopenRunValidatesTerminalAudit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `test -f`")
	}
	e, store, _ := hostFirstEngine(t)
	state, _, err := e.StartRun("task", "quick", "")
	if err != nil {
		t.Fatal(err)
	}
	// Drive the run to a terminal state on disk.
	state.Status = core.StatusCompleted
	if serr := store.SaveState(state); serr != nil {
		t.Fatal(serr)
	}
	// Corrupt the audit with a parseable-but-incoherent log (no run_created).
	if werr := os.WriteFile(filepath.Join(store.RunDir(state.RunID), "events.ndjson"), []byte("{}\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, _, rerr := e.ReopenRun(state.RunID, ResumeOptions{}); rerr == nil {
		t.Fatal("ReopenRun reported a terminal run as done over a corrupt audit")
	}
}

// TestResumeNonexistentRunCreatesNothing (ronda 25): resuming an id with no run must return
// ErrRunNotFound and NOT materialize its directory. Both resume paths now take the lock before
// loading state; the lock must NOT create the run dir for a run that never existed (I2 — a rejected
// command leaves no trace).
func TestResumeNonexistentRunCreatesNothing(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	if _, _, err := e.ReopenRun("ghost-run", ResumeOptions{}); !errors.Is(err, rt.ErrRunNotFound) {
		t.Fatalf("ReopenRun on a nonexistent run = %v, want ErrRunNotFound", err)
	}
	if _, err := e.Resume(context.Background(), "ghost-run", ResumeOptions{}); !errors.Is(err, rt.ErrRunNotFound) {
		t.Fatalf("Resume on a nonexistent run = %v, want ErrRunNotFound", err)
	}
	if _, err := os.Stat(store.RunDir("ghost-run")); err == nil {
		t.Fatal("resume of a nonexistent run materialized its directory")
	}
}

// TestNewOpEventScopeCountsFromSnapshot (ronda 25): op-id dedup counts events from the ALREADY-VERIFIED
// snapshot runOpCtx loaded, not from a second read of the log. This pins the count semantics the
// single read feeds: entries match on BOTH op-id and fingerprint (a changed payload counts zero).
func TestNewOpEventScopeCountsFromSnapshot(t *testing.T) {
	events := []core.Event{
		{Event: "run_created"},
		{Event: "worker_started", OpID: "op1", OpFP: "fp1", Seq: 1},
		{Event: "worker_started", OpID: "op2", OpFP: "fp2", Seq: 1},
		{Event: "worker_completed", OpID: "op1", OpFP: "fp1", Seq: 2},
	}
	if sc := newOpEventScope(events, "op1", "fp1"); sc.alreadyWritten != 2 {
		t.Fatalf("op1 alreadyWritten = %d, want 2", sc.alreadyWritten)
	}
	if sc := newOpEventScope(events, "op2", "fp2"); sc.alreadyWritten != 1 {
		t.Fatalf("op2 alreadyWritten = %d, want 1", sc.alreadyWritten)
	}
	// Same op-id, DIFFERENT fingerprint (payload changed) → zero prior events of THIS operation.
	if sc := newOpEventScope(events, "op1", "changed"); sc.alreadyWritten != 0 {
		t.Fatalf("op1 with changed fingerprint alreadyWritten = %d, want 0", sc.alreadyWritten)
	}
}
