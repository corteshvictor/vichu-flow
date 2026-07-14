package workspace

import (
	"path"
	"strings"
)

// sensitivePrefixes and sensitiveNames identify files that always warrant an
// event and a policy check when a worker touches them.
var sensitivePrefixes = []string{
	".git/",
	".vichu/",
	".github/workflows/",
	".gitlab-ci",
	// The coding host's own configuration: its skills, subagents, slash commands and
	// permission allowlist. An agent that writes here is not changing your code — it is
	// changing what the agent that runs NEXT is allowed and instructed to do. That is
	// the highest-leverage thing in the tree, so every touch is surfaced.
	".claude/",
	".agents/",
}

var sensitiveNames = map[string]struct{}{
	"vichu.yaml":        {},
	"vichu.yml":         {},
	".gitignore":        {},
	"package-lock.json": {},
	"pnpm-lock.yaml":    {},
	"yarn.lock":         {},
	"go.sum":            {},
	"cargo.lock":        {},
	"poetry.lock":       {},
	"uv.lock":           {},
	// Standing instructions every future agent reads: editing them is prompt injection
	// with a long fuse.
	"claude.md": {},
	"agents.md": {},
}

// IsSensitive reports whether a repo-relative path is sensitive: VCS internals,
// the runtime directory, CI configuration, project config, lockfiles, or secrets.
func IsSensitive(p string) bool {
	p = toSlash(p)
	for _, pre := range sensitivePrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	base := strings.ToLower(p)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		// Secrets. Almost always gitignored, which is exactly why this matters: a derived
		// path is exempt from the mutation policy, and `.env` must not be — being
		// sensitive is what keeps it policed. See core.Mutation.Derived.
		return true
	}
	_, ok := sensitiveNames[base]
	return ok
}

// InScope reports whether a path matches any of the scope globs. An empty scope
// means "no declared scope" — every path is in scope.
func InScope(p string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, g := range scope {
		if matchGlob(g, p) {
			return true
		}
	}
	return false
}

// matchGlob supports exact matches, "dir/**" and "dir/" directory prefixes, and
// path.Match patterns. Patterns without a slash also match by basename.
func matchGlob(pattern, p string) bool {
	pattern = toSlash(pattern)
	p = toSlash(p)

	switch {
	case strings.HasSuffix(pattern, "/**"):
		prefix := strings.TrimSuffix(pattern, "/**")
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	case strings.HasSuffix(pattern, "/"):
		return strings.HasPrefix(p, pattern)
	}
	if ok, _ := path.Match(pattern, p); ok {
		return true
	}
	if !strings.Contains(pattern, "/") {
		base := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			base = p[i+1:]
		}
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
	}
	return false
}

// toSlash normalizes OS separators to forward slashes. Git already
// reports forward slashes, but scope globs may come from config on Windows.
func toSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// runtimeDirName is VichuFlow's per-project runtime directory, which holds run
// state, locks, and the filesystem provider's baseline. It is never a worker
// mutation and is always excluded from change detection.
const runtimeDirName = ".vichu"

// isRuntimePath reports whether a path is inside VichuFlow's own runtime
// directory, which must never be treated as a worker mutation or as drift,
// independent of whether the project gitignores it.
func isRuntimePath(p string) bool {
	p = toSlash(p)
	return p == runtimeDirName || strings.HasPrefix(p, runtimeDirName+"/")
}

// hostLocalStateFiles is the EXACT set of paths excluded from the mutation audit
// because they are a coding host's own machine-local bookkeeping, not the agent's
// work on your code. Today that is the permission allowlist the host rewrites the
// moment you approve a command mid-run: attributing it to a worker would block every
// read-only stage (explore/propose/plan) for a file the agent never touched.
// (`.claude` is often a symlink to `.agents`, so both spellings are listed.)
//
// This is an EXACT list on purpose. It used to be the pattern `*.local.*` under the
// host's config dir, and that was a hole: a coding host loads its subagents with a
// glob (`.claude/agents/*.md`), so an agent could write `.claude/agents/backdoor.local.md`
// — a file the host would load on the next session and the audit would never report.
// Every exclusion here is a blind spot in a product whose whole promise is "I audit
// every mutation", so each one must be named, justified, and kept to the minimum.
// Everything else under `.claude/` and `.agents/` is audited AND flagged sensitive.
var hostLocalStateFiles = map[string]struct{}{
	".claude/settings.local.json": {},
	".agents/settings.local.json": {},
}

// isHostLocalState reports whether a path is a coding host's machine-local state,
// which — like `.vichu/` and `.git/` — is never a worker mutation.
func isHostLocalState(p string) bool {
	_, ok := hostLocalStateFiles[toSlash(p)]
	return ok
}

// IsHostBookkeeping reports whether a path is the coding host's own machine-local
// state. Such a change is still RECORDED in the audit (with its hash); it is only
// exempt from the mutation policy, so a read-only stage does not fail because the host
// wrote its own config while the agent was reading code.
func IsHostBookkeeping(p string) bool { return isHostLocalState(p) }
