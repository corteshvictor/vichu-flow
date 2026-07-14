package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/safeio"
)

// ErrRunNotFound is returned when a run id has no directory on disk.
var ErrRunNotFound = errors.New("run not found")

// CreateRun materializes a run directory and writes its initial state.
//
// It does NOT refuse an existing id: the host-first `run start` retry deliberately
// re-materializes an incomplete run under its reserved id. Collision protection for a
// freshly GENERATED id lives where the id is minted (see engine.createRun), which is the
// only place a new id can clash with an unrelated run.
func (s *Store) CreateRun(st *core.State) error {
	// No raw os.MkdirAll here: SaveState writes through the confined writer, which creates the
	// run dir under a root anchored at the PROJECT. A raw MkdirAll would follow a symlinked
	// .vichu and create runs/<id> OUTSIDE the project before the confined write refused it.
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
	if st.SchemaVersion > core.SchemaVersion {
		return fmt.Errorf("refusing to write run %s at schema_version %d with a binary that understands only %d — it would drop fields it cannot represent", st.RunID, st.SchemaVersion, core.SchemaVersion)
	}
	now := time.Now().UTC()
	if st.CreatedAt.IsZero() {
		st.CreatedAt = now
	}
	st.UpdatedAt = now
	return writeJSON(s.projectRoot, s.statePath(st.RunID), st)
}

// LoadState reads a run's state from disk. It refuses a state written by a NEWER schema than
// this binary understands: JSON silently drops fields it does not know, so a downgraded (or
// simply older) binary that loaded a future run and then saved it would ERASE that run's
// newer fields — a lossy round-trip presented as success. A future state must be read by a
// binary new enough for it.
func (s *Store) LoadState(runID string) (*core.State, error) {
	var st core.State
	if err := readJSON(s.projectRoot, s.statePath(runID), &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if st.SchemaVersion > core.SchemaVersion {
		return nil, fmt.Errorf("run %s was written with schema_version %d, but this vichu understands only up to %d — upgrade vichu to read or drive this run (an older binary would silently drop its newer fields)", runID, st.SchemaVersion, core.SchemaVersion)
	}
	return &st, nil
}

// RunExists reports whether a run directory with a state.json exists.
func (s *Store) RunExists(runID string) bool {
	ok, _ := existsConfined(s.projectRoot, s.statePath(runID))
	return ok
}

// ListRuns returns run ids that have a state.json, newest first.
func (s *Store) ListRuns() ([]string, error) {
	entries, err := readDirConfined(s.projectRoot, s.RunsDir())
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

// maxEventLineBytes bounds a single event line, shared by the readers below.
const maxEventLineBytes = 8 * 1024 * 1024

// AppendEvent appends one normalized event to the run's events.ndjson, creating it if absent (the
// first event, run_created, is what materializes the log).
func (s *Store) AppendEvent(ev core.Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// Confined + no symlink following: events.ndjson lives under agent-writable .vichu/, and
	// a plain os.OpenFile(O_APPEND) followed a symlink an agent could plant there, appending
	// the kernel's audit trail into an arbitrary file. OpenAppend also creates the run dir through
	// the confined root, so a symlinked .vichu cannot make the append land outside the project.
	r, rel, err := confine(s.projectRoot, s.eventsPath(ev.Run))
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	f, err := r.OpenAppend(rel, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// AuditAppender holds an OPEN descriptor to a run's audit log across a validate-then-append sequence,
// so the append lands on the EXACT file that was validated. cancel needs this: it validates the log,
// saves the canceled state, then records run_canceled — three steps a concurrent same-user process can
// interleave. With two separate opens (validate, then append) a log REPLACED in between could absorb
// the event while the real history is lost, and cancel would still exit 0. One held descriptor binds
// the append to the validated inode; the identity check (Append) reports a repointed path rather than
// lying about having recorded the cancel.
type AuditAppender struct {
	root   *safeio.Root
	f      *os.File
	rel    string
	path   string
	openFI fs.FileInfo
}

// OpenVerifiedAudit opens the run's audit log ONCE (read+append, no-create, no-follow), reads it and
// checks its integrity, and returns the validated events plus a handle whose Append writes THROUGH the
// same descriptor. A missing or corrupt log returns a nil handle and an error (a materialized run
// always has a coherent one). The caller MUST Close the returned handle (nil-safe).
func (s *Store) OpenVerifiedAudit(runID string) (*AuditAppender, []core.Event, error) {
	path := s.eventsPath(runID)
	r, rel, err := confine(s.projectRoot, path)
	if err != nil {
		return nil, nil, err
	}
	f, err := r.OpenReadAppendExisting(rel, 0o644)
	if err != nil {
		_ = r.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("audit log %s is missing — a materialized run always has one (run_created); it was deleted or lost", path)
		}
		return nil, nil, err
	}
	a := &AuditAppender{root: r, f: f, rel: rel, path: path}
	openFI, err := f.Stat()
	if err != nil {
		_ = a.Close()
		return nil, nil, err
	}
	a.openFI = openFI
	data, err := io.ReadAll(f)
	if err != nil {
		_ = a.Close()
		return nil, nil, err
	}
	events, err := parseEventLines(data, path, runID)
	if err != nil {
		_ = a.Close()
		return nil, nil, err
	}
	if cerr := checkEventLogCoherence(events, path); cerr != nil {
		_ = a.Close()
		return nil, nil, cerr
	}
	return a, events, nil
}

// Append writes one event through the held descriptor and fsyncs, THEN confirms the path still
// resolves to the same file that was opened. If the log was replaced or removed since — even by a
// regular file — the event landed on the original, now-detached inode, so the canonical log does not
// reflect it; the caller is told and must not report success.
func (a *AuditAppender) Append(ev core.Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := a.f.Sync(); err != nil {
		return err
	}
	// Identity check: is the path still the inode we appended to? os.SameFile compares dev+inode
	// (Unix) / file index (Windows). A regular-file replacement — the case a no-create open misses —
	// changes the inode, so this catches it.
	currentFI, err := a.root.Lstat(a.rel)
	if err != nil {
		return fmt.Errorf("the audit log %s could not be re-checked after recording the event (%w) — it may have been replaced; do not trust the timeline", a.path, err)
	}
	if !os.SameFile(a.openFI, currentFI) {
		return fmt.Errorf("the audit log %s was replaced while the event was being recorded — the event went to the original file, which is no longer the run's log; the timeline is inconsistent, do not trust it", a.path)
	}
	return nil
}

// Close releases the descriptor and the confined root. Nil-safe so a caller can defer it even when
// OpenVerifiedAudit returned an error (nil handle).
func (a *AuditAppender) Close() error {
	if a == nil {
		return nil
	}
	var ferr error
	if a.f != nil {
		ferr = a.f.Close()
	}
	rerr := a.root.Close()
	if ferr != nil {
		return ferr
	}
	return rerr
}

// ValidateEventLog checks the run's audit log is present, non-empty, and structurally coherent — a
// precondition for ANY host-first mutation, whether or not the command carries an op-id. A
// MATERIALIZED run always has an events.ndjson beginning with run_created, so a missing or empty log
// is corruption, not "zero events". Confined, no-follow. It does NOT detect truncation after a valid
// prefix — that needs a hash chain (H8) — but it catches the empty log, the `{}` non-event, and the
// planted/out-of-order operation tuple that would suppress a real event through the dedup count.
func (s *Store) ValidateEventLog(runID string) error {
	_, err := s.LoadVerifiedEvents(runID)
	return err
}

// LoadVerifiedEvents reads the audit log ONCE, checks its integrity, and returns the events parsed
// from those SAME bytes. A caller that must both TRUST the log and READ it — cancel deciding whether
// a run_canceled already exists, status rendering the recent timeline — uses this instead of
// ValidateEventLog followed by ReadEvents. Two separate reads open a TOCTOU window: the bytes proven
// coherent are not the bytes acted on, so a log that changes (or is corrupted) in between is validated
// in one state and used in another. One read closes the window. A missing log is an ERROR here (a
// materialized run always carries run_created); callers that legitimately tolerate absence keep using
// ReadEvents, which returns nil for a missing log.
func (s *Store) LoadVerifiedEvents(runID string) ([]core.Event, error) {
	path := s.eventsPath(runID)
	data, err := readFileConfined(s.projectRoot, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("audit log %s is missing — a materialized run always has one (run_created); it was deleted or lost", path)
	}
	if err != nil {
		return nil, err
	}
	events, err := parseEventLines(data, path, runID)
	if err != nil {
		return nil, err
	}
	if err := checkEventLogCoherence(events, path); err != nil {
		return nil, err
	}
	return events, nil
}

// parseEventLines decodes each non-empty line and checks the fields every real event carries. Syntax
// alone is not integrity: `{}` parses but is not an event. Legacy events without the (op_id, op_fp,
// seq) tuple are accepted — only ts/run/event are mandatory here (the tuple is checked separately).
func parseEventLines(data []byte, path, runID string) ([]core.Event, error) {
	var events []core.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	line := 0
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		line++
		var ev core.Event
		if uerr := json.Unmarshal(raw, &ev); uerr != nil {
			return nil, fmt.Errorf("audit log %s line %d is unparseable: %w", path, line, uerr)
		}
		switch {
		case ev.Event == "":
			return nil, fmt.Errorf("audit log %s line %d has no event name", path, line)
		case ev.Run != runID:
			return nil, fmt.Errorf("audit log %s line %d names a different run %q", path, line, ev.Run)
		case ev.TS.IsZero():
			return nil, fmt.Errorf("audit log %s line %d has no timestamp", path, line)
		}
		events = append(events, ev)
	}
	if serr := sc.Err(); serr != nil {
		return nil, serr
	}
	return events, nil
}

// checkEventLogCoherence enforces that the log begins with run_created and that each event's
// operation tuple is either FULLY absent (a legacy/non-op event) or FULLY present (op_id, op_fp set
// and seq>=1). Within each (op_id, op_fp) the seqs must APPEAR IN ORDER 1,2,3,… — checked in the
// order events occur in the file, not as an unordered set. That order is load-bearing: the append
// path only ever writes an operation's events in increasing seq, and the dedup counts them, so a log
// where seq=2 precedes seq=1 (or repeats, or gaps) could not have come from a correct run and would
// let a planted/duplicate entry inflate the count and suppress a real event. Operations may interleave
// (A1,B1,A2,B2) — each op advances its OWN counter. A perfectly forged in-order seq=1 still needs
// intent↔event correlation (H8); this closes only what is detectable from the log alone.
func checkEventLogCoherence(events []core.Event, path string) error {
	if len(events) == 0 {
		return fmt.Errorf("audit log %s is empty — a materialized run always has at least run_created", path)
	}
	if events[0].Event != core.EventRunCreated {
		return fmt.Errorf("audit log %s does not begin with run_created (found %q) — its start was lost", path, events[0].Event)
	}
	nextSeq := map[string]int{}
	for i, ev := range events {
		anyOp := ev.OpID != "" || ev.OpFP != "" || ev.Seq != 0
		fullOp := ev.OpID != "" && ev.OpFP != "" && ev.Seq >= 1
		if anyOp && !fullOp {
			return fmt.Errorf("audit log %s line %d has a partial operation tuple (op_id/op_fp/seq must be all set or all absent)", path, i+1)
		}
		if !fullOp {
			continue
		}
		key := ev.OpID + "\x00" + ev.OpFP
		want := nextSeq[key]
		if want == 0 {
			want = 1 // first event seen for this operation
		}
		if ev.Seq != want {
			return fmt.Errorf("audit log %s line %d: operation %q has seq %d out of order (expected %d — an operation's events must appear in sequence 1,2,3…)", path, i+1, ev.OpID, ev.Seq, want)
		}
		nextSeq[key] = want + 1
	}
	return nil
}

// ReadEvents reads the full event timeline for a run.
func (s *Store) ReadEvents(runID string) ([]core.Event, error) {
	data, err := readFileConfined(s.projectRoot, s.eventsPath(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var events []core.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes) // tolerate long event lines
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
	return writeJSON(s.projectRoot, s.workspacePath(runID), ws)
}

// LoadWorkspace reads the run's git snapshot.
func (s *Store) LoadWorkspace(runID string) (*core.Workspace, error) {
	var ws core.Workspace
	if err := readJSON(s.projectRoot, s.workspacePath(runID), &ws); err != nil {
		return nil, err
	}
	return &ws, nil
}

// SaveWorkerStatus persists workers/<id>/status.json.
func (s *Store) SaveWorkerStatus(runID string, w *core.WorkerStatus) error {
	return writeJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, w.ID), "status.json"), w)
}

// LoadWorkerStatus reads workers/<id>/status.json.
func (s *Store) LoadWorkerStatus(runID, workerID string) (*core.WorkerStatus, error) {
	var w core.WorkerStatus
	if err := readJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), "status.json"), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// WriteWorkerFile writes an arbitrary file inside a worker's directory
// (prompt.md, result.md, result.json, session.json, ...).
func (s *Store) WriteWorkerFile(runID, workerID, name string, data []byte) error {
	return writeFileAtomic(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), name), data, 0o644)
}

// SaveMutationReport persists workers/<id>/mutations.json.
func (s *Store) SaveMutationReport(runID, workerID string, r *core.MutationReport) error {
	return writeJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), "mutations.json"), r)
}

// SaveConfigSnapshot persists the run's frozen config through the confined writer, so it can
// never be redirected out of the project by a symlink (a plain os.WriteFile via Config.Save
// followed one). data is the already-serialized YAML.
func (s *Store) SaveConfigSnapshot(runID string, data []byte) error {
	return writeFileAtomic(s.projectRoot, s.ConfigSnapshotPath(runID), data, 0o644)
}

// ConfigSnapshotExists reports whether the run's frozen config was persisted.
func (s *Store) ConfigSnapshotExists(runID string) bool {
	ok, _ := existsConfined(s.projectRoot, s.ConfigSnapshotPath(runID))
	return ok
}

// ReadConfigSnapshot reads the run's frozen config through the confined root, REFUSING a
// final symlink — an agent that replaced config.snapshot.yaml with a symlink (even one
// pointing back INSIDE the project, at the live tampered vichu.yaml) is rejected rather than
// followed. A direct regular-file overwrite of the snapshot is not caught here; that is H11
// (the agent writing .vichu directly), which needs signing/isolation, not a confined read.
func (s *Store) ReadConfigSnapshot(runID string) ([]byte, error) {
	r, err := safeio.Open(s.projectRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	rel, err := filepath.Rel(s.projectRoot, s.ConfigSnapshotPath(runID))
	if err != nil {
		return nil, err
	}
	return r.ReadFileNoFollow(filepath.ToSlash(rel))
}

// SaveOperation records a host-first transactional command's result under
// runs/<id>/operations/<op-id>.json, keyed by the caller's --op-id, so a retry
// returns the same result instead of re-applying the operation (idempotency).
func (s *Store) SaveOperation(runID, opID string, rec any) error {
	return writeJSON(s.projectRoot, filepath.Join(s.RunDir(runID), "operations", opID+".json"), rec)
}

// SaveGlobalOperation records an operation that runs BEFORE a run exists (run
// start), under .vichu/operations/<scope>/<op-id>.json, so a retry maps to the
// same run instead of creating a duplicate.
func (s *Store) SaveGlobalOperation(scope, opID string, rec any) error {
	return writeJSON(s.projectRoot, filepath.Join(s.root, "operations", scope, opID+".json"), rec)
}

// LoadGlobalOperation reads a global operation record. found=false (nil error)
// means this op-id has not run yet for the scope.
func (s *Store) LoadGlobalOperation(scope, opID string, rec any) (found bool, err error) {
	err = readJSON(s.projectRoot, filepath.Join(s.root, "operations", scope, opID+".json"), rec)
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
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return false, err
	}
	// Confined: the claim file lives under agent-writable .vichu/, so a raw os.MkdirAll +
	// os.OpenFile would create it (and its parent dirs) through a symlinked .vichu before
	// anything else refused the escape.
	r, rel, err := confine(s.projectRoot, filepath.Join(s.root, "operations", scope, opID+".json"))
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()

	f, err := r.OpenExclusive(rel, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			data, rerr := r.ReadFile(rel) // someone else claimed it — read it back confined
			if rerr != nil {
				return false, rerr
			}
			return false, json.Unmarshal(data, existing)
		}
		return false, err
	}
	// We created the claim file exclusively (O_EXCL). If we then cannot FILL it — disk full,
	// quota, an I/O fault — the file must not be left behind: a zero-byte or truncated record
	// would make every future attempt read it back, fail to decode, and burn the op-id forever.
	// Remove it on any failure so a retry re-reserves cleanly. fsync so a crash cannot leave the
	// name durable but the content empty. (A crash BETWEEN create and fill still leaves a partial
	// file; the fully-atomic temp-then-install fix is tracked with H7/H8.)
	_, werr := f.Write(append(data, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = r.Remove(rel)
		return false, werr
	}
	return true, nil
}

// LoadOperation reads a previously recorded operation result. found=false (with a
// nil error) means this op-id has not run yet.
func (s *Store) LoadOperation(runID, opID string, rec any) (found bool, err error) {
	err = readJSON(s.projectRoot, filepath.Join(s.RunDir(runID), "operations", opID+".json"), rec)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// SaveArtifact writes a named artifact's content to artifacts/<filename> within a
// run. The caller resolves filename from the core allowlist (core.ArtifactFilename)
// — this never takes a host-supplied path, preserving single-writer + no escape.
func (s *Store) SaveArtifact(runID, filename string, content []byte) error {
	return writeFileAtomic(s.projectRoot, filepath.Join(s.ArtifactsDir(runID), filename), content, 0o644)
}

// LoadArtifact reads an artifact's bytes CONFINED and WITHOUT following a symlink (final or any
// parent component). An artifact is evidence the kernel gates on, so it must come from the
// runtime store itself — a `artifacts/plan.md` an agent turned into a link to an external file
// must be refused, not read as if the kernel had produced it.
func (s *Store) LoadArtifact(runID, filename string) ([]byte, error) {
	return readFileConfined(s.projectRoot, filepath.Join(s.ArtifactsDir(runID), filename))
}

// SaveArtifactMeta persists an artifact's provenance (which stage/iteration produced
// it) to artifacts/<name>.json — distinct from the artifact's own <filename> — so
// checkRequiredArtifact can verify a required artifact is THIS stage entry's
// evidence, not a stale file from another stage or iteration.
func (s *Store) SaveArtifactMeta(runID, name string, meta core.ArtifactMeta) error {
	return writeJSON(s.projectRoot, filepath.Join(s.ArtifactsDir(runID), name+".json"), meta)
}

// LoadArtifactMeta reloads the provenance saved by SaveArtifactMeta.
func (s *Store) LoadArtifactMeta(runID, name string) (core.ArtifactMeta, error) {
	var meta core.ArtifactMeta
	err := readJSON(s.projectRoot, filepath.Join(s.ArtifactsDir(runID), name+".json"), &meta)
	return meta, err
}

// SaveWorkerTracking persists a worker's "before" snapshot to
// workers/<id>/tracking.json, so a host-first `worker complete` (a separate
// process) can reload exactly what `worker start` captured.
func (s *Store) SaveWorkerTracking(runID, workerID string, before map[string]core.FileSig) error {
	return writeJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), "tracking.json"), before)
}

// LoadWorkerTracking reloads the "before" snapshot saved by SaveWorkerTracking.
func (s *Store) LoadWorkerTracking(runID, workerID string) (map[string]core.FileSig, error) {
	var before map[string]core.FileSig
	if err := readJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), "tracking.json"), &before); err != nil {
		return nil, err
	}
	return before, nil
}

// SaveReviewVerdict persists reviews/<stage>/iteration-<n>/verdict.json — the
// validated review outcome, the runtime's public contract for a review stage.
func (s *Store) SaveReviewVerdict(runID, stage string, iteration int, v *core.Verdict) error {
	return writeJSON(s.projectRoot, filepath.Join(s.ReviewDir(runID, stage, iteration), "verdict.json"), v)
}

// LoadReviewVerdict reads reviews/<stage>/iteration-<n>/verdict.json — the
// persisted review decision, read back on resume so a crash between saving the
// verdict and advancing the stage loses no decision.
func (s *Store) LoadReviewVerdict(runID, stage string, iteration int) (*core.Verdict, error) {
	var v core.Verdict
	if err := readJSON(s.projectRoot, filepath.Join(s.ReviewDir(runID, stage, iteration), "verdict.json"), &v); err != nil {
		return nil, err
	}
	// A PERSISTED verdict is evidence the kernel branches on — validate it like a fresh one, so a
	// legacy/forged `approved` carrying a blocker (or `blocked` with no reason) cannot approve a
	// review off disk. Invalid → error (the caller blocks the transition); never rewritten.
	if err := core.ValidateVerdict(v); err != nil {
		return nil, fmt.Errorf("persisted review verdict is invalid: %w", err)
	}
	return &v, nil
}

// SaveStageSummary persists summaries/<stage>.md — the bounded summary later
// stages receive instead of full transcripts (context budget).
func (s *Store) SaveStageSummary(runID, stage string, md []byte) error {
	return writeFileAtomic(s.projectRoot, filepath.Join(s.SummariesDir(runID), stage+".md"), md, 0o644)
}

// readOwnedNoFollow reads a kernel-owned file under .vichu through the confined root,
// REFUSING a final symlink. These files (contextpack.md, summaries/*.md) are injected into a
// worker's prompt, and os.ReadFile follows a symlink an agent could plant — pointing it at,
// say, ~/.ssh/id_rsa would copy that secret into the prompt the next agent receives. A
// missing file, or a symlinked one, yields "" (no context) rather than leaking anything.
func (s *Store) readOwnedNoFollow(abs string) string {
	r, err := safeio.Open(s.projectRoot)
	if err != nil {
		return ""
	}
	defer func() { _ = r.Close() }()
	rel, err := filepath.Rel(s.projectRoot, abs)
	if err != nil {
		return ""
	}
	data, err := r.ReadFileNoFollow(filepath.ToSlash(rel))
	if err != nil {
		return ""
	}
	return string(data)
}

// StageSummary reads summaries/<stage>.md, or "" if the stage has none.
func (s *Store) StageSummary(runID, stage string) string {
	return s.readOwnedNoFollow(filepath.Join(s.SummariesDir(runID), stage+".md"))
}

// SaveContextPack writes the auditable contextpack.md for a run.
func (s *Store) SaveContextPack(runID string, md []byte) error {
	return writeFileAtomic(s.projectRoot, s.ContextPackPath(runID), md, 0o644)
}

// ContextPack reads the run's stored context pack, or "" if none.
func (s *Store) ContextPack(runID string) string {
	return s.readOwnedNoFollow(s.ContextPackPath(runID))
}

// ListWorkers returns the worker ids recorded under a run, sorted.
func (s *Store) ListWorkers(runID string) ([]string, error) {
	entries, err := readDirConfined(s.projectRoot, filepath.Join(s.RunDir(runID), "workers"))
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
	if err := readJSON(s.projectRoot, filepath.Join(s.WorkerDir(runID, workerID), "mutations.json"), &r); err != nil {
		return nil, err
	}
	return &r, nil
}
