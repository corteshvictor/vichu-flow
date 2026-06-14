package core

import "time"

// Isolation modes describe where a run's agents are allowed to write.
const (
	// IsolationCurrentWorktree runs against the current working tree (v0.1 default).
	IsolationCurrentWorktree = "current-worktree"
	// IsolationGitWorktree runs in a dedicated git worktree (v0.5+).
	IsolationGitWorktree = "git-worktree"
	// IsolationTempClone runs in a throwaway clone (exploratory).
	IsolationTempClone = "temp-clone"
)

// Workspace is the snapshot captured when a run starts, persisted to
// workspace.json. On resume, a mismatch between this snapshot (plus the run's
// own recorded mutations) and the live workspace state means workspace drift and
// blocks the run.
type Workspace struct {
	// Provider is the workspace backend this run was snapshotted with ("git" or
	// "filesystem"). Resume reopens the same backend so a folder that later gains
	// (or loses) a .git can't silently flip provider and trigger avoidable drift.
	Provider   string   `json:"provider,omitempty"`
	Isolation  string   `json:"isolation"`
	Branch     string   `json:"branch"`
	BaseSHA    string   `json:"base_sha"`
	DirtyFiles []string `json:"dirty_files"`
	// Fingerprints maps each dirty path to its content hash at snapshot time, so
	// drift detection compares content, not just file names.
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
	CapturedAt   time.Time         `json:"captured_at"`
}

// MutationKind classifies a file change relative to the previous snapshot.
type MutationKind string

const (
	MutationModified  MutationKind = "modified"
	MutationAdded     MutationKind = "added"
	MutationDeleted   MutationKind = "deleted"
	MutationUntracked MutationKind = "untracked"
)

// Mutation is a single file changed by a worker.
type Mutation struct {
	Path string       `json:"path"`
	Kind MutationKind `json:"kind"`
	// Hash is the file's content hash after the worker ran ("" for deletions).
	// Resume uses it to tell the run's own changes apart from later external
	// edits to the same file.
	Hash       string `json:"hash,omitempty"`
	Added      int    `json:"added"`
	Deleted    int    `json:"deleted"`
	OutOfScope bool   `json:"out_of_scope,omitempty"`
	Sensitive  bool   `json:"sensitive,omitempty"`
}

// MutationReport is the set of changes a single worker produced, persisted to
// workers/<id>/mutations.json. The runtime computes this by diffing the repo
// before and after the worker runs — it never trusts the agent's own account.
type MutationReport struct {
	Worker     string     `json:"worker"`
	Stage      string     `json:"stage"`
	BaseSHA    string     `json:"base_sha"`
	Mutations  []Mutation `json:"mutations"`
	CapturedAt time.Time  `json:"captured_at"`
}
