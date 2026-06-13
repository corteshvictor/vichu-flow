package contextpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/config"
)

func TestBuildIncludesConventions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Always write tests first.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	pack, err := Build(dir, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(pack.Markdown, "Always write tests first.") {
		t.Fatal("context pack should inline CLAUDE.md content")
	}
	if !strings.Contains(pack.Markdown, "Language: go") {
		t.Fatal("context pack should report the detected language")
	}
	if len(pack.Sources) != 1 || pack.Sources[0] != "CLAUDE.md" {
		t.Fatalf("expected CLAUDE.md as a source, got %v", pack.Sources)
	}
}

// TestBuildRejectsConventionEscape: a convention path that points outside the
// repo (absolute, or via "..") must never be inlined — a malicious vichu.yaml
// must not be able to leak local files into the agent prompt.
func TestBuildRejectsConventionEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret OUTSIDE the repo (in the temp parent) and one in its own dir.
	parentSecret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(parentSecret, []byte("TOP SECRET KEY"), 0o644); err != nil {
		t.Fatal(err)
	}
	absSecret := filepath.Join(t.TempDir(), "abs-secret.txt")
	if err := os.WriteFile(absSecret, []byte("TOP SECRET KEY"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, conv := range []string{"../secret.txt", absSecret} {
		cfg := config.Default()
		cfg.Conventions = []string{conv}
		pack, err := Build(root, cfg)
		if err != nil {
			t.Fatalf("Build(%q): %v", conv, err)
		}
		if strings.Contains(pack.Markdown, "TOP SECRET KEY") {
			t.Fatalf("convention %q escaped the repo and leaked secret content", conv)
		}
		for _, s := range pack.Sources {
			if s == conv {
				t.Fatalf("escaping convention %q must not be a source", conv)
			}
		}
	}
}

// TestBuildRejectsSymlinkEscape: a symlink inside the repo that resolves outside
// it must not leak the target's content either.
func TestBuildRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET KEY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "evil.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	cfg := config.Default()
	cfg.Conventions = []string{"evil.md"}
	pack, err := Build(root, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(pack.Markdown, "TOP SECRET KEY") {
		t.Fatal("a symlink escaping the repo must not leak content into the prompt")
	}
}

func TestBuildReferencesOversizeFiles(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("convention line\n", 5000) // well over a tiny budget
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Budgets.Context.MaxContextPackKB = 1 // force the file over budget
	pack, err := Build(dir, cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(pack.Markdown, "convention line\nconvention line") {
		t.Fatal("oversize file should be referenced, not inlined")
	}
	if !strings.Contains(pack.Markdown, "over context budget") {
		t.Fatal("oversize file should carry an over-budget note")
	}
}
