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
  workspace.json          # git snapshot captured at start
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
  summaries/<stage>.md    # bounded per-stage summary passed to later stages
  artifacts/              # workflow artifacts
```

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
  "budgets": { "cost_usd_spent": 0, "wall_clock_spent_seconds": 12.4, "agent_invocations": 2, "tokens_in_spent": 1200, "tokens_out_spent": 450 },
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

## events.ndjson

One JSON object per line, append-only:

```json
{"ts":"2026-06-10T04:17:23Z","run":"run-...","stage":"implement","worker":"implement-02","event":"worker_started","detail":{"adapter":"fake","role":"implementer"}}
{"ts":"2026-06-10T04:17:23Z","run":"run-...","stage":"verify","event":"gate_completed","detail":{"gate":"test","passed":true,"exit_code":0}}
```

Stable event names include: `run_created`, `run_resumed`, `run_completed`,
`run_blocked`, `run_failed`, `run_canceled`, `stage_started`,
`stage_completed`, `stage_transition`, `worker_started`, `worker_finished`,
`tool_use`, `agent_text`, `token_usage`, `gate_started`, `gate_completed`,
`gate_mutation`, `gate_rolled_back`, `mutation_tracked`, `out_of_scope_mutation`,
`sensitive_mutation`, `policy_blocked`, `workspace_drift`,
`workspace_rebaselined`, `budget_exceeded`, `output_truncated`.

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
{ "isolation": "current-worktree", "branch": "main", "base_sha": "55728672...",
  "dirty_files": ["notes.md"],
  "fingerprints": { "notes.md": "9f86d081..." },
  "captured_at": "..." }
```

`fingerprints` maps each dirty path to its sha256 content hash at snapshot time;
drift detection on resume compares content, not just file names.

## verdict.json (gate evidence)

```json
{ "name": "test", "command": "go test ./...", "exit_code": 0, "passed": true,
  "duration_ms": 842, "output_path": ".../output.log", "output_bytes": 1203,
  "started_at": "...", "finished_at": "..." }
```

`passed` is the authoritative signal for a stage transition.

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
