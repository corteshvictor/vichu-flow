<!--
PR title MUST follow Conventional Commits (it becomes the squash-merge commit):
  feat(engine): add fix loop      fix(status): order stages by workflow
  docs: ...   refactor: ...   test: ...   chore: ...   perf: ...
CI validates the title automatically.
-->

## What & why

<!-- What does this change and why? Link any issue: Closes #123 -->

## How was it verified?

- [ ] `gofmt -l .` clean
- [ ] `go vet ./...`
- [ ] `go test -race ./...`
- [ ] `golangci-lint` (via `go run ...@v2.12.2 run`)
- [ ] `go mod verify`
- [ ] `govulncheck ./...` (via `go run ...@v1.3.0 ./...`)
- [ ] Tests added/updated for the change

If this PR touches the examples:

- [ ] Affected example's real gate passes (`go test ./...`, `node --test`,
      `python3 -B -m unittest`, or `cargo test`)

If this PR touches release/distribution (`.goreleaser.yml`, release workflow):

- [ ] `goreleaser check`

## Notes

<!-- Anything reviewers should know: trade-offs, follow-ups, screenshots. -->
