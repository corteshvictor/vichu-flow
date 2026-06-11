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

## Future web dashboard supply chain (v0.5)

The web dashboard (`web/`, planned for v0.5) is the only surface that will pull
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
agents — a central command policy that blocks dangerous commands before they
run, git workspace snapshots, per-worker mutation tracking, and hard budgets.
See [docs/user/configuration.md](docs/user/configuration.md#security).
