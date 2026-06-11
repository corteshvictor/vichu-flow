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
