# `.agents/` — agent tooling for contributors (NOT shipped to users)

This directory holds configuration and **skills used by AI coding agents that work
ON this repository** (Claude Code, etc.). It is developer/contributor tooling — it is
**not** part of the VichuFlow product and is **never** installed into an end user's
project.

> Do not confuse this with VichuFlow's **host packs**
> (`internal/hostpacks/packs/...`). Those ARE the product: the `vichu-orchestrator`
> skill + subagents that `vichu init --host claude-code` installs into a *user's*
> agent. The skills here are only for people (and agents) hacking on vichu-flow.

## What's here

| Path | What | Why it's committed |
| --- | --- | --- |
| `skills/commit/` | VichuFlow's own commit/PR helper (Conventional Commits) | First-party; written for this repo |
| `skills/frontend-design/` | Vendored from `anthropics/skills` | Used when we build the planned **web dashboard / docs site** (v0.6) — good visual-design practices |
| `skills/skill-creator/` | Vendored from `anthropics/skills` | Used to **create/validate/optimize** the skills in this repo (incl. the host-pack `vichu-orchestrator`) |
| `settings.json` | Shared agent settings (e.g. attribution off) | Project-wide, machine-independent |
| `settings.local.json`, `scheduled_tasks.lock` | Per-machine state | **gitignored** (see `.gitignore`) |

The repo-root `.claude` is a symlink to `.agents/` so Claude- and AGENTS-aware tools
read the same config.

## Vendored skills are pinned

`frontend-design` and `skill-creator` are third-party (Anthropic, **Apache-2.0** — see
each skill's `LICENSE.txt`). They are pinned in **`skills-lock.json`** at the repo
root: each entry records `source` (`anthropics/skills`), `skillPath`, and a
`computedHash` for integrity/reproducibility — the same idea as a package lockfile.

When committing or updating these skills, commit `skills-lock.json` **together** with
the skill files, so the vendored set stays reproducible and the change is deliberate
(e.g. `chore(agents): vendor <skill> from anthropics/skills (Apache-2.0)`).
