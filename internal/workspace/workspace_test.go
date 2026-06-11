package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

func initRepo(t *testing.T) string {
	t.Helper()
	if !GitAvailable() {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "README.md", "hello\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAndDrift(t *testing.T) {
	dir := initRepo(t)
	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	snap, err := repo.Snapshot("")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.BaseSHA == "" {
		t.Fatal("expected non-empty base SHA")
	}
	if snap.Isolation != core.IsolationCurrentWorktree {
		t.Fatalf("want default isolation, got %q", snap.Isolation)
	}

	drift, _, err := repo.Drifted(snap)
	if err != nil || drift {
		t.Fatalf("fresh snapshot should not drift: drift=%v err=%v", drift, err)
	}

	// A new commit moves HEAD → drift.
	writeFile(t, dir, "b.txt", "x\n")
	runGit(t, dir, "add", "b.txt")
	runGit(t, dir, "commit", "-m", "second")
	drift, reason, err := repo.Drifted(snap)
	if err != nil {
		t.Fatalf("Drifted: %v", err)
	}
	if !drift {
		t.Fatal("expected drift after new commit")
	}
	if reason == "" {
		t.Fatal("expected a drift reason")
	}
}

func TestMutationTracking(t *testing.T) {
	dir := initRepo(t)
	repo, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}

	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}
	writeFile(t, dir, "src/new.go", "package main\nfunc main(){}\n")
	writeFile(t, dir, "README.md", "hello\nworld\n") // modify tracked file
	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	byPath := map[string]core.Mutation{}
	for _, m := range muts {
		byPath[m.Path] = m
	}
	if _, ok := byPath["src/new.go"]; !ok {
		t.Fatalf("expected new file tracked, got %v", muts)
	}
	if byPath["src/new.go"].Kind != core.MutationUntracked {
		t.Fatalf("new file should be untracked, got %q", byPath["src/new.go"].Kind)
	}
	readme, ok := byPath["README.md"]
	if !ok || readme.Kind != core.MutationModified {
		t.Fatalf("README.md should be modified, got %+v", readme)
	}
}

func TestMutationTrackingDetectsDeletion(t *testing.T) {
	dir := initRepo(t)
	repo, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}

	// An untracked file that exists before tracking starts.
	writeFile(t, dir, "build/sentinel.txt", "user work\n")
	// And a tracked file we will delete.
	writeFile(t, dir, "tracked.txt", "x\n")
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "tracked")

	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "build", "sentinel.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	muts, err := tracker.Finish()
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]core.Mutation{}
	for _, m := range muts {
		byPath[m.Path] = m
	}
	if m, ok := byPath["build/sentinel.txt"]; !ok || m.Kind != core.MutationDeleted {
		t.Fatalf("untracked deletion must be reported as deleted, got %+v (all: %v)", m, muts)
	}
	if m, ok := byPath["tracked.txt"]; !ok || m.Kind != core.MutationDeleted {
		t.Fatalf("tracked deletion must be reported as deleted, got %+v", m)
	}
}

func TestSensitiveAndScope(t *testing.T) {
	sensitive := []string{".git/config", ".vichu/runs/x", "vichu.yaml", ".github/workflows/ci.yml", "go.sum"}
	for _, p := range sensitive {
		if !IsSensitive(p) {
			t.Errorf("%q should be sensitive", p)
		}
	}
	if IsSensitive("src/main.go") {
		t.Error("src/main.go should not be sensitive")
	}

	if !InScope("anything", nil) {
		t.Error("empty scope should allow any path")
	}
	if !InScope("src/auth/login.go", []string{"src/**"}) {
		t.Error("src/** should match src/auth/login.go")
	}
	if InScope("docs/readme.md", []string{"src/**"}) {
		t.Error("src/** should not match docs/readme.md")
	}
	if !InScope("pkg/util.go", []string{"*.go"}) {
		t.Error("*.go should match by basename")
	}
}
