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
}

// IsSensitive reports whether a repo-relative path is sensitive: VCS internals,
// the runtime directory, CI configuration, project config, or lockfiles.
func IsSensitive(p string) bool {
	p = toSlash(p)
	for _, pre := range sensitivePrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	_, ok := sensitiveNames[strings.ToLower(base)]
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
