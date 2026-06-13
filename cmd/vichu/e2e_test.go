package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// TestCLIInitAndRun exercises the full CLI path — init then run with the fake
// adapter — with no credentials or network, exactly as CI does, across the
// Python and Node stacks the v0.1 exit criterion names (plus a bare repo).
func TestCLIInitAndRun(t *testing.T) {
	cases := []struct {
		name     string
		marker   string // stack marker file
		content  string
		language string // expected detected language in vichu.yaml
	}{
		{"python", "pyproject.toml", "[project]\nname = \"demo\"\n", "python"},
		{"node", "package.json", "{\"name\":\"demo\",\"private\":true}\n", "javascript"},
		{"bare", "seed.txt", "x\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runCLIInitAndRun(t, tc.marker, tc.content, tc.language)
		})
	}
}

func runCLIInitAndRun(t *testing.T, marker, content, language string) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	dir := setupRepoWithMarker(t, marker, content)
	t.Chdir(dir)

	assertInit(t, dir, language)
	configureFakeWorker(t, dir)

	if err := cmdRun([]string{"add a feature file"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	assertRunCompleted(t, dir)
}

// TestCLIRunWithRealGate covers the part of the v0.1 exit criterion that
// TestCLIInitAndRun stops short of: `vichu run` end-to-end with a REAL
// verification gate (not a disabled one) whose verdict actually gates the
// transition, plus a correct mutations.json. It uses `go test`/`go vet`/`go
// build` as the gates because the Go toolchain is present wherever this test
// runs (the whole CI matrix), unlike pytest or node.
func TestCLIRunWithRealGate(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := setupGoModule(t)
	t.Chdir(dir)

	assertInit(t, dir, "go") // init detects Go → real go test/vet/build gates
	writeFakeScript(t, dir)  // fake worker writes feature.txt; gates stay REAL

	if err := cmdRun([]string{"add a feature file"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	// completed ⇒ the real gates ran and PASSED — a failing gate would block.
	assertRunCompleted(t, dir)
	// explicit: the gate actually executed, not silently skipped.
	assertGatePassed(t, dir)
}

// setupGoModule creates a committed git repo with a minimal but real Go module
// (a package with a passing test), so the `go test` gate verifies actual code.
func setupGoModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	files := map[string]string{
		"go.mod":       "module example.com/smoke\n\ngo 1.26\n",
		"calc.go":      "package smoke\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int { return a + b }\n",
		"calc_test.go": "package smoke\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"want 5\")\n\t}\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommit(t, dir)
	return dir
}

// assertGatePassed confirms a verification gate actually ran and passed (not
// that the verify stage found no gates and waved the run through).
func assertGatePassed(t *testing.T, dir string) {
	t.Helper()
	store := runtime.Open(dir)
	runID, _ := store.LatestRun()
	events, err := store.ReadEvents(runID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, ev := range events {
		if ev.Event == core.EventGateCompleted {
			if passed, _ := ev.Detail["passed"].(bool); passed {
				return
			}
		}
	}
	t.Fatal("no passing gate_completed event — the real gate did not run/pass")
}

// TestCLIReviewLoopNeedsFixesThenApproves drives the `review` workflow through
// the REAL CLI path (DefaultRegistry builds a fresh fake per stage from
// VICHU_FAKE_SCRIPT). The reviewer rejects once, then approves on the next
// iteration — proving verdict sequencing is driven by the engine's iteration,
// not a shared adapter instance. This is the end-to-end guard for that gap.
func TestCLIReviewLoopNeedsFixesThenApproves(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	dir := setupRepoWithMarker(t, "seed.txt", "x\n")
	t.Chdir(dir)

	assertInit(t, dir, "")
	// A reviewer that needs_fixes on the first review, then approves on the
	// second; the implementer/fix worker writes the feature file.
	script := `{"result_text":"done",` +
		`"actions":{"implementer":[{"type":"write_file","path":"src/feature.txt","content":"feature\n"}]},` +
		`"verdicts":{"reviewer":[` +
		`{"status":"needs_fixes","summary":"missing piece","findings":[{"severity":"major","message":"add it"}]},` +
		`{"status":"approved","summary":"fixed"}]}}`
	scriptPath := filepath.Join(dir, ".vichu-fake.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICHU_FAKE_SCRIPT", scriptPath)
	disableGates(t, filepath.Join(dir, "vichu.yaml")) // keep cross-platform; verify waves through

	if err := cmdRun([]string{"--workflow", "review", "add a feature"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}

	store := runtime.Open(dir)
	runID, _ := store.LatestRun()
	state, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("review loop must complete after approval, got %s (%s)", state.Status, state.BlockedReason)
	}
	// Exactly two reviews must have happened: the reject and then the approval.
	events, _ := store.ReadEvents(runID)
	reviews := 0
	for _, ev := range events {
		if ev.Event == core.EventReviewCompleted {
			reviews++
		}
	}
	if reviews != 2 {
		t.Fatalf("want 2 reviews (needs_fixes then approved), got %d — verdict sequencing is broken via the CLI", reviews)
	}
}

// setupRepoWithMarker creates a committed git repo containing a stack marker.
func setupRepoWithMarker(t *testing.T, marker, content string) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir)
	return dir
}

// gitInit initializes a git repo with a test identity.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "T"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// assertInit runs `vichu init` and checks it detected the stack and ignored .vichu/.
func assertInit(t *testing.T, dir, language string) {
	t.Helper()
	if err := cmdInit(nil); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "vichu.yaml"))
	if err != nil {
		t.Fatalf("vichu.yaml not created: %v", err)
	}
	if language != "" && !strings.Contains(string(cfgData), "language: "+language) {
		t.Fatalf("stack not detected as %s in vichu.yaml", language)
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(gi), ".vichu/") {
		t.Fatal(".gitignore should ignore .vichu/")
	}
}

// configureFakeWorker wires a deterministic fake worker and disables gates so
// the run is cross-platform in CI.
func configureFakeWorker(t *testing.T, dir string) {
	t.Helper()
	writeFakeScript(t, dir)
	disableGates(t, filepath.Join(dir, "vichu.yaml"))
}

// writeFakeScript wires a deterministic fake worker that writes feature.txt.
func writeFakeScript(t *testing.T, dir string) {
	t.Helper()
	script := `{"result_text":"done","actions":{"implementer":[{"type":"write_file","path":"feature.txt","content":"feature\n"}]}}`
	scriptPath := filepath.Join(dir, ".vichu-fake.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICHU_FAKE_SCRIPT", scriptPath)
}

// assertRunCompleted checks the run finished and its mutation is recorded.
func assertRunCompleted(t *testing.T, dir string) {
	t.Helper()
	store := runtime.Open(dir)
	runID, err := store.LatestRun()
	if err != nil || runID == "" {
		t.Fatalf("no run recorded: %v", err)
	}
	state, err := store.LoadState(runID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (%s)", state.Status, state.BlockedReason)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatalf("agent change not persisted: %v", err)
	}
	if !mutationRecorded(store, runID, "feature.txt") {
		t.Fatal("feature.txt missing from mutations.json")
	}
}

// mutationRecorded reports whether any worker's mutations.json includes path.
func mutationRecorded(store *runtime.Store, runID, path string) bool {
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

func gitCommit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// disableGates rewrites the commands block so no verification command runs,
// keeping this test cross-platform (gate execution is covered elsewhere).
func disableGates(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	skip := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "commands:") {
			out = append(out, "commands:", "  test: auto", "  lint: auto", "  typecheck: auto")
			skip = true
			continue
		}
		if skip {
			if strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
				if strings.TrimSpace(line) == "" {
					skip = false
					out = append(out, line)
				}
				continue
			}
			skip = false
		}
		out = append(out, line)
	}
	// Disabling gates is demo/fake intent — opt out of requireGates so the run
	// completes instead of blocking on "nothing verified".
	content := strings.Replace(strings.Join(out, "\n"), "requireGates: true", "requireGates: false", 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
