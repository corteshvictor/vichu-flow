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

// WriteWorkerFile writes an arbitrary file inside a worker's directory
// (prompt.md, result.md, result.json, session.json, ...).
func (s *Store) WriteWorkerFile(runID, workerID, name string, data []byte) error {
	return writeFileAtomic(filepath.Join(s.WorkerDir(runID, workerID), name), data, 0o644)
}

// SaveMutationReport persists workers/<id>/mutations.json.
func (s *Store) SaveMutationReport(runID, workerID string, r *core.MutationReport) error {
	return writeJSON(filepath.Join(s.WorkerDir(runID, workerID), "mutations.json"), r)
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
