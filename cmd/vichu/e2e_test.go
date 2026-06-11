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

// setupRepoWithMarker creates a committed git repo containing a stack marker.
func setupRepoWithMarker(t *testing.T, marker, content string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "T"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir)
	return dir
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
	script := `{"result_text":"done","actions":{"implementer":[{"type":"write_file","path":"feature.txt","content":"feature\n"}]}}`
	scriptPath := filepath.Join(dir, ".vichu-fake.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICHU_FAKE_SCRIPT", scriptPath)
	disableGates(t, filepath.Join(dir, "vichu.yaml"))
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
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}
