// Package hostpacks embeds the VichuFlow host packs (the skills/agents/commands
// installed into a coding host like Claude Code) into the binary, so
// `vichu init --host` works from the installed binary with no external files.
package hostpacks

import "embed"

// FS holds every host pack under packs/<host>/. The kernel reads manifests and
// files from here and copies them into a project (cross-platform: copy, never
// symlink).
//
//go:embed all:packs
var FS embed.FS
