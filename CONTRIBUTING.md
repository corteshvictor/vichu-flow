# Contributing to VichuFlow

Thanks for helping build VichuFlow. This guide covers the workflow, commit
convention, and how releases are cut.

## Development workflow

Work from the project root (the directory with `go.mod`). Before opening a PR,
run the full local gate:

```bash
gofmt -l .                                                          # formatting
go vet ./...                                                        # vet
go test -race ./...                                                 # tests + race detector
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run   # lint
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...               # vulnerabilities
```

Or, with [Task](https://taskfile.dev): `task check`.

The same checks run in CI across Linux, macOS, and Windows.

## Commit convention: Conventional Commits

We use [Conventional Commits v1.0.0](https://www.conventionalcommits.org/en/v1.0.0/).
Format: `<type>(<optional scope>): <description>`. The commit **type** drives
automated versioning and the changelog, so it matters:

| Type | Meaning | Version bump (pre-1.0) |
|---|---|---|
| `feat` | a wholly new feature or capability | minor (`0.1.0` → `0.2.0`) |
| `fix` | a bug fix | patch (`0.1.0` → `0.1.1`) |
| `perf` | performance improvement | patch |
| `revert` | revert a previous commit | patch |
| `refactor` | code restructuring, no behavior change | none |
| `docs` | documentation only | none |
| `style` | formatting/whitespace, no logic change | none |
| `test` | adding or updating tests | none |
| `build` | build system or external dependencies | none |
| `ci` | CI/CD configuration | none |
| `chore` | maintenance (deps, configs, tooling) | none |

A **breaking change** adds `!` after the type/scope (`feat(api)!: remove
endpoint`) or a `BREAKING CHANGE:` footer — pre-1.0 this bumps the minor.

**Scopes** (optional) reference the affected package — e.g. `engine`, `runtime`,
`security`, `adapters`, `config`, `workflows`, `cli`. They group the changelog.

Examples:

```text
feat(adapters): add codex adapter with review gates
fix(engine): clear active_worker when a run blocks
docs: document the gateMutations rollback behavior
style: gofmt the security package
```

**PRs are squash-merged**, so the **PR title** becomes the commit on `main` —
and that title must follow Conventional Commits. CI (`pr-title.yml`) checks it
automatically; a non-conforming title fails the check. The exact same type list
is enforced by the linter and by release-please's changelog config.

## Branch naming

Name branches `<type>/<short-description>`, reusing the commit types above:

```text
feat/codex-adapter      fix/orphan-lock-reclaim      docs/getting-started
```

With squash-merge the branch name doesn't reach `main` (the PR title does), so
this is a convention for clarity, not enforced by CI.

## Releases

Releases are automated and follow [SemVer](https://semver.org). See
[RELEASING.md](RELEASING.md) for the full flow. In short:

1. Conventional commits land on `main`.
2. **release-please** keeps an open "Release PR" that bumps the version and
   updates `CHANGELOG.md` from those commits.
3. Merging the Release PR creates the tag and GitHub Release; in the **same
   workflow run**, the chained **GoReleaser** job attaches the cross-platform
   binaries when `release_created == true`.

You never hand-edit `CHANGELOG.md` or create tags manually.
