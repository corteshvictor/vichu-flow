package workspace

import (
	"fmt"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// Provider is a workspace backend: it captures the repository state, tells the
// engine exactly what a worker changed, and can roll those changes back. v0.1–v0.2
// ship the Git provider; v0.3 adds a filesystem provider so VichuFlow runs in any
// folder, with or without a VCS. The engine depends only on this interface — it
// is the seam that makes Git recommended, not required.
type Provider interface {
	// Root is the workspace top-level directory.
	Root() string
	// Snapshot captures the current state (a baseline id + the dirty fileset),
	// persisted to workspace.json and compared on resume.
	Snapshot(isolation string) (*core.Workspace, error)
	// FingerprintChanged returns the current changed-vs-baseline fileset as a
	// path→content-hash map (excluding VichuFlow's own runtime directory).
	FingerprintChanged() (map[string]string, error)
	// BeginTracking snapshots the changed set before a worker runs; Tracker.Finish
	// diffs it after to report the worker's exact mutations.
	BeginTracking() (*Tracker, error)
	// ResumeTracking reconstructs a Tracker from a before-snapshot produced by
	// Tracker.Before, so a host-first `worker complete` can diff against what a
	// separate `worker start` process captured. It fails on a snapshot whose recorded
	// state is ambiguous — an older one that cannot say whether a file was absent or
	// merely unreadable — rather than guess between two opposite outcomes.
	ResumeTracking(before map[string]core.FileSig) (*Tracker, error)
	// BackupChanged captures the content of every currently-changed file, so a
	// blocking gate's damage to pre-existing work can be rolled back.
	BackupChanged() (*Backup, error)
	// RestoreBaseline restores paths to their baseline content (git: the HEAD
	// commit; filesystem: the snapshot copy), recreating deletions and reverting
	// edits. Returns how many paths were restored.
	RestoreBaseline(paths []string) (int, error)
	// BaseID identifies the baseline a snapshot was taken against — git: the HEAD
	// commit ("" on an unborn branch); filesystem: a snapshot id.
	BaseID() string
	// Kind names the backend ("git" or "filesystem"). It is persisted on the run
	// so resume reopens the same provider.
	Kind() string
}

// Workspace provider kinds, persisted in workspace.json and accepted by Open.
const (
	KindGit        = "git"
	KindFilesystem = "filesystem"
)

// The Git provider satisfies Provider; the filesystem provider (v0.3) too.
var _ Provider = (*Repo)(nil)

// Open returns the workspace provider for dir according to mode:
//   - "git": require a git repository (errors if git or the repo is missing).
//   - "filesystem": snapshot the tree under .vichu/, no VCS required.
//   - "auto" / "": use git when dir is inside a repository, otherwise fall back
//     to the filesystem provider — Git is recommended, never required.
func Open(dir, mode string) (Provider, error) {
	switch mode {
	case KindGit:
		return Detect(dir)
	case KindFilesystem:
		return OpenFilesystem(dir)
	case "", "auto":
		if repo, err := Detect(dir); err == nil {
			return repo, nil
		}
		return OpenFilesystem(dir)
	default:
		return nil, fmt.Errorf("unknown workspace provider %q (use auto, git, or filesystem)", mode)
	}
}
