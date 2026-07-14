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
5. **Also in the same run**, the `record-pack` job records the host pack that
   this tag shipped (see the next section for *why*) and **opens a `chore: record
   vX.Y.Z …` PR** with the fixtures and hashes. **Merge that PR** — it is the one
   manual step left, and until you do, `main` is red on
   `TestEveryReleasedPackIsRecorded`. (This can only run *after* the tag exists,
   so it cannot be part of the Release PR that creates the tag.)

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

## Recording a released host pack

> **Normally automated.** The `record-pack` job (step 5 above) records each release's pack and
> opens a PR right after the tag is cut — you just merge it. The manual command below is for when
> you want to record a release by hand (the automation was skipped, or you are recording an older
> tag as a one-off).

`vichu init --host` and `vichu uninstall` decide whether a file is *ours* by comparing its
bytes against the pack this binary ships **and every version we ever released**. That history
lives in two places, and both must be updated for a release before its pack can be recognized:

1. `internal/hostpacks/packs/<host>/known-hashes.json` — the sha256 of each file, per release.
   Compiled into the binary; the only reference a cloned repo cannot forge.
2. `cmd/vichu/testdata/packs/<host>/<tag>/` — the released files themselves, checked in.

Skip this and the failure is silent and nasty: a user's untouched pack from the last release
stops looking like ours, so `vichu doctor` tells them to refresh — and the refresh **refuses**.
The upgrade path dead-ends. That happened once; three tests now make sure it cannot happen
again.

```bash
git fetch --tags
go run ./tools/packhistory --host claude-code --tag v0.4.0   # writes catalog + fixtures
go test ./cmd/vichu/ -run 'TestKnownHashesCatalogIsTruthful|TestEveryReleasedPackIsRecorded'
```

Then, and only then, edit the pack.

> **Do not do this with `git show --name-only <tag>`.** That lists what the *tagged commit*
> changed — and a release-please tag commit only touches the version and the changelog, so it
> enumerates the pack **zero times**. The pack must be read from the tag's **tree**, and the
> manifest *at that tag* is the only thing that knows which files the pack had. `packhistory`
> does exactly that; a shell one-liner quietly does not.

**What guards this:**

| Test | Catches |
|---|---|
| `TestKnownHashesCatalogIsTruthful` | catalog and fixtures disagree (a hash with no file, a file with no hash) |
| `TestEveryReleasedPackIsRecorded` | a whole release never recorded — the case the gate above is blind to, because both sides would simply be missing it |
| `TestUpgradeFromAReleasedPackNeedsNoForce` | an untouched pack from a past release still upgrades without `--force` |
