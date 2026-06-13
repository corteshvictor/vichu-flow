// Package config loads and represents vichu.yaml, the per-project configuration
// that parameterizes a run: workflow, agents per role, verification commands,
// workspace isolation, budgets, and security policy.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the project config file at the repository root.
const FileName = "vichu.yaml"

// Config is the full vichu.yaml shape. The v0.1 engine reads a subset; the rest
// is modeled for forward compatibility and round-trips cleanly.
type Config struct {
	Project       ProjectConfig          `yaml:"project"`
	UI            UIConfig               `yaml:"ui"`
	Workflow      WorkflowConfig         `yaml:"workflow"`
	Workspace     WorkspaceConfig        `yaml:"workspace"`
	Observability ObservabilityConfig    `yaml:"observability"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	Commands      map[string]OSCommand   `yaml:"commands"`
	Budgets       BudgetsConfig          `yaml:"budgets"`
	Security      SecurityConfig         `yaml:"security"`
	Conventions   []string               `yaml:"conventions"`
}

type ProjectConfig struct {
	Name          string `yaml:"name"`
	Language      string `yaml:"language"`
	DefaultBranch string `yaml:"defaultBranch"`
}

type UIConfig struct {
	Language            string `yaml:"language"`            // en | es
	AgentOutputLanguage string `yaml:"agentOutputLanguage"` // project | en | es
	Timezone            string `yaml:"timezone"`
}

type WorkflowConfig struct {
	Default           string `yaml:"default"`
	Provider          string `yaml:"provider"`
	MaxAutoIterations int    `yaml:"maxAutoIterations"`
}

type WorkspaceConfig struct {
	Isolation        string `yaml:"isolation"`        // current-worktree
	RequireCleanTree string `yaml:"requireCleanTree"` // warn | block | allow
}

type ObservabilityConfig struct {
	TUI         bool `yaml:"tui"`
	Web         bool `yaml:"web"`
	WebPort     int  `yaml:"webPort"`
	OpenBrowser bool `yaml:"openBrowser"`
}

type AgentConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model,omitempty"`
	Effort   string `yaml:"effort,omitempty"`
	Command  string `yaml:"command,omitempty"` // for the shell provider
	// AllowNonZeroExit lets a shell worker's non-zero exit count as a normal
	// result instead of failing the stage.
	AllowNonZeroExit bool `yaml:"allowNonZeroExit,omitempty"`
}

type BudgetsConfig struct {
	Run     RunBudget              `yaml:"run"`
	Stage   map[string]StageBudget `yaml:"stage,omitempty"`
	Context ContextBudget          `yaml:"context"`
}

type RunBudget struct {
	MaxWallClock        Duration `yaml:"maxWallClock"`
	MaxCostUSD          float64  `yaml:"maxCostUSD"`
	MaxAgentInvocations int      `yaml:"maxAgentInvocations"`
	MaxInputTokens      int      `yaml:"maxInputTokens"`
	MaxOutputTokens     int      `yaml:"maxOutputTokens"`
	MaxTotalTokens      int      `yaml:"maxTotalTokens"`
}

type StageBudget struct {
	MaxIterations int      `yaml:"maxIterations"`
	MaxWallClock  Duration `yaml:"maxWallClock"`
}

type ContextBudget struct {
	MaxContextPackKB int `yaml:"maxContextPackKB"`
	// MaxFilesPerPrompt is RESERVED and not yet enforced: workflows do not attach
	// per-prompt file lists (Invocation.ContextPaths) yet, so there is nothing to
	// limit. It takes effect once per-prompt context paths are wired.
	MaxFilesPerPrompt int `yaml:"maxFilesPerPrompt"`
	MaxLogExcerptKB   int `yaml:"maxLogExcerptKB"`
}

type SecurityConfig struct {
	AllowGitMutations bool `yaml:"allowGitMutations"`
	// AllowNetwork is RESERVED in v0.1: VichuFlow cannot yet portably isolate an
	// adapter's or gate's network access, so this flag is not enforced. It is
	// kept for forward compatibility; do not rely on it as a guarantee.
	AllowNetwork           bool     `yaml:"allowNetwork"`
	RequireConfirmationFor []string `yaml:"requireConfirmationFor"`
	// SensitiveMutations is what happens when a worker touches a sensitive file
	// (VCS internals, CI config, vichu.yaml, lockfiles): block (default) | warn.
	SensitiveMutations string `yaml:"sensitiveMutations"`
	// OutOfScopeMutations is what happens when a worker touches a file outside
	// the stage's declared scope: warn (default) | block.
	OutOfScopeMutations string `yaml:"outOfScopeMutations"`
	// GateMutations governs gates, which are verification commands and should
	// not change the tree: block (default) stops the run when a gate modifies or
	// deletes an existing tracked or pre-existing untracked file (and rolls it
	// back); warn only records it; allow disables the check. This is the
	// backstop for gates that mutate via an interpreter the policy can't
	// introspect (e.g. `python -c '...'`).
	GateMutations string `yaml:"gateMutations"`
}

// Load reads and decodes vichu.yaml at path, then fills defaults for unset
// values.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.applyDefaults()
	return &c, nil
}

// Exists reports whether a config file exists at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Save writes the config as YAML to path.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Default returns a config populated with v0.1 defaults.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	defaultStr(&c.UI.Language, "en")
	defaultStr(&c.UI.AgentOutputLanguage, "project")
	defaultStr(&c.Workflow.Default, "quick")
	defaultInt(&c.Workflow.MaxAutoIterations, 5)
	defaultStr(&c.Workspace.Isolation, "current-worktree")
	defaultStr(&c.Workspace.RequireCleanTree, "warn")
	defaultInt(&c.Observability.WebPort, 3737)
	defaultInt(&c.Budgets.Run.MaxAgentInvocations, 40)
	if c.Budgets.Run.MaxWallClock == 0 {
		c.Budgets.Run.MaxWallClock = Duration(2 * time.Hour)
	}
	defaultInt(&c.Budgets.Context.MaxContextPackKB, 64)
	defaultInt(&c.Budgets.Context.MaxFilesPerPrompt, 30)
	defaultInt(&c.Budgets.Context.MaxLogExcerptKB, 16)
	defaultStr(&c.Security.SensitiveMutations, "block")
	defaultStr(&c.Security.OutOfScopeMutations, "warn")
	defaultStr(&c.Security.GateMutations, "block")
	if c.Security.RequireConfirmationFor == nil {
		c.Security.RequireConfirmationFor = []string{"git_push", "destructive_shell", "package_install"}
	}
	if c.Agents == nil {
		c.Agents = map[string]AgentConfig{}
	}
}

// defaultStr sets *p to def when it is the zero value. defaultInt is the int
// equivalent. They keep applyDefaults a flat, readable list of defaults.
func defaultStr(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

func defaultInt(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

// Agent returns the config for a role, falling back to the "default" role, then
// to the fake adapter so a freshly initialized project runs without agent CLIs.
func (c *Config) Agent(role string) AgentConfig {
	if a, ok := c.Agents[role]; ok && a.Provider != "" {
		return a
	}
	if a, ok := c.Agents["default"]; ok && a.Provider != "" {
		return a
	}
	return AgentConfig{Provider: "fake"}
}

// CommandFor returns the resolved (OS-specific) command for a name, or empty if
// not configured / set to "auto".
func (c *Config) CommandFor(name string) string {
	cmd, ok := c.Commands[name]
	if !ok {
		return ""
	}
	resolved := cmd.Resolve()
	if resolved == "auto" {
		return ""
	}
	return resolved
}

// ErrNotFound indicates a missing config file.
var ErrNotFound = errors.New("vichu.yaml not found — run `vichu init`")

// IsNotFound reports whether err is a missing-config error.
func IsNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotFound)
}

// CurrentOS reports the resolution key used for OS-specific commands.
func CurrentOS() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "unix"
}
