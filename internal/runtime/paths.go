// Package runtime persists a run to flat files under .vichu/runs/<run-id>/.
// It is the only writer of the runtime format; everything else reads through it.
//
// Design rules:
//   - state.json is written atomically (temp + rename) so a crash never leaves
//     a half-written source of truth.
//   - events.ndjson is append-only and immutable.
//   - lock.json carries a pid + hostname + heartbeat so orphaned locks from
//     dead processes can be detected and reclaimed cross-platform.
package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

// DirName is the runtime directory created at the project root.
const DirName = ".vichu"

// Store owns reads and writes to a project's .vichu directory.
type Store struct {
	projectRoot string
	root        string
}

// Open returns a Store rooted at projectRoot/.vichu.
func Open(projectRoot string) *Store {
	return &Store{
		projectRoot: projectRoot,
		root:        filepath.Join(projectRoot, DirName),
	}
}

// ProjectRoot is the repository root the runtime lives under.
func (s *Store) ProjectRoot() string { return s.projectRoot }

// Root is the .vichu directory path.
func (s *Store) Root() string { return s.root }

// RunsDir is .vichu/runs.
func (s *Store) RunsDir() string { return filepath.Join(s.root, "runs") }

// RunDir is .vichu/runs/<run-id>.
func (s *Store) RunDir(runID string) string { return filepath.Join(s.RunsDir(), runID) }

func (s *Store) statePath(runID string) string { return filepath.Join(s.RunDir(runID), "state.json") }
func (s *Store) eventsPath(runID string) string {
	return filepath.Join(s.RunDir(runID), "events.ndjson")
}

// HostPackScope is the reserved lock scope for host-pack install/uninstall. It is not a
// run id — those are timestamped (`run-…`) — so it can never collide with one. The lock is
// PROJECT-wide, not per-run, because two `vichu init --host` processes edit the same
// `.claude/settings.json`: without it they read the same allow-list, each append their
// rules, and the last write wins, silently dropping the other's.
const HostPackScope = "hostpack"

// lockPath is the lock file for a scope: a run, or the project-wide host-pack scope.
func (s *Store) lockPath(scope string) string {
	if scope == HostPackScope {
		return filepath.Join(s.root, "hostpack.lock.json")
	}
	return filepath.Join(s.RunDir(scope), "lock.json")
}

func (s *Store) workspacePath(runID string) string {
	return filepath.Join(s.RunDir(runID), "workspace.json")
}

// ContextPackPath is the auditable copy of the project context injected into workers.
func (s *Store) ContextPackPath(runID string) string {
	return filepath.Join(s.RunDir(runID), "contextpack.md")
}

// ConfigSnapshotPath is the frozen config used for this run.
func (s *Store) ConfigSnapshotPath(runID string) string {
	return filepath.Join(s.RunDir(runID), "config.snapshot.yaml")
}

// WorkerDir is workers/<worker-id> within a run.
func (s *Store) WorkerDir(runID, workerID string) string {
	return filepath.Join(s.RunDir(runID), "workers", workerID)
}

// GateDir is gates/<stage>/<n> within a run.
func (s *Store) GateDir(runID, stage string, n int) string {
	return filepath.Join(s.RunDir(runID), "gates", stage, strconv.Itoa(n))
}

// ReviewDir is reviews/<stage>/iteration-<n> within a run.
func (s *Store) ReviewDir(runID, stage string, iteration int) string {
	return filepath.Join(s.RunDir(runID), "reviews", stage, "iteration-"+strconv.Itoa(iteration))
}

// SummariesDir is summaries/ within a run.
func (s *Store) SummariesDir(runID string) string {
	return filepath.Join(s.RunDir(runID), "summaries")
}

// ArtifactsDir is artifacts/ within a run.
func (s *Store) ArtifactsDir(runID string) string {
	return filepath.Join(s.RunDir(runID), "artifacts")
}

// NewRunID builds a sortable, unique run id of the form run-YYYYMMDD-HHMMSS-xxxxxxxxxxxx.
//
// The random suffix is 6 bytes (48 bits), not 2. Two run starts in the same wall-clock
// second collide with birthday probability over the suffix space, and a 16-bit suffix put
// that at ~50% around 300 concurrent starts/second — at which point CreateRun silently
// overwrote the first run's state. 48 bits makes it negligible; CreateRun also now refuses
// an id that already exists, so a collision fails loudly instead of clobbering.
func NewRunID(now time.Time) string {
	return fmt.Sprintf("run-%s-%s", now.UTC().Format("20060102-150405"), randSuffix(6))
}

func randSuffix(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is effectively impossible; fall back to a fixed token.
		return "0000"
	}
	return hex.EncodeToString(b)
}
