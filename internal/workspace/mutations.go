package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// changedFile records a path's porcelain status code, a content hash, whether it is
// actually ON DISK, and its permission bits.
//
// exists is explicit and not inferred from the hash. That inference is what made an
// unreadable file (mode 000, a denied ACL) indistinguishable from an absent one: it
// hashed to "", looked like a path the worker had just created, was never backed up, and
// a gate could chmod it and overwrite it with the run still reaching `completed`. Reading
// now fails loudly, and existence is recorded rather than guessed.
type changedFile struct {
	code   string
	hash   string
	exists bool
	mode   os.FileMode
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

// Before returns the tracker's pre-worker snapshot in serializable form, so the
// host-first `worker start` can persist it for `worker complete` to reload.
func (t *Tracker) Before() map[string]core.FileSig {
	out := make(map[string]core.FileSig, len(t.before))
	for p, f := range t.before {
		exists := f.exists
		out[p] = core.FileSig{Code: f.code, Hash: f.hash, Exists: &exists, Mode: uint32(f.mode.Perm())}
	}
	return out
}

// resumeTracker reconstructs a Tracker from a persisted before-snapshot, so `worker
// complete` diffs against exactly what `worker start` captured — even in a separate
// process.
//
// A snapshot written before `exists` was recorded is ambiguous whenever the hash is empty
// and the code is not a deletion: the path may have been absent, or present and unreadable.
// Those two lead to opposite decisions (ignore it / block and preserve it), so the
// reconstruction FAILS rather than pick one. The worker is restarted, not guessed at.
func resumeTracker(src changeSource, before map[string]core.FileSig) (*Tracker, error) {
	b := make(map[string]changedFile, len(before))
	for p, s := range before {
		exists, err := s.Existed()
		if err != nil {
			return nil, fmt.Errorf("worker's before-snapshot for %s: %w — cancel this worker and start it again", p, err)
		}
		b[p] = changedFile{code: s.Code, hash: s.Hash, exists: exists, mode: os.FileMode(s.Mode)}
	}
	return &Tracker{src: src, before: b}, nil
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
		if m, ok := t.mutationFor(p, after); ok {
			muts = append(muts, m)
		}
	}
	return muts, nil
}

// mutationFor decides what, if anything, the worker did to one path.
func (t *Tracker) mutationFor(p string, after map[string]changedFile) (core.Mutation, bool) {
	b, okB := t.before[p]
	a, okA := after[p]

	switch {
	case okA && (!okB || !b.exists):
		// Not on disk before this worker (either not in the changed set at all, or
		// recorded as absent), and changed now: the worker created it, or changed a file
		// that was clean against the baseline. The status code is the truth.
		return mutation(t.src, p, a, false), true

	case okB && okA && b.exists && changedSince(b, a):
		// It ALREADY differed from the baseline before this worker, and the worker changed
		// it again. A status code is relative to the BASELINE, not to the worker: a file
		// that was already untracked (or ignored) still reads "??" after a worker
		// overwrites it. So a gate that destroyed pre-existing untracked user work was
		// classified `untracked`, the gate policy only blocks `modified`/`deleted` — and
		// the run reached `completed` with the work gone. Classify by LIFECYCLE instead:
		// it existed before, and its content, type or MODE changed, so it was MODIFIED.
		return mutation(t.src, p, a, true), true

	case okB && b.exists && !okA:
		// It differed from the baseline before but no longer appears as changed. What is at
		// the path NOW decides whether that is destruction or a benign revert.
		return t.vanishedMutation(p, b)
	}
	return core.Mutation{}, false
}

// vanishedMutation classifies a path that was a changed regular file before the worker and
// is no longer in the changed set. The type at the path now is decisive, and it is inspected
// through a confined Lstat — NOT os.Stat/os.Lstat wrapped in a bool. A plain "does it still
// exist" returned true for a DIRECTORY, so a gate that replaced draft.txt with draft.txt/out
// looked like the file was still there: it was dropped from the audit, the policy saw only
// the new file inside (an allowed creation), and the run completed with the original lost.
func (t *Tracker) vanishedMutation(p string, b changedFile) (core.Mutation, bool) {
	deleted := core.Mutation{Path: p, Kind: core.MutationDeleted, Hash: "", Sensitive: IsSensitive(p), Derived: isDerivedCode(b.code)}

	root, err := openRoot(t.src.Root())
	if err != nil {
		return deleted, true // cannot inspect — record destruction rather than silently drop it
	}
	defer func() { _ = root.Close() }()

	switch kind, _, lerr := lstatEntry(root, p); {
	case lerr != nil || kind == entryMissing:
		// Gone from disk, or unreadable: the regular file that was here is no longer here.
		return deleted, true
	case kind == entryRegular:
		// Still a regular file and no longer changed → reverted to the baseline. Benign.
		return core.Mutation{}, false
	default:
		// A directory, symlink, socket, … now sits where the regular file was — the file was
		// destroyed by a type change. Record it as MODIFIED so the gate policy blocks and the
		// rollback restores the original (quarantining whatever replaced it).
		return mutation(t.src, p, changedFile{code: b.code, hash: "", exists: true}, true), true
	}
}

// changedSince reports whether the worker altered the file in any way that matters:
// its content, its type (a file swapped for a symlink), or its permission bits. A mode
// change alone IS a mutation — a gate that widens 0600 to 0644 has exposed a private file
// without touching a byte of it.
func changedSince(before, after changedFile) bool {
	if before.hash != after.hash {
		return true
	}
	// Mode is only comparable when the before-snapshot recorded it. One written by an older
	// binary did not, so a mode-only change to an in-flight worker's file goes unreported
	// rather than falsely reported; new snapshots always carry it.
	return before.mode != 0 && before.mode != after.mode
}

// mutation builds the Mutation for a changed path. existedBefore says the path was
// already on disk when the worker started, which overrides the status code — see
// Tracker.mutationFor.
func mutation(src changeSource, path string, f changedFile, existedBefore bool) core.Mutation {
	kind := kindFromCode(f.code)
	if existedBefore && (kind == core.MutationUntracked || kind == core.MutationAdded) {
		kind = core.MutationModified
	}
	added, deleted := src.lineStats(path, kind == core.MutationUntracked)
	return core.Mutation{
		Path:            path,
		Kind:            kind,
		Hash:            f.hash,
		Added:           added,
		Deleted:         deleted,
		Sensitive:       IsSensitive(path),
		HostBookkeeping: isHostLocalState(path),
		Derived:         isDerivedCode(f.code),
	}
}

// captureChanged returns the current changed-vs-HEAD fileset with content hashes.
func (r *Repo) captureChanged() (map[string]changedFile, error) {
	// -z, not the default line format: git QUOTES and C-escapes any path outside plain
	// ASCII (core.quotepath is on by default), so "café.txt" arrived as the literal
	// "caf\303\251.txt" — a path that does not exist on disk, so it hashed to "" and a
	// gate could overwrite the real file with the diff seeing nothing. -z emits raw
	// bytes, NUL-terminated: no quoting, no escaping, and no ambiguity for a name holding
	// a space, a newline, or the literal " -> " that the old rename split keyed on.
	//
	// --ignored=matching, not the default (which omits ignored paths entirely): a
	// read-only worker that overwrote a gitignored file produced "mutations: null",
	// because the audit could only see what git reports. "matching" reports a path
	// ignored by a FILE pattern individually — that is the case that matters, and the one
	// we hash — and collapses a whole ignored DIRECTORY (node_modules/, dist/, target/)
	// into a single trailing-slash entry, which we skip. That line is deliberate:
	// declaring a directory ignored is the project saying "this subtree is derived
	// output, not my work", and walking + hashing 50k node_modules files on every worker
	// start and finish would cost far more than it buys. It is a documented limit.
	out, err := r.git("status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return nil, err
	}
	root, err := openRoot(r.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	m := map[string]changedFile{}
	for _, rec := range parsePorcelainZ(out) {
		if isRuntimePath(rec.path) || strings.HasSuffix(rec.path, "/") {
			continue // .vichu/ is the kernel's own runtime; a trailing slash is a
			// collapsed ignored directory (see above), not a file we can hash.
		}
		// Host-local state (settings.local.json) is NOT skipped: it is captured like any
		// other change and only exempted from the read-only policy later. Skipping it
		// here would erase it from mutations.json — and that file is the host's permission
		// allowlist, so "we never saw it change" is not something we get to say.
		hash, e, herr := hashEntry(root, rec.path)
		if herr != nil {
			// Git says this path changed and we cannot read it. We can neither audit it
			// nor back it up, so we do not pretend otherwise — the caller stops.
			return nil, herr
		}
		m[rec.path] = changedFile{code: rec.code, hash: hash, exists: e.kind != entryMissing, mode: e.mode}
	}
	return m, nil
}

// porcelainRec is one record of `git status --porcelain=v1 -z`: the two-column status
// code and the path, raw (-z never quotes or escapes).
type porcelainRec struct {
	code string
	path string
}

// parsePorcelainZ splits the NUL-terminated records of `git status --porcelain=v1 -z`.
// A rename or copy is followed by a SECOND record holding the ORIGINAL path; it is
// consumed here and dropped, because the tracker cares about where the content is now.
func parsePorcelainZ(out string) []porcelainRec {
	fields := strings.Split(out, "\x00")
	recs := make([]porcelainRec, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue // "XY " plus at least one byte of path; also skips the trailing empty
		}
		code, p := f[:2], f[3:]
		if strings.ContainsAny(code, "RC") {
			i++ // the next record is the original path, not a status of its own
		}
		recs = append(recs, porcelainRec{code: code, path: p})
	}
	return recs
}

// lineStats returns approximate added/deleted line counts for a mutated path.
func (r *Repo) lineStats(path string, untracked bool) (added, deleted int) {
	full := filepath.Join(r.root, filepath.FromSlash(path))
	if untracked {
		return countLines(full), 0
	}
	out, err := r.git("diff", "--numstat", "HEAD", "--", path)
	if err != nil || out == "" {
		return countLines(full), 0 // ignored paths have no HEAD to diff against
	}
	fields := strings.Fields(out)
	if len(fields) >= 2 {
		added, _ = strconv.Atoi(fields[0]) // "-" (binary) parses to 0
		deleted, _ = strconv.Atoi(fields[1])
	}
	return added, deleted
}

// kindFromCode maps a status code to a mutation kind. It says what the path is
// relative to the BASELINE — never whether the worker created it; only the tracker's
// before-snapshot knows that (see Tracker.mutationFor).
func kindFromCode(code string) core.MutationKind {
	switch {
	case strings.ContainsAny(code, "?!"): // "??" not in HEAD, "!!" ignored
		return core.MutationUntracked
	case strings.Contains(code, "D"):
		return core.MutationDeleted
	case strings.Contains(code, "A"):
		return core.MutationAdded
	default:
		return core.MutationModified
	}
}

// isDerivedCode reports the "!!" status: a path the project's own ignore rules exclude.
// It is INFORMATIONAL — it says where the path sits relative to your ignore rules, not
// that the path is disposable. An ignored file can be a private note, a certificate or a
// local config, so nothing is exempted from the mutation policy for being ignored; see
// core.Mutation.Derived and security.gateOutputs.
func isDerivedCode(code string) bool { return strings.Contains(code, "!") }

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
