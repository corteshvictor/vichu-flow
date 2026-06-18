package config

import (
	"fmt"
	"strings"
)

// DefaultYAML renders a commented vichu.yaml for `vichu init`, seeded with the
// detected stack. Comments are preserved by writing a template (not marshaling).
func DefaultYAML(d Detected, projectName string) string {
	lang := d.Language
	if lang == "" {
		lang = "auto"
	}
	cmd := func(v string) string {
		if v == "" {
			return "auto"
		}
		return v
	}

	// The test gate is per-OS when a template supplies a distinct Windows command;
	// otherwise it is a single cross-platform string.
	testBlock := fmt.Sprintf("  test: %q", cmd(d.TestCmd))
	if d.TestCmdWindows != "" && d.TestCmdWindows != d.TestCmd {
		testBlock = fmt.Sprintf("  test:\n    unix: %q\n    windows: %q", cmd(d.TestCmd), d.TestCmdWindows)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `# VichuFlow project configuration. Docs: docs/user/configuration.md
project:
  name: %s
  language: %s
  defaultBranch: main

ui:
  language: en          # en | es  (UI language; English default, Spanish first-class)
  agentOutputLanguage: project   # project | en | es
  timezone: local

workflow:
  default: quick           # quick | review | sdd
  provider: ""             # workflow provider label; empty for quick
  maxAutoIterations: 5     # max review iterations for the review auto-fix loop
  reviewContext: diff-only # diff-only | full — reviewer sees just the diff (cheaper) or explores
  requireGates: true       # block (don't "complete") if no verify gates are configured; set false for demo/fake

workspace:
  provider: auto                # auto | git | filesystem — git when the folder is a repo, else snapshot under .vichu/
  isolation: current-worktree   # agents write to the current worktree
  requireCleanTree: warn        # warn | block | allow

observability:
  tui: false            # RESERVED — today 'vichu observe' is text/read-only; rich TUI planned for v0.6
  web: false            # RESERVED — rich web dashboard planned for v0.6
  webPort: 3737         # RESERVED — used once the web dashboard ships

# Which adapter runs each worker role. "fake" runs deterministically with no
# agent CLI, so a fresh project works out of the box. Switch to claude-code or
# codex (each requires its CLI) to run real agents.
agents:
  default:
    provider: fake
  # default:
  #   provider: claude-code   # requires the Claude Code CLI (claude)
  #   model: sonnet
  # reviewer:
  #   provider: codex         # requires the Codex CLI (codex)

# Verification commands VichuFlow runs itself to gate stage transitions. Each
# may be a single string or a {unix, windows} map. "auto" disables the gate.
commands:
%s
  lint: %q
  typecheck: %q

budgets:
  run:
    maxWallClock: 2h
    maxCostUSD: 15
    maxAgentInvocations: 40
    maxInputTokens: 0          # 0 = no limit; summed across all workers in a run
    maxOutputTokens: 0
    maxTotalTokens: 1000000    # conservative backstop against a runaway run
  stage:
    review:
      maxTotalTokens: 250000   # cap the review→fix loop (a real review can be token-heavy)
  context:
    maxContextPackKB: 64
    maxFilesPerPrompt: 30   # reserved; not yet enforced (no per-prompt context paths)
    maxLogExcerptKB: 16

security:
  allowGitMutations: false
  allowNetwork: true            # RESERVED — not yet enforced (no portable network isolation)
  sensitiveMutations: block     # block | warn — worker touches CI/VCS/config/lockfiles
  outOfScopeMutations: warn     # warn | block — worker touches files outside stage scope
  gateMutations: block          # block | warn | allow — a gate changes/deletes an existing tracked or pre-existing untracked file (rolled back on block)
  requireConfirmationFor:
    - git_push
    - destructive_shell
    - package_install

# Extra project conventions to inject into every worker's context pack.
conventions: []
`, projectName, lang, testBlock, cmd(d.LintCmd), cmd(d.TypecheckCmd))
	return b.String()
}
