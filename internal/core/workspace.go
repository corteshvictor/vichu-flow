package core

import (
	"errors"
	"strings"
	"time"
)

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
	// FingerprintVersion names the hashing rules these fingerprints were computed with.
	// Empty means a run snapshotted before symlinks were fingerprinted by their target
	// TEXT (older versions followed the link and hashed the content it pointed at), so its
	// hashes are not comparable with today's for any symlink. Resume refuses such a run
	// rather than read through a link to reconstruct the old value — see
	// FingerprintSymlinkTarget.
	FingerprintVersion string    `json:"fingerprint_version,omitempty"`
	CapturedAt         time.Time `json:"captured_at"`
}

// FingerprintSymlinkTarget is the current fingerprint format: a symlink is hashed by its
// target text, never by following it. Runs snapshotted before it carry no version.
const FingerprintSymlinkTarget = "symlink-target-v1"

// FileSig is a tracked path's porcelain status code, content hash, existence and mode —
// the serializable form of a worker's "before" snapshot. The host-first `worker start`
// persists it and `worker complete` (a SEPARATE process) reloads it, so mutation
// attribution survives across the two commands.
type FileSig struct {
	Code string `json:"code"`
	Hash string `json:"hash"`
	// Exists says the path was ON DISK when the snapshot was taken. It is a pointer so a
	// snapshot written before this field existed (nil) is distinguishable from one that
	// recorded absence (false) — see Existed.
	//
	// It is recorded rather than inferred from an empty hash, because that inference was
	// wrong in the one case that matters: a file that exists but cannot be READ (mode 000,
	// a denied ACL) also hashes to "". It therefore looked like a path the worker had just
	// created — so it was never backed up, and a gate could chmod it, overwrite it, and
	// leave the run reaching `completed` with the original content gone.
	Exists *bool `json:"exists,omitempty"`
	// Mode is the path's permission bits at snapshot time, so a mode-only change (a gate
	// widening 0600 to 0644 without touching a byte) is still a mutation. 0 means the
	// snapshot did not record it.
	Mode uint32 `json:"mode,omitempty"`
}

// Existed reports whether the path was on disk when the snapshot was taken.
//
// A snapshot written before Exists was recorded is AMBIGUOUS when the hash is empty and
// the code is not a deletion: the path may have been absent, or present and unreadable.
// Those two lead to opposite decisions — ignore it, or block and preserve it — so this
// returns an error rather than pick one. The caller restarts the worker; it does not guess.
func (f FileSig) Existed() (bool, error) {
	if f.Exists != nil {
		return *f.Exists, nil
	}
	if f.Hash != "" {
		return true, nil // a hash could only have come from a file we read
	}
	if strings.Contains(f.Code, "D") {
		return false, nil // recorded as deleted; absence is the point
	}
	return false, errors.New("written by an older VichuFlow that did not record whether the file was on disk, and its hash is empty — it was either absent or unreadable, and those need opposite handling")
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
	// HostBookkeeping marks a change the coding HOST made to its own machine-local
	// state (e.g. Claude Code rewriting .claude/settings.local.json the moment you
	// approve a command). It is recorded as evidence — with its hash, like everything
	// else — but exempt from the read-only policy, because blocking on it would fail
	// every read-only stage for a file the agent never touched.
	//
	// Recorded, not hidden: that file IS the host's permission allowlist, so an agent
	// that wrote to it would be granting itself tools. We refuse to say "no mutation
	// happened" about a file we chose not to block on. The audit reports it; the policy
	// ignores it; you can still see it and ask why it changed.
	HostBookkeeping bool `json:"host_bookkeeping,omitempty"`
	// Derived marks a path the project's OWN ignore rules exclude (a gitignored file). It is
	// INFORMATIONAL — it does NOT by itself exempt a change from the policy. The gate path uses it
	// to let a build write a genuinely NEW artifact (coverage, logs) without blocking; a gate that
	// modifies a PRE-EXISTING file still blocks unless `security.gateOutputs` allows it. The WORKER
	// path does not exempt it at all — only HostBookkeeping is exempt there (see
	// mutationPolicyVerdict), because a read-only worker has no business rewriting your coverage
	// file. And a Sensitive path (a gitignored `.env`) is never exempt, worker or gate.
	//
	// Recorded, not hidden — with its hash, like HostBookkeeping. Before this existed the audit
	// could not see a gitignored path at all, so a read-only worker that overwrote one reported
	// "no mutations". Being ignored is not the same as being invisible, or exempt.
	Derived bool `json:"derived,omitempty"`
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
