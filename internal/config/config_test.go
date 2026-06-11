package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("project:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UI.Language != "en" {
		t.Errorf("default UI language should be en, got %q", c.UI.Language)
	}
	if c.Workflow.Default != "quick" {
		t.Errorf("default workflow should be quick, got %q", c.Workflow.Default)
	}
	if c.Budgets.Run.MaxWallClock.Std() != 2*time.Hour {
		t.Errorf("default wall clock should be 2h, got %v", c.Budgets.Run.MaxWallClock.Std())
	}
}

func TestOSCommandScalarAndMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	yaml := `
commands:
  test: pytest
  lint:
    unix: "ruff check ."
    windows: "ruff check ."
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.CommandFor("test"); got != "pytest" {
		t.Errorf("scalar command: want pytest, got %q", got)
	}
	if got := c.CommandFor("lint"); got != "ruff check ." {
		t.Errorf("map command: want ruff, got %q", got)
	}
}

func TestAgentFallback(t *testing.T) {
	c := Default()
	if c.Agent("implementer").Provider != "fake" {
		t.Errorf("unconfigured role should fall back to fake")
	}
	c.Agents["default"] = AgentConfig{Provider: "shell"}
	if c.Agent("implementer").Provider != "shell" {
		t.Errorf("should fall back to default role")
	}
	c.Agents["reviewer"] = AgentConfig{Provider: "codex-cli"}
	if c.Agent("reviewer").Provider != "codex-cli" {
		t.Errorf("explicit role should win")
	}
}

func TestCommandForAuto(t *testing.T) {
	c := Default()
	c.Commands = map[string]OSCommand{"test": {Unix: "auto", Windows: "auto"}}
	if c.CommandFor("test") != "" {
		t.Errorf("auto should resolve to empty (gate disabled)")
	}
}

// TestDefaultYAMLSurfacesBudgets makes sure `vichu init` exposes the budget
// knobs (including token limits) so they are discoverable in a new project, and
// that the generated YAML round-trips back through the parser.
func TestDefaultYAMLSurfacesBudgets(t *testing.T) {
	yaml := DefaultYAML(Detect("."), "demo")
	for _, want := range []string{
		"maxTotalTokens", "maxInputTokens", "maxOutputTokens",
		"maxCostUSD", "maxWallClock", "gateMutations",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("vichu init template missing %q", want)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("generated template must parse: %v", err)
	}
}
