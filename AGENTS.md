# Agent Instructions

Instructions for AI coding agents working in this repository. `CLAUDE.md` is a
symlink to this file (so both Claude- and AGENTS-aware tools read the same one).

## Project overview

VichuFlow is an open-source, cross-platform **Go** runtime that runs workflows as
persistent state machines over any repository. It coordinates external coding
agents and decides stage transitions from evidence it verifies itself (running
the project's tests/lint/typecheck) — never from the agent's say-so. It ships a
single self-contained binary and is **stack-agnostic** (works on Node, Python,
Rust, Go, …); Go is only the language the binary is built in. Internal design
notes live in `docs/internal/` (kept local, not committed).

## Git

- **NEVER commit, stage, push, create a branch, open a PR, or merge unless the
  user explicitly asks for that exact action in the current turn.** Treat git as
  read-only by default. "Fix this" / "make the change" is permission to EDIT
  FILES only — not to commit, push, or open/merge a PR. After editing, show the
  diff and wait for the user to say what to do. Allowed without asking: read-only
  inspection (`git status`, `git diff`, `git log`).
- **NEVER merge anything to `main`.** Only the user decides what gets integrated,
  and only after they have reviewed it. Never integrate unreviewed changes.
- **NEVER add a `Co-Authored-By` trailer (or any AI/assistant attribution) to
  commit messages.** Commits are authored solely by the user. Hard rule.
- Follow [Conventional Commits v1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):
  `<type>(<optional scope>): <description>`. Types: `feat`, `fix`, `perf`,
  `revert`, `refactor`, `docs`, `style`, `test`, `build`, `ci`, `chore`. Use `!`
  for breaking changes. Scopes: `engine`, `runtime`, `security`, `adapters`,
  `config`, `workflows`, `cli`.
- PRs are squash-merged; the **PR title** becomes the commit on `main` and must
  be a valid Conventional Commit (CI `pr-title.yml` enforces it).
- Branch naming: `<type>/<short-description>` (e.g. `feat/codex-adapter`).

## Build, test, and the quality gate

Run from the project root (the directory with `go.mod`):

```bash
gofmt -l .                                                              # formatting (must be empty)
go vet ./...
go test -race ./...                                                    # tests + race detector (as CI)
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run   # lint
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...                  # vulnerabilities
go build ./cmd/vichu                                                   # build the binary
```

Or `task check` for the whole gate. The same checks run in CI across Linux,
macOS, and Windows. Keep dependencies minimal (one runtime dependency: `yaml.v3`).

## Conventions

- Go 1.26. Match the surrounding code's style, naming, and idioms.
- The security policy is central (`internal/security`): it classifies dangerous
  commands before execution, and the agent tool-permission rules are generated
  from the same table — change the table, not the two outputs separately.
- The runtime persists runs as flat files under `.vichu/` (gitignored).
- Examples in `examples/{go,node,python,rust}` use each stack's **built-in** test
  runner (no package install): `go test`, `node --test`, `python3 -B -m unittest`,
  `cargo test`. For Node, never use npm — use **pnpm** if a package manager is
  ever needed.

## Pointers

- `CONTRIBUTING.md` — workflow and commit convention.
- `SECURITY.md` — supply-chain posture and how to report vulnerabilities.
- `RELEASING.md` — automated release flow (release-please + GoReleaser).
- `docs/user/` — user-facing documentation.
