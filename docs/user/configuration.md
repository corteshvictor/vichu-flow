# Configuration

VichuFlow is configured by `vichu.yaml` at the repository root, created by
`vichu init`. This page documents every block.

## project

```yaml
project:
  name: my-app
  language: go        # auto-detected; informational
  defaultBranch: main
```

## ui

```yaml
ui:
  language: en               # en | es — UI language (English default, Spanish first-class)
  agentOutputLanguage: project  # project | en | es — language asked of agents
  timezone: local
```

`language: es` switches all CLI output (labels, progress, hints) to Spanish;
`VICHU_LANG=es` does the same before a project exists (e.g. for `vichu init`).
Runtime files under `.vichu/` always stay English — they are a machine-readable
contract.

`agentOutputLanguage: project` lets each agent follow the repository's existing
conventions; set `en` or `es` to force a language in worker prompts.

## workflow

```yaml
workflow:
  default: quick      # quick | review — which workflow `vichu run` uses without --workflow
  provider: ""        # optional workflow provider label; recorded on the run
  maxAutoIterations: 5  # review loop: max review iterations before blocking
```

- `default` — `quick` (explore → implement → verify) or `review` (adds an
  adversarial review → auto-fix loop).
- `maxAutoIterations` — for the `review` workflow, the most review iterations the
  auto-fix loop runs before it blocks for a human (counts reviews: N reviews
  allow up to N−1 auto-fixes, and the Nth review can still approve). Override per
  stage with `budgets.stage.review.maxIterations`.

## workspace

```yaml
workspace:
  isolation: current-worktree   # v0.1: runs against the current working tree
  requireCleanTree: warn        # warn | block | allow
```

- `warn` — start even with uncommitted changes, but log a warning (default).
- `block` — refuse to start with a dirty tree.
- `allow` — start silently.

## agents

Maps each worker **role** to the adapter that runs it. A `default` role applies
to any role not listed; if nothing matches, VichuFlow falls back to the `fake`
adapter so a fresh project runs out of the box.

```yaml
agents:
  default:
    provider: fake
  implementer:
    provider: claude-code   # requires the Claude Code CLI (`claude`)
    model: sonnet
  reviewer:
    provider: codex         # requires the Codex CLI (`codex`)
    # or: provider: shell; command: "./scripts/review.sh"
    # allowNonZeroExit: true   # shell only: treat non-zero exit as a normal result
```

The `quick` workflow uses the roles `explorer` and `implementer`. The `review`
workflow adds a `reviewer` (the review stage) and reuses `implementer` for its
`fix` stage. A `reviewer` must return a structured verdict — a JSON object with
a `status` of `approved`, `needs_fixes`, or `blocked` — either as structured
output or as the final JSON object in its message (so a `shell` reviewer can
just print it to stdout). A `shell` worker that exits non-zero **fails the
stage** (the run must not advance on a failed script) unless
`allowNonZeroExit: true`.

### claude-code adapter

Runs workers via the Claude Code CLI in headless mode (`claude -p` with
streamed JSON output), captures cost/token usage, and persists the session id
so a blocked run resumes the same agent session. `vichu doctor` probes the CLI
fully: binary present, version within the supported range (1.x–2.x), and
authentication (`claude auth status`) — an unauthenticated or incompatible CLI
reports unavailable with an actionable reason instead of failing mid-run.
Environment overrides:

- `VICHU_CLAUDE_BIN` — path to the `claude` executable (default `claude`).
- `VICHU_CLAUDE_PERMISSION_MODE` — `--permission-mode` value (default
  `acceptEdits`: the worker can edit files, while tools needing an interactive
  permission prompt are auto-denied so a headless run never hangs).
- `VICHU_CLAUDE_EXTRA_ARGS` — extra CLI args (e.g. `--allowedTools ...`).

### codex adapter

Runs workers via the Codex CLI in non-interactive exec mode with streamed JSON
(`codex exec --json`), captures token usage (Codex does not report a USD cost),
and persists the thread id so a blocked run continues the same agent session.
`vichu doctor` probes the CLI: binary present, version within the supported
range (0.x–1.x), and authentication — `OPENAI_API_KEY`/`CODEX_API_KEY` in the
environment authenticate non-interactively, otherwise `codex login status` is
consulted; an unauthenticated or incompatible CLI reports unavailable with an
actionable reason. Codex's safety boundary is its sandbox (it has no per-tool
deny list), so the runtime's own mutation tracking and policy remain the
verified backstop. Environment overrides:

- `VICHU_CODEX_BIN` — path to the `codex` executable (default `codex`).
- `VICHU_CODEX_SANDBOX` — `--sandbox` value (default `workspace-write`: the
  worker edits files in the work dir but cannot reach the network or paths
  outside it).
- `VICHU_CODEX_EXTRA_ARGS` — extra CLI args appended verbatim (e.g.
  `-c model_reasoning_effort=high`).

## commands

The verification commands VichuFlow runs itself to gate transitions. Each may be
a single string (all platforms) or a `{unix, windows}` map. The value `auto`
disables that gate.

```yaml
commands:
  test: "go test ./..."
  lint:
    unix: "golangci-lint run"
    windows: "golangci-lint run"
  typecheck: auto
```

Commands are tokenized with shell-like quoting: single or double quotes group a
token and preserve the spaces inside it, so `pytest -k "not slow"` and
`sh -c 'a; b'` work. It is **not** a full shell — there is no escape character
(backslashes stay literal, so Windows paths survive), and no variable, glob, or
operator (`&&`, `|`, `>`) expansion. argv[0] is run directly; for shell features
wrap explicitly: `sh -c '...'` (Unix) or `cmd /c '...'` (Windows).

## budgets

Hard limits; exhausting any one blocks the run with a clear reason.

```yaml
budgets:
  run:
    maxWallClock: 2h
    maxCostUSD: 15            # honored when the adapter reports cost
    maxAgentInvocations: 40
    maxInputTokens: 0         # 0 = no limit; honored when the adapter reports usage
    maxOutputTokens: 0
    maxTotalTokens: 0
  stage:                      # optional per-stage limits
    implement:
      maxWallClock: 30m
      maxIterations: 5        # re-entries (resume, review/fix loops)
  context:
    maxContextPackKB: 64      # cap on injected context pack size
    maxFilesPerPrompt: 30     # RESERVED — not yet enforced (no per-prompt context paths)
    maxLogExcerptKB: 16       # gate output handed to agents is truncated to this
```

Enforcement is real, not advisory:

- **Wall-clock** (run and per-stage) becomes a deadline on the stage's
  execution context: a worker or gate still running when the budget expires is
  **killed mid-flight** and the run blocks (`budget_exceeded` in the timeline;
  the interrupted worker is recorded as `canceled`).
- **Cost and tokens** are aggregated across every worker and re-checked before
  each stage, including the terminal one: an over-budget run can never reach
  `completed`. Token totals (`tokens_in_spent` + `tokens_out_spent`) give a
  multi-agent run central accounting — the sum of all workers, not per-call.
- **Iterations** count stage entries (including re-entries via `resume`); the
  budget blocks before the stage re-runs.
- Wall-clock spend **accumulates across resumes** — resuming never resets the
  meter.

## security

```yaml
security:
  allowGitMutations: false
  allowNetwork: true          # RESERVED in v0.1 — see note below; not yet enforced
  sensitiveMutations: block   # block (default) | warn
  outOfScopeMutations: warn   # warn (default) | block
  gateMutations: block        # block (default) | warn | allow
  requireConfirmationFor:
    - git_push
    - destructive_shell
    - package_install
```

Enforcement happens at two moments.

**Before execution** (central policy): every command VichuFlow is about to run —
verification gates and `shell` workers — is classified first. `git push` is
blocked while `allowGitMutations: false`; commands classified as
`destructive_shell` (`rm -rf`, `sudo`, `git reset --hard`, `git clean`, …) or
`package_install` (`npm install`, `pip install`, …) that appear in
`requireConfirmationFor` **block the run before running** (in a headless
runtime, "requires confirmation" means a human must intervene). The same policy
is translated into Claude Code tool-permission rules (`--disallowedTools`),
**generated from the same command table the classifier uses** — so a
`claude-code` worker is denied the same install forms (including global-flag
variants like `pnpm --filter` or `pip --cache-dir`) inside its own session,
while plain dual-use commands (`npm test`, `go test`) are never broadly banned.
Claude's prefix-based rule syntax is coarser than vichu's parser, so vichu's
pre-execution `CheckCommand` (on gates and shell workers) remains the
authoritative layer.

**After every worker** (mutation policy), from the runtime's own diff of the
working tree:

- **Sensitive files** (VCS internals, `.vichu/`, CI configs, `vichu.yaml`,
  lockfiles): touching one **blocks the run** by default.
- **Out-of-scope files** (outside a stage's declared scope globs): warns by
  default; set `block` to stop the run.
- **Read-only stages** (like `explore`): any mutation blocks the run regardless
  of policy — the instruction "do not modify files" is enforced, not just asked.

> **`allowNetwork` is reserved in v0.1.** VichuFlow cannot yet portably isolate
> an adapter's or gate's network access, so this flag is **not enforced** — do
> not rely on it as a guarantee. It is kept for forward compatibility.

Classification sees through shell wrappers (`sh -c '...'` and combined flags
like `sh -ec`, `cmd /c ...`, `pwsh -Command ...`, including nested and compound
`a && b` payloads), parses the global options of `git` and package managers so
`git -C . clean` and `npm --prefix . install` are judged on their real
subcommand, and flags inline-code interpreters (`python -c`, `node -e`,
`ruby -e`, `pwsh -EncodedCommand`, …) since inline code is arbitrary execution.
It is still conservative — it matches well-known dangerous shapes, not every
possible disguise.

For **gates specifically** there is a second, effect-based backstop:
`gateMutations` (default `block`). A gate is a verification command and should
not change the tree, so VichuFlow diffs the working tree around each gate and —
if a gate modifies or deletes an **existing tracked or pre-existing untracked
file** (e.g. via an interpreter the classifier can't read) — blocks the run,
records `gates/<stage>/<n>/mutations.json`, and **rolls back** that file to its
pre-gate content (recreating it if deleted), so real user work is not lost. New
untracked files the gate creates (test caches, coverage) only emit an event and
are left in place; gitignored artifacts never appear. Set `warn` to record
without blocking or rolling back, or `allow` to disable the check entirely
(e.g. for an auto-fixing formatter used as a gate). For workers, mutation
tracking is always on.

## conventions

Extra files to fold into every worker's context pack, on top of the well-known
`CLAUDE.md` / `AGENTS.md` / `.cursorrules` / `CONTRIBUTING.md`:

```yaml
conventions:
  - docs/architecture.md
  - docs/style-guide.md
```

## Privacy

VichuFlow collects **no telemetry** and is local-first: the entire runtime lives
under `.vichu/` in your repository, and nothing leaves your machine except calls
made by the agent adapters you configure (using your own credentials).
