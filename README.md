# VichuFlow

> **The runtime that doesn't take your coding agent's word for it** — it verifies every step against your tests, lint, and typecheck, itself.

[![CI](https://github.com/corteshvictor/vichu-flow/actions/workflows/ci.yml/badge.svg)](https://github.com/corteshvictor/vichu-flow/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/corteshvictor/vichu-flow.svg)](https://pkg.go.dev/github.com/corteshvictor/vichu-flow)
[![Go Report Card](https://goreportcard.com/badge/github.com/corteshvictor/vichu-flow)](https://goreportcard.com/report/github.com/corteshvictor/vichu-flow)
[![Release](https://img.shields.io/github/v/release/corteshvictor/vichu-flow?sort=semver)](https://github.com/corteshvictor/vichu-flow/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

VichuFlow installs into the coding agent you already use (Claude Code today) and
turns a natural-language request into a **verified run** — a persistent state
machine over your repository. You talk to your agent; VichuFlow orchestrates,
delegating the coding to native subagents and **deciding every stage transition
from evidence it verifies itself** — running your tests, lint, and typecheck —
never from the agent's own say-so. The `vichu` binary is the kernel/verifier; you
drive it from inside your agent (or headless with `vichu exec` for CI).

> **Adapter status:** VichuFlow ships the `claude-code`, `codex`, `shell`, and
> `fake` adapters. More agents (OpenCode, Gemini CLI) are planned through the
> same contract.

## Why VichuFlow?

Coding agents are great at writing code and terrible at proving it works. They
say *"all done ✅"* and move on. Other tools either run agents *inside* the
editor (no external record, no resume after a crash) or fan out parallel agents
with a diff UI (no workflow, no verified gates). VichuFlow is the missing piece:
an **external runtime that doesn't trust the agent**.

- **It won't take the agent's word for it.** A run only **completes** when VichuFlow runs your tests
  itself and the verify gate passes; intermediate stages advance only on
  kernel-validated evidence — a mutation audit, an artifact's provenance, a
  structured review verdict — never the agent's say-so. An agent that claims success
  without that evidence is blocked, with the proof on disk.
- **It survives crashes.** A run is plain files (`state.json` + `events.ndjson`).
  Kill it, reboot, and resume from where it stopped: `vichu run resume <id>`
  reopens and re-validates the run so your host keeps driving it, or `vichu exec
  resume <id>` continues it headless.
- **It won't wreck your work.** Workspace snapshots (Git or filesystem),
  per-worker mutation tracking, and automatic rollback if a check touches your
  files. When VichuFlow runs commands itself (gates, `shell` workers, the headless
  `claude-code`/`codex` adapters), a command policy blocks `rm -rf`/`git
  push`/installs **before** they run. In host-first native mode the host (Claude
  Code) runs its own subagents, so preventive control belongs to the host; the
  kernel's guarantee is that it **audits what each worker changed and blocks the run
  from advancing** on a violation — disallowed changes can't move the run forward.
  (One run per working tree; see [Known limits](#known-limits).)
- **It won't burn your budget.** Hard limits on agent invocations and wall-clock
  always apply (the kernel measures them itself); cost and token caps apply per dimension
  whenever the agent reports it — though a crash can currently lose a worker's reported usage,
  see [Known limits](#known-limits) — `claude-code` reports both, `codex` reports tokens but not USD cost, `shell`
  reports neither, and a native host reports whatever it exposes. Together they
  stop runaway loops and surprise bills (see the usage matrix in
  [configuration.md](docs/user/configuration.md#budgets)).
- **It's vendor-neutral.** Implement with one agent, review with another — or
  none, using plain shell commands.

VichuFlow coordinates agents; it does not replace them or write code itself.

Three ideas hold it together:

1. **External, observable runtime.** Every run is flat files on disk
   (`state.json` + `events.ndjson`); the CLI is a view today, with a TUI and web
   dashboard planned on the same data. A run survives a crash, resumes, and leaves
   its evidence on disk for anyone to read.
2. **Verified evidence.** VichuFlow runs your test/lint/typecheck commands
   itself, captures exit code and output, and only that verdict authorizes a
   transition. An agent that claims success without passing the gate does not
   advance.
3. **Cross-vendor by design.** Implement with one agent and review with another
   (or just one). The adapter contract is the heart of the architecture.

## Status

The latest release is shown by the **Release badge** above; the version is
tracked by git tags and `CHANGELOG.md`, not hardcoded here. The current build
ships:

- **Host packs** (`vichu init --host claude-code`): install the orchestrator skill + native subagents into your coding agent, then drive verified runs by talking to it. The kernel owns state and gates; the host runs the agents.
- **Host-first kernel commands** the pack drives, in four kinds — they are *not* uniform, and the next host should not assume they are:
  - `worker start`/`complete` · `review complete` · `stage close` — take the run lock, record their evidence, and are retry-safe via `--op-id`.
  - `run start` — creates the run and issues its driver token; `--op-id` makes the *creation* retry-safe (a global reservation, since the run does not exist yet to be locked).
  - `run resume` — a human action, under the run lock, that rotates the driver token; it does **not** take `--op-id`.
  - `status --json` · `observe` — read-only views of a live run: no lock, no `--op-id`, no writes.

  See [Known limits](#known-limits) for where the transactional recovery of the mutating commands is still being hardened.
- `vichu init [--template]`, `new`, `doctor`, `exec` (headless fallback), `status [--watch]`, `cancel`, `adapters`, `config`
- **Project templates** (`vichu new <name> --template go|node|python|rust|empty`, or `vichu init --template`): scaffold a runnable project with a real gate, so the first run completes from scratch — Git optional
- Persistent runtime: atomic `state.json`, append-only `events.ndjson`, heartbeat locks with orphan reclaim, cooperative cancel
- Workflows: **`sdd`** (explore → propose → plan → implement → review → (approved: verify, needs_fixes: fix → review), with allowlisted `proposal`/`plan` artifacts and TDD-intent enforcement), **`review`** (adversarial review → auto-fix loop), and **`quick`**
- Adapters: **`claude-code`** and **`codex`** (headless, streamed events, session resume), `shell`, and `fake` (deterministic, for CI)
- **Workspace providers** — `git` or `filesystem` (`workspace.provider: auto`), so runs work with or without a VCS (see below)
- Verified gates, workspace snapshots with content fingerprints, per-worker mutation tracking, and enforced mutation policy (sensitive files block, read-only stages enforced)

> **Works with or without Git** (v0.3). `workspace.provider: auto | git |
> filesystem` (default `auto`): on a Git repo VichuFlow uses Git as the baseline;
> in any other folder it snapshots the tree under `.vichu/` — so change
> detection, mutation tracking, and rollback work the same way, **no VCS
> required**. Git stays the recommended path for Git repos; it is no longer a
> requirement of the runtime.

The architecture is documented in [Concepts](docs/user/concepts.md) and the
[runtime format](docs/user/runtime-format.md).

## Known limits

VichuFlow is pre-1.0, and the honest version of "what it guarantees" has edges. These are
real, reproducible, and being fixed — we would rather you knew than found out.

- **One run per working tree.** Two runs in the same folder share the same files. On the
  `filesystem` provider, starting a second run re-baselines the tree, and a change the first
  run's worker made can stop looking like a mutation — so it would not be attributed, and
  would not block. **Finish or cancel a run before starting another in the same folder.**
- **One process per run.** The run lock is a heartbeat lease, not an OS lock. A process that
  merely *stalls* for 30s (a suspended laptop, a slow network filesystem, antivirus) can look
  abandoned and have its run reclaimed. Don't drive one run from two processes, and avoid NFS.
- **Crash recovery is still being hardened.** A kernel command that dies mid-flight is designed
  to be safe to retry, and mostly is — but five gaps are known:
  - **A crash can lose a worker's token/cost accounting.** If the process dies between closing
    the worker and saving the run's state, the retry recovers the worker but not the usage it
    reported. A run whose budget is already spent can therefore keep going. **Invocation and
    wall-clock caps are unaffected** (the kernel measures those itself) — the token and cost
    caps are the ones that can under-count.
  - A retry after a failed event write can succeed without re-writing the event.
  - **An `--op-id` whose record failed to write can be reused for a different operation.** The
    record that binds an id to its operation is written last; if that write is the thing that
    fails, the id is free again, and the next command may claim it.
  - `worker start` can be recovered by a *different* `--op-id` than the one that opened it.
  - **A gate's rollback is held in memory, so an abrupt kill during the gate loses it.** When a
    gate misbehaves and changes your tree, the kernel rolls the damage back — but the pre-gate
    backup lives in RAM until the gate returns. A `SIGKILL` or power loss *while the gate is
    running* drops it, and resume cannot undo the damage. This is the one crash gap that can lose
    work; making the gate backup durable before the gate runs is the fix.

  Apart from that last one, none of these lose your code or let an agent skip a gate — they make
  the audit trail, and one of the budget caps, less complete than the guarantees above claim.
- **The audit does not look inside an ignored directory.** An ignored *file* (`.env`,
  `coverage.out`) is captured with its hash and held to the policy. But a path inside an ignored
  **directory** (`node_modules/`, `dist/`, `target/`) is not audited at all — the subtree
  collapses to one entry and is skipped, because walking and hashing it before and after every
  worker would cost far more than it buys. **An agent can hide a file there.** If a directory
  holds something you would want to know about, ignore the files rather than the directory.
- **The `filesystem` provider does not track symlinks.** It walks regular files, so a symlink a
  worker creates is not reported as a mutation. (Rollback is confined either way — it never
  writes *through* a link, in either provider.)
- **The run state under `.vichu/` is not yet forgery-proof.** A run's verdict lives in
  `.vichu/runs/<id>/state.json`, and the coding agent has shell access to the same working tree.
  Nothing an agent *tells* the kernel is trusted — but an agent that writes that file *directly*
  (`echo '{"status":"completed"}' > …`) forges the verdict, and because `.vichu/` is the kernel's
  own runtime it is excluded from the mutation audit, so the write is not flagged. The driver
  token stops an agent from *driving* the run; it does not yet stop this. Signing the state files
  with a key the agent never sees is designed and scheduled, not shipped. **Until then, treat the
  `.vichu/` verdict as trustworthy only against an agent you would already let run your gates —
  not against a deliberately hostile one with shell access.**

**What does hold, and is the reason to use this at all:** a run reaches `completed` only when the
kernel ran your tests/lint/typecheck **itself** and they passed — no agent can *talk* its way past
that by reporting fake results, in any of the situations above. The one caveat is the last bullet:
an agent with shell access can forge the *record* of that verdict by writing `.vichu/` directly,
which the signing work closes.

The full list, with acceptance criteria, is tracked in the project's internal plan.

## Install

VichuFlow is a single self-contained binary — **you do not need Go (or any
runtime) to use it.** It works on any project: Node, Python, Rust, Go, mixed.

**1. Download a prebuilt binary** for your OS/arch (macOS, Linux, Windows) from
the [Releases page](https://github.com/corteshvictor/vichu-flow/releases),
unpack it, and put `vichu` on your `PATH`. That's it.

**2. Package managers** (Homebrew / Scoop / winget) are planned — see the
roadmap.

**3. For Go developers**, you can also install or build from source:

```bash
go install github.com/corteshvictor/vichu-flow/cmd/vichu@latest   # Go 1.26.5+
# or:  git clone … && cd vichu-flow && go build -o vichu ./cmd/vichu
```

> **Already on an older version?** A new binary does not refresh the host pack already
> copied into your project — run `vichu init --host claude-code` and restart your agent.
> `vichu doctor` tells you when a project needs it. See
> [Upgrading](docs/user/upgrading.md).

> **VichuFlow itself** needs no runtime — no Go, no Git. Git is recommended (the
> `git` provider is efficient and ties into your history) but optional: in a
> non-Git folder the `filesystem` provider gives the same undo guarantees. The
> **verification commands you configure** (test/lint/typecheck) do run with your
> project's own toolchain: a Python gate needs Python, a Node gate needs Node,
> `cargo test` needs Rust, and so on. VichuFlow runs your commands; it doesn't
> bundle the toolchains they call.

## Quick start

**Install the host pack, then type `/vichu <your task>` in your agent.** VichuFlow
installs into the coding agent you already use (Claude Code today); you describe the
task in natural language and it orchestrates a verified run — the kernel owns the
state, runs the gates, and decides every transition.

```bash
cd your-project                       # a Git repo, or any folder — Git is optional
vichu init --host claude-code         # install the orchestrator skill + subagents
# then, inside Claude Code, start the request with /vichu:
#   /vichu implement password reset using sdd
#   /vichu fix the failing login test
#   /vichu continue the run
```

`/vichu` is the reliable entry point: it loads the orchestrator explicitly. Whether a
skill auto-activates on plain natural language is the host's call, not ours — so type
the slash command and you always get the verified run instead of an ordinary edit.

The orchestrator drives the run through the kernel's transactional commands —
delegating the coding to native subagents and letting the kernel verify every
stage against your real tests/lint/typecheck. The workflow `sdd`
(explore → propose → plan → implement → review → (approved: verify, needs_fixes:
fix → review)) is spec-driven; `quick` (explore → implement → verify) is for small
changes; `review` adds an adversarial review → auto-fix loop on top of `quick`.
Observe any run with `vichu status <id>` or `vichu observe <id>`.

**Starting from nothing?** Scaffold a runnable project (source + a real gate) so
the first run completes with no manual config:

```bash
vichu new my-app --template go        # or: empty | node | python | rust
cd my-app && vichu init --host claude-code
```

**Headless / CI (fallback).** Without a host pack, run a whole workflow from the
terminal with `vichu exec`:

```bash
vichu exec "add a sum function"       # → completed (gate: go test ./...)
```

`vichu exec` runs the agent headless and gates its work against your tests.
(`vichu run "task"` is a deprecated alias for `vichu exec`.) A fresh project uses
the `fake` adapter, so `exec` works with **no agent CLI installed**; a run reaches
`completed` only when a verification gate passes. Full walkthrough:
[Getting started](docs/user/getting-started.md).

> **Cost:** VichuFlow itself is free and open source (MIT) and collects no
> telemetry. The agents it coordinates are billed by their own providers (e.g.
> Anthropic for Claude Code) — VichuFlow's per-run cost and token budgets help
> you cap that spend. The `shell` and `fake` adapters cost nothing.

## Documentation

- [Getting started](docs/user/getting-started.md)
- [Concepts](docs/user/concepts.md)
- [Configuration](docs/user/configuration.md)
- [Upgrading](docs/user/upgrading.md)
- [Runtime format](docs/user/runtime-format.md)

## Development

Run all commands below from the **project root** (the directory containing
`go.mod`). The `./...` suffix means "this module and every package under it".

These need only the Go toolchain:

```bash
gofmt -l .           # formatting check (no output = clean)
go vet ./...         # suspicious-construct check
go test -race ./...  # test suite with the race detector (as CI does)
go mod verify        # dependency integrity
```

Linting and vulnerability scanning run with **pinned versions via `go run`** —
nothing needs to be installed as a binary or be on your `PATH` (the first run
may download the tool's modules into the module cache):

```bash
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
```

Or run the whole gate at once with [Task](https://taskfile.dev):

```bash
task check   # gofmt + vet + test -race + lint + vuln
```

> Installing the tools as binaries is faster than `go run`. After
> `go install ...@version`, they land in `$(go env GOPATH)/bin` — make sure that
> directory is on your `PATH` (e.g. add `export PATH="$PATH:$(go env GOPATH)/bin"`
> to your shell profile), or call them by full path.

## Contributing

Issues and PRs are welcome. We use [Conventional Commits](https://www.conventionalcommits.org)
(they drive automated versioning and the changelog) — see
[CONTRIBUTING.md](CONTRIBUTING.md). Releases are automated: how that works is in
[RELEASING.md](RELEASING.md), and shipped changes are tracked in
[CHANGELOG.md](CHANGELOG.md).

## License

MIT — see [LICENSE](LICENSE).
