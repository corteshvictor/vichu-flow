package workspace

import (
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// Repo is a git repository rooted at its top-level working directory.
type Repo struct {
	root string
}

// Detect verifies git is available and dir is inside a repository, returning a
// Repo rooted at the repository top level. It returns ErrNoGit or ErrNotRepo
// with actionable messages so callers can block a run cleanly.
func Detect(dir string) (*Repo, error) {
	if !GitAvailable() {
		return nil, ErrNoGit
	}
	cmd := exec.Command("git", "-C", dir, gitRevParse, "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return nil, ErrNotRepo
	}
	return &Repo{root: strings.TrimSpace(string(out))}, nil
}

// Root returns the repository top-level directory.
func (r *Repo) Root() string { return r.root }

// Snapshot captures the current git state: base commit, branch, and the set of
// dirty paths. It is persisted to workspace.json and compared on resume.
func (r *Repo) Snapshot(isolation string) (*core.Workspace, error) {
	if isolation == "" {
		isolation = core.IsolationCurrentWorktree
	}
	base, _ := r.git(gitRevParse, "HEAD") // empty on an unborn branch — that's fine
	branch, _ := r.git("branch", "--show-current")

	changed, err := r.captureChanged()
	if err != nil {
		return nil, err
	}
	dirty := make([]string, 0, len(changed))
	prints := make(map[string]string, len(changed))
	for p, f := range changed {
		dirty = append(dirty, p)
		prints[p] = f.hash
	}
	sort.Strings(dirty)
	return &core.Workspace{
		Isolation:    isolation,
		Branch:       branch,
		BaseSHA:      base,
		DirtyFiles:   dirty,
		Fingerprints: prints,
		CapturedAt:   time.Now().UTC(),
	}, nil
}

// FingerprintChanged returns the current changed-vs-HEAD fileset as a
// path→content-hash map (excluding VichuFlow's own runtime directory).
func (r *Repo) FingerprintChanged() (map[string]string, error) {
	changed, err := r.captureChanged()
	if err != nil {
		return nil, err
	}
	prints := make(map[string]string, len(changed))
	for p, f := range changed {
		prints[p] = f.hash
	}
	return prints, nil
}

// Drifted reports whether the live repo diverged from a snapshot in a way that
// makes it unsafe to keep working: the base commit moved, or the dirty fileset
// differs from the snapshot by name or by content. The returned string explains
// the drift when true.
func (r *Repo) Drifted(snap *core.Workspace) (bool, string, error) {
	base, _ := r.git(gitRevParse, "HEAD")
	if base != snap.BaseSHA {
		return true, "base commit changed since the run started", nil
	}
	current, err := r.FingerprintChanged()
	if err != nil {
		return false, "", err
	}
	for p, h := range current {
		eh, ok := snap.Fingerprints[p]
		if !ok {
			return true, "new uncommitted change to " + p, nil
		}
		if eh != h {
			return true, "content of " + p + " changed since the snapshot", nil
		}
	}
	for p := range snap.Fingerprints {
		if _, ok := current[p]; !ok {
			return true, "uncommitted change to " + p + " disappeared since the snapshot", nil
		}
	}
	return false, "", nil
}

// HeadSHA returns the current HEAD commit, or "" on an unborn branch.
func (r *Repo) HeadSHA() string {
	sha, _ := r.git(gitRevParse, "HEAD")
	return sha
}

// RestoreFromHEAD restores tracked files to their committed (HEAD) content,
// recreating ones that were deleted and reverting ones that were modified. Used
// to roll back a blocking gate's damage to files that were tracked-and-clean
// (so not held in a content backup). Returns how many paths were restored.
func (r *Repo) RestoreFromHEAD(paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	args := append([]string{"checkout", "HEAD", "--"}, paths...)
	if _, err := r.git(args...); err != nil {
		return 0, err
	}
	return len(paths), nil
}

// DirtyPaths returns the sorted set of paths that differ from HEAD.
func (r *Repo) DirtyPaths() ([]string, error) {
	return r.dirtyPaths()
}

// dirtyPaths returns the sorted set of paths reported by git status.
func (r *Repo) dirtyPaths() ([]string, error) {
	out, err := r.git("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p := parsePorcelainPath(line)
		if isRuntimePath(p) {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}
