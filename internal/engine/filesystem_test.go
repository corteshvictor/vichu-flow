package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// TestQuickRunOnFilesystemNoGit proves a full quick run works in a folder with
// no VCS: the filesystem provider snapshots the tree, records the implementer's
// verified mutation, and the gate passes — Git is recommended, not required.
func TestQuickRunOnFilesystemNoGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := rt.Open(dir)
	prov, err := workspace.OpenFilesystem(dir)
	if err != nil {
		t.Fatalf("OpenFilesystem: %v", err)
	}

	cfg := config.Default()
	cfg.Workspace.Provider = config.WorkspaceFilesystem
	cfg.Workspace.RequireCleanTree = "allow"
	checkCmd := "test -f src/feature.txt"
	if runtime.GOOS == "windows" {
		checkCmd = "cmd /c if exist src\\feature.txt (exit 0) else (exit 1)"
	}
	cfg.Commands = map[string]config.OSCommand{"test": {Unix: checkCmd, Windows: checkCmd}}

	reg := adapters.NewRegistry()
	reg.Register(adapters.ShellName, func() (adapters.Adapter, error) { return adapters.NewShell(), nil })
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) {
		return adapters.NewFake(adapters.FakeScript{
			ResultText: "did the work",
			Actions: map[string][]adapters.FakeAction{
				"implementer": {{Type: "write_file", Path: "src/feature.txt", Content: "feature\n"}},
			},
		}), nil
	})

	e := New(Options{Store: store, Registry: reg, Config: cfg, Repo: prov})

	state, err := e.Start(context.Background(), "add a feature file", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusCompleted {
		t.Fatalf("want completed, got %s (blocked: %s)", state.Status, state.BlockedReason)
	}

	// The implementer's mutation must be recorded as verified evidence, exactly
	// as it would be under git — proving the filesystem tracker works.
	if !mutationRecorded(store, state.RunID, "src/feature.txt") {
		t.Fatal("expected src/feature.txt recorded as a verified mutation")
	}

	// The persisted snapshot records the filesystem provider and an fs: baseline,
	// so resume reopens the same backend even if the folder later gains a .git.
	ws, err := store.LoadWorkspace(state.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Provider != "filesystem" {
		t.Fatalf("want provider persisted as filesystem, got %q", ws.Provider)
	}
	if !strings.HasPrefix(ws.BaseSHA, "fs:") {
		t.Fatalf("want a filesystem baseline id, got %q", ws.BaseSHA)
	}
}

// TestResumeReopensOriginalProvider: a run started with the filesystem provider
// must keep using it on resume even if the folder gains a .git in the meantime,
// so `auto` flipping to git can't report avoidable base drift.
func TestResumeReopensOriginalProvider(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := rt.Open(dir)
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	// A read-only explore stage that mutates blocks the run — a resumable state.
	fake := adapters.NewFake(adapters.FakeScript{
		ResultText: "explored",
		Actions: map[string][]adapters.FakeAction{
			"explorer": {{Type: "write_file", Path: "sneaky.txt", Content: "x\n"}},
		},
	})
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return fake, nil })

	// Start with the filesystem provider (no git yet).
	fsProv, err := workspace.OpenFilesystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := New(Options{Store: store, Registry: reg, Config: cfg, Repo: fsProv}).
		Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("read-only explore mutation should block, got %s", state.Status)
	}

	// The folder gains a Git repo AFTER the run started.
	if out, gerr := exec.Command("git", "-C", dir, "init").CombinedOutput(); gerr != nil {
		t.Fatalf("git init: %v\n%s", gerr, out)
	}

	// A fresh engine resolves `auto` → git now; resume must reopen filesystem.
	autoProv, err := workspace.Open(dir, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if autoProv.Kind() != workspace.KindGit {
		t.Fatalf("after git init, auto should pick git, got %q", autoProv.Kind())
	}
	resumed, err := New(Options{Store: store, Registry: reg, Config: cfg, Repo: autoProv}).
		Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if strings.Contains(resumed.BlockedReason, "drift") {
		t.Fatalf("resume must not report drift after reopening filesystem, got %q", resumed.BlockedReason)
	}
}

// TestResumeReopensProviderAtProjectRoot: the project lives in a subdirectory
// and a PARENT directory gains the .git. `auto` then resolves git rooted at the
// parent (above the project), so reopening must use the project root where the
// filesystem baseline actually lives — not the git top level.
func TestResumeReopensProviderAtProjectRoot(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not available")
	}
	parent := t.TempDir()
	app := filepath.Join(parent, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := rt.Open(app) // project root is app, where .vichu lives
	cfg := config.Default()
	cfg.Workspace.RequireCleanTree = "allow"

	fake := adapters.NewFake(adapters.FakeScript{
		ResultText: "explored",
		Actions: map[string][]adapters.FakeAction{
			"explorer": {{Type: "write_file", Path: "sneaky.txt", Content: "x\n"}},
		},
	})
	reg := adapters.NewRegistry()
	reg.Register(adapters.FakeName, func() (adapters.Adapter, error) { return fake, nil })

	fsProv, err := workspace.OpenFilesystem(app)
	if err != nil {
		t.Fatal(err)
	}
	state, err := New(Options{Store: store, Registry: reg, Config: cfg, Repo: fsProv}).
		Start(context.Background(), "task", "quick")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state.Status != core.StatusBlocked {
		t.Fatalf("read-only explore mutation should block, got %s", state.Status)
	}

	// The PARENT gains a Git repo after the run started.
	if out, gerr := exec.Command("git", "-C", parent, "init").CombinedOutput(); gerr != nil {
		t.Fatalf("git init: %v\n%s", gerr, out)
	}

	// auto resolves git rooted at the parent (above app) — the bug trigger.
	autoProv, err := workspace.Open(app, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if autoProv.Kind() != workspace.KindGit {
		t.Fatalf("after parent git init, auto should pick git, got %q", autoProv.Kind())
	}
	resumed, err := New(Options{Store: store, Registry: reg, Config: cfg, Repo: autoProv}).
		Resume(context.Background(), state.RunID, ResumeOptions{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if strings.Contains(resumed.BlockedReason, "drift") {
		t.Fatalf("resume must reopen filesystem at the project root, not the git top level; got %q", resumed.BlockedReason)
	}
}
