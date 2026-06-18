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

// TestSDDWorkflowRegistered: the sdd workflow exists and has the spec-driven chain.
func TestSDDWorkflowRegistered(t *testing.T) {
	wf, err := workflows.Get("sdd")
	if err != nil {
		t.Fatalf("sdd workflow must be registered: %v", err)
	}
	want := []string{"explore", "propose", "plan", "implement", "review", "fix", "verify", "done"}
	if len(wf.Stages) != len(want) {
		t.Fatalf("sdd should have %d stages, got %d", len(want), len(wf.Stages))
	}
	for i, name := range want {
		if wf.Stages[i].Name != name {
			t.Fatalf("stage %d: want %q, got %q", i, name, wf.Stages[i].Name)
		}
	}
	// propose and plan must be read-only (they produce documents, not code).
	for _, s := range []string{"propose", "plan"} {
		st, _ := wf.Stage(s)
		if !st.ReadOnly {
			t.Errorf("stage %q must be read-only", s)
		}
	}
}

// TestSDDArtifactDefaultAndExplicit: a propose worker's result becomes the
// `proposal` artifact by default; plan can pass an explicit artifact. Both land
// under artifacts/ via the kernel (single-writer).
func TestSDDArtifactDefaultAndExplicit(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID

	// explore → propose. The propose result becomes the default `proposal` artifact.
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")

	wid, _, err := e.WorkerStart(runID, "propose", "proposer", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "## Proposal\nDo X because Y."}); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(store.ArtifactsDir(runID), "proposal.md")
	if data, err := os.ReadFile(proposal); err != nil || len(data) == 0 {
		t.Fatalf("propose result must become the default proposal artifact: %v", err)
	}
	mustStageClose(t, e, runID, "propose")

	// plan → pass an EXPLICIT plan artifact.
	wid2, _, err := e.WorkerStart(runID, "plan", "planner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkerComplete(runID, wid2, "", WorkerOutcome{Result: "result", Artifacts: map[string]string{"plan": "1. step one\n2. step two\n## Tests\n- test add"}}); err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(store.ArtifactsDir(runID), "plan.md")
	if data, err := os.ReadFile(planFile); err != nil || len(data) == 0 {
		t.Fatalf("explicit plan artifact must be written: %v", err)
	}
	// artifact_saved event recorded.
	events, _ := store.ReadEvents(runID)
	if !hasEvent(events, core.EventArtifactSaved) {
		t.Fatal("an artifact_saved event must be recorded")
	}
}

// TestSDDPlanRequiresTestsSection: the sdd `plan` stage enforces TDD intent —
// closing it blocks unless the plan artifact declares a `## Tests` section.
func TestSDDPlanRequiresTestsSection(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")
	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "## Proposal"}); err != nil {
		t.Fatal(err)
	}
	mustStageClose(t, e, runID, "propose")

	// A plan WITHOUT a Tests section → stage close blocks.
	wid2, _, _ := e.WorkerStart(runID, "plan", "planner", "")
	if _, err := e.WorkerComplete(runID, wid2, "", WorkerOutcome{Result: "1. do the thing"}); err != nil {
		t.Fatal(err)
	}
	reason, err := e.StageClose(runID, "plan", "")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("a plan without a `## Tests` section must block stage close (TDD intent)")
	}
}

// TestSDDProposeBlocksOnMissingProposalHostFirst: host-first, closing `propose`
// without any result or artifact (no proposal.md materialized) must block at
// `stage close` — the proposal is a contract, not just a prompt.
func TestSDDProposeBlocksOnMissingProposalHostFirst(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")

	// propose with an EMPTY result and no artifact → nothing materialized.
	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.ArtifactsDir(runID), "proposal.md")); err == nil {
		t.Fatal("an empty propose result must not materialize a proposal artifact")
	}
	reason, err := e.StageClose(runID, "propose", "")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("closing propose without a proposal artifact must block")
	}
}

// TestSDDProposeBlocksOnEmptyArtifactHostFirst: an explicit but blank proposal
// artifact is no evidence — `stage close` must reject a whitespace-only proposal.
func TestSDDProposeBlocksOnEmptyArtifactHostFirst(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")

	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{
		Result: "x", Artifacts: map[string]string{"proposal": "   \n  "},
	}); err != nil {
		t.Fatal(err)
	}
	// The blank artifact WAS written, but it is no evidence.
	if data, _ := os.ReadFile(filepath.Join(store.ArtifactsDir(runID), "proposal.md")); strings.TrimSpace(string(data)) != "" {
		t.Fatal("precondition: the proposal artifact should be blank")
	}
	reason, err := e.StageClose(runID, "propose", "")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("a whitespace-only proposal must block stage close")
	}
}

// TestSDDProposeCannotProducePlanArtifact: a stage may only produce artifacts it
// owns. `propose` writing a `plan` artifact must be REJECTED — a stage's evidence
// can't be smuggled in from another stage (the exact reported bug).
func TestSDDProposeCannotProducePlanArtifact(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")

	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	// propose tries to slip in a `plan` artifact — must error, write nothing.
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{
		Result: "## Proposal", Artifacts: map[string]string{"plan": "1. step\n## Tests\n- t"},
	}); err == nil {
		t.Fatal("propose must not be allowed to produce a `plan` artifact")
	}
	if _, err := os.Stat(filepath.Join(store.ArtifactsDir(runID), "plan.md")); err == nil {
		t.Fatal("a rejected cross-stage artifact must not be written to disk")
	}
}

// TestSDDPlanRejectsForeignProvenance: even when plan.md exists with a `## Tests`
// section, `stage close --stage plan` must block if its provenance metadata
// attributes it to a different stage — the evidence must be plan's own.
func TestSDDPlanRejectsForeignProvenance(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")
	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "## Proposal\nX"}); err != nil {
		t.Fatal(err)
	}
	mustStageClose(t, e, runID, "propose")

	// plan produces a valid plan (with the Tests section).
	wid2, _, _ := e.WorkerStart(runID, "plan", "planner", "")
	if _, err := e.WorkerComplete(runID, wid2, "", WorkerOutcome{Result: "## Plan\n## Tests\n- t"}); err != nil {
		t.Fatal(err)
	}
	// Tamper the provenance: claim the plan came from `propose`, not `plan`.
	meta, err := store.LoadArtifactMeta(runID, "plan")
	if err != nil {
		t.Fatalf("plan provenance must have been recorded: %v", err)
	}
	meta.Stage = "propose"
	if err := store.SaveArtifactMeta(runID, "plan", meta); err != nil {
		t.Fatal(err)
	}
	reason, err := e.StageClose(runID, "plan", "")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("a plan artifact attributed to another stage must block stage close")
	}
}

// TestSDDPlanRejectsTamperedArtifactContent: the kernel attributes evidence by
// content hash. Editing plan.md on disk AFTER `worker complete` (without updating its
// provenance) must block `stage close --stage plan` — the accepted evidence must be
// exactly what the worker produced.
func TestSDDPlanRejectsTamperedArtifactContent(t *testing.T) {
	e, store, _ := hostFirstEngine(t)
	state, err := e.StartRun("build a thing", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")
	wid, _, _ := e.WorkerStart(runID, "propose", "proposer", "")
	if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "## Proposal\nX"}); err != nil {
		t.Fatal(err)
	}
	mustStageClose(t, e, runID, "propose")

	wid2, _, _ := e.WorkerStart(runID, "plan", "planner", "")
	if _, err := e.WorkerComplete(runID, wid2, "", WorkerOutcome{Result: "## Plan\n## Tests\n- t"}); err != nil {
		t.Fatal(err)
	}
	// Tamper the artifact CONTENT on disk, leaving its provenance metadata intact.
	planFile := filepath.Join(store.ArtifactsDir(runID), "plan.md")
	if err := os.WriteFile(planFile, []byte("## Plan\n## Tests\n- injected after the fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	reason, err := e.StageClose(runID, "plan", "")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("a plan artifact edited after the worker produced it must block stage close")
	}
}

// sddScript builds a fake script that drives a full sdd run: the implementer writes
// the gated file and the reviewer approves. resultText is every worker's result
// (so propose/plan artifacts carry it); byRole overrides it per role (e.g. an empty
// proposer) to exercise the required-artifact contract.
func sddScript(resultText string, byRole map[string]string) adapters.FakeScript {
	return adapters.FakeScript{
		ResultText:       resultText,
		ResultTextByRole: byRole,
		Actions: map[string][]adapters.FakeAction{
			"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
		},
		Verdicts: map[string][]adapters.FakeVerdict{
			"reviewer": {{Status: "approved", Summary: "ok"}},
		},
	}
}

// sddHeadlessEngine wires a headless engine for the sdd workflow with the given
// fake script and a test gate that checks the implementer's file.
func sddHeadlessEngine(t *testing.T, dir string, script adapters.FakeScript) *Engine {
	t.Helper()
	store := rt.Open(dir)
	repo, err := workspace.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"
	check := "test -f src/feature.txt"
	if runtime.GOOS == "windows" {
		check = "cmd /c if exist src\\feature.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: check, Windows: check}}
	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return adapters.NewFake(script), nil })
	return New(Options{Store: store, Registry: reg, Config: cfg, Repo: repo})
}

// TestSDDHeadlessCompletesWithArtifacts: `vichu exec --workflow sdd` (Engine.Start)
// must materialize proposal.md and plan.md via the KERNEL — parity with host-first —
// and complete when the plan declares `## Tests` and the gate passes.
func TestSDDHeadlessCompletesWithArtifacts(t *testing.T) {
	dir := newTestRepo(t)
	e := sddHeadlessEngine(t, dir, sddScript("## Plan\nstep one\n## Tests\n- add returns the sum", nil))
	state, err := e.Start(context.Background(), "build a thing", "sdd")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("sdd headless should complete, got %s (blocked: %s)", state.Status, state.BlockedReason)
	}
	store := rt.Open(dir)
	for _, f := range []string{"proposal.md", "plan.md"} {
		p := filepath.Join(store.ArtifactsDir(state.RunID), f)
		if data, err := os.ReadFile(p); err != nil || len(data) == 0 {
			t.Fatalf("headless sdd must materialize %s via the kernel: %v", f, err)
		}
	}
}

// TestSDDHeadlessBlocksWithoutTestsSection: the plan stage's `## Tests` contract
// must be enforced headless too — a plan without it BLOCKS `vichu exec` at plan,
// never silently advancing to implement.
func TestSDDHeadlessBlocksWithoutTestsSection(t *testing.T) {
	dir := newTestRepo(t)
	e := sddHeadlessEngine(t, dir, sddScript("## Plan\njust do it, no tests declared", nil))
	state, err := e.Start(context.Background(), "build a thing", "sdd")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("a plan without `## Tests` must block headless, got %s", state.Status)
	}
	if !strings.Contains(state.BlockedReason, "Tests") {
		t.Fatalf("the block reason should name the Tests requirement, got %q", state.BlockedReason)
	}
	// It must NOT have advanced to implement — the gated file is never written.
	if _, err := os.Stat(filepath.Join(dir, "src", "feature.txt")); err == nil {
		t.Fatal("the run must block at plan, before the implementer runs")
	}
	// Both artifacts WERE materialized before the gate ran — proposal.md (propose
	// already passed its required-artifact gate) and plan.md (so plan's gate had it).
	store := rt.Open(dir)
	for _, f := range []string{"proposal.md", "plan.md"} {
		if _, err := os.Stat(filepath.Join(store.ArtifactsDir(state.RunID), f)); err != nil {
			t.Fatalf("%s must be materialized before the gate runs: %v", f, err)
		}
	}
}

// TestSDDHeadlessBlocksOnEmptyProposal: the `propose` stage requires a non-empty
// proposal artifact. A proposer that returns nothing must BLOCK at propose —
// headless must never reach plan/implement without a verifiable proposal on disk.
func TestSDDHeadlessBlocksOnEmptyProposal(t *testing.T) {
	dir := newTestRepo(t)
	// proposer returns an EMPTY result; the planner would be valid if reached.
	e := sddHeadlessEngine(t, dir, sddScript("", map[string]string{
		"proposer": "",
		"planner":  "## Plan\nstep\n## Tests\n- t",
	}))
	state, err := e.Start(context.Background(), "build a thing", "sdd")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("an empty proposal must block at propose, got %s", state.Status)
	}
	if !strings.Contains(state.BlockedReason, "proposal") {
		t.Fatalf("the block reason should name the proposal requirement, got %q", state.BlockedReason)
	}
	// It must block at propose — plan never runs, so no plan.md and no gated file.
	store := rt.Open(dir)
	if _, err := os.Stat(filepath.Join(store.ArtifactsDir(state.RunID), "plan.md")); err == nil {
		t.Fatal("the run must block at propose, before the planner runs")
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "feature.txt")); err == nil {
		t.Fatal("the run must block at propose, before the implementer runs")
	}
}

// TestArtifactNameMustBeAllowlisted: a name outside the catalog is rejected — the
// host can never write an arbitrary path or filename.
func TestArtifactNameMustBeAllowlisted(t *testing.T) {
	e, _, _ := hostFirstEngine(t)
	state, err := e.StartRun("x", "sdd", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := state.RunID
	mustWorker(t, e, runID, "explore", "explorer", "", "")
	mustStageClose(t, e, runID, "explore")

	wid, _, err := e.WorkerStart(runID, "propose", "proposer", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/etc/passwd", "evil", "secret.txt"} {
		if _, err := e.WorkerComplete(runID, wid, "", WorkerOutcome{Result: "r", Artifacts: map[string]string{bad: "x"}}); err == nil {
			t.Fatalf("artifact name %q must be rejected", bad)
		}
	}
}
