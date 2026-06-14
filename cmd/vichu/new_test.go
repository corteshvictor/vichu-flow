package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
)

func TestNewScaffoldsRunnableProject(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	if err := cmdNew([]string{"my-app", "--template", "go"}); err != nil {
		t.Fatalf("cmdNew: %v", err)
	}
	root := filepath.Join(base, "my-app")
	for _, f := range []string{"go.mod", "calc.go", "calc_test.go", "vichu.yaml", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CommandFor("test"); got != "go test ./..." {
		t.Fatalf("want go test gate, got %q", got)
	}
	// .gitignore must protect .vichu/ even with no git in the scaffold.
	gi, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if !strings.Contains(string(gi), ".vichu/") {
		t.Fatal(".gitignore must ignore .vichu/")
	}
	// go.mod must carry the slugified project name.
	mod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if !strings.Contains(string(mod), "module my-app") {
		t.Fatalf("go.mod should use the project name, got %q", mod)
	}
}

func TestNewRefusesNonEmptyDir(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	taken := filepath.Join(base, "taken")
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taken, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdNew([]string{"taken", "--template", "node"}); err == nil {
		t.Fatal("cmdNew must refuse a non-empty directory without --force")
	}
}

func TestNewRequiresNameAndKnownTemplate(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := cmdNew([]string{"--template", "go"}); err == nil {
		t.Fatal("cmdNew must require a project name")
	}
	if err := cmdNew([]string{"app", "--template", "bogus"}); err == nil {
		t.Fatal("cmdNew must reject an unknown template")
	}
}

// TestInitTemplateNoPartialWriteOnConflict: a scaffold that hits a conflicting
// file must abort BEFORE writing any file — never leave a half-seeded project.
func TestInitTemplateNoPartialWriteOnConflict(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	if err := os.WriteFile(filepath.Join(base, "calc_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdInit([]string{"--template", "go"}); err == nil {
		t.Fatal("init --template must fail when a seeded file already exists")
	}
	for _, f := range []string{"go.mod", "calc.go", "vichu.yaml"} {
		if _, err := os.Stat(filepath.Join(base, f)); err == nil {
			t.Errorf("partial scaffold: %s was written despite the conflict", f)
		}
	}
}

func TestValidateProjectName(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "../x", `a\b`} {
		if err := validateProjectName(n); err == nil {
			t.Errorf("name %q should be rejected", n)
		}
	}
	for _, n := range []string{"my-app", "svc_v2", "App2"} {
		if err := validateProjectName(n); err != nil {
			t.Errorf("name %q should be valid: %v", n, err)
		}
	}
}

func TestInitWithTemplateSeedsFiles(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	if err := cmdInit([]string{"--template", "python"}); err != nil {
		t.Fatalf("cmdInit --template: %v", err)
	}
	for _, f := range []string{"calc.py", "test_calc.py", "pyproject.toml", "vichu.yaml"} {
		if _, err := os.Stat(filepath.Join(base, f)); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	cfg, _ := config.Load(filepath.Join(base, config.FileName))
	if got := cfg.CommandFor("test"); got != "python3 -B -m unittest" {
		t.Fatalf("want python gate, got %q", got)
	}
}

// TestNewGoProjectRunCompletes is the headline promise: scaffold from nothing,
// then the very first run reaches `completed` against the REAL `go test` gate —
// no config edits, no Git.
func TestNewGoProjectRunCompletes(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	base := t.TempDir()
	t.Chdir(base)
	if err := cmdNew([]string{"svc", "--template", "go"}); err != nil {
		t.Fatalf("cmdNew: %v", err)
	}
	svc := filepath.Join(base, "svc")
	t.Chdir(svc)
	if err := cmdRun([]string{"no-op task"}); err != nil {
		t.Fatalf("cmdRun should complete on the seeded passing gate: %v", err)
	}

	store := runtime.Open(svc)
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
	// And it ran on the filesystem provider (no Git in the scaffold).
	ws, err := store.LoadWorkspace(runID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Provider != "filesystem" {
		t.Fatalf("scaffold without git should use filesystem provider, got %q", ws.Provider)
	}
}
