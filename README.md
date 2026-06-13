# VichuFlow

> Observable, verifiable agentic workflow orchestration for real software tasks.

[![CI](https://github.com/corteshvictor/vichu-flow/actions/workflows/ci.yml/badge.svg)](https://github.com/corteshvictor/vichu-flow/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/corteshvictor/vichu-flow.svg)](https://pkg.go.dev/github.com/corteshvictor/vichu-flow)
[![Go Report Card](https://goreportcard.com/badge/github.com/corteshvictor/vichu-flow)](https://goreportcard.com/report/github.com/corteshvictor/vichu-flow)
[![Release](https://img.shields.io/github/v/release/corteshvictor/vichu-flow?sort=semver)](https://github.com/corteshvictor/vichu-flow/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

VichuFlow is an open-source, cross-platform runtime that runs **workflows as
persistent state machines** over any repository. It coordinates existing coding
agents from the outside, and **decides stage transitions from evidence it
verifies itself** — running your tests, lint, and typecheck — never from the
agent's own say-so.

> **Adapter status:** today VichuFlow ships the `claude-code`, `shell`, and
> `fake` adapters. It is *designed* to support Codex CLI, OpenCode, and Gemini
> CLI through the same contract — those land on the roadmap (v0.2+).

## Why VichuFlow?

Coding agents are great at writing code and terrible at proving it works. They
say *"all done ✅"* and move on. Other tools either run agents *inside* the
editor (no external record, no resume after a crash) or fan out parallel agents
with a diff UI (no workflow, no verified gates). VichuFlow is the missing piece:
an **external runtime that doesn't trust the agent**.

- **It can't lie to you.** A stage only advances when VichuFlow runs your tests
  itself and sees them pass. An agent that claims success without a green gate
  is blocked, with the evidence on disk.
- **It survives crashes.** A run is plain files (`state.json` + `events.ndjson`).
  Kill it, reboot, `vichu resume` — it picks up where it stopped.
- **It won't wreck your work.** Git snapshots, per-worker mutation tracking, a
  command policy that blocks `rm -rf`/`git push`/installs before they run, and
  automatic rollback if a check touches your files.
- **It won't burn your budget.** Hard limits on wall-clock, cost, and tokens
  (summed across every agent) stop runaway loops and surprise bills.
- **It's vendor-neutral.** Propose with one agent, implement with another,
  review with a third — or none, using plain shell commands.

VichuFlow coordinates agents; it does not replace them or write code itself.

Three ideas hold it together:

1. **External, observable runtime.** Every run is flat files on disk
   (`state.json` + `events.ndjson`); the CLI, TUI, and web are just views. A run
   survives a crash, resumes, and is fully auditable.
2. **Verified evidence.** VichuFlow runs your test/lint/typecheck commands
   itself, captures exit code and output, and only that verdict authorizes a
   transition. An agent that claims success without passing the gate does not
   advance.
3. **Cross-vendor by design.** Propose with one agent, implement with another,
   review with a third. The adapter contract is the heart of the architecture.

## Status

**v0.1.0 released.** Shipping today:

- `vichu init`, `doctor`, `run`, `status [--watch]`, `resume`, `cancel`, `adapters`, `config`
- Persistent runtime: atomic `state.json`, append-only `events.ndjson`, heartbeat locks with orphan reclaim, cooperative cancel
- `quick` workflow (explore → implement → verify)
- Adapters: **`claude-code`** (headless, streamed events, cost reporting, session resume), `shell`, and `fake` (deterministic, for CI)
- Verified gates, git workspace snapshots with content fingerprints, per-worker mutation tracking, and enforced mutation policy (sensitive files block, read-only stages enforced)
- Git is required (agents writing code without version control have no undo)

The architecture is documented in [Concepts](docs/user/concepts.md) and the
[runtime format](docs/user/runtime-format.md).

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
go install github.com/corteshvictor/vichu-flow/cmd/vichu@latest   # Go 1.26+
# or:  git clone … && cd vichu-flow && go build -o vichu ./cmd/vichu
```

> **VichuFlow itself** needs only `git` at runtime — no Go, no other runtime.
> But the **verification commands you configure** (test/lint/typecheck) run with
> your project's own toolchain: a Python gate needs Python, a Node gate needs
> Node, `cargo test` needs Rust, and so on. VichuFlow runs your commands; it
> doesn't bundle the toolchains they call.

## Quick start

```bash
cd your-git-repo
vichu init                        # detect stack, write vichu.yaml, ignore .vichu/
vichu run "add a hello function"  # run the default workflow
vichu status                      # inspect the latest run
```

By default a fresh project uses the `fake` adapter, so `vichu run` works out of
the box with **no agent CLI installed**. To use a real agent, install the
[Claude Code CLI](https://www.anthropic.com/claude-code) and set the `agents`
block in `vichu.yaml`:

```yaml
agents:
  default:
    provider: claude-code
    model: sonnet
```

Then `vichu run "your task"` runs the agent headless and gates its work against
your tests. Full walkthrough: [Getting started](docs/user/getting-started.md).

> **Cost:** VichuFlow itself is free and open source (MIT) and collects no
> telemetry. The agents it coordinates are billed by their own providers (e.g.
> Anthropic for Claude Code) — VichuFlow's per-run cost and token budgets help
> you cap that spend. The `shell` and `fake` adapters cost nothing.

## Documentation

- [Getting started](docs/user/getting-started.md)
- [Concepts](docs/user/concepts.md)
- [Configuration](docs/user/configuration.md)
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
