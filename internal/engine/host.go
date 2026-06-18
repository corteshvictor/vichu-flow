package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
)

// This file holds the host-first transactional commands: the kernel exposes
// these so a host pack (Claude Code, …) can drive a run from inside the agent —
// the host runs the native subagent, the kernel owns the verified state. Each
// command runs as a separate short-lived process and takes the run lock, so
// .vichu/runs stays single-writer. The kernel — not the host — decides the stage
// and role from the workflow and the persisted evidence.

// opRecord is the cached result of a host-first transactional command, keyed by
// its --op-id, so a retry returns the SAME result without re-applying. It also
// records the command Kind and a Fingerprint of the identifying args, so the same
// op-id reused for a DIFFERENT operation is rejected instead of silently returning
// a wrong cached result. Persisted to runs/<id>/operations/<op-id>.json.
type opRecord struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"fp,omitempty"`
	WorkerID    string `json:"worker_id,omitempty"`
	RunID       string `json:"run_id,omitempty"` // run-start: the run the op-id maps to
	BlockReason string `json:"block_reason,omitempty"`
}

// opFingerprint is a short stable digest of an operation's identifying args, so a
// cached op-id is only reused when it matches the same command + args.
func opFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// validOpID reports whether an op-id is a safe single token (no path traversal),
// so it can name a file under operations/ without escaping the run dir.
func validOpID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// runOp runs a host-first transactional command under the run lock, with
// idempotency: if --op-id was already recorded FOR THE SAME kind+fingerprint, it
// returns the cached result WITHOUT re-applying. An empty op-id disables
// idempotency (each call applies). Single-writer: only the kernel touches
// .vichu/runs.
func (e *Engine) runOp(runID, opID, kind, fp string, fn func(*core.State) (opRecord, error)) (opRecord, error) {
	return e.runOpCtx(runID, opID, kind, fp, false, func(_ context.Context, state *core.State) (opRecord, error) {
		return fn(state)
	})
}

// runOpHeartbeat is runOp for potentially LONG operations (running gates): it
// keeps the lock heartbeat fresh so another process can't reclaim the run mid-gate,
// and cancels fn's context if the lock is lost.
func (e *Engine) runOpHeartbeat(runID, opID, kind, fp string, fn func(context.Context, *core.State) (opRecord, error)) (opRecord, error) {
	return e.runOpCtx(runID, opID, kind, fp, true, fn)
}

// runOpCtx is the shared lock + idempotency + (optional) heartbeat machinery.
func (e *Engine) runOpCtx(runID, opID, kind, fp string, heartbeat bool, fn func(context.Context, *core.State) (opRecord, error)) (opRecord, error) {
	if opID != "" && !validOpID(opID) {
		return opRecord{}, fmt.Errorf("invalid --op-id %q (use letters, digits, '.', '_', '-')", opID)
	}
	handle, err := e.store.AcquireLock(runID)
	if err != nil {
		return opRecord{}, err
	}
	defer func() { _ = handle.Release() }()

	if rec, ok := e.cachedOp(runID, opID); ok {
		if rec.Kind != kind || rec.Fingerprint != fp {
			return opRecord{}, fmt.Errorf("--op-id %q was already used for a different operation (%s) — use a fresh op-id per operation", opID, rec.Kind)
		}
		return rec, nil // same op-id, same operation → return the cached result
	}
	state, err := e.store.LoadState(runID)
	if err != nil {
		return opRecord{}, err
	}
	ensureMaps(state)

	ctx := context.Background()
	if heartbeat {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		hbCtx, stopHB := context.WithCancel(context.Background())
		defer stopHB()
		go handle.StartHeartbeat(hbCtx, cancel) // lock lost → cancel fn's context
	}

	// Strict scope: any must-succeed write failure inside fn aborts the command,
	// so a reported success never leaves the runtime corrupt (single-writer).
	e.strict = &strictScope{}
	defer func() { e.strict = nil }()
	rec, err := fn(ctx, state)
	if err != nil {
		return opRecord{}, err
	}
	if e.strict.err != nil {
		return opRecord{}, fmt.Errorf("operation failed to persist (no success reported, retry is safe): %w", e.strict.err)
	}
	rec.Kind, rec.Fingerprint = kind, fp
	// Persisting the operation is part of success when --op-id is set: without the
	// record a retry would re-apply, so a failed write must NOT report success.
	if err := e.recordOp(runID, opID, rec); err != nil {
		return opRecord{}, err
	}
	return rec, nil
}

func (e *Engine) cachedOp(runID, opID string) (opRecord, bool) {
	if opID == "" {
		return opRecord{}, false
	}
	var rec opRecord
	if ok, _ := e.store.LoadOperation(runID, opID, &rec); ok {
		return rec, true
	}
	return opRecord{}, false
}

func (e *Engine) recordOp(runID, opID string, rec opRecord) error {
	if opID == "" {
		return nil
	}
	if err := e.store.SaveOperation(runID, opID, rec); err != nil {
		return fmt.Errorf("recording operation %q failed — a retry would no longer be safe: %w", opID, err)
	}
	return nil
}

// ensureMaps re-initializes maps dropped by `omitempty` when an empty state is
// reloaded across host-first processes, so per-stage counters never nil-panic.
func ensureMaps(state *core.State) {
	if state.Iterations == nil {
		state.Iterations = map[string]int{}
	}
	if state.Stages == nil {
		state.Stages = map[string]core.StageStatus{}
	}
}

// stageOf resolves a run's stage by name from its workflow.
func (e *Engine) stageOf(state *core.State, stageName string) (workflows.Stage, bool) {
	wf, err := workflows.Get(state.Workflow)
	if err != nil {
		return workflows.Stage{}, false
	}
	return wf.Stage(stageName)
}

// validateWorkerStart enforces the workflow contract for opening a worker: the
// run must be active with no worker running, the stage must be the CURRENT one,
// exist, be a worker/review stage, and the role must match. The kernel — not the
// host — decides the stage and role.
func (e *Engine) validateWorkerStart(state *core.State, stageName, role string) (workflows.Stage, error) {
	switch {
	case state.Status != core.StatusActive:
		return workflows.Stage{}, fmt.Errorf("run %s is %s — cannot start a worker", state.RunID, state.Status)
	case state.ActiveWorker != "":
		return workflows.Stage{}, fmt.Errorf("worker %s is already active — complete it before starting another", state.ActiveWorker)
	case stageName != state.CurrentStage:
		return workflows.Stage{}, fmt.Errorf("run is at stage %q, not %q — the kernel decides the stage from the workflow", state.CurrentStage, stageName)
	}
	stage, ok := e.stageOf(state, stageName)
	switch {
	case !ok:
		return workflows.Stage{}, fmt.Errorf("stage %q is not in workflow %q", stageName, state.Workflow)
	case stage.Kind != workflows.KindWorker && stage.Kind != workflows.KindReview:
		return workflows.Stage{}, fmt.Errorf("stage %q is a %s stage, not a worker stage", stageName, stage.Kind)
	case role != stage.Role:
		return workflows.Stage{}, fmt.Errorf("stage %q expects role %q, not %q", stageName, stage.Role, role)
	}
	return stage, nil
}

// hostBudgetBlock blocks the run if a budget that gates STARTING an agent is
// exhausted — the agent-invocation count and the run-resource budgets. Cost and
// tokens may be unknown in host-first, but invocations, iterations and resources
// still cut runaway spend. Returns true if it blocked.
func (e *Engine) hostBudgetBlock(state *core.State) bool {
	if reason := e.agentBudgetExceeded(state); reason != "" {
		e.emit(state, state.CurrentStage, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
		e.block(state, reason)
		return true
	}
	return e.budgetBlocked(state)
}

// WorkerStart opens a worker for host-first execution: it validates the workflow
// contract and the budget, captures and persists the BEFORE mutation snapshot (so
// `worker complete`, a separate process, can attribute exactly what the agent
// touched), marks the stage and worker active, and returns the worker id. If a
// budget is exhausted it blocks the run and returns a non-empty blockReason with
// an EMPTY workerID — the host must not launch a subagent in that case.
func (e *Engine) WorkerStart(runID, stageName, role, opID string) (workerID, blockReason string, err error) {
	rec, err := e.runOp(runID, opID, "worker.start", opFingerprint(runID, stageName, role), func(state *core.State) (opRecord, error) {
		// Recovery: with an op-id, if a prior attempt already opened the worker for
		// this stage+role (but the op record never wrote), return that worker id
		// instead of erroring with "worker already active".
		if wid, ok := e.recoverWorkerStart(state, stageName, role, opID); ok {
			return opRecord{WorkerID: wid}, nil
		}
		stage, verr := e.validateWorkerStart(state, stageName, role)
		if verr != nil {
			return opRecord{}, verr
		}
		if e.hostBudgetBlock(state) {
			return opRecord{BlockReason: state.BlockedReason}, nil // workerID stays empty
		}
		// Count this entry into the stage — enforces the per-stage iteration budget
		// and drives the review iteration counter (so the fix→review loop is bounded).
		if e.stageIterationsBlocked(state, stage) {
			return opRecord{BlockReason: state.BlockedReason}, nil
		}

		state.Budgets.AgentInvocations++
		wid := fmt.Sprintf("%s-%02d", stageName, state.Budgets.AgentInvocations)
		tracker, terr := e.repo.BeginTracking()
		if terr != nil {
			return opRecord{}, fmt.Errorf("cannot track worker mutations: %w — refusing to run an agent without an audit trail", terr)
		}
		if serr := e.store.SaveWorkerTracking(runID, wid, tracker.Before()); serr != nil {
			return opRecord{}, serr
		}
		ws := &core.WorkerStatus{ID: wid, Role: role, Stage: stageName, Status: core.WorkerRunning, StartedAt: time.Now().UTC()}
		e.criticalWrite(e.store.SaveWorkerStatus(runID, ws), warnSaveWorkerStatus)
		state.Stages[stageName] = core.StageActive
		state.ActiveWorker = wid
		state.NextAction = "host running " + role
		e.saveState(state)
		e.emit(state, stageName, wid, core.EventWorkerStarted, map[string]any{"role": role, "host": true})
		if perr := e.persistFailed(); perr != nil {
			return opRecord{}, perr // worker not durably opened → retry recovers
		}
		return opRecord{WorkerID: wid}, nil
	})
	return rec.WorkerID, rec.BlockReason, err
}

// recoverWorkerStart detects a `worker start` that already opened a worker for
// this stage+role (a prior attempt whose op record never wrote). With an op-id
// (the explicit idempotency signal) it returns that active worker id so the retry
// is recoverable instead of failing with "worker already active".
func (e *Engine) recoverWorkerStart(state *core.State, stageName, role, opID string) (string, bool) {
	if opID == "" || state.ActiveWorker == "" {
		return "", false
	}
	ws, err := e.store.LoadWorkerStatus(state.RunID, state.ActiveWorker)
	if err != nil || ws.Status != core.WorkerRunning || ws.Stage != stageName || ws.Role != role {
		return "", false
	}
	return state.ActiveWorker, true
}

// persistFailed returns the first must-succeed write failure recorded in the
// current strict scope, or nil. Host-first commands check it after each critical
// step to FAIL FAST — stopping before mutating more state — so a partial failure
// leaves the run recoverable on retry instead of half-applied.
func (e *Engine) persistFailed() error {
	if e.strict != nil && e.strict.err != nil {
		return fmt.Errorf("operation failed to persist (no success reported, retry is safe): %w", e.strict.err)
	}
	return nil
}

// loadActiveWorker loads and validates the run's active worker for closing. The
// real guard is ActiveWorker == workerID: a worker whose run no longer points at
// it (already fully completed, or a stale/wrong id) is rejected. A worker that is
// `done` but STILL the active worker is accepted — that means a prior attempt
// committed the status but crashed/failed before saving state; the retry finishes
// it idempotently. Shared by `worker complete` and `review complete`.
func (e *Engine) loadActiveWorker(runID, workerID, activeWorker string) (*core.WorkerStatus, map[string]core.FileSig, error) {
	if activeWorker != workerID {
		return nil, nil, fmt.Errorf("worker %s is not the run's active worker (%q) — refusing to close a stale or wrong worker", workerID, activeWorker)
	}
	ws, err := e.store.LoadWorkerStatus(runID, workerID)
	if err != nil {
		return nil, nil, fmt.Errorf("worker %s: %w", workerID, err)
	}
	before, err := e.store.LoadWorkerTracking(runID, workerID)
	if err != nil {
		return nil, nil, fmt.Errorf("no `worker start` tracking for %s: %w", workerID, err)
	}
	return ws, before, nil
}

// finishWorker marks a worker done, persists it, and clears the run's active
// worker. The status write goes FIRST: only if it persists do we clear
// ActiveWorker and emit — so a failed status write (caught by the caller via
// persistFailed) leaves the run pointing at the worker, and the retry recovers.
func (e *Engine) finishWorker(state *core.State, ws *core.WorkerStatus) {
	fin := time.Now().UTC()
	ws.Status = core.WorkerDone
	ws.FinishedAt = &fin
	if serr := e.store.SaveWorkerStatus(state.RunID, ws); serr != nil {
		e.criticalWrite(serr, warnSaveWorkerStatus)
		return // do NOT clear ActiveWorker — the worker isn't durably done
	}
	state.ActiveWorker = ""
	e.emit(state, ws.Stage, ws.ID, core.EventWorkerFinished, map[string]any{"host": true})
}

// WorkerOutcome is what the host reports about a completed worker: its result
// text, the optional native session id (for resume continuity), optional usage/
// cost the host may expose (unknown is fine in native mode), and any artifacts.
type WorkerOutcome struct {
	Result    string
	SessionID string
	TokensIn  int
	TokensOut int
	CostUSD   float64
	// TokensReported / CostReported are set by the host command for the kind of
	// usage the host actually exposed (--tokens-* sets tokens, --cost-usd sets cost).
	// They are independent: a host may surface tokens but not cost. When false, that
	// dimension stays "unknown" rather than being counted as a real zero.
	TokensReported bool
	CostReported   bool
	Artifacts      map[string]string
}

// result builds the core.Result persisted for the worker.
func (o WorkerOutcome) result() core.Result {
	return core.Result{
		Markdown: o.Result, SessionID: o.SessionID,
		TokensIn: o.TokensIn, TokensOut: o.TokensOut, CostUSD: o.CostUSD,
		TokensReported: o.TokensReported, CostReported: o.CostReported,
	}
}

// applyUsage records the host-reported session and usage on the worker and the
// run budget, mirroring the headless runner — so cost/token caps work in
// host-first too when the host exposes usage. Values left at zero are simply not
// counted (native cost/tokens may be unknown).
func (e *Engine) applyUsage(state *core.State, ws *core.WorkerStatus, workerID string, out WorkerOutcome) {
	if out.SessionID != "" {
		ws.SessionID = out.SessionID
	}
	// Cost and tokens are recorded independently — a native host may surface one but
	// not the other. The shared accounting leaves an unreported dimension untouched,
	// so it stays honestly "unknown" instead of a fake zero.
	e.accrueReportedUsage(state, ws.Stage, workerID, out.result())
}

// recoverIfApplied detects a worker-close operation that already took effect
// (status==done) but whose op record never got written — the op-record-write
// failure window. It reconstructs the result from state (idempotent recovery) and
// ensures ActiveWorker is cleared. Only used when an op-id was given (the explicit
// idempotency signal); without one, the strict guard rejects a double-close.
func (e *Engine) recoverIfApplied(state *core.State, workerID, opID string) (opRecord, bool) {
	if opID == "" {
		return opRecord{}, false
	}
	ws, err := e.store.LoadWorkerStatus(state.RunID, workerID)
	if err != nil || ws.Status != core.WorkerDone {
		return opRecord{}, false
	}
	// The operation already applied. Reconstruct its block reason from state and
	// finish any persistence the failed attempt left incomplete.
	br := ""
	if state.Status == core.StatusBlocked {
		br = state.BlockedReason
	}
	if state.ActiveWorker == workerID {
		state.ActiveWorker = ""
		e.saveState(state)
	}
	return opRecord{BlockReason: br}, true
}

// WorkerComplete closes a host-first worker: it reloads the BEFORE snapshot, diffs
// the tree to attribute mutations (writing mutations.json and emitting sensitive /
// out-of-scope events), persists the result, marks the worker done, and clears
// ActiveWorker. It rejects a stale/wrong worker and blocks the run if the mutation
// policy is violated. A retry with the same op-id after the op record failed to
// write recovers from the already-applied state instead of erroring.
// auditWorkerClose runs the close path shared by `worker complete` and `review
// complete`: validate the active worker, persist its result + usage (and, for a
// normal worker, its artifacts), attribute mutations, and finish it — failing fast
// at each critical write so a partial failure stays recoverable. requireReview
// rejects a non-review stage. Returns the stage and the mutation block reason.
func (e *Engine) auditWorkerClose(state *core.State, runID, workerID string, out WorkerOutcome, requireReview bool) (workflows.Stage, string, error) {
	ws, before, lerr := e.loadActiveWorker(runID, workerID, state.ActiveWorker)
	if lerr != nil {
		return workflows.Stage{}, "", lerr
	}
	stage, ok := e.stageOf(state, ws.Stage)
	if !ok {
		stage = workflows.Stage{Name: ws.Stage}
	}
	if requireReview && stage.Kind != workflows.KindReview {
		return stage, "", fmt.Errorf("worker %s is on stage %q, which is not a review stage — use `worker complete`", workerID, ws.Stage)
	}

	e.persistWorkerResult(runID, workerID, out.result())
	e.applyUsage(state, ws, workerID, out)
	if !requireReview {
		if aerr := e.materializeArtifacts(state, ws.Stage, workerID, out.Result, out.Artifacts); aerr != nil {
			return stage, "", aerr
		}
	}
	if perr := e.persistFailed(); perr != nil {
		return stage, "", perr // result/artifacts not persisted → don't finish
	}
	blockReason := e.trackMutations(state, stage, workerID, e.runBaseSHA(runID), e.repo.ResumeTracking(before))
	if perr := e.persistFailed(); perr != nil {
		return stage, "", perr // mutations.json not written → don't finish
	}
	e.finishWorker(state, ws)
	if perr := e.persistFailed(); perr != nil {
		return stage, "", perr // status not durably written → retry recovers
	}
	return stage, blockReason, nil
}

func (e *Engine) WorkerComplete(runID, workerID, opID string, out WorkerOutcome) (string, error) {
	rec, err := e.runOp(runID, opID, "worker.complete", opFingerprint(runID, workerID), func(state *core.State) (opRecord, error) {
		if r, ok := e.recoverIfApplied(state, workerID, opID); ok {
			return r, nil
		}
		_, blockReason, cerr := e.auditWorkerClose(state, runID, workerID, out, false)
		if cerr != nil {
			return opRecord{}, cerr
		}
		if blockReason != "" {
			e.block(state, blockReason)
		} else {
			e.saveState(state)
		}
		return opRecord{BlockReason: blockReason}, nil
	})
	return rec.BlockReason, err
}

// materializeArtifacts writes a worker's named artifacts under the run's
// artifacts/ dir. Names are validated against the allowlist (core.ArtifactCatalog)
// AND against what THIS stage may produce (core.ArtifactAllowedForStage) — so a
// `propose` worker can never write a `plan` (a stage's evidence must be its own).
// Each artifact is stamped with provenance metadata (stage, iteration, hash). When
// a stage has a default artifact (e.g. propose → proposal) and the host did not pass
// it explicitly, the worker's result becomes that artifact.
func (e *Engine) materializeArtifacts(state *core.State, stage, workerID, resultText string, artifacts map[string]string) error {
	for name, content := range artifacts {
		if _, ok := core.ArtifactFilename(name); !ok {
			return fmt.Errorf("unknown artifact %q (allowed: proposal, plan, test_intent)", name)
		}
		if !core.ArtifactAllowedForStage(stage, name) {
			return fmt.Errorf("stage %q cannot produce a %q artifact — a stage's evidence must be its own", stage, name)
		}
		if err := e.saveArtifact(state, stage, workerID, name, content); err != nil {
			return err
		}
	}
	// Default: a propose/plan stage with no explicit artifact uses its result.
	if def := core.DefaultArtifactForStage(stage); def != "" && artifacts[def] == "" && resultText != "" {
		if err := e.saveArtifact(state, stage, workerID, def, resultText); err != nil {
			return err
		}
	}
	return nil
}

// saveArtifact writes one artifact and its provenance metadata (which stage entry
// produced it, plus a content hash), then emits artifact_saved.
func (e *Engine) saveArtifact(state *core.State, stage, workerID, name, content string) error {
	filename, _ := core.ArtifactFilename(name)
	if err := e.store.SaveArtifact(state.RunID, filename, []byte(content)); err != nil {
		return fmt.Errorf("saving artifact %q: %w", name, err)
	}
	meta := core.ArtifactMeta{
		Name: name, Filename: filename, Stage: stage, WorkerID: workerID,
		Iteration: state.Iterations[stage], SHA256: sha256Hex(content),
		CapturedAt: time.Now().UTC(),
	}
	if err := e.store.SaveArtifactMeta(state.RunID, name, meta); err != nil {
		return fmt.Errorf("saving artifact metadata %q: %w", name, err)
	}
	e.emit(state, stage, workerID, core.EventArtifactSaved, map[string]any{"artifact": name})
	return nil
}

// sha256Hex is the full hex SHA-256 of s (artifact content fingerprint).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ReviewComplete closes a host-first REVIEW worker: it audits the reviewer's
// mutations, persists the structured verdict as the review's evidence, and
// transitions on it — approved → verify, needs_fixes → fix (bounded by the review
// iteration budget), blocked / invalid → block. The host passes the reviewer's
// verdict content. It reuses the engine's applyVerdict, so host-first and the full
// runner decide the branch identically — from persisted evidence, not the prompt.
func (e *Engine) ReviewComplete(runID, workerID, opID string, out WorkerOutcome) (string, error) {
	rec, err := e.runOp(runID, opID, "review.complete", opFingerprint(runID, workerID), func(state *core.State) (opRecord, error) {
		if r, ok := e.recoverIfApplied(state, workerID, opID); ok {
			return r, nil
		}
		stage, policyBlock, cerr := e.auditWorkerClose(state, runID, workerID, out, true)
		if cerr != nil {
			return opRecord{}, cerr
		}
		if policyBlock != "" {
			e.block(state, policyBlock)
			return opRecord{BlockReason: policyBlock}, nil
		}
		// Parse + persist the verdict, emit findings, and decide whether to advance
		// (approved / needs_fixes) or stop (blocked / invalid / budget exhausted).
		if !e.applyVerdict(state, stage, out.result()) {
			return opRecord{BlockReason: state.BlockedReason}, nil
		}
		if !e.advanceStage(state, stage) {
			return opRecord{BlockReason: state.BlockedReason}, nil
		}
		return opRecord{}, nil
	})
	return rec.BlockReason, err
}

// validateStageClose enforces that the current stage can be closed: the run is
// active with no worker running, the stage is the CURRENT one, exists, and is not
// a review stage (those use `review complete`).
func (e *Engine) validateStageClose(state *core.State, stageName string) (workflows.Stage, error) {
	switch {
	case state.Status != core.StatusActive:
		return workflows.Stage{}, fmt.Errorf("run %s is %s — cannot close a stage", state.RunID, state.Status)
	case state.ActiveWorker != "":
		return workflows.Stage{}, fmt.Errorf("worker %s is still active — `worker complete` it before closing the stage", state.ActiveWorker)
	case stageName != state.CurrentStage:
		return workflows.Stage{}, fmt.Errorf("run is at stage %q, not %q", state.CurrentStage, stageName)
	}
	stage, ok := e.stageOf(state, stageName)
	switch {
	case !ok:
		return workflows.Stage{}, fmt.Errorf("stage %q is not in workflow %q", stageName, state.Workflow)
	case stage.Kind == workflows.KindReview:
		return workflows.Stage{}, fmt.Errorf("stage %q is a review stage — use `review complete`", stageName)
	}
	return stage, nil
}

// verifyStageEvidence validates a stage's exit evidence before transition: a gate
// stage runs its configured gates; a worker stage must have been entered via
// `worker start`. ok=false means the run is now blocked (gate failed / no
// evidence) — the caller stops.
func (e *Engine) verifyStageEvidence(ctx context.Context, state *core.State, stage workflows.Stage) (bool, error) {
	if stage.Kind == workflows.KindGate {
		state.Stages[stage.Name] = core.StageActive
		e.emit(state, stage.Name, "", core.EventStageStarted, nil)
		return e.runGateStage(ctx, state, nil, stage)
	}
	if state.Stages[stage.Name] != core.StageActive {
		e.block(state, fmt.Sprintf("stage %q has no worker evidence — run `worker start`/`worker complete` first", stage.Name))
		return false, nil
	}
	if reason := e.checkRequiredArtifact(state, stage); reason != "" {
		e.block(state, reason)
		return false, nil
	}
	return true, nil
}

// checkRequiredArtifact enforces a stage's RequiresArtifact / section contract —
// e.g. the sdd `plan` stage must have produced a `plan` artifact with a `## Tests`
// section before implementing. Returns a blocking reason, or "" if satisfied.
func (e *Engine) checkRequiredArtifact(state *core.State, stage workflows.Stage) string {
	if stage.RequiresArtifact == "" {
		return ""
	}
	filename, ok := core.ArtifactFilename(stage.RequiresArtifact)
	if !ok {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(e.store.ArtifactsDir(state.RunID), filename))
	if err != nil {
		return fmt.Sprintf("stage %q requires a %q artifact — provide it with `worker complete --artifact %s=<file>`", stage.Name, stage.RequiresArtifact, stage.RequiresArtifact)
	}
	// An empty (or whitespace-only) artifact is no evidence at all — a proposer that
	// "wrote" a blank proposal must not satisfy the contract.
	if strings.TrimSpace(string(data)) == "" {
		return fmt.Sprintf("stage %q requires a non-empty %q artifact", stage.Name, stage.RequiresArtifact)
	}
	// Provenance: the artifact must be THIS stage entry's evidence — produced by this
	// stage at the current iteration, with content unchanged since the worker wrote it.
	if reason := e.checkArtifactProvenance(state, stage, data); reason != "" {
		return reason
	}
	if sec := stage.RequiresArtifactSection; sec != "" && !hasMarkdownSection(string(data), sec) {
		return fmt.Sprintf("the %q artifact must contain a `## %s` section (declare the tests before implementing)", stage.RequiresArtifact, sec)
	}
	return ""
}

// checkArtifactProvenance verifies the required artifact is THIS stage entry's
// evidence: produced by this stage, at the current iteration, and with content that
// still matches the hash recorded when the worker wrote it — so neither a stale
// file from another stage/iteration NOR a post-hoc edit of the file can pass the
// gate. Returns a block reason or "".
func (e *Engine) checkArtifactProvenance(state *core.State, stage workflows.Stage, data []byte) string {
	meta, err := e.store.LoadArtifactMeta(state.RunID, stage.RequiresArtifact)
	if err != nil || meta.Stage != stage.Name {
		return fmt.Sprintf("the %q artifact was not produced by stage %q — a stage's evidence must be its own", stage.RequiresArtifact, stage.Name)
	}
	if meta.Iteration != state.Iterations[stage.Name] {
		return fmt.Sprintf("the %q artifact is stale (from a previous %q iteration) — re-run the stage to produce it", stage.RequiresArtifact, stage.Name)
	}
	// The content must be exactly what the worker produced — an edit to the file
	// after `worker complete` is not the kernel-attributed evidence.
	if sha256Hex(string(data)) != meta.SHA256 {
		return fmt.Sprintf("the %q artifact changed after stage %q produced it — re-run the stage", stage.RequiresArtifact, stage.Name)
	}
	return ""
}

// hasMarkdownSection reports whether content has a markdown heading for section
// (e.g. "## Tests"), case-insensitive, at any heading level.
func hasMarkdownSection(content, section string) bool {
	want := strings.ToLower(section)
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		if strings.ToLower(strings.TrimSpace(strings.TrimLeft(t, "# "))) == want {
			return true
		}
	}
	return false
}

// stageOutcome builds the op result from the run's current state — an active run
// advanced cleanly; a blocked run carries its reason.
func stageOutcome(state *core.State) opRecord {
	if state.Status == core.StatusBlocked {
		return opRecord{BlockReason: state.BlockedReason}
	}
	return opRecord{}
}

// completeIfTerminal finishes the run if it has advanced into the terminal stage.
func (e *Engine) completeIfTerminal(state *core.State) {
	if next, ok := e.stageOf(state, state.CurrentStage); ok && next.Kind == workflows.KindTerminal {
		e.complete(state)
	}
}

// StageClose validates the current stage's evidence and transitions to the next —
// the host-first per-stage advance. A gate stage runs its gates (a failing gate
// blocks); a worker stage advances on its already-audited mutations. Advancing
// into the terminal stage completes the run. Returns a non-empty blockReason if
// the run is blocked. Runs gates under a heartbeat so long gates can't lose the
// lock.
func (e *Engine) StageClose(runID, stageName, opID string) (string, error) {
	rec, err := e.runOpHeartbeat(runID, opID, "stage.close", opFingerprint(runID, stageName), func(ctx context.Context, state *core.State) (opRecord, error) {
		// Recovery: with an op-id, if this stage already advanced (its close took
		// effect but the op record never wrote), reconstruct the result from state.
		if opID != "" && state.Stages[stageName] == core.StageDone {
			return stageOutcome(state), nil
		}
		stage, verr := e.validateStageClose(state, stageName)
		if verr != nil {
			return opRecord{}, verr
		}
		ok, gerr := e.verifyStageEvidence(ctx, state, stage)
		if gerr != nil {
			return opRecord{}, gerr
		}
		if !ok || !e.advanceStage(state, stage) {
			return stageOutcome(state), nil
		}
		e.completeIfTerminal(state)
		return opRecord{}, nil
	})
	return rec.BlockReason, err
}

// runBaseSHA reads the run's baseline id from its persisted workspace snapshot.
func (e *Engine) runBaseSHA(runID string) string {
	if snap, err := e.store.LoadWorkspace(runID); err == nil {
		return snap.BaseSHA
	}
	return ""
}
