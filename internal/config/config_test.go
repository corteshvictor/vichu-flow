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
	c.Agents["reviewer"] = AgentConfig{Provider: "codex"}
	if c.Agent("reviewer").Provider != "codex" {
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
	yaml := DefaultYAML(DefaultOptions{Detected: Detect("."), ProjectName: "demo"})
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

// TestTypoInASecuritySettingFailsClosed: every one of these is read with `== "block"` or
// `!= "warn"`, so an unknown value silently means "do nothing". Write `hostLocalState:
// bloock` and you believe you turned a protection on when you did not — a security setting
// a misspelling can disable is worse than no setting, because it buys false confidence.
func TestTypoInASecuritySettingFailsClosed(t *testing.T) {
	typos := map[string]string{
		"hostLocalState":      "security:\n  hostLocalState: bloock\n",
		"sensitiveMutations":  "security:\n  sensitiveMutations: blok\n",
		"outOfScopeMutations": "security:\n  outOfScopeMutations: Block\n", // case matters
		"gateMutations":       "security:\n  gateMutations: yes\n",
		"workspace.provider":  "workspace:\n  provider: filesytem\n",
	}
	for name, yaml := range typos {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatalf("a misspelled %s must fail to load, not silently disable the check", name)
			}
		})
	}
}

// TestValidValuesLoad: the enums we do accept must keep working, including an omitted
// field (which takes the documented default).
func TestValidValuesLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	body := "security:\n  hostLocalState: block\n  gateMutations: allow\nworkspace:\n  provider: filesystem\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Security.HostLocalState != "block" || c.Security.GateMutations != "allow" {
		t.Fatalf("valid values must survive the round trip: %+v", c.Security)
	}
	// An omitted field gets its default and still validates.
	if c.Security.SensitiveMutations != "block" {
		t.Fatalf("omitted sensitiveMutations must default to block, got %q", c.Security.SensitiveMutations)
	}
}

// TestLoadRejectsUnknownKeys: a typo'd security key must FAIL loudly, not be silently
// dropped (leaving the real setting at its default — a protection you think is on).
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vichu.yaml")
	// `hostLocalStates` (plural) is a typo of `hostLocalState`.
	yaml := "security:\n  hostLocalStates: block\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a typo'd security key must be rejected, not silently ignored")
	}
}

// TestLoadRejectsNegativeBudgets: a negative cap would DISABLE the limit (the engine enforces
// only > 0), so it must be rejected rather than silently turn a budget off.
func TestLoadRejectsNegativeBudgets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vichu.yaml")
	yaml := "budgets:\n  run:\n    maxAgentInvocations: -1\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a negative budget must be rejected — it would disable the cap")
	}
}

// TestLoadAcceptsValidConfig: the guardrails must not reject a legitimate config (0 = no
// limit is still valid).
func TestLoadAcceptsValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vichu.yaml")
	yaml := "security:\n  hostLocalState: block\nbudgets:\n  run:\n    maxTotalTokens: 0\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("a valid config must load: %v", err)
	}
}

// TestLoadRejectsNegativeMaxAutoIterations: the review auto-fix cap is enforced only when
// > 0, so a negative value would disable it — reject it like any other budget.
func TestLoadRejectsNegativeMaxAutoIterations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vichu.yaml")
	if err := os.WriteFile(path, []byte("workflow:\n  maxAutoIterations: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a negative maxAutoIterations must be rejected — it disables the review loop cap")
	}
}

// TestLoadRejectsUnknownOSCommandKey: a typo'd per-OS command key must fail, not be silently
// dropped (leaving the gate empty or misfiring on Windows).
func TestLoadRejectsUnknownOSCommandKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vichu.yaml")
	if err := os.WriteFile(path, []byte("commands:\n  test:\n    unx: \"node --test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("`unx` (typo of unix) must be rejected, not silently ignored")
	}
}

// TestLoadAcceptsValidOSCommandForms: scalar and both mapping shapes still load.
func TestLoadAcceptsValidOSCommandForms(t *testing.T) {
	for _, yaml := range []string{
		"commands:\n  test: \"go test ./...\"\n",
		"commands:\n  test:\n    unix: \"go test ./...\"\n    windows: \"go test ./...\"\n",
		"commands:\n  test:\n    unix: \"go test ./...\"\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "vichu.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("valid command form must load: %v\n%s", err, yaml)
		}
	}
}
