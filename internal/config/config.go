// Package config loads and represents vichu.yaml, the per-project configuration
// that parameterizes a run: workflow, agents per role, verification commands,
// workspace isolation, budgets, and security policy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
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
	// ReviewContext controls what a review stage's prompt carries: "diff-only"
	// (default) gives the reviewer the changed files + content built from the
	// run's mutation reports, so it judges the change without re-reading the whole
	// repo (cheaper, fewer tokens); "full" gives only the task and lets the
	// reviewer explore freely.
	ReviewContext string `yaml:"reviewContext"`
	// RequireGates blocks a run whose verify stage wanted gates but none were
	// configured — so a run never reports "completed" having verified nothing. It
	// is a *bool so an OMITTED value (older vichu.yaml from v0.2) defaults to
	// required, while an explicit `requireGates: false` still opts out for
	// demo/fake. Read it through GatesRequired().
	RequireGates *bool `yaml:"requireGates"`
}

// GatesRequired reports whether a run must verify something: true unless the
// config explicitly set requireGates: false. An omitted value (nil) defaults to
// true so projects that predate the option are protected on upgrade.
func (w WorkflowConfig) GatesRequired() bool {
	return w.RequireGates == nil || *w.RequireGates
}

type WorkspaceConfig struct {
	// Provider selects the workspace backend: "git" (require a git repo),
	// "filesystem" (snapshot the tree under .vichu/, no VCS), or "auto" (use git
	// when the folder is a repo, otherwise fall back to filesystem). Default auto.
	Provider         string `yaml:"provider"`
	Isolation        string `yaml:"isolation"`        // current-worktree
	RequireCleanTree string `yaml:"requireCleanTree"` // warn | block | allow
}

// Workspace provider modes.
const (
	WorkspaceAuto       = "auto"
	WorkspaceGit        = "git"
	WorkspaceFilesystem = "filesystem"
)

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
	// Per-stage token caps (0 = no limit). They bound the CUMULATIVE spend of a
	// stage across all its iterations — e.g. budgets.stage.review.maxTotalTokens
	// stops a review→fix loop that keeps burning tokens.
	MaxTotalTokens  int `yaml:"maxTotalTokens"`
	MaxInputTokens  int `yaml:"maxInputTokens"`
	MaxOutputTokens int `yaml:"maxOutputTokens"`
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
	// GateOutputs are the paths a gate is ALLOWED to rewrite — the coverage profile, the
	// log, the report your test command legitimately produces. Globs, matched like a
	// stage's scope. Empty by default: no path is disposable until you say so.
	//
	// It exists because the alternative is guessing, and the obvious guess is wrong. "The
	// file is gitignored, so it must be build output" also describes a private note, a
	// credential, a certificate, a local config, and anything a GLOBAL gitignore excludes
	// that this project never mentioned. A gate that overwrites one of those has destroyed
	// something irreplaceable. So the project declares which paths it is fine to lose.
	//
	// A sensitive path (`.env`, lockfiles, CI config) is never allowed, whatever is listed
	// here. Only pre-existing files need allowlisting — a file the gate CREATES is always
	// fine; it is not overwriting anything.
	GateOutputs []string `yaml:"gateOutputs"`
	// HostLocalState is what happens when the coding host's machine-local permission
	// file changes during a worker (`.claude/settings.local.json`): warn (default) |
	// block.
	//
	// This is a real, named limitation, not an oversight. In host-first mode VichuFlow
	// does NOT launch the agent, so it cannot tell whether that file was written by the
	// HOST (the user clicked "approve", which is normal and constant) or by the AGENT
	// (granting itself tools, which is an escalation). The two are byte-identical on
	// disk.
	//
	//   warn  — record it (mutations.json, flagged host_bookkeeping) and emit a loud
	//           event, but do not block. Default, because blocking would kill a run every
	//           time the user approves any command — the agent touched nothing.
	//   block — block on any change. Correct when you have pre-authorized every command
	//           your agents need, so the file should never move mid-run. This is the
	//           setting to use if an agent escalating its own host permissions is in your
	//           threat model.
	//
	// Either way the escalation cannot fool the KERNEL: it still runs your gates itself
	// and still audits every file. What it buys an attacker is the host's approval
	// prompt, not VichuFlow's verdict.
	HostLocalState string `yaml:"hostLocalState"`
}

// Load reads and decodes vichu.yaml at path, then fills defaults for unset
// values.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes config YAML from bytes, applies defaults, and validates. It is the shared
// core of Load and of loading a run's frozen config.snapshot.yaml through the confined Store
// (which reads the bytes, so it cannot be redirected by a symlink) rather than by path.
func Parse(data []byte) (*Config, error) {
	var c Config
	// KnownFields(true): an UNKNOWN key is a hard error, not silently ignored. A typo in a
	// security or budget key (`hostLocalStates: block`, `maxAgentInvokations: 5`) otherwise
	// leaves the real setting at its default while you believe you set it — a protection you
	// think is on but is not. Every checked-in config and the generated template are within
	// the schema, so this only ever rejects a genuine mistake.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate rejects unknown values for the settings that gate behavior.
//
// Every one of these is checked elsewhere with `== "block"` or `!= "warn"`, which means a
// TYPO FAILS OPEN: write `hostLocalState: bloock` and the option silently does nothing —
// you believe you turned a protection on, and you did not. A security setting that can be
// disabled by a misspelling is worse than no setting, because it buys false confidence.
// So an unknown value is a hard error at load: `vichu doctor`, `run start` and `exec` all
// go through here, and all of them stop before a run exists.
func (c *Config) Validate() error {
	for _, f := range []struct {
		name  string
		value string
		allow []string
	}{
		{"security.sensitiveMutations", c.Security.SensitiveMutations, []string{"block", "warn"}},
		{"security.outOfScopeMutations", c.Security.OutOfScopeMutations, []string{"block", "warn"}},
		{"security.gateMutations", c.Security.GateMutations, []string{"block", "warn", "allow"}},
		{"security.hostLocalState", c.Security.HostLocalState, []string{"block", "warn"}},
		{"workspace.provider", c.Workspace.Provider, []string{"", WorkspaceAuto, WorkspaceGit, WorkspaceFilesystem}},
		{"workspace.requireCleanTree", c.Workspace.RequireCleanTree, []string{"warn", "block", "allow"}},
		{"workspace.isolation", c.Workspace.Isolation, []string{"current-worktree"}},
		{"workflow.reviewContext", c.Workflow.ReviewContext, []string{"diff-only", "full"}},
	} {
		if !slices.Contains(f.allow, f.value) {
			return fmt.Errorf("%s: unknown value %q (expected one of: %s)", f.name, f.value, strings.Join(f.allow, ", "))
		}
	}
	return c.validateBudgets()
}

// validateBudgets rejects budgets that would DISABLE a limit by accident. A limit of 0 means
// "no limit" by design, but a NEGATIVE value is never meaningful — and the engine only
// enforces caps `> 0`, so `maxAgentInvocations: -1` silently turns the cap off. A cost that
// is NaN or infinite compares false against every spend and disables the cost cap the same
// way. Both are rejected so a budget you wrote is a budget that holds.
func (c *Config) validateBudgets() error {
	ints := []struct {
		name string
		v    int
	}{
		{"budgets.run.maxAgentInvocations", c.Budgets.Run.MaxAgentInvocations},
		{"budgets.run.maxInputTokens", c.Budgets.Run.MaxInputTokens},
		{"budgets.run.maxOutputTokens", c.Budgets.Run.MaxOutputTokens},
		{"budgets.run.maxTotalTokens", c.Budgets.Run.MaxTotalTokens},
		{"budgets.context.maxContextPackKB", c.Budgets.Context.MaxContextPackKB},
		{"budgets.context.maxLogExcerptKB", c.Budgets.Context.MaxLogExcerptKB},
		// The review auto-fix cap is enforced only when > 0, so a negative value silently
		// turns off the loop bound — a review that always says needs_fixes runs until some
		// OTHER backstop (tokens, invocations) trips. It belongs with the budgets above.
		{"workflow.maxAutoIterations", c.Workflow.MaxAutoIterations},
	}
	for _, f := range ints {
		if f.v < 0 {
			return fmt.Errorf("%s: %d is negative (0 means no limit; a negative value would disable the cap)", f.name, f.v)
		}
	}
	if c.Budgets.Run.MaxWallClock < 0 {
		return fmt.Errorf("budgets.run.maxWallClock: %s is negative", time.Duration(c.Budgets.Run.MaxWallClock))
	}
	if cost := c.Budgets.Run.MaxCostUSD; math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return fmt.Errorf("budgets.run.maxCostUSD: %v is not a usable limit (must be a finite, non-negative number; 0 means no limit)", cost)
	}
	for name, sb := range c.Budgets.Stage {
		if sb.MaxIterations < 0 || sb.MaxTotalTokens < 0 || sb.MaxInputTokens < 0 || sb.MaxOutputTokens < 0 || sb.MaxWallClock < 0 {
			return fmt.Errorf("budgets.stage.%s: has a negative limit (0 means no limit; a negative value would disable the cap)", name)
		}
	}
	return nil
}

// Exists reports whether a config file exists at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Save writes the config as YAML to path. This is the human-driven `vichu init`/`config`
// path, operating on the repo root before any agent runs. The RUN's frozen snapshot goes
// through the confined Store instead (Store.SaveConfigSnapshot) — see MarshalYAML.
func (c *Config) Save(path string) error {
	data, err := c.MarshalYAML()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// MarshalYAML serializes the config to YAML bytes, so a caller can persist it through a
// confined writer rather than a raw os.WriteFile.
func (c *Config) MarshalYAML() ([]byte, error) { return yaml.Marshal(c) }

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
	defaultStr(&c.Workflow.ReviewContext, "diff-only")
	defaultStr(&c.Workspace.Provider, WorkspaceAuto)
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
	defaultStr(&c.Security.HostLocalState, "warn")
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
