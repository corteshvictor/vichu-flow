package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// ErrRunNotFound is returned when a run id has no directory on disk.
var ErrRunNotFound = errors.New("run not found")

// CreateRun materializes a run directory and writes its initial state.
func (s *Store) CreateRun(st *core.State) error {
	if err := os.MkdirAll(s.RunDir(st.RunID), 0o755); err != nil {
		return err
	}
	now := time.Now().UTC()
	if st.CreatedAt.IsZero() {
		st.CreatedAt = now
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = core.SchemaVersion
	}
	return s.SaveState(st)
}

// SaveState atomically persists a run's state, stamping schema version and
// timestamps. It mutates st.UpdatedAt (and CreatedAt / SchemaVersion if unset).
func (s *Store) SaveState(st *core.State) error {
	if st.SchemaVersion == 0 {
		st.SchemaVersion = core.SchemaVersion
	}
	now := time.Now().UTC()
	if st.CreatedAt.IsZero() {
		st.CreatedAt = now
	}
	st.UpdatedAt = now
	return writeJSON(s.statePath(st.RunID), st)
}

// LoadState reads a run's state from disk.
func (s *Store) LoadState(runID string) (*core.State, error) {
	var st core.State
	if err := readJSON(s.statePath(runID), &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	return &st, nil
}

// RunExists reports whether a run directory with a state.json exists.
func (s *Store) RunExists(runID string) bool {
	_, err := os.Stat(s.statePath(runID))
	return err == nil
}

// ListRuns returns run ids that have a state.json, newest first.
func (s *Store) ListRuns() ([]string, error) {
	entries, err := os.ReadDir(s.RunsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && s.RunExists(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	// Run ids embed a sortable timestamp; reverse-lexical = newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// LatestRun returns the id of the most recent run, or "" if there are none.
func (s *Store) LatestRun() (string, error) {
	ids, err := s.ListRuns()
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

// AppendEvent appends one normalized event to the run's events.ndjson.
func (s *Store) AppendEvent(ev core.Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(s.RunDir(ev.Run), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.eventsPath(ev.Run), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadEvents reads the full event timeline for a run.
func (s *Store) ReadEvents(runID string) ([]core.Event, error) {
	f, err := os.Open(s.eventsPath(runID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []core.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long event lines
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev core.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, sc.Err()
}

// SaveWorkspace persists the run's git snapshot.
func (s *Store) SaveWorkspace(runID string, ws *core.Workspace) error {
	return writeJSON(s.workspacePath(runID), ws)
}

// LoadWorkspace reads the run's git snapshot.
func (s *Store) LoadWorkspace(runID string) (*core.Workspace, error) {
	var ws core.Workspace
	if err := readJSON(s.workspacePath(runID), &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// SaveWorkerStatus persists workers/<id>/status.json.
func (s *Store) SaveWorkerStatus(runID string, w *core.WorkerStatus) error {
	return writeJSON(filepath.Join(s.WorkerDir(runID, w.ID), "status.json"), w)
}

// LoadWorkerStatus reads workers/<id>/status.json.
func (s *Store) LoadWorkerStatus(runID, workerID string) (*core.WorkerStatus, error) {
	var w core.WorkerStatus
	if err := readJSON(filepath.Join(s.WorkerDir(runID, workerID), "status.json"), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// WriteWorkerFile writes an arbitrary file inside a worker's directory
// (prompt.md, result.md, result.json, session.json, ...).
func (s *Store) WriteWorkerFile(runID, workerID, name string, data []byte) error {
	return writeFileAtomic(filepath.Join(s.WorkerDir(runID, workerID), name), data, 0o644)
}

// SaveMutationReport persists workers/<id>/mutations.json.
func (s *Store) SaveMutationReport(runID, workerID string, r *core.MutationReport) error {
	return writeJSON(filepath.Join(s.WorkerDir(runID, workerID), "mutations.json"), r)
}

// ConfigSnapshotExists reports whether the run's frozen config was persisted.
func (s *Store) ConfigSnapshotExists(runID string) bool {
	_, err := os.Stat(s.ConfigSnapshotPath(runID))
	return err == nil
}

// SaveOperation records a host-first transactional command's result under
// runs/<id>/operations/<op-id>.json, keyed by the caller's --op-id, so a retry
// returns the same result instead of re-applying the operation (idempotency).
func (s *Store) SaveOperation(runID, opID string, rec any) error {
	return writeJSON(filepath.Join(s.RunDir(runID), "operations", opID+".json"), rec)
}

// SaveGlobalOperation records an operation that runs BEFORE a run exists (run
// start), under .vichu/operations/<scope>/<op-id>.json, so a retry maps to the
// same run instead of creating a duplicate.
func (s *Store) SaveGlobalOperation(scope, opID string, rec any) error {
	return writeJSON(filepath.Join(s.root, "operations", scope, opID+".json"), rec)
}

// LoadGlobalOperation reads a global operation record. found=false (nil error)
// means this op-id has not run yet for the scope.
func (s *Store) LoadGlobalOperation(scope, opID string, rec any) (found bool, err error) {
	err = readJSON(filepath.Join(s.root, "operations", scope, opID+".json"), rec)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ReserveGlobalOperation atomically claims an op-id for a scope: it writes the
// record only if one does not already exist (O_EXCL). reserved=true means THIS
// caller won the claim; reserved=false with existing populated means another
// attempt already claimed it (read it back into existing). This is what makes
// `run start --op-id` create exactly one run even across crashes/retries.
func (s *Store) ReserveGlobalOperation(scope, opID string, rec any, existing any) (reserved bool, err error) {
	dir := filepath.Join(s.root, "operations", scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, opID+".json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, readJSON(path, existing) // someone else claimed it — read it
		}
		return false, err
	}
	defer func() { _ = f.Close() }()
	if _, werr := f.Write(append(data, '\n')); werr != nil {
		return false, werr
	}
	return true, nil
}

// LoadOperation reads a previously recorded operation result. found=false (with a
// nil error) means this op-id has not run yet.
func (s *Store) LoadOperation(runID, opID string, rec any) (found bool, err error) {
	err = readJSON(filepath.Join(s.RunDir(runID), "operations", opID+".json"), rec)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// SaveArtifact writes a named artifact's content to artifacts/<filename> within a
// run. The caller resolves filename from the core allowlist (core.ArtifactFilename)
// — this never takes a host-supplied path, preserving single-writer + no escape.
func (s *Store) SaveArtifact(runID, filename string, content []byte) error {
	return writeFileAtomic(filepath.Join(s.ArtifactsDir(runID), filename), content, 0o644)
}

// SaveArtifactMeta persists an artifact's provenance (which stage/iteration produced
// it) to artifacts/<name>.json — distinct from the artifact's own <filename> — so
// checkRequiredArtifact can verify a required artifact is THIS stage entry's
// evidence, not a stale file from another stage or iteration.
func (s *Store) SaveArtifactMeta(runID, name string, meta core.ArtifactMeta) error {
	return writeJSON(filepath.Join(s.ArtifactsDir(runID), name+".json"), meta)
}

// LoadArtifactMeta reloads the provenance saved by SaveArtifactMeta.
func (s *Store) LoadArtifactMeta(runID, name string) (core.ArtifactMeta, error) {
	var meta core.ArtifactMeta
	err := readJSON(filepath.Join(s.ArtifactsDir(runID), name+".json"), &meta)
	return meta, err
}

// SaveWorkerTracking persists a worker's "before" snapshot to
// workers/<id>/tracking.json, so a host-first `worker complete` (a separate
// process) can reload exactly what `worker start` captured.
func (s *Store) SaveWorkerTracking(runID, workerID string, before map[string]core.FileSig) error {
	return writeJSON(filepath.Join(s.WorkerDir(runID, workerID), "tracking.json"), before)
}

// LoadWorkerTracking reloads the "before" snapshot saved by SaveWorkerTracking.
func (s *Store) LoadWorkerTracking(runID, workerID string) (map[string]core.FileSig, error) {
	var before map[string]core.FileSig
	if err := readJSON(filepath.Join(s.WorkerDir(runID, workerID), "tracking.json"), &before); err != nil {
		return nil, err
	}
	return before, nil
}

// SaveReviewVerdict persists reviews/<stage>/iteration-<n>/verdict.json — the
// validated review outcome, the runtime's public contract for a review stage.
func (s *Store) SaveReviewVerdict(runID, stage string, iteration int, v *core.Verdict) error {
	return writeJSON(filepath.Join(s.ReviewDir(runID, stage, iteration), "verdict.json"), v)
}

// LoadReviewVerdict reads reviews/<stage>/iteration-<n>/verdict.json — the
// persisted review decision, read back on resume so a crash between saving the
// verdict and advancing the stage loses no decision.
func (s *Store) LoadReviewVerdict(runID, stage string, iteration int) (*core.Verdict, error) {
	var v core.Verdict
	if err := readJSON(filepath.Join(s.ReviewDir(runID, stage, iteration), "verdict.json"), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SaveStageSummary persists summaries/<stage>.md — the bounded summary later
// stages receive instead of full transcripts (context budget).
func (s *Store) SaveStageSummary(runID, stage string, md []byte) error {
	return writeFileAtomic(filepath.Join(s.SummariesDir(runID), stage+".md"), md, 0o644)
}

// StageSummary reads summaries/<stage>.md, or "" if the stage has none.
func (s *Store) StageSummary(runID, stage string) string {
	data, err := os.ReadFile(filepath.Join(s.SummariesDir(runID), stage+".md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// SaveContextPack writes the auditable contextpack.md for a run.
func (s *Store) SaveContextPack(runID string, md []byte) error {
	return writeFileAtomic(s.ContextPackPath(runID), md, 0o644)
}

// ContextPack reads the run's stored context pack, or "" if none.
func (s *Store) ContextPack(runID string) string {
	data, err := os.ReadFile(s.ContextPackPath(runID))
	if err != nil {
		return ""
	}
	return string(data)
}

// ListWorkers returns the worker ids recorded under a run, sorted.
func (s *Store) ListWorkers(runID string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.RunDir(runID), "workers"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// LoadMutationReport reads a worker's mutations.json.
func (s *Store) LoadMutationReport(runID, workerID string) (*core.MutationReport, error) {
	var r core.MutationReport
	if err := readJSON(filepath.Join(s.WorkerDir(runID, workerID), "mutations.json"), &r); err != nil {
		return nil, err
	}
	return &r, nil
}
