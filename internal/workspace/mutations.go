package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// changedFile records a path's porcelain status code and a content hash, so the
// tracker can tell whether a worker altered a file that was already dirty.
type changedFile struct {
	code string
	hash string
}

// changeSource is the provider-specific machinery a Tracker needs to compute a
// worker's mutations: the current changed-vs-baseline fileset, per-path line
// stats, and the workspace root. Both the Git Repo and the FilesystemWorkspace
// satisfy it, so Tracker/Backup are shared across providers.
type changeSource interface {
	Root() string
	captureChanged() (map[string]changedFile, error)
	lineStats(path string, untracked bool) (added, deleted int)
}

// Tracker captures the working tree before a worker runs and diffs it after, so
// the runtime knows exactly which files the worker mutated — never trusting the
// agent's own account.
type Tracker struct {
	src    changeSource
	before map[string]changedFile
}

// BeginTracking snapshots the set of changed files before a worker runs.
func (r *Repo) BeginTracking() (*Tracker, error) { return newTracker(r) }

// newTracker captures the changed set from any provider as the before-state.
func newTracker(src changeSource) (*Tracker, error) {
	before, err := src.captureChanged()
	if err != nil {
		return nil, err
	}
	return &Tracker{src: src, before: before}, nil
}

// Finish diffs the working tree against the start snapshot and returns the
// mutations the worker produced.
func (t *Tracker) Finish() ([]core.Mutation, error) {
	after, err := t.src.captureChanged()
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for p := range t.before {
		seen[p] = struct{}{}
	}
	for p := range after {
		seen[p] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var muts []core.Mutation
	for _, p := range paths {
		b, okB := t.before[p]
		a, okA := after[p]
		switch {
		case !okB && okA:
			muts = append(muts, mutation(t.src, p, a.code, a.hash))
		case okB && okA && b.hash != a.hash:
			muts = append(muts, mutation(t.src, p, a.code, a.hash))
		case okB && !okA:
			// The path differed from the baseline before but no longer appears as
			// changed. Two cases: the working-tree file was deleted (it is gone
			// from disk now — a real destructive change, including untracked
			// files that are real user work), or a tracked file was reverted to
			// the baseline (still on disk — benign). Only the former is a mutation.
			if !fileExists(filepath.Join(t.src.Root(), p)) {
				muts = append(muts, core.Mutation{
					Path:      p,
					Kind:      core.MutationDeleted,
					Hash:      "",
					Sensitive: IsSensitive(p),
				})
			}
		}
	}
	return muts, nil
}

func mutation(src changeSource, path, code, hash string) core.Mutation {
	kind := kindFromCode(code)
	added, deleted := src.lineStats(path, kind == core.MutationUntracked)
	return core.Mutation{
		Path:      path,
		Kind:      kind,
		Hash:      hash,
		Added:     added,
		Deleted:   deleted,
		Sensitive: IsSensitive(path),
	}
}

// captureChanged returns the current changed-vs-HEAD fileset with content hashes.
func (r *Repo) captureChanged() (map[string]changedFile, error) {
	out, err := r.git("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	m := map[string]changedFile{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		code := line[:2]
		path := parsePorcelainPath(line)
		if isRuntimePath(path) {
			continue // VichuFlow's own .vichu/ bookkeeping is never a worker mutation
		}
		m[path] = changedFile{code: code, hash: hashFile(filepath.Join(r.root, path))}
	}
	return m, nil
}

// lineStats returns approximate added/deleted line counts for a mutated path.
func (r *Repo) lineStats(path string, untracked bool) (added, deleted int) {
	if untracked {
		return countLines(filepath.Join(r.root, path)), 0
	}
	out, err := r.git("diff", "--numstat", "HEAD", "--", path)
	if err != nil || out == "" {
		return countLines(filepath.Join(r.root, path)), 0
	}
	fields := strings.Fields(out)
	if len(fields) >= 2 {
		added, _ = strconv.Atoi(fields[0]) // "-" (binary) parses to 0
		deleted, _ = strconv.Atoi(fields[1])
	}
	return added, deleted
}

func kindFromCode(code string) core.MutationKind {
	switch {
	case strings.Contains(code, "?"):
		return core.MutationUntracked
	case strings.Contains(code, "D"):
		return core.MutationDeleted
	case strings.Contains(code, "A"):
		return core.MutationAdded
	default:
		return core.MutationModified
	}
}

// parsePorcelainPath extracts the (final) path from a porcelain v1 status line,
// handling renames ("R  old -> new").
func parsePorcelainPath(line string) string {
	if len(line) <= 3 {
		return strings.TrimSpace(line)
	}
	path := strings.TrimSpace(line[3:])
	if idx := strings.Index(path, " -> "); idx >= 0 {
		path = path[idx+len(" -> "):]
	}
	return strings.Trim(path, "\"")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // missing (e.g. deleted) — distinct from any real hash
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	if len(data) == 0 {
		return 0
	}
	return strings.Count(string(data), "\n") + 1
}
