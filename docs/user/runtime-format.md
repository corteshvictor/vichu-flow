# Runtime format

The runtime format is a **public contract**: a run is a directory of flat files
under `.vichu/runs/<run-id>/`, and any external tool may read them. This page
documents the layout and schemas for `schema_version: 1`.

## Directory layout

```text
.vichu/runs/<run-id>/
  state.json              # source of truth (atomic writes)
  events.ndjson           # append-only timeline
  lock.json               # owner pid + hostname + heartbeat
  workspace.json          # workspace snapshot captured at start (git or filesystem)
  contextpack.md          # project context injected into workers
  config.snapshot.yaml    # frozen config for this run
  workers/<worker-id>/
    prompt.md             # the full prompt sent to the agent
    status.json           # worker lifecycle
    result.md             # the agent's human-facing result
    result.json           # machine-readable result + usage/cost
    session.json          # agent session id (for resume)
    mutations.json        # files this worker changed (computed by the runtime)
  gates/<stage>/<n>/
    command.json          # exact command run
    output.log            # full captured stdout+stderr
    verdict.json          # exit code, duration, passed
    excerpt.txt           # bounded tail of a FAILED gate's output (context budget)
  reviews/<stage>/iteration-<n>/
    verdict.json          # validated review verdict for that iteration
  summaries/<stage>.md    # bounded per-stage summary passed to later stages
  artifacts/              # named workflow artifacts (e.g. proposal.md, plan.md)
  operations/<op-id>.json # host-first idempotency record (see below)
```

Host-first transactional commands (`worker complete`, `review complete`,
`stage close`) accept an `--op-id`. The kernel records each one's result under
`operations/<op-id>.json` so a retry with the same id returns the same result
without re-applying. `run start --op-id` is recorded globally under
`.vichu/operations/run-start/<op-id>.json` (the run does not exist yet). Fields:
`kind` (`worker.complete` / `stage.close` / `run-start` / …), `fp` (a digest of
the operation's identifying args — reusing an op-id for a different operation is
rejected), and the cached `worker_id` / `run_id` / `block_reason`.

With the `filesystem` workspace provider, a sibling `.vichu/baseline/` holds the
tree copy that run snapshots are taken against (plus `baseline.manifest` and
`baseline.id`); the `git` provider uses the repository itself and writes no
baseline. Either way, everything under `.vichu/` is runtime bookkeeping — it is
gitignored, never counted as a worker mutation, and safe to delete between runs.

## state.json

```json
{
  "schema_version": 1,
  "run_id": "run-20260610-041723-222a",
  "status": "active",
  "workflow": "quick",
  "provider": "",
  "task": "add a feature",
  "current_stage": "implement",
  "stages": { "explore": "done", "implement": "active", "verify": "pending", "done": "pending" },
  "iterations": {},
  "budgets": { "cost_usd_spent": 0, "wall_clock_spent_seconds": 12.4, "agent_invocations": 2, "tokens_in_spent": 1200, "tokens_out_spent": 450, "tokens_reported": true, "cost_reported": false },
  "active_worker": "implement-02",
  "blocked_reason": "",
  "next_action": "running implementer",
  "created_at": "2026-06-10T04:17:23Z",
  "updated_at": "2026-06-10T04:17:35Z"
}
```

`status` is one of `active`, `blocked`, `paused`, `completed`, `canceled`,
`failed`. Stage status is one of `pending`, `active`, `done`, `skipped`,
`failed`. State is written atomically (temp file + rename), so a reader never
sees a half-written file.

`budgets.tokens_reported` and `budgets.cost_reported` are independent: each turns
`true` once any worker reported that kind of usage. Invocations and wall-clock are
always kernel-measured; tokens and cost are only known when the runner (headless)
or host (native) exposes them, and a source may surface one but not the other —
**codex reports tokens but not USD cost**, so a codex run has `tokens_reported:
true, cost_reported: false`. When a flag is `false`, that spend is **unknown**, not
a real zero: `vichu status` renders "cost unknown" / "tokens unknown", and `status
--json` emits `null` for `cost_usd` (or `tokens_total`) while the other may still
carry a real value.

## events.ndjson

One JSON object per line, append-only:

```json
{"ts":"2026-06-10T04:17:23Z","run":"run-...","stage":"implement","worker":"implement-02","event":"worker_started","detail":{"adapter":"fake","role":"implementer"}}
{"ts":"2026-06-10T04:17:23Z","run":"run-...","stage":"verify","event":"gate_completed","detail":{"gate":"test","passed":true,"exit_code":0}}
```

Stable event names include: `run_created`, `run_resumed`, `run_completed`,
`run_blocked`, `run_failed`, `run_canceled`, `stage_started`,
`stage_completed`, `stage_transition`, `worker_started`, `worker_finished`,
`worker_interrupted`, `worker_resumed`, `worker_resume_failed`, `tool_use`,
`agent_text`, `token_usage`, `gate_started`, `gate_completed`, `gate_mutation`,
`gate_rolled_back`, `review_completed`, `review_findings`,
`review_context_truncated`, `mutation_tracked`,
`out_of_scope_mutation`, `sensitive_mutation`, `policy_blocked`,
`workspace_drift`, `workspace_rebaselined`, `budget_exceeded`,
`output_truncated`.

All runtime data is English regardless of the UI language: these files are a
machine-readable contract, and views translate labels around them.

## lock.json

```json
{ "pid": 4321, "hostname": "host", "run_id": "run-...", "acquired_at": "...", "heartbeat_at": "..." }
```

The owning process renews `heartbeat_at` every 5s. A lock whose heartbeat is
older than 30s, or whose owning process is gone, is treated as **orphaned** and
can be reclaimed — this is how an interrupted run is safely resumed.

## workspace.json

```json
{ "provider": "git", "isolation": "current-worktree", "branch": "main",
  "base_sha": "55728672...",
  "dirty_files": ["notes.md"],
  "fingerprints": { "notes.md": "9f86d081..." },
  "captured_at": "..." }
```

`provider` is the workspace backend this run was snapshotted with (`git` or
`filesystem`); resume reopens that same backend so a folder that later gains (or
loses) a `.git` can't silently flip provider and trigger avoidable drift.
`base_sha` identifies the baseline the snapshot was taken against. Its form
depends on the provider ([configuration.md](configuration.md)): the `git`
provider records the HEAD commit (and `branch`), while the `filesystem` provider
records a content digest prefixed `fs:` (and leaves `branch` empty).
`fingerprints` maps each changed-vs-baseline path to its sha256 content hash at
snapshot time; drift detection on resume compares content, not just file names.

## verdict.json (gate evidence)

```json
{ "name": "test", "command": "go test ./...", "exit_code": 0, "passed": true,
  "duration_ms": 842, "output_path": ".../output.log", "output_bytes": 1203,
  "started_at": "...", "finished_at": "..." }
```

`passed` is the authoritative signal for a stage transition.

## verdict.json (review evidence)

Written by a review stage to `reviews/<stage>/iteration-<n>/verdict.json`. It is
the runtime's validated record of a review — not the raw text the agent emitted.

```json
{ "status": "needs_fixes", "summary": "missing tests",
  "findings": [ { "severity": "major", "file": "calc.go", "message": "add a test" } ],
  "stage": "review", "iteration": 1, "captured_at": "..." }
```

`status` is one of `approved`, `needs_fixes`, or `blocked`. The engine branches
on it (`approved` → advance, `needs_fixes` → loop to the fix stage, `blocked` →
stop for a human) and recomputes that branch from this file on resume, so the
decision survives a crash. A missing or invalid verdict blocks the run — it
never silently becomes `approved`. `severity` is `blocker`, `major`, or `minor`.

## mutations.json (per worker)

```json
{ "worker": "implement-02", "stage": "implement", "base_sha": "55728672...",
  "mutations": [
    { "path": "feature.py", "kind": "untracked", "hash": "2cf24dba...",
      "added": 3, "deleted": 0, "out_of_scope": false, "sensitive": false }
  ],
  "captured_at": "..." }
```

`kind` is `modified`, `added`, `deleted`, or `untracked`. `hash` is the file's
content hash after the worker ran ("" for deletions) — resume uses it to tell
the run's own changes apart from later external edits. `sensitive` flags changes
to VCS/CI/config/lockfiles (blocking by default); `out_of_scope` flags changes
outside a stage's declared scope. VichuFlow's own `.vichu/` directory is never
reported as a mutation.

## Schema versioning

`schema_version` is written into `state.json` from the first release. Future
binaries detect older runs and migrate them on read, so tooling built against
version 1 keeps working.
