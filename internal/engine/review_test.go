package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// reviewEngine wires an engine for the `review` workflow: the implementer writes
// a file (so the verify gate passes), and the reviewer returns the given
// verdicts in sequence. The registry builds a FRESH Fake per stage (as the real
// CLI does), so this also guards that verdict sequencing is driven by the
// engine's iteration, not adapter-internal state.
func reviewEngine(t *testing.T, dir string, verdicts []adapters.FakeVerdict) (*Engine, *rt.Store) {
	t.Helper()
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Workflow.MaxAutoIterations = 3 // small auto-fix budget for fast tests
	checkCmd := "test -f src/feature.txt"
	if runtime.GOOS == "windows" {
		checkCmd = "cmd /c if exist src\\feature.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: checkCmd, Windows: checkCmd}}

	script := adapters.FakeScript{
		ResultText: "did the work",
		Actions: map[string][]adapters.FakeAction{
			"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
		},
		Verdicts: map[string][]adapters.FakeVerdict{"reviewer": verdicts},
	}
	reg := adapters.NewRegistry()
	// A fresh Fake per Get — the real-world path — to prove verdict sequencing
	// does not depend on a shared adapter instance.
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(script), nil })

	return New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo}), store
}

// TestReviewApprovedRunsToCompletion: a direct "approved" verdict advances
// through verify to done, with the verdict persisted as evidence.
func TestReviewApprovedRunsToCompletion(t *testing.T) {
	dir := newTestRepo(t)
	e, store := reviewEngine(t, dir, []adapters.FakeVerdict{{Status: "approved", Summary: "looks good"}})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (%s)", state.Status, state.BlockedReason)
	}
	if _, err := os.Stat(filepath.Join(store.ReviewDir(state.RunID, "review", 1), "verdict.json")); err != nil {
		t.Fatalf("expected review verdict.json: %v", err)
	}
	events, _ := store.ReadEvents(state.RunID)
	if n := countEvent(events, core.EventReviewCompleted); n != 1 {
		t.Fatalf("want exactly 1 review, got %d", n)
	}
}

// TestReviewLoopFixesThenApproves: needs_fixes loops to fix and re-review; the
// second verdict approves and the run completes.
func TestReviewLoopFixesThenApproves(t *testing.T) {
	dir := newTestRepo(t)
	e, store := reviewEngine(t, dir, []adapters.FakeVerdict{
		{Status: "needs_fixes", Summary: "missing piece", Findings: []core.Finding{{Severity: "major", Message: "add the piece"}}},
		{Status: "approved", Summary: "fixed"},
	})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (%s)", state.Status, state.BlockedReason)
	}
	events, _ := store.ReadEvents(state.RunID)
	if n := countEvent(events, core.EventReviewCompleted); n != 2 {
		t.Fatalf("want 2 reviews (reject then approve), got %d", n)
	}
	for _, it := range []int{1, 2} {
		if _, err := os.Stat(filepath.Join(store.ReviewDir(state.RunID, "review", it), "verdict.json")); err != nil {
			t.Fatalf("expected verdict for iteration %d: %v", it, err)
		}
	}
}

// TestReviewExhaustsAutoFixBudget: a review that always asks for fixes blocks
// once the auto-fix budget is spent — it never silently gives up or completes.
func TestReviewExhaustsAutoFixBudget(t *testing.T) {
	dir := newTestRepo(t)
	e, _ := reviewEngine(t, dir, []adapters.FakeVerdict{{Status: "needs_fixes", Summary: "still wrong"}})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("want blocked, got %s", state.Status)
	}
	if !strings.Contains(state.BlockedReason, "iteration budget") {
		t.Fatalf("blocked reason should mention the iteration budget, got %q", state.BlockedReason)
	}
}

// TestReviewPerStageBudgetOverridesDefault: budgets.stage.review.maxIterations
// is the single source of truth for the loop and overrides the workflow-wide
// maxAutoIterations default — here a budget of 1 blocks at the first needs_fixes.
func TestReviewPerStageBudgetOverridesDefault(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Workflow.MaxAutoIterations = 5                                              // generous default…
	cfg.Budgets.Stage = map[string]config.StageBudget{"review": {MaxIterations: 1}} // …overridden to 1

	script := adapters.FakeScript{
		Actions: map[string][]adapters.FakeAction{
			"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
		},
		Verdicts: map[string][]adapters.FakeVerdict{"reviewer": {{Status: "needs_fixes", Summary: "nope"}}},
	}
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(script), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "(1 reviews)") {
		t.Fatalf("per-stage budget of 1 must block at the first review, got %s (%s)", state.Status, state.BlockedReason)
	}
	// Exactly one review ran — the override capped the loop, not the default of 5.
	events, _ := store.ReadEvents(state.RunID)
	if n := countEvent(events, core.EventReviewCompleted); n != 1 {
		t.Fatalf("want exactly 1 review under a budget of 1, got %d", n)
	}
}

// TestReviewStageTokenBudgetStopsLoop: a per-stage token budget caps the
// CUMULATIVE token spend of the review loop — a reviewer that keeps asking for
// fixes (and burning tokens) is stopped once the stage's budget is spent, even
// if the iteration budget is generous.
func TestReviewStageTokenBudgetStopsLoop(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Workflow.MaxAutoIterations = 10 // generous — NOT the binding limit
	cfg.Budgets.Stage = map[string]config.StageBudget{"review": {MaxTotalTokens: 200}}

	script := adapters.FakeScript{
		TokensIn:  100, // 150 tokens per review call
		TokensOut: 50,
		Actions: map[string][]adapters.FakeAction{
			"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
		},
		Verdicts: map[string][]adapters.FakeVerdict{"reviewer": {{Status: "needs_fixes", Summary: "again"}}},
	}
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(script), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked || !strings.Contains(state.BlockedReason, "token budget") {
		t.Fatalf("review loop must stop on the stage token budget, got %s (%s)", state.Status, state.BlockedReason)
	}
	if state.Budgets.StageTokensTotal("review") < 200 {
		t.Fatalf("review stage tokens should have accumulated past the budget, got %d", state.Budgets.StageTokensTotal("review"))
	}
}

// TestReviewInvalidVerdictBlocks: a verdict with an unknown status must block the
// run — it must NEVER fall through to approved.
func TestReviewInvalidVerdictBlocks(t *testing.T) {
	dir := newTestRepo(t)
	e, _ := reviewEngine(t, dir, []adapters.FakeVerdict{{Status: "lgtm"}})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("want blocked, got %s (%s)", state.Status, state.BlockedReason)
	}
	if !strings.Contains(state.BlockedReason, "no valid verdict") {
		t.Fatalf("blocked reason should flag the invalid verdict, got %q", state.BlockedReason)
	}
}

// TestReviewBlockedVerdictStopsRun: a "blocked" verdict stops the run for a
// human — it does not loop to the fix stage.
func TestReviewBlockedVerdictStopsRun(t *testing.T) {
	dir := newTestRepo(t)
	e, store := reviewEngine(t, dir, []adapters.FakeVerdict{{Status: "blocked", Summary: "task is unsafe"}})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("want blocked, got %s", state.Status)
	}
	if !strings.Contains(state.BlockedReason, "task is unsafe") {
		t.Fatalf("blocked reason should carry the reviewer's reason, got %q", state.BlockedReason)
	}
	events, _ := store.ReadEvents(state.RunID)
	if hasEvent(events, core.EventStageStarted) && countEvent(events, "stage_started") > 0 {
		// the fix stage must NOT have run
		for _, ev := range events {
			if ev.Stage == "fix" {
				t.Fatal("a blocked verdict must not run the fix stage")
			}
		}
	}
}

// TestReviewShellReviewerApprovesFromStdout proves a REAL adapter works end to
// end: a shell reviewer that prints a JSON verdict to stdout (no structured
// Result.Data, exactly like claude-code/codex/shell) is parsed and the run
// completes. This is the scenario that exposed the Data-only verdict gap.
func TestReviewShellReviewerApprovesFromStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh-style echo quoting")
	}
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: "test -f src/feature.txt"}}
	// The reviewer is a SHELL worker that prints its verdict to stdout — it never
	// fills Result.Data with a status, so the engine must extract it from text.
	cfg.Agents["reviewer"] = config.AgentConfig{
		Provider: "shell",
		Command:  `echo '{"status":"approved","summary":"looks good"}'`,
	}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
			},
		}), nil
	})
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("shell reviewer printing JSON must complete the run, got %s (%s)", state.Status, state.BlockedReason)
	}
	// The verdict extracted from stdout must be persisted as evidence.
	if _, err := os.Stat(filepath.Join(store.ReviewDir(state.RunID, "review", 1), "verdict.json")); err != nil {
		t.Fatalf("expected review verdict.json from a shell reviewer: %v", err)
	}
}

// TestReviewBranchStrictOnMissingVerdict: the branch is decided from the
// persisted verdict, and a verdict that cannot be read must NOT silently route
// to the fix stage — reviewBranch reports ok=false so the caller blocks.
func TestReviewBranchStrictOnMissingVerdict(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	e := New(Options{Store: store, Registry: adapters.NewRegistry(), Config: config.Default(), Repo: repo})
	state := &core.State{RunID: "run-x", Iterations: map[string]int{"review": 1}}
	stage := workflows.Stage{Name: "review", Kind: workflows.KindReview, NextOnApproved: "verify", NextOnNeedsFixes: "fix"}

	// No verdict on disk → must report not-ok, never default to "fix".
	if branch, ok := e.reviewBranch(state, stage); ok {
		t.Fatalf("missing verdict must yield ok=false, got branch=%q ok=true", branch)
	}
	// A persisted approved verdict → the approved branch.
	if err := store.SaveReviewVerdict("run-x", "review", 1, &core.Verdict{Status: core.VerdictApproved}); err != nil {
		t.Fatal(err)
	}
	if branch, ok := e.reviewBranch(state, stage); !ok || branch != "verify" {
		t.Fatalf("approved verdict must branch to verify, got %q ok=%v", branch, ok)
	}
}

// TestReviewDiffOnlyPromptCarriesChanges: by default (diff-only), the reviewer's
// prompt carries the changed files and their content — so it judges the change
// without re-reading the whole repo (the token win).
func TestReviewDiffOnlyPromptCarriesChanges(t *testing.T) {
	dir := newTestRepo(t)
	e, store := reviewEngine(t, dir, []adapters.FakeVerdict{{Status: "approved", Summary: "ok"}})

	state, err := e.Start(context.Background(), "add a feature", "review")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (%s)", state.Status, state.BlockedReason)
	}

	prompt := readStagePrompt(t, store, state.RunID, "review")
	if !strings.Contains(prompt, "Changes to review") {
		t.Fatal("diff-only review prompt should include a 'Changes to review' section")
	}
	if !strings.Contains(prompt, "src/feature.txt") || !strings.Contains(prompt, "feature") {
		t.Fatalf("review prompt should include the changed file and its content, got:\n%s", prompt)
	}
}

// TestReviewContextTruncationIsRecorded: when the change set is truncated/omitted
// for the context budget, the timeline records it — a reviewer judging on
// incomplete context is never a silent fact.
func TestReviewContextTruncationIsRecorded(t *testing.T) {
	dir := newTestRepo(t)
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	big := strings.Repeat("x", 20*1024) // over the per-file review-context budget
	script := adapters.FakeScript{
		Actions: map[string][]adapters.FakeAction{
			"implementer": {{Type: "write_file", Path: "src/big.txt", Content: big}},
		},
		Verdicts: map[string][]adapters.FakeVerdict{"reviewer": {{Status: "approved"}}},
	}
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(script), nil })
	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})

	state, err := e.Start(context.Background(), "task", "review")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadEvents(state.RunID)
	if !hasEvent(events, core.EventReviewContextTruncated) {
		t.Fatalf("review-context truncation must be recorded (status %s)", state.Status)
	}
}

// readStagePrompt returns the prompt.md of the (latest) worker for a stage.
func readStagePrompt(t *testing.T, store *rt.Store, runID, stage string) string {
	t.Helper()
	workers, _ := store.ListWorkers(runID)
	found := ""
	for _, w := range workers {
		ws, err := store.LoadWorkerStatus(runID, w)
		if err != nil || ws.Stage != stage {
			continue
		}
		data, err := os.ReadFile(filepath.Join(store.WorkerDir(runID, w), "prompt.md"))
		if err == nil {
			found = string(data)
		}
	}
	return found
}

func countEvent(events []core.Event, name string) int {
	n := 0
	for _, ev := range events {
		if ev.Event == name {
			n++
		}
	}
	return n
}
