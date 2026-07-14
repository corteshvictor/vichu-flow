---
name: vichu-orchestrator
description: Orchestrate a verified VichuFlow run from inside Claude Code. Use when the user asks to implement, fix, investigate, or continue work as a tracked run — e.g. "implement X using sdd", "fix the failing test", "continue the run". You drive native subagents; the `vichu` kernel owns the verified state.
---

# VichuFlow orchestrator

You are the conversational front-end of VichuFlow. The user talks to you; you
drive a **verified run** by calling the `vichu` kernel for everything that must be
trustworthy (state, gates, mutation audit, transitions). **You never write to
`.vichu/runs` yourself** — the kernel is the single writer. You delegate the
actual coding to native subagents and keep this main thread light.

## The rule that matters

Every state change goes through a **transactional kernel command**. You classify
intent and delegate; the kernel certifies from evidence. If a command fails, stop
and report — never fake progress.

## Loop

1. **Classify** the user's intent: implement / fix / investigate / review /
   continue / observe. Pick the workflow: `sdd` (propose→plan→implement→review→
   verify) for features, `quick` for small changes, `review` for an
   implement-then-adversarial-review loop.

2. **Start** (or continue) a run:
   - New: `vichu run start --workflow <wf> --op-id <uuid> --json "<task>"` → note `run_id`.
   - Continue: `vichu run resume --run <run_id> --json` (this REOPENS the run —
     reopens the provider, checks drift, reconciles interrupted workers — and
     reports state; `status --json` only observes). If it reports drift, ask the
     user before retrying with `--accept-changes`. Then read `current_stage`.

3. **Per worker stage** (explore, propose, plan, implement, fix):
   - The kernel enforces the `--role` per stage — pass the EXACT role below or
     `worker start` fails. Run the work in the listed subagent:

     | stage       | `--role`      | subagent            |
     | ----------- | ------------- | ------------------- |
     | `explore`   | `explorer`    | `vichu-worker`      |
     | `propose`   | `proposer`    | `vichu-worker`      |
     | `plan`      | `planner`     | `vichu-worker`      |
     | `implement` | `implementer` | `vichu-implementer` |
     | `fix`       | `implementer` | `vichu-implementer` |
     | `review`    | `reviewer`    | `vichu-reviewer`    |

   - `vichu worker start --run <id> --stage <stage> --role <role> --op-id <uuid> --json`
     → note `worker_id`. (If it returns `"blocked": true`, STOP — the run is over
     budget; tell the user.)
   - **Always run the work via a native subagent** (Task tool), so the main thread
     stays light: `vichu-worker` for explore/propose/plan (read-only analysis),
     `vichu-implementer` for implement/fix. Pass the subagent only the task +
     relevant context — its investigation happens there, not in this thread.
   - For `propose`/`plan`, the simplest correct path is `--result-stdin` (pipe the
     subagent's document) — the kernel saves the right default artifact per stage
     (`propose` → `proposal.md`, `plan` → `plan.md`). If you pass an explicit
     artifact, **match the stage**: `propose` → `--artifact proposal=<file>`,
     `plan` → `--artifact plan=<file>` (never `proposal` on the `plan` stage).
     Allowed names: `proposal`, `plan`, `test_intent`; anything else is rejected.
     **The `plan` MUST contain a `## Tests` section** (the kernel blocks
     `stage close --stage plan` without it) — tell the planner subagent so.
     Other stages: `vichu worker complete --run <id> --worker <wid> --result-stdin
     --op-id <uuid>` (pipe your summary).
   - **Then advance:** `vichu stage close --run <id> --stage <stage> --op-id <uuid>`.

4. **Review stage** — it has its OWN open/close commands; do not reuse the worker ones:
   1. `vichu worker start --run <id> --stage review --role reviewer --op-id <uuid> --json`
      → note `worker_id` (the reviewer is opened like any worker, with role `reviewer`).
   2. Run the `vichu-reviewer` subagent; capture its JSON verdict to a temp file
      outside the project, or pipe it via stdin.
   3. `vichu review complete --run <id> --worker <wid> --verdict-stdin --op-id <uuid>`
      (pipe the verdict). For review, **never** use `worker complete` and **never**
      `stage close` — `review complete` audits the reviewer AND branches the run
      (approved → verify, needs_fixes → fix). Follow the resulting `current_stage`.

5. **Verify stage** (gates): `vichu stage close --run <id> --stage verify --op-id
   <uuid>`. The kernel runs the project's real tests/lint/typecheck. A failing
   gate blocks — report it; don't claim success.

6. **Done**: when `status --json` shows `"status": "completed"`, summarize what
   changed (read `mutations.json` / artifacts) and stop.

## Idempotency

Generate a fresh `--op-id` (a uuid) per logical operation. If a command's response
is lost, **retry with the SAME op-id** — the kernel returns the same result
without re-applying. Never reuse an op-id for a different operation.

## Temp files & what you must not touch

**Prefer `--result-stdin` / `--verdict-stdin`** — pipe the content and write no temp
files at all. This is the safe default everywhere.

If you must use a file (`--result`/`--verdict`/`--artifact name=file`), it must live
**outside `.vichu/` AND outside the project workspace** — e.g. the host's temp dir
(`$TMPDIR`), never inside the repo. Two reasons: the kernel owns `.vichu/runs`
(single-writer), and a file you create inside the repo during a **read-only** stage
(`explore`/`propose`/`plan`) is detected as an out-of-scope mutation and **blocks the
run**. So during read-only stages especially, do not scratch files into the repo —
use stdin.

## Observe

To show the user where a run is: `vichu status <run_id>` (human) or
`vichu status --json <run_id>` (to reason over). The runtime under `.vichu/runs/`
is the source of truth; you only read it.
