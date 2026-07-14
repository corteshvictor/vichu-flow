# Upgrading

VichuFlow ships two things that live in different places, and **upgrading one does not
upgrade the other**:

| | What it is | Where it lives | How it upgrades |
|---|---|---|---|
| **The kernel** | the `vichu` binary — runs the gates, owns the run state | on your `PATH` | you replace the binary |
| **The host pack** | the orchestrator skill + subagents your coding agent reads | **copied into your project**, under `.claude/` | `vichu init --host <host>` |

The pack is *copied* into the project on purpose: your team can commit it, review it, and
pin it. The cost of that choice is the one thing to remember — **a new binary does not
refresh the files already sitting in `.claude/`.**

## The upgrade, in three commands

```bash
# 1. new binary — download it from the Releases page and replace the one on your PATH,
#    or, if you installed from source:
go install github.com/corteshvictor/vichu-flow/cmd/vichu@latest

# 2. refresh the host pack in each project that has one
cd your-project
vichu init --host claude-code

# 3. restart your coding agent
```

**Step 3 is not optional.** The skill and subagent files are read when your agent starts
its session. Refresh them mid-session and the agent keeps running the old ones — you will
be looking at a fixed file on disk and a stale one in memory, which is a genuinely
confusing place to debug from.

## If you edited a pack file

`vichu init --host` is **all-or-nothing**. If *any* pack file has been modified since it
was installed, the refresh is **refused entirely** — nothing is written — and you are told
which file. That is deliberate: silently overwriting your edit would be worse than
stopping.

To proceed, look at what you changed and decide:

```bash
git diff .claude/                      # what did I change, and do I still want it?
vichu init --host claude-code --force  # take the new pack, discarding my edits
```

`--force` controls **the pack's files only**. It has nothing to do with permissions.

**What `init --host` does to your `.claude/settings.json`** (every time, `--force` or not):
it **adds** the rules the current pack needs, and **withdraws** rules an earlier VichuFlow
release added that this one no longer wants — today, the over-broad `Bash(vichu *)`. It says
which is which, in groups, every run. Every other key is preserved, and `settings.local.json`
is never opened at all.

> **An honest limit.** VichuFlow decides which rules are "its own" from a ledger under
> `.vichu/hosts/<host>/` — which lives inside your workspace, so a process there could have
> written it. We withdraw a stale rule anyway, and here is why: removing a permission can only
> ever *reduce* what an agent may do, and the rules we withdraw are ones we have decided are
> unsafe. Leaving a dangerous grant in place because the ledger *might* have been tampered with
> is the worse failure. If we get it wrong, you re-add one line.
>
> **`vichu uninstall` takes the opposite stance** and leaves your permissions alone by default:
> deleting a rule you wrote is not something you can undo by re-reading a message. Pass
> `--withdraw-permissions` when you want it cleaned up.

**Your edited pack files are safe unless you pass `--force`.** *Without* `--force`, `init` and
`uninstall` only ever touch a file whose bytes are *exactly* the pack's — either the version this
binary ships, or a version VichuFlow released earlier. That is the one claim of ownership that
cannot be forged, because it is checked against the binary, not against any file in your repo.
`--force` is exactly the switch that lifts this: it replaces (on `init`) or removes (on
`uninstall`) a modified pack file too — but only files declared in the pack manifest, never
anything else in your repo. Use `--dry-run --force` to see precisely what it would touch first.

So an **untouched pack from an older release upgrades cleanly, with no flags**. A file **you
edited** matches nothing we ever shipped, so:

- `vichu init --host …` refuses and names it. Review it, then `--force` to replace it.
- `vichu uninstall --host …` **changes nothing at all** and names it. Same deal: `--force`.

Neither will half-do the job and tell you it finished.

## Don't remember which projects need it? Ask.

```bash
vichu doctor
```

It compares the pack in your project against the one embedded in the binary and tells you
exactly what to run:

```text
✗ host: claude-code   5 file(s) intact, but this vichu ships a newer pack —
                      run `vichu init --host claude-code`, then restart your coding agent
```

> **Note on versions below.** These changes ship together as **0.4.1** — a fix release on top of
> 0.4.0. The two sections below split the upgrade by *what* changed and *why* (the driver-token
> contract, then the stricter mutation audit), not by release: both land under the single 0.4.1 tag.

## ⚠️ Upgrading to 0.4.1: the binary and the pack must move together

0.4.1 closes a real security hole, and the fix changes the contract between the kernel and
the host pack. **Upgrade the binary and refresh the pack in the same sitting** — a new
binary with an old pack (or the reverse) will not drive a run.

What changed, and why you should care:

- **A run now has a driver token.** `vichu run start` prints it once; the orchestrator
  passes it to every command that *changes* the run. The **kernel** persists only its HASH under
  `.vichu` — never the token itself. The **host pack**, however, stashes the token in a temporary
  file OUTSIDE the workspace (e.g. `$TMPDIR/vichu-run-<id>.token`) so it can pipe it via
  `--driver-token-stdin` instead of exposing it in argv, and deletes it when the run ends. So the
  token *can* touch disk, in a scratch file the pack manages — it is not a boundary against another
  process running as your user (see SECURITY.md, H15).

  This exists because your coding host's permission rules are **session-wide**. The
  implementer subagent has Bash — it needs it to run your tests — so it could already type
  `vichu worker complete`, close its own worker, and then keep editing files after the
  mutation audit stopped watching. The permission layer cannot tell the orchestrator and the
  subagent apart. The token can.

- **`vichu run start` and `vichu run resume` now ask for your approval.** They are not
  pre-authorized, deliberately. Starting a run is an act of intent (one approval per task),
  and resuming a *blocked* run clears the kernel's verdict — which is yours to make, not the
  agent's.

- **Runs started before 0.4.1 need one `vichu run resume`** before they can be driven again.
  That resume issues their token. It is a human action; that is the point.

## ⚠️ 0.4.1 also tightened the mutation audit

0.4.1 also fixes four ways a gate or a worker could change something and not be stopped. The
fixes are **observable** — a run that used to pass may now block, and that is the point. In
every case the block is telling you about something that was silently happening before.

**1. A gitignored file is now audited, and policed.** It used to be invisible: `git status`
omits ignored paths, so a worker that overwrote your `.env` reported "no mutations". Now it
is captured with its hash and held to the policy.

The consequence you will actually hit: **if your test command writes a coverage profile or
a report, and that file already exists, the gate now blocks.** Declare what your gate is
meant to write:

```yaml
security:
  gateOutputs:
    - coverage.out       # globs allowed: reports/*.xml
```

We do not infer this from your `.gitignore` on purpose. "Ignored, therefore disposable"
also describes a private note, a credential and a certificate — see
[configuration.md](configuration.md#gateoutputs--the-paths-your-gate-is-meant-to-write).

**2. Overwriting a pre-existing untracked file is now a *modification*, not a creation.**
Git's status codes are relative to your last commit, not to the worker — so a scratch file
you never committed still read as "new" after a gate clobbered it, and the gate policy only
blocks modifications. It does now.

**3. A file VichuFlow cannot read stops the run.** A path that exists but is unreadable
(mode `000`, a denied ACL) used to look exactly like an absent one, so it was never backed
up. If you have such a file in your tree, the run now refuses to start and names it.

**4. Rollback restores permission bits and file type, not just content.** A gate that
widened `0600` to `0644` while editing a file used to keep the widened mode after the
"restore".

### If a run in flight tracks a symlink

A symlink is now fingerprinted by its **target text** rather than by following it and
hashing what it points at. A run started by an older binary carries the old fingerprints,
and the two are not comparable. Rather than guess whether the link changed — or read
*through* it, which is the thing this release is closing — such a run is failed closed:

```text
run blocked: this run was started by an older VichuFlow that fingerprinted the
symlink <path> by following it … resume with --accept-changes to re-baseline
```

Review the diff, then `vichu run resume --run <id> --accept-changes`. Runs with no symlinks
in their changed set are unaffected and resume normally.

## Finish your runs BEFORE you refresh the pack

Refreshing the pack rewrites files under `.claude/`. To VichuFlow, that is a change to your
workspace — so a run that was **in flight** when you refreshed will see **workspace drift**
and refuse to resume:

```text
run blocked: workspace_drift: …
```

That is the audit working correctly (it does not know the change came from the installer
rather than from an agent), but it is an annoying way to find out. So:

```bash
vichu status                      # anything active or blocked?
# let it finish, or: vichu cancel <run-id>
vichu init --host claude-code     # then refresh
```

**If a run is already stuck this way**, look at the diff before you unblock it —
`vichu run resume --accept-changes` accepts **every** pending change, not just the pack's:

```bash
git diff                                        # is .claude/ ALL that changed?
vichu run resume --run <id> --accept-changes    # only if it is
```

## What happens to runs that were already in flight

The on-disk format carries a `schema_version` and new fields are additive, so an older run's
state is **read** by the new binary without migration. But "resumes with no action" is not
true for every case — a few need one explicit step first. Find your row:

| Coming from | What the run has | Do this before continuing |
|---|---|---|
| v0.4.1+ | already has a driver token | `vichu run resume --run <id>` — nothing special |
| **v0.4.0** | no driver token yet | `vichu run resume --run <id>` **issues** the token (a human action, by design) — [see above](#-upgrading-to-041-the-binary-and-the-pack-must-move-together) |
| any, with a **dirty/untracked symlink** in the run | fingerprints in the old format | resume **blocks**; review the diff, then `--accept-changes` — [see above](#if-a-run-in-flight-tracks-a-symlink) |
| any, **pack refreshed** mid-run | `.claude/` moved under it | resume sees **drift**; finish the run before refreshing, or `--accept-changes` if the diff is only `.claude/` — [see above](#finish-your-runs-before-you-refresh-the-pack) |

In the common case (v0.4.1+, no in-flight symlink, pack not touched) it is just:

```bash
vichu run resume --run <run-id>
```

And if the workspace changed while you were away, resume tells you and stops rather than
guessing — an upgrade does not make it lenient.

## Renamed commands

Old spellings still work and print a warning naming the new one, so nothing breaks
silently and nothing breaks immediately:

| Old | New |
|---|---|
| `vichu run "<task>"` | `vichu exec "<task>"` |
| `vichu resume <id>` | `vichu run resume --run <id>` |

## Coming from a version with no host pack (v0.3 and earlier)

Host packs arrived in v0.4. Before that, VichuFlow only ran headless from the terminal
(`vichu run "<task>"`, now `vichu exec`). That mode still works and is still supported —
it is the CI/automation path. But the pack is the point now:

```bash
cd your-project
vichu init --host claude-code
```

Then open your coding agent and type `/vichu <your task>`. Your `vichu.yaml` carries over
unchanged; the gates you already declared are the gates the kernel runs.
