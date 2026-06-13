# Getting started

This walks you through your first VichuFlow run in about five minutes.

## Prerequisites

- **Git.** VichuFlow requires a git repository — agents writing code without
  version control have no undo. `vichu init` and `vichu run` refuse to run
  outside one.
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

From inside any git repository:

```bash
vichu init
```

This detects your stack (Go, Rust, JavaScript/TypeScript, Python), writes a
`vichu.yaml` with sensible verification commands, and adds `.vichu/` to your
`.gitignore` (runs contain code fragments and prompts and must never be
committed).

Check everything is wired up:

```bash
vichu doctor
```

## 3. Run a workflow

```bash
vichu run "add a greeting function"
```

A fresh project uses the **`fake` adapter** by default, so this works with no
agent CLI installed: it runs the `quick` workflow (explore → implement →
verify), and the runtime executes your configured test/lint/typecheck commands
to gate the result.

You'll see each stage as it runs, then a summary:

```
Run run-20260610-041723-222a
  status:   completed
  stage:    ✓explore ✓implement ✓verify ✓done
  budget:   2 agent call(s), $0.00, 0s, 0 tokens
```

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

Resume guards against **workspace drift**: if the repository changed underneath
the run (a new commit, or an edit the run itself didn't make — including to a
file a worker touched), plain `vichu resume` blocks rather than working on an
unexpected state. If the external change was you fixing the problem by hand,
accept it explicitly:

```bash
vichu resume <run-id>                   # refuses on drift
vichu resume --accept-changes <run-id>  # re-baseline the snapshot and continue
```

The re-baseline is recorded in the run's timeline (`workspace_rebaselined`).

## Next steps

- Wire a real agent: see [configuration.md](configuration.md) and the `agents`
  block in `vichu.yaml`.
- Understand the moving parts: [concepts.md](concepts.md).
