package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
)

// TestSDDDriveViaCLI exercises the full host-first SDD flow through the REAL CLI
// commands, exactly as the host pack would: run start → (explore/propose/plan/
// implement) worker start/complete → stage close → review complete → verify. It
// asserts the artifacts, the audited mutation, and a completed run — the §9.1
// acceptance flow end to end, with no engine-internal shortcuts.
func TestSDDDriveViaCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a unix shell gate")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: e2e, language: go}\nworkflow: {default: sdd}\nagents: {default: {provider: fake}}\ncommands: {test: \"test -f feature.go\"}\n")
	store := rt.Open(dir)
	// Host input files (artifacts/verdict) must live OUTSIDE the workspace, or the
	// kernel would attribute them as mutations by the active (read-only) worker.
	host := t.TempDir()

	cli := func(fn func([]string) error, args ...string) {
		t.Helper()
		if err := fn(args); err != nil {
			t.Fatalf("cmd %v: %v", args, err)
		}
	}
	rid, tok := startRunWithToken(t, "--workflow", "sdd", "--op-id", "rs1", "feat")

	// worker runs `worker start` (reading back the assigned id from state),
	// optionally writes a file (the "agent"), then `worker complete`.
	worker := func(stage, role, op, writeFile string, completeArgs ...string) {
		t.Helper()
		cli(cmdWorker, "start", "--run", runID(t, store), "--stage", stage, "--role", role, "--op-id", op, "--driver-token", tok)
		wid := activeWorker(t, store)
		if writeFile != "" {
			mustWrite(t, filepath.Join(dir, writeFile), "package main\n")
		}
		args := append([]string{"complete", "--run", runID(t, store), "--worker", wid, "--op-id", op + "c", "--driver-token", tok}, completeArgs...)
		cli(cmdWorker, args...)
	}

	worker("explore", "explorer", "we", "")
	cli(cmdStage, "close", "--driver-token", tok, "--run", rid, "--stage", "explore", "--op-id", "se")

	mustWrite(t, filepath.Join(host, "prop.md"), "## Proposal\nDo X.")
	worker("propose", "proposer", "wp", "", "--artifact", "proposal="+filepath.Join(host, "prop.md"))
	cli(cmdStage, "close", "--driver-token", tok, "--run", rid, "--stage", "propose", "--op-id", "sp")

	mustWrite(t, filepath.Join(host, "plan.md"), "1. add feature.go\n## Tests\n- feature exists")
	worker("plan", "planner", "wpl", "", "--artifact", "plan="+filepath.Join(host, "plan.md"))
	cli(cmdStage, "close", "--driver-token", tok, "--run", rid, "--stage", "plan", "--op-id", "spl")

	worker("implement", "implementer", "wi", "feature.go", "--result", filepath.Join(host, "prop.md"))
	if !mutationRecorded(store, rid, "feature.go") {
		t.Fatal("implement worker's change must be audited")
	}
	cli(cmdStage, "close", "--driver-token", tok, "--run", rid, "--stage", "implement", "--op-id", "si")

	cli(cmdWorker, "start", "--run", rid, "--stage", "review", "--role", "reviewer", "--op-id", "wr", "--driver-token", tok)
	wid := activeWorker(t, store)
	mustWrite(t, filepath.Join(host, "verdict.json"), `{"status":"approved","summary":"ok"}`)
	cli(cmdReview, "complete", "--driver-token", tok, "--run", rid, "--worker", wid, "--verdict", filepath.Join(host, "verdict.json"), "--op-id", "rc")

	cli(cmdStage, "close", "--driver-token", tok, "--run", rid, "--stage", "verify", "--op-id", "sv")

	state, _ := store.LoadState(rid)
	if state.Status != core.StatusCompleted {
		t.Fatalf("SDD CLI drive should complete, got %s (%s)", state.Status, state.BlockedReason)
	}
	for _, a := range []string{"proposal.md", "plan.md"} {
		if _, err := os.Stat(filepath.Join(store.ArtifactsDir(rid), a)); err != nil {
			t.Errorf("artifact %s missing: %v", a, err)
		}
	}
}

func runID(t *testing.T, store *rt.Store) string {
	t.Helper()
	id, err := store.LatestRun()
	if err != nil || id == "" {
		t.Fatalf("no run: %v", err)
	}
	return id
}

func activeWorker(t *testing.T, store *rt.Store) string {
	t.Helper()
	st, err := store.LoadState(runID(t, store))
	if err != nil || st.ActiveWorker == "" {
		t.Fatalf("no active worker: %v", err)
	}
	return st.ActiveWorker
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
