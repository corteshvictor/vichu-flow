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
`kind` (`worker.complete` / `stage.close` / `run-start` / …), `fp` (a digest of the operation's
identifying args and payload), and the cached `worker_id` / `run_id` / `block_reason`.

Reusing an op-id for a **different** operation is rejected — **once this record exists**. That
is the honest limit: the record is written **last**, so if the command applied its effects and
then failed to write it, the id is free again and the next command may claim it. A record that
exists but cannot be read also fails closed (it is what carries the id's identity, so we refuse
rather than guess). Closing that last window means reserving the operation's identity *before*
the work rather than after — a rework of the transaction layer, scheduled rather than patched.
See [Known limits](../../README.md#known-limits).

## `.vichu/rollback/` — rollback quarantine

When a gate is blocked and rolled back, VichuFlow restores each pre-existing file it damaged.
Almost always that is a direct overwrite. The one case it cannot do in place is a gate that
**replaced a file with a directory** (`draft.txt` → `draft.txt/generated.js`): a non-empty
directory cannot be renamed away, and cannot be deleted while it holds contents VichuFlow did
not create.

Rather than delete it — destroying data to undo the destruction of data — VichuFlow **moves**
it to `.vichu/rollback/<hash>-<name>/`, beside a `<hash>-<name>.json` record naming where it
came from and when. The original file is then restored over the space it vacates.

Nothing here is ever cleaned up automatically; it is evidence. Inspect it, and once you have
recovered anything you want, delete the entry yourself:

```bash
ls .vichu/rollback/             # what was moved aside, and its record
rm -rf .vichu/rollback/<entry>  # once you've salvaged what you need
```

With the `filesystem` workspace provider, a sibling `.vichu/baseline/` holds the
tree copy that run snapshots are taken against (plus `baseline.manifest` and
`baseline.id`); the `git` provider uses the repository itself and writes no
baseline. Everything under `.vichu/` is gitignored and is never counted as a worker
mutation.

**What is safe to delete.** `.vichu/runs/` and the filesystem provider's `baseline/` are
disposable once you have no runs you want to keep — deleting them throws away run history,
nothing else.

**`.vichu/hosts/`** records which permission rules `vichu init --host` added to *your*
`.claude/settings.json`. Deleting it costs you nothing dangerous: `vichu uninstall` then has
no idea which rules were ours, so it withdraws none and says so — you remove them by hand.

It is deliberately **not** trusted for anything destructive. It lives inside your workspace,
so a process there could have written it; VichuFlow treats its claims as a *proposal*, never
as authority. That is why `uninstall` leaves permissions alone unless you pass
`--withdraw-permissions`, and why **pack files are identified by content** (matching the pack
compiled into the binary), not by anything a file in your repo says.

For a full clean slate: `vichu uninstall --host <host> --withdraw-permissions`, then delete
`.vichu/`.

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

The owning process renews `heartbeat_at` every 5s. A lock whose heartbeat is older than 30s,
or whose owning process is gone, is treated as **orphaned** and can be reclaimed — this is how
an interrupted run is resumed.

> **This is a heuristic lease, not a guarantee — know its limits.** A live process that is
> merely *stalled* for 30s (a suspended laptop, a slow network filesystem, antivirus scanning
> every write) looks orphaned, and reclaiming is a delete-then-create rather than an atomic
> swap. Two processes can therefore both believe they own a run. In practice:
> **do not run two VichuFlow processes against the same run, and avoid NFS.** A real
> OS-level lock (`flock` / `LockFileEx`) is planned.

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
      "added": 3, "deleted": 0, "out_of_scope": false, "sensitive": false },
    { "path": "coverage.out", "kind": "modified", "hash": "d2a84f4b...",
      "added": 12, "deleted": 9, "derived": true },
    { "path": ".claude/settings.local.json", "kind": "modified", "hash": "9f86d081...",
      "added": 1, "deleted": 0, "sensitive": true, "host_bookkeeping": true }
  ],
  "captured_at": "..." }
```

`host_bookkeeping` marks the coding host's own machine-local permission file
(`.claude/settings.local.json`), which the host rewrites the moment you approve a
command — mid-run, on a file the agent never touched. VichuFlow **records** it like any
other change, with its hash, but by default does **not** block on it
(`security.hostLocalState: warn`); blocking would fail every read-only stage the first
time you click "approve".

It is recorded rather than hidden on purpose: that file *is* the host's permission
allowlist, and in host-first mode VichuFlow does not launch the agent, so it cannot tell
whether the host wrote it or the agent did. Not blocking is a decision we can defend;
claiming the change never happened is not. Set `security.hostLocalState: block` to stop
the run on any change to it — see
[configuration.md](configuration.md#security).

`kind` is `modified`, `added`, `deleted`, or `untracked`, and it describes the file's
**lifecycle across this worker**, not its status against your VCS. A file that was
*already* untracked when the worker started and that the worker then overwrote is
`modified` — it was pre-existing work, and it was destroyed. Only a file the worker
actually created is `untracked`.

`hash` is the file's content hash after the worker ran ("" for deletions) — resume uses
it to tell the run's own changes apart from later external edits. A **symlink** is
fingerprinted by its *target text*, prefixed `symlink:`, never by the content it points
at: retargeting a link is a mutation even when both targets hold identical bytes, and
following it would fingerprint something the workspace does not own.

`sensitive` flags changes to VCS/CI/config/lockfiles and to `.env*` (blocking by
default); `out_of_scope` flags changes outside a stage's declared scope.

`derived` flags a path your own ignore rules exclude. It is **informational** — recorded
with its hash — and does NOT by itself exempt a change from the policy:

- **Workers** get the normal mutation policy regardless of `derived`; the only exemption is
  `host_bookkeeping` (the host rewriting its own settings). A read-only stage that rewrote a
  gitignored file still blocks — an ignored path can be a private note or a credential.
- **Gates** *build* — `go build`, `cargo test`, `npm run build` write coverage files, logs and
  binaries — so a gate creating a **genuinely new** gitignored artifact only produces an event.
  But a gate that **modifies or deletes a pre-existing** file (tracked, or untracked-and-present)
  blocks by default; the escape hatch for legitimate build output is `security.gateOutputs`,
  scoped to gates.

A `sensitive` path (a gitignored `.env`) is never exempt, for a worker or a gate.

Two things are **not** reported as mutations at all: VichuFlow's own `.vichu/` directory,
and anything inside an **ignored directory** (`node_modules/`, `dist/`, `target/`). The
second is a real limit, not an oversight — see
[SECURITY.md](../../SECURITY.md#what-the-mutation-audit-does-and-does-not-see).

## Schema versioning

`schema_version` is written into `state.json` from the first release. Future
binaries detect older runs and migrate them on read, so tooling built against
version 1 keeps working.
