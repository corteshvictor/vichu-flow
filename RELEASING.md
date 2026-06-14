# Releasing

Releases are fully automated from [Conventional Commits](https://www.conventionalcommits.org).
You do not tag, edit the changelog, or build binaries by hand.

## How a release happens

1. **Commits land on `main`** using the Conventional Commits convention
   (`feat:`, `fix:`, …). See [CONTRIBUTING.md](CONTRIBUTING.md).
2. **[release-please](https://github.com/googleapis/release-please)** opens (and
   keeps updating) a **Release PR** titled like `chore(main): release X.Y.Z`. It
   computes the next [SemVer](https://semver.org) version from the commit types
   and updates `CHANGELOG.md`.
3. **You merge the Release PR** when you want to ship. release-please then tags
   the commit (e.g. `vX.Y.Z`) and creates the GitHub Release with the changelog
   notes.
4. **In the same workflow run**, the `goreleaser` job runs — it is chained to
   release-please with `needs` + `if: release_created == 'true'`, so
   **[GoReleaser](https://goreleaser.com)** builds the macOS/Linux/Windows
   binaries and attaches them to the release.

> **Why one workflow, not two?** A GitHub Release created with the default
> `GITHUB_TOKEN` does **not** trigger a separate `on: release` workflow (GitHub
> blocks that to prevent loops). So both steps live in `release.yml` and run in
> the same workflow run.

That's it — `go install ...@vX.Y.Z` (or `@latest`) works immediately, because Go
resolves versions straight from git tags (no registry to publish to).

## First release (bootstrap)

The manifest starts at `0.0.0`. Once enough `feat:`/`fix:` commits have landed,
release-please will propose `0.1.0`. To force a specific first version, add a
commit with a `Release-As: 0.1.0` footer.

## Package managers (optional, later)

Homebrew, Scoop, and winget publishing is pre-wired but commented out in
`.goreleaser.yml`:

- **Homebrew / Scoop** — create a tap/bucket repo, add a token secret with write
  access to it, and uncomment the `brews:`/`scoops:` blocks.
- **winget** — uncomment the `winget:` block; it opens a pull request to a fork
  of [`microsoft/winget-pkgs`](https://github.com/microsoft/winget-pkgs), which
  also needs a token.

Binaries on the GitHub Release work on every OS without any of this.
