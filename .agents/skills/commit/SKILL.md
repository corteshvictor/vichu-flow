---
name: commit
description: >
  Handles git branches, commits, and pull requests for VichuFlow following
  Conventional Commits v1.0.0. Use this skill whenever the user asks to commit,
  create a commit, stage changes, create a PR, open a pull request, push changes,
  or create a branch — e.g. "/commit", "commit this", "make a PR", "push my
  changes", "stage and commit", "new branch", or any variation of branching,
  committing, or opening PRs.
---

# Branch, Commit & PR Skill (VichuFlow)

Three workflows: creating branches, creating commits, and opening pull requests.
All follow Conventional Commits v1.0.0, matching the linter in
`.github/workflows/pr-title.yml` and the changelog config in
`release-please-config.json`.

## Activation guardrail (mandatory)

Use this skill only when the user explicitly asks for a git mutation in the
current turn (branch, stage, commit, push, or PR). Never infer permission from
workflow progress, "ready" states, or any other contextual signal.

## Hard rules (never break)

- **NEVER add a `Co-Authored-By` trailer or any AI/assistant attribution.**
  Commits are authored solely by the user. No exceptions.
- **Stage specific files by name** — never `git add -A` or `git add .`. They can
  pull in machine-local files (`.agents/settings.local.json`, `.env`,
  credentials) or build artifacts.
- **Write all git artifacts in English** (subject, body, footer, PR title, PR
  body, branch names) even when the chat is in Spanish. This keeps history
  consistent; it does not affect chat replies.

## Conventional Commits

Format: `<type>(<optional scope>): <description>`

| Rule | Value |
|------|-------|
| types | `feat`, `fix`, `perf`, `revert`, `refactor`, `docs`, `style`, `test`, `build`, `ci`, `chore` |
| type/scope case | lower-case |
| subject | required, imperative mood, no trailing period |
| header length | ≤ 100 chars |
| breaking change | `!` after type/scope (`feat(engine)!: ...`) or `BREAKING CHANGE:` footer |

**Scopes** (optional) reference the affected package: `engine`, `runtime`,
`security`, `adapters`, `config`, `workflows`, `cli`. Omit if changes span many.

**Choosing the type:** `feat` new capability · `fix` bug fix · `perf` performance
· `refactor` no behavior change · `style` formatting only · `docs` docs only ·
`test` tests · `build` build/deps · `ci` CI config · `chore` tooling/maintenance
· `revert` revert a prior commit.

Pre-1.0 version impact (via release-please): `feat`→minor, `fix`/`perf`→patch,
breaking→minor, everything else→none.

## Before committing: run the quality gate

From the project root (the directory with `go.mod`):

```bash
gofmt -l .                                                              # must print nothing
go vet ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
```

Or `task check`. Don't commit code that fails the gate; the same checks run in CI.

## Commit workflow

1. **Inspect** (run in parallel): `git status`, `git diff`, `git diff --cached`,
   `git log --oneline -5`.
2. **Group logically**: related changes in one commit; unrelated changes split.
   Tests go with the code they cover. Formatting-only stays separate from logic.
3. **Stage by name**: `git add path/one.go path/two_test.go`.
4. **Write the message** with a HEREDOC (preserves formatting, no escaping):

   ```bash
   git commit -m "$(cat <<'EOF'
   fix(engine): clear active_worker when a run blocks

   Explain the why, not the what. Wrap body lines at ~100 chars.
   EOF
   )"
   ```

   Focus the description on *why*; the diff shows *what*.
5. **Verify**: `git status`. If a pre-commit hook fails, fix and make a NEW
   commit — never `--amend` (the failed commit never happened).

## Branch workflow

Branch from the latest `main`:

```bash
git checkout main && git pull origin main
git checkout -b <type>/<short-description>   # lowercase kebab-case, e.g. feat/codex-adapter
```

## PR workflow

PRs are **squash-merged**, so the **PR title** becomes the commit on `main` and
**must be a valid Conventional Commit** (CI rejects non-conforming titles).

1. Inspect the branch: `git log --oneline main..HEAD`, `git diff main...HEAD --stat`.
2. Push: `git push -u origin <branch-name>`.
3. Draft the body from `.github/pull_request_template.md` (fill every section,
   in English; check the verification boxes you actually ran).
4. Create: `gh pr create` with a HEREDOC body and a Conventional-Commit title.
5. Return the PR URL to the user.

Never merge to `main` yourself — only the user decides when to merge.
