# Concepts

VichuFlow has a small number of moving parts. This page explains each and how
they fit together.

## Runtime

The **runtime** is the source of truth for a run: a directory of flat files
under `.vichu/runs/<run-id>/`. The CLI, TUI, and web dashboard are all just views
over it. Because everything is on disk and written atomically, a run survives a
crash and can be resumed or audited later. See
[runtime-format.md](runtime-format.md) for the exact files.

The guiding rule: **the runtime does not trust the agent.** Anything that can be
checked by a deterministic process is checked by the runtime, not taken on the
agent's word.

## Workflow and stages

A **workflow** is an ordered set of **stages** the engine executes as a state
machine. The `quick` workflow is the minimal path:

```
explore → implement → verify → done
```

The `review` workflow adds an adversarial review with an auto-fix loop:

```
explore → implement → review ─approved→ verify → done
                         ↑                │
                         └──── fix ←───────┘ needs_fixes
```

Each stage is one of four kinds:

- **worker** — invokes an agent (via an adapter) to do work.
- **review** — invokes an agent like a worker, then requires a structured
  **verdict** (`approved` / `needs_fixes` / `blocked`) and branches on it. A
  review is not pass/fail: a reviewer asking for changes is `needs_fixes`, which
  loops to a fix stage and re-reviews. The loop is bounded by an iteration
  budget (`workflow.maxAutoIterations`, or `budgets.stage.<review>.maxIterations`
  to override); a `blocked` verdict, or a missing/invalid one, stops the run for
  a human — it never silently becomes `approved`.
- **gate** — runs verification commands the runtime executes itself.
- **terminal** — ends the run.

A stage only advances when its evidence is valid. There is no path where an
agent's claim alone moves the run forward.

## Adapters

An **adapter** is the boundary between VichuFlow and a specific coding agent.
Agent CLIs change their flags and output formats constantly; adapters isolate
all of that churn so it never reaches the engine. Every adapter normalizes its
agent's output into a common event stream and result.

VichuFlow ships four adapters (the `codex` adapter is new in v0.2, on `main`):

- **`claude-code`** — runs workers via the Claude Code CLI in headless mode:
  streamed tool-use events land in the run timeline, cost and token usage are
  captured, and the agent session id is persisted so a blocked run resumes the
  same session.
- **`codex`** — runs workers via the Codex CLI in non-interactive exec mode with
  streamed JSON: tool-use events land in the timeline, token usage and the
  thread id are captured (the thread id is the session resumed later). Its
  sandbox (`workspace-write` by default) is its safety boundary.
- **`shell`** — runs a configured command (tokenized with shell-like quoting,
  run directly without a shell) as a worker. Always available.
- **`fake`** — a deterministic adapter used for tests and CI; it runs with no
  network and produces reproducible changes. A fresh project uses it by default.

More agent adapters (OpenCode, Gemini CLI) arrive later. They implement the same
`Adapter` contract.

## Gates

A **gate** runs a verification command — your tests, linter, or typechecker —
and records a verdict: the exact command, its full output (`output.log`), exit
code, and pass/fail. This verdict, **not** any markdown the agent writes, is
what authorizes a stage transition. If the gate fails, the run blocks.

This is the concrete mechanism behind "verified evidence": VichuFlow runs the
command itself and reads the real exit code.

## Workspace safety and mutation tracking

When a run starts, VichuFlow captures a **workspace snapshot**: the current
commit, branch, and the uncommitted files *with content fingerprints* (hashes).
Before and after each worker, it diffs the repository to record exactly which
files the worker changed — and their resulting hashes — in that worker's
`mutations.json`, never trusting the agent's own account.

On **resume**, it compares the live repository's fingerprints to the snapshot
plus the run's own recorded changes. If anything moved underneath the run — a
new commit, a new file, an external edit *even to a file the run itself
touched*, or a vanished change — it blocks with `workspace_drift` instead of
continuing on an unexpected state.

Mutations are also **policed**: touching a sensitive file (CI config, VCS
internals, `vichu.yaml`, lockfiles) blocks the run by default, stages can
declare scopes, and read-only stages (like `explore`) block on any mutation at
all. See the `security` block in [configuration.md](configuration.md).

Git is required for all of this in v0.1.

## Context pack

A generic orchestrator over an unknown repository produces mediocre work — the
quality depends on the agents knowing the project's conventions. The **context
pack** carries that knowledge: detected stack facts plus any `CLAUDE.md`,
`AGENTS.md`, `.cursorrules`, or `CONTRIBUTING.md` in the repo (and files you
declare under `conventions:` in `vichu.yaml`). It is injected into every worker
and copied into the run (`contextpack.md`) for auditability, bounded by the
context-pack size budget.

## Budgets

Every run carries hard budgets — wall-clock, cost, **tokens**, agent
invocations, and context size. When one is exhausted the run blocks with a clear
reason, so there is never an infinite loop or a surprise bill.

Cost and tokens are **aggregated across every worker** in the run, not measured
per call. That central accounting is what a multi-agent run needs to know it is
saving context, not silently burning tokens — set `maxTotalTokens` (or the
input/output limits) in `vichu.yaml` to cap the whole run.

## Events

`events.ndjson` is an append-only timeline of everything that happened: stage
transitions, worker activity, gate results, mutations, drift, budget trips. It's
the audit trail and the data source for `vichu status` and (later) the TUI and
web dashboard.
