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
   - New: `vichu run start --workflow <wf> --op-id <literal-id> --json "<task>"` → note
     **`run_id` AND `driver_token`**. The host will ask the user to approve this one — it
     is not pre-authorized, on purpose. Starting a run is an act of human intent, and the
     approval costs them one click per task, not per command. (It also stops a subagent
     from opening its own run to escape the audit of the one it is in.)
   - **Immediately stash the token in a file, off the command line.** Use your
     file-writing tool (NOT `echo`/`printf` — those put the secret in argv, where `ps`
     can read it) to write the `driver_token` to a temp file outside the project, e.g.
     `/tmp/vichu-<run_id>.token`. Every mutating command below reads it with
     `--driver-token-stdin < /tmp/vichu-<run_id>.token`. Never paste the token into a
     command string, and never give the file or the token to a subagent.
   - Continue: `vichu run resume --run <run_id> --json` — **this is a HUMAN action, not
     yours.** It reopens the run, and on a BLOCKED run it clears the block. The pack does
     not pre-authorize it, so it will stop and ask the user; that prompt is the point. It
     also returns a **new** `driver_token` — use that one from then on.

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

   - `vichu worker start --run <id> --stage <stage> --role <role> --op-id <literal-id>
     --driver-token-stdin --json < <token-file>` → note `worker_id`. (If it returns
     `"blocked": true`, STOP — the run is over budget; tell the user.)
   - **Always run the work via a native subagent** (Task tool), so the main thread
     stays light: `vichu-worker` for explore/propose/plan (read-only analysis),
     `vichu-implementer` for implement/fix. Pass the subagent only the task +
     relevant context — its investigation happens there, not in this thread.
   - For `propose`/`plan`, write the subagent's document to a file and pass
     `--result <file>` — the kernel saves the right default artifact per stage
     (`propose` → `proposal.md`, `plan` → `plan.md`). Use a **file, not `--result-stdin`**:
     stdin now carries the driver token. If you pass an explicit
     artifact, **match the stage**: `propose` → `--artifact proposal=<file>`,
     `plan` → `--artifact plan=<file>` (never `proposal` on the `plan` stage).
     Allowed names: `proposal`, `plan`, `test_intent`; anything else is rejected.
     **The `plan` MUST contain a `## Tests` section** (the kernel blocks
     `stage close --stage plan` without it) — tell the planner subagent so.
     Other stages: `vichu worker complete --run <id> --worker <wid> --result <file>
     --op-id <literal-id> --driver-token-stdin < <token-file>` (the token is on stdin, so
     the result must be a `--result <file>`, not `--result-stdin`).
   - **Then advance:** `vichu stage close --run <id> --stage <stage> --op-id <literal-id>
     --driver-token-stdin < <token-file>`.

4. **Review stage** — it has its OWN open/close commands; do not reuse the worker ones:
   1. `vichu worker start --run <id> --stage review --role reviewer --op-id <literal-id>
      --driver-token-stdin --json < <token-file>` → note `worker_id` (the reviewer is
      opened like any worker, with role `reviewer`).
   2. Run the `vichu-reviewer` subagent. It ends its reply with a verdict object —
      copy that object **verbatim** into a temp file outside the project. Do not
      re-encode it, rename its keys, or wrap it in one of your own. The kernel
      requires exactly this shape, and the key is `status` (NOT `verdict`):

      ```json
      {"status": "approved", "summary": "<one line>", "findings": []}
      ```

      `status` is one of `approved` | `needs_fixes` | `blocked`. Anything else and
      the kernel rejects the call — it will not transition on evidence it cannot read.
   3. `vichu review complete --run <id> --worker <wid> --verdict <file> --op-id <literal-id>
      --driver-token-stdin < <token-file>`.
      For review, **never** use `worker complete` and **never** `stage close` —
      `review complete` audits the reviewer AND branches the run (approved → verify,
      needs_fixes → fix). Follow the resulting `current_stage`.
   - If the kernel rejects the verdict, the reviewer worker **stays open**: fix the
     JSON and re-issue `review complete` for the SAME `worker_id` with a fresh
     op-id. Do not open a second reviewer, and do not `run resume` — nothing is
     blocked.

5. **Verify stage** (gates): `vichu stage close --run <id> --stage verify --op-id
   <literal-id> --driver-token-stdin < <token-file>`. The kernel runs the project's real
   tests/lint/typecheck. A failing gate blocks — report it; don't claim success.

6. **Done**: when `status --json` shows `"status": "completed"`, summarize what
   changed (read `mutations.json` / artifacts) and stop. **Then delete the token file**
   (`rm -f /tmp/vichu-<run_id>.token`) so it does not outlive the run.

## The driver token — you hold it, nobody else

`run start` gives you a `driver_token`. **Every command that CHANGES the run needs it** —
`worker start`, `worker complete`, `review complete`, `stage close`.

**Keep the secret off every command line.** Write the token ONCE to a temp file with your
file-writing tool — not `echo`/`printf`/`export`, which place it in argv where `ps` (and a
subagent's shell) can read it — then feed it to each command from that file:

```bash
vichu stage close --run <id> --stage verify --op-id <id> --driver-token-stdin < /tmp/vichu-<run_id>.token
```

`--driver-token-stdin` reads the token from stdin, so it never appears in the process's argv.
For `worker complete`, stdin is the token, so pass the result as a **file**
(`--result /tmp/vichu-explore.md`), not `--result-stdin` — the two cannot share stdin.
(`--driver-token <token>` still works but warns, because it leaks via `ps`; `status` and
`observe` need no token.)

> **What the token does and does NOT do.** It stops a subagent from *casually* driving the
> run: you never put it in a subagent's prompt, so a subagent that follows its instructions
> simply does not have it. It is **not** a wall against a subagent that goes looking — a
> file, an env var, and this orchestrator's own memory are all readable by any process
> running as your user, so a determined or compromised subagent can find the token no matter
> where you keep it. Real isolation there is the *host's* job (a sandbox the subagent cannot
> escape), not something the token can provide. Keep the token file short-lived and out of
> the workspace, and do not hand it to a subagent — but do not treat it as a secret a hostile
> same-user process cannot reach.

**Never put the token in a subagent's prompt, and never ask a subagent to run a `vichu`
command.** This is not bureaucracy, it is the boundary the whole product rests on.

Your host's permission rules are **session-wide**. The implementer subagent has Bash — it
needs it to run the project's tests — so it can already *type* `vichu worker complete`. The
token is the only thing stopping it from closing its own worker and then carrying on
editing files after the mutation audit stopped watching. You hold it; they do not.

## When the kernel blocks the run — STOP

A block is the kernel's verdict that the evidence does not permit going on: a read-only
worker changed files, a gate failed, a reviewer said `blocked`. **Report it to the user
and stop.** Do not run `vichu run resume`, `--accept-changes` or `vichu cancel` to get
past it, and do not ask a subagent to.

This is not a style rule. The single thing that makes VichuFlow worth running is that the
agent cannot advance the run without evidence the kernel verified. An agent that clears
its own block has turned the whole system into theater. The human decides.

## Idempotency

Pass a fresh `--op-id` per logical operation. If a command's response is lost,
**retry with the SAME op-id** — the kernel returns the same result without
re-applying. Never reuse an op-id for a different operation.

**Write the op-id as a literal string you make up** (e.g. `--op-id ws-7f3a2b91`) —
**never `$(uuidgen)`** or any other shell substitution. A command containing `$(…)`
cannot be checked statically against the host's permission allowlist, so the host
falls back to asking the user to approve **every single kernel call**, which makes a
run miserable to sit through. A literal keeps the kernel commands on the pre-authorized path
(`vichu init --host` allowlists exactly the per-step commands: `worker start/complete`,
`review complete`, `stage close`, `status`, `observe` — and deliberately NOT `run start`,
`run resume`, `cancel`, `init` or `uninstall`, each of which a human must approve.)


For the same reason, **issue one `vichu` command per call**: don't chain them with
`;` or `&&`, and don't pipe into `head`/`python3`. A compound command may not match
the allowlist either. The commands already print the JSON you need — read that.

## Passing results, verdicts and artifacts

Content you hand the kernel (`--result`, `--verdict`, `--artifact name=file`) must
come from a file **outside the project workspace** — write it to the host's temp dir
(e.g. `/tmp/vichu-plan.md`) with your **Write tool**, then point the kernel at it:

```
vichu worker complete --run <id> --worker <wid> --result /tmp/vichu-explore.md --op-id <literal-id>
```

Two reasons this shape matters:

- **It stays pre-authorized.** The command begins with `vichu`, so it matches the
  allowlist `vichu init --host` installed. Piping instead (`cat f | vichu …`,
  `echo … | vichu …`) makes the command begin with `cat`/`echo`, which is NOT
  allowlisted — the host then prompts the user for approval on every call.
- **It never looks like a mutation.** A file you scratch *inside* the repo during a
  read-only stage (`explore`/`propose`/`plan`) is attributed to the worker and
  **blocks the run**. `$TMPDIR` is outside the workspace, so it is invisible to the
  audit. Never write inside `.vichu/` either — the kernel is its single writer.

(`--result-stdin` / `--verdict-stdin` still work and are fine when you are already
being prompted, but the temp-file form above is what keeps a run flowing without
interruptions.)

## Observe

To show the user where a run is: `vichu status <run_id>` (human) or
`vichu status --json <run_id>` (to reason over). The runtime under `.vichu/runs/`
is the source of truth; you only read it.
