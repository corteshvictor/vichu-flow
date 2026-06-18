# Getting started

This walks you through your first VichuFlow run in about five minutes.

## Prerequisites

- **Git is optional, recommended.** On a Git repo VichuFlow uses Git as the
  workspace baseline; in any other folder the `filesystem` provider snapshots
  the tree under `.vichu/` instead — either way an agent's work is tracked and
  reversible. Pick the backend with `workspace.provider: auto | git | filesystem`
  (default `auto`). See [configuration.md](configuration.md#workspace).
- **The VichuFlow binary needs nothing else** — not Go, not any runtime. It works
  on any project (Node, Python, Rust, Go, mixed).
- **Your project's toolchain**, though, is still needed for the gates: the
  test/lint/typecheck commands in `vichu.yaml` run with your own tools (a Python
  gate needs Python, a Node gate needs Node, etc.). VichuFlow runs those commands
  — it doesn't bundle the toolchains.

## 1. Install

Download the prebuilt binary for your OS/arch from the
[Releases page](https://github.com/corteshvictor/vichu-flow/releases), unpack
it, and put `vichu` on your `PATH`. Verify:

```bash
vichu version
```

> Prefer to build from source? With Go 1.26+:
> `go install github.com/corteshvictor/vichu-flow/cmd/vichu@latest`
> (or clone and `go build -o vichu ./cmd/vichu`). See the
> [README](../../README.md#install) for all install options.

## 2. Initialize a project

From inside any project folder (a Git repo, or any directory — Git is optional):

```bash
vichu init                       # detect the stack, write vichu.yaml, ignore .vichu/
vichu init --host claude-code    # install the host pack into .claude/
```

`vichu init` detects your stack (Go, Rust, JavaScript/TypeScript, Python), writes
a `vichu.yaml` with sensible verification commands, and adds `.vichu/` to your
`.gitignore`. `--host claude-code` installs the **orchestrator skill + native
subagents** so you can drive runs from inside Claude Code. Check everything:

```bash
vichu doctor
```

## 3. Drive a run from your agent (host-first)

Open Claude Code in the repo and talk to it:

```
implement a greeting function using sdd
```

The orchestrator classifies the request, picks a workflow, and drives a **verified
run**: it delegates the coding to native subagents and calls the `vichu` kernel
for everything that must be trustworthy — capturing what each worker changed,
running your real gates, and deciding each transition from that evidence. You stay
in your agent; the kernel owns the state under `.vichu/runs/`.

### Headless fallback

Without a host pack (CI, automation), run a whole workflow from the terminal:

```bash
vichu exec "add a greeting function"
```

A fresh project uses the **`fake` adapter** by default, so this works with no
agent CLI installed: it runs the `quick` workflow (explore → implement →
verify), and the runtime executes your configured test/lint/typecheck commands
to gate the result. (`vichu run "task"` is a deprecated alias for `vichu exec`.)

You'll see each stage as it runs, then a summary:

```
Run run-20260610-041723-222a
  status:   completed
  stage:    ✓explore ✓implement ✓verify ✓done
  budget:   2 agent call(s), $0.00, 0s, 0 tokens
```

> **Empty folder?** A run reaches `completed` only when a verification gate
> passes. With no detectable stack, `vichu init` configures no gates, so a run
> honestly **blocks at `verify`** rather than claim success without verification —
> that is by design, not a failure. To start from nothing with a gate already
> wired up, scaffold from a template:
>
> ```bash
> vichu new my-app --template go     # empty | go | node | python | rust
> cd my-app && vichu exec "add a sum function"   # → completed
> # or, in the current folder:  vichu init --template python
> ```
>
> Each template seeds minimal source plus a real gate using the stack's built-in
> test runner (no package install needed), so the first run completes.

## 4. Inspect the run

```bash
vichu status                 # the latest run
vichu status <run-id>        # a specific run
vichu status --watch         # follow until it completes, blocks, or pauses
```

Everything is on disk under `.vichu/runs/<run-id>/` — `state.json` is the source
of truth, `events.ndjson` is the full timeline, `gates/` holds verified
evidence, and each worker's `mutations.json` records exactly which files it
changed. See [runtime-format.md](runtime-format.md).

## 5. When a run blocks

If a verification gate fails, the run stops in `blocked` state with the reason,
a pointer to the gate's full `output.log`, and a bounded `excerpt.txt` next to
it with the tail of the failure.

There are two ways to resume, and they do different things:

- **Host-first — `vichu run resume --run <id>`**: reopens and re-validates the run
  (reopens the provider, checks drift, reconciles interrupted workers) and reports
  the current state. It does **not** execute any stage — your host/skill continues
  the run with `worker start` / `worker complete` / `stage close`.
- **Headless fallback — `vichu exec resume <id>`**: reopens the run **and** runs the
  workflow loop to completion itself (the kernel drives the agents). For CI and
  automation.

Both guard against **workspace drift**: if the workspace changed underneath the run
(the baseline moved, or an edit the run itself didn't make — including to a file a
worker touched), resume blocks rather than working on an unexpected state. If the
external change was you fixing the problem by hand, accept it explicitly:

```bash
vichu run resume --run <run-id>                   # refuses on drift; reports state
vichu run resume --run <run-id> --accept-changes  # re-baseline and report state;
                                                  #   the host continues the run
vichu exec resume <run-id> --accept-changes       # re-baseline and continue headless
```

The re-baseline is recorded in the run's timeline (`workspace_rebaselined`).

## Next steps

- Wire a real agent: see [configuration.md](configuration.md) and the `agents`
  block in `vichu.yaml`.
- Understand the moving parts: [concepts.md](concepts.md).
