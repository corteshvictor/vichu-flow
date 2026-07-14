# Security

## Reporting a vulnerability

Please report security issues privately via GitHub's "Report a vulnerability"
(Security → Advisories) rather than a public issue.

## Supply-chain posture

VichuFlow is built to minimize supply-chain risk, taking advantage of Go's
module security model:

- **Tiny dependency surface.** The entire project has **one** direct dependency
  (`gopkg.in/yaml.v3`). Fewer dependencies means a far smaller attack surface
  than ecosystems where a single app pulls in hundreds of transitive packages.
- **Checksum-pinned dependencies.** `go.sum` records the SHA-256 of every
  module and is committed. `go mod verify` confirms nothing was tampered with,
  and `GOSUMDB=sum.golang.org` cross-checks every download against Go's global,
  append-only transparency log. We never set `GOSUMDB=off`.
- **No install scripts.** Unlike npm `postinstall`, Go modules cannot run code
  at install time — fetching a dependency does not execute it.
- **Vulnerability scanning in CI.** `govulncheck` runs on every change; it walks
  the call graph and fails the build only on vulnerabilities our code actually
  reaches. Run it locally with `task vuln`.
- **GitHub Actions pinned by commit SHA.** Workflows reference actions by full
  SHA (not mutable `@v4`-style tags), so a tag can't be repointed at malicious
  code under us. The version is kept in a trailing comment.
- **Automated updates.** Dependabot opens PRs for Go module and GitHub-Actions
  updates weekly (it bumps the pinned SHAs too), and GitHub security alerts flag
  advisories.

## What this does NOT protect against

Go's model defends against tampering with a *published* version, but not against
a maintainer publishing a malicious version, or **typosquatting** (a fake module
with a name close to a real one). Mitigations we follow:

- Add dependencies deliberately and review the import path's source repository.
- Keep the dependency count near zero; prefer the standard library.
- Pin versions (the default) and review Dependabot PRs rather than auto-merging.

## Future web dashboard supply chain (v0.6)

The web dashboard (`web/`, planned for v0.6) is the only surface that will pull
third-party JavaScript dependencies. It will use **pnpm only** (never npm), with
these controls in `pnpm-workspace.yaml`:

```yaml
packages:
  - "web/*"
minimumReleaseAge: 2880        # 2 days (minutes); applies to transitive deps too
blockExoticSubdeps: true       # no transitive deps via git/tarball
trustPolicy: no-downgrade
dangerouslyAllowAllBuilds: false
allowBuilds: {}                # explicit allowlist of packages allowed to run build scripts
```

Plus: pnpm pinned via `packageManager` + Corepack, a committed lockfile,
`pnpm install --frozen-lockfile` in CI, and `pnpm audit`. Install scripts are
off by default (pnpm v10+). This section applies once `web/` exists; until then
the project ships a single Go-runtime dependency and no JS.

## Runtime safety

Beyond dependencies, VichuFlow's runtime enforces safety while orchestrating
agents — workspace snapshots, per-worker mutation tracking, and hard budgets.
How command prevention applies depends on the run mode:

- **When VichuFlow runs the command** — verification gates, `shell` workers, and
  the headless `claude-code`/`codex` adapters — a central command policy blocks
  dangerous commands (`rm -rf`, `git push`, installs) **before they run**.
- **Host-first native** (the installed host pack, where Claude Code runs its own
  subagents) — VichuFlow does not launch the agent, so preventive control is the
  host's (Claude Code permissions / `.claude/settings.json`). VichuFlow's guarantee
  is **detection, not prevention**: it audits every worker's mutations and **blocks
  the run from advancing** on a violation.

### What the mutation audit does and does not see

The audit is deliberately explicit about its own limits, because a security guarantee
you cannot state precisely is not one:

- **The coding host's own machine-local state** (`.claude/settings.local.json`, which
  the host rewrites the instant you approve a command) is **recorded** in
  `mutations.json` — with its content hash, flagged `host_bookkeeping` — but is
  **exempt from the mutation policy**. Otherwise every read-only stage would fail
  because the host wrote its own config while the agent was only reading code. It is
  exempted from blocking; it is not hidden.
- **Everything else** under `.claude/` and `.agents/` — subagents, skills, slash
  commands, the shared `settings.json` — is a normal mutation and is flagged
  **sensitive**. A write there does not change your code; it changes what the *next*
  agent is allowed and instructed to do. Note the default for `settings.local.json`
  specifically is `hostLocalState: warn` (the host rewrites it on every approval), and in
  Claude Code that file can carry a `hooks` entry that runs an arbitrary command on the next
  tool event — so an agent that writes one gets **code execution** next session, not merely
  extra tool grants. Set `hostLocalState: block` if that matters to you.
- **An ignored FILE is audited; an ignored DIRECTORY is not.** A path your ignore rules
  exclude by name — `.env`, `coverage.out`, `settings.local.json` — is captured in the
  audit with its hash, and a worker that writes to it is held to the policy like any
  other. But a path *inside* an ignored **directory** (`node_modules/`, `dist/`,
  `target/`) is **invisible to the audit**: the whole subtree collapses to one entry that
  VichuFlow skips.

  That line is deliberate. Declaring a directory ignored is your project saying "this
  subtree is derived output, not my work", and walking and hashing 50k `node_modules`
  files before and after every worker would cost far more than it buys. **An agent can
  hide a file there and VichuFlow will not report it.** If a directory holds something you
  would want to know about, ignore the *files* you need to keep out of Git rather than the
  directory that contains them.
- **Being ignored is not a license to mutate.** An ignored file is marked `derived` in the
  audit, but that is *informational* — it says where the path sits relative to your ignore
  rules, not that the path is disposable. It is policed like any other: a read-only worker
  that overwrote it blocks, and a gate that overwrote it blocks and is rolled back.

  The tempting shortcut is to infer "gitignored, therefore build output". That sentence
  also describes a private note, a credential, a certificate, a local config, and anything
  a *global* gitignore excludes that the project never mentioned — so a gate could destroy
  something irreplaceable and "it was in your .gitignore" would be the defence. Instead,
  the project **declares** which paths a gate may rewrite, in `security.gateOutputs`
  (empty by default). A sensitive path is never allowed there, and the allowlist applies
  to gates only — never to workers.
- **The `filesystem` provider does not track symlinks.** It walks regular files only, so a
  symlink a worker creates is not reported as a mutation. Rollback is still confined — it
  will never write *through* one — but the link itself is not audited. The Git provider
  tracks symlinks and fingerprints them by their target text.
- **A file VichuFlow cannot read stops the run, rather than being skipped.** A path that
  exists but is unreadable (mode `000`, a denied ACL) used to be indistinguishable from an
  absent one, so it was never backed up and a gate could chmod it and overwrite it. Failing
  to read a file is not evidence that it isn't there. If the audit cannot see it, no agent
  and no gate runs.
- **Every read and write the audit performs is confined to the workspace** by `os.Root`,
  and never resolves a final symlink. A rollback restores content, permission bits *and*
  type — so a gate that widened `0600` to `0644`, or swapped a file for a link pointing
  outside the project, is fully undone. The same confinement now covers the runtime store
  under `.vichu/` (state, events, gate output, artifacts): an agent that plants a symlink
  there cannot redirect the kernel's writes onto a file outside the project.
- **Gate and adapter processes are NOT sandboxed.** A gate (`go test`, `npm run build`, …) and
  an agent adapter run as normal child processes with the project as their working directory and
  your full user permissions — no filesystem, network, or resource isolation. The mutation audit
  compares only files *inside the workspace*, so a gate that writes through a symlink the tree
  already contains (e.g. a gitignored `coverage.out → /outside/file`) can modify a file OUTSIDE
  the workspace, and the run still completes: the symlink itself did not change, so there is
  nothing for `gateMutations:block` to catch. Treat the commands you configure and the agents you
  run as trusted code with your own reach. A capability-based sandbox (Landlock/container on
  Linux, sandbox/container on macOS, AppContainer on Windows) is designed but not yet built.
- **The `.vichu/` state files are not yet forgery-proof.** The kernel treats `.vichu/runs`
  as its own trusted store, but the coding agent has shell access to the same tree, and
  `.vichu/` is excluded from the mutation audit (it *is* the runtime). So an agent that
  writes `.vichu/runs/<id>/state.json` directly can forge the run's verdict — the driver
  token stops it from *driving* the run, not from overwriting the record. Signing the state
  files is designed and scheduled — but note the honest limit: the signing key **cannot be
  the driver token**, because (see the next bullet) any process running as your user can read
  that token and would then be able to compute a valid signature. A signature keyed on the
  token detects accidental corruption and a *cooperative* agent, not a hostile same-user
  process; defeating that needs a key the agent cannot read — host-level isolation or a
  broker under a different identity. Until then, the `.vichu/` verdict is trustworthy against
  an agent you would already let run your gates, not against a deliberately hostile one.
- **The driver token is a session-wide capability, not a per-process secret.** Pass it with
  `--driver-token-stdin` so it never lands in argv (where any process could read it via
  `ps`) — the host pack does this. But a token read by the orchestrator still lives in that
  process, and VichuFlow does not isolate a subagent from it: a subagent running as the same
  user *could* read the orchestrator's memory or environment. The token defends against a
  subagent that only has the kernel's own commands; it is not a boundary against a
  deliberately hostile process at the same privilege level. That boundary needs host-level
  isolation (a sandbox the subagent cannot escape), which is the host's job, not the
  kernel's.

See [docs/user/configuration.md](docs/user/configuration.md#security) for the full
two-mode breakdown.
