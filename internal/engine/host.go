package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
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
	// Existing-only: a host-first op always targets a run that was already started. Acquiring the
	// EXISTING lock means a bad/stale run id returns "not found" without materializing its directory
	// as a side effect of a rejected command (I2).
	handle, err := e.store.AcquireLockExisting(runID)
	if err != nil {
		return opRecord{}, err
	}
	defer func() { _ = handle.Release() }()

	// Read the audit ONCE, validated, and dedup from THAT SAME snapshot below. Validating with one
	// read (ValidateEventLog) and then counting op-id entries with a SECOND read (CountOpEvents) let
	// the two see different bytes: a log swapped in between reads was proven coherent in the first and
	// consumed for dedup in the second, so a planted entry for this op-id could suppress the real
	// event. A corrupt or missing events.ndjson means we cannot trust (or even read) the run's
	// history, so we must not return a cached "success" on top of it, nor mutate blind.
	events, verr := e.store.LoadVerifiedEvents(runID)
	if verr != nil {
		return opRecord{}, fmt.Errorf("refusing to act on the run: its audit is unreadable (%w)", verr)
	}
	if rec, done, cerr := e.cachedOpResult(runID, opID, kind, fp); done {
		return rec, cerr
	}
	state, err := e.store.LoadState(runID)
	if err != nil {
		return opRecord{}, err
	}
	ensureMaps(state)

	if perr := e.pinFrozenRun(runID); perr != nil {
		return opRecord{}, perr
	}

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
	// Event scope: count what a previous attempt of THIS op already appended, so a replay re-emits
	// nothing. Counted from the SAME verified snapshot loaded above — no second read of the log — so
	// the dedup count and the coherence check can never disagree about what the log contains.
	if opID != "" {
		e.opEvents = newOpEventScope(events, opID, fp)
		defer func() { e.opEvents = nil }()
	}
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

// cachedOp reads the record of a previously-run operation. It distinguishes ABSENT (this
// op-id has not run) from UNREADABLE — and an unreadable record must NOT be treated as
// absent.
//
// That conflation was a real hole. The record is what carries an op-id's kind + fingerprint,
// and therefore what stops the SAME id being reused for a DIFFERENT operation. Corrupt the
// record of a `worker.start` and its op-id becomes free again: a `worker.complete` reusing it
// was accepted, closed the worker, and exited 0. Corruption silently removed the guard.
//
// So we fail closed. A run stopped by a corrupt record needs a human, which is annoying — and
// far better than re-applying an operation whose identity we cannot read.
func (e *Engine) cachedOp(runID, opID string) (opRecord, bool, error) {
	if opID == "" {
		return opRecord{}, false, nil
	}
	var rec opRecord
	ok, err := e.store.LoadOperation(runID, opID, &rec)
	if err != nil {
		return opRecord{}, false, fmt.Errorf("the record of operation %q exists but cannot be read (%w) — refusing to run, because that record is what stops this op-id being reused for a different operation. Inspect .vichu/runs/%s/operations/%s.json", opID, err, runID, opID)
	}
	return rec, ok, nil
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
	e.accrueNativeWallClock(state)
	if reason := e.agentBudgetExceeded(state); reason != "" {
		e.emit(state, state.CurrentStage, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
		e.block(state, reason)
		return true
	}
	return e.budgetBlocked(state)
}

// accrueNativeWallClock updates the run's wall-clock from the durable CreatedAt anchor. The
// headless loop keeps this via runState, but native (host-first) commands are separate processes
// with no such loop, so without this a native run's wall_clock stays 0 and maxWallClock never
// fires — a run could take hours under a 2h budget. It is measured as total elapsed since the run
// was created (idle/human waits INCLUDED — that is what wall-clock means) and enforced at the next
// transition: native cannot preempt a subagent already running. The `>` guard keeps it monotonic
// so a backwards clock cannot shrink the recorded spend.
func (e *Engine) accrueNativeWallClock(state *core.State) {
	if state.CreatedAt.IsZero() {
		return
	}
	if spent := time.Since(state.CreatedAt).Seconds(); spent > state.Budgets.WallClockSpentSeconds {
		state.Budgets.WallClockSpentSeconds = spent
	}
}

// WorkerStart opens a worker for host-first execution: it validates the workflow
// contract and the budget, captures and persists the BEFORE mutation snapshot (so
// `worker complete`, a separate process, can attribute exactly what the agent
// touched), marks the stage and worker active, and returns the worker id. If a
// budget is exhausted it blocks the run and returns a non-empty blockReason with
// an EMPTY workerID — the host must not launch a subagent in that case.
func (e *Engine) WorkerStart(runID, stageName, role, opID, token string) (workerID, blockReason string, err error) {
	rec, err := e.runOp(runID, opID, "worker.start", opFingerprint(runID, stageName, role), func(state *core.State) (opRecord, error) {
		if derr := requireDriver(state, token); derr != nil {
			return opRecord{}, derr
		}

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

		wid, oerr := e.openWorker(state, runID, stageName, role)
		if oerr != nil {
			return opRecord{}, oerr
		}
		return opRecord{WorkerID: wid}, nil
	})
	return rec.WorkerID, rec.BlockReason, err
}

// openWorker materializes a new worker: it takes the BEFORE snapshot (mutation tracking
// must start before the agent touches anything — an agent we cannot audit is one we refuse
// to run), records the worker, and points the run at it.
func (e *Engine) openWorker(state *core.State, runID, stageName, role string) (string, error) {
	state.Budgets.AgentInvocations++
	wid := fmt.Sprintf("%s-%02d", stageName, state.Budgets.AgentInvocations)
	tracker, terr := e.repo.BeginTracking()
	if terr != nil {
		return "", fmt.Errorf("cannot track worker mutations: %w — refusing to run an agent without an audit trail", terr)
	}
	if serr := e.store.SaveWorkerTracking(runID, wid, tracker.Before()); serr != nil {
		return "", serr
	}
	ws := &core.WorkerStatus{ID: wid, Role: role, Stage: stageName, Status: core.WorkerRunning, StartedAt: time.Now().UTC()}
	e.criticalWrite(e.store.SaveWorkerStatus(runID, ws), warnSaveWorkerStatus)
	state.Stages[stageName] = core.StageActive
	state.ActiveWorker = wid
	state.NextAction = "host running " + role
	e.saveState(state)
	e.emit(state, stageName, wid, core.EventWorkerStarted, map[string]any{"role": role, "host": true})
	if perr := e.persistFailed(); perr != nil {
		return "", perr // worker not durably opened → retry recovers
	}
	return wid, nil
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

// loadActiveWorker loads and validates the run's active worker for AUDITING. It admits
// only a worker that is still RUNNING: auditing is what produces the evidence, and a
// worker that is already closed has its evidence on disk, decided and depended on.
//
// The recovery of a worker that IS closed goes through resumeIfCommitted, which re-applies
// the recorded outcome instead of recomputing it. Letting a closed worker back in here —
// as this used to, for any worker still named as active — meant a retry could re-audit it
// and OVERWRITE its evidence with a different payload. That is the failure mode this
// kernel exists to prevent, arriving through the recovery path.
//
// A closed worker with no recorded fingerprint (a run from a build before the journal
// existed) therefore cannot be recovered here: we would have to guess that the payload
// matches, and guessing is what we are refusing to do. `vichu run resume` reconciles it.
func (e *Engine) loadActiveWorker(runID, workerID, activeWorker string) (*core.WorkerStatus, map[string]core.FileSig, error) {
	if activeWorker != workerID {
		return nil, nil, fmt.Errorf("worker %s is not the run's active worker (%q) — refusing to close a stale or wrong worker", workerID, activeWorker)
	}
	ws, err := e.store.LoadWorkerStatus(runID, workerID)
	if err != nil {
		return nil, nil, fmt.Errorf("worker %s: %w", workerID, err)
	}
	if ws.Status != core.WorkerRunning {
		return nil, nil, fmt.Errorf("worker %s is already %s — refusing to re-audit a closed worker and overwrite the evidence it was closed on. If a retry was interrupted, `vichu run resume --run %s` reconciles it", workerID, ws.Status, runID)
	}
	before, err := e.store.LoadWorkerTracking(runID, workerID)
	if err != nil {
		return nil, nil, fmt.Errorf("no `worker start` tracking for %s: %w", workerID, err)
	}
	return ws, before, nil
}

// commitWorkerClose is the COMMIT POINT of a worker-close operation. It writes the
// worker's terminal status together with the op-id that closed it AND the outcome
// that op decided (the mutation-policy block reason, if any) — one write, one
// journal entry.
//
// Everything BEFORE this call is replayable (result, artifacts, mutations.json and
// the review verdict are all recomputed or rewritten identically on a retry).
// Everything AFTER it — blocking, branching, advancing the run — is re-derivable
// from what this write recorded, which is what resumeIfCommitted does. That is what
// makes a crash anywhere in the operation recoverable without ever reporting a
// success that did not happen.
func (e *Engine) commitWorkerClose(state *core.State, ws *core.WorkerStatus, opID, fingerprint, blockReason string) error {
	fin := time.Now().UTC()
	ws.Status = core.WorkerDone
	ws.FinishedAt = &fin
	ws.CloseOpID = opID
	ws.CloseFingerprint = fingerprint
	ws.CloseBlockReason = blockReason
	e.criticalWrite(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
	if perr := e.persistFailed(); perr != nil {
		return perr // NOT committed — the worker stays active and the retry replays it
	}
	e.emit(state, ws.Stage, ws.ID, core.EventWorkerFinished, map[string]any{"host": true})
	return nil
}

// applyWorkerOutcome applies a COMMITTED plain-worker close to the run state.
//
// It runs once on the happy path and AGAIN on every recovery, so it must not just be
// safe to repeat — it must not repeat EFFECTS. A run that is already blocked was
// already applied by an earlier attempt: re-blocking it would append a second
// `run_blocked` event, and "do not duplicate effects on retry" includes the event log,
// which is the audit trail.
func (e *Engine) applyWorkerOutcome(state *core.State, _ *core.WorkerStatus, blockReason string) string {
	if state.Status == core.StatusBlocked {
		return state.BlockedReason // an earlier attempt already applied this outcome
	}
	if blockReason != "" {
		e.block(state, blockReason) // clears ActiveWorker and persists
		return blockReason
	}
	state.ActiveWorker = ""
	e.saveState(state)
	return ""
}

// applyReviewOutcome applies a COMMITTED review close: a mutation-policy violation
// blocks; otherwise the branch is computed from the PERSISTED verdict (never from the
// prompt), exactly as the headless runner does. Like applyWorkerOutcome it is replayed
// on recovery, so it short-circuits on any outcome an earlier attempt already applied.
func (e *Engine) applyReviewOutcome(state *core.State, ws *core.WorkerStatus, blockReason string) string {
	if state.Status == core.StatusBlocked {
		return state.BlockedReason // already applied (policy block, blocked verdict, budget)
	}
	if blockReason != "" {
		e.block(state, blockReason)
		return blockReason
	}
	state.ActiveWorker = ""
	stage, ok := e.stageOf(state, ws.Stage)
	if !ok {
		stage = workflows.Stage{Name: ws.Stage}
	}
	if state.Stages[stage.Name] == core.StageDone {
		e.saveState(state) // already branched; re-advancing would re-emit the transition
		return ""
	}
	if !e.decideFromVerdict(state, stage) || !e.advanceStage(state, stage) {
		return state.BlockedReason
	}
	return ""
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

// validate rejects usage a host cannot legitimately report. It runs BEFORE anything is
// written, because what is at stake is the budget itself — the mechanism that stops a
// runaway agent.
//
// A NEGATIVE cost or token count SUBTRACTS from what the run has spent, so a buggy or
// hostile host can spend forever by reporting -1000 every other worker. A NaN cost makes
// every `spent >= max` comparison false — NaN compares false against everything — so the
// cap silently stops existing. Neither failure is visible in any log: the run just never
// hits its limit.
func (o WorkerOutcome) validate() error {
	return validateReportedUsage(o.TokensIn, o.TokensOut, o.CostReported, o.CostUSD)
}

// validateReportedUsage rejects usage that would poison the budget: negative tokens or cost
// (which SUBTRACT from the spend), and NaN/Inf cost (every comparison against which is false,
// disabling the cost cap). It is shared by host-first (WorkerOutcome.validate, checked before
// any write) and headless (accrueReportedUsage) — the headless path used to accrue an
// adapter's numbers with no check, so a buggy/hostile adapter reporting -100 tokens drove the
// run under its own budget and still reached `completed`.
func validateReportedUsage(tokensIn, tokensOut int, costReported bool, costUSD float64) error {
	if tokensIn < 0 || tokensOut < 0 {
		return fmt.Errorf("token counts cannot be negative (in=%d, out=%d) — they would subtract from the run's budget", tokensIn, tokensOut)
	}
	if !costReported {
		return nil
	}
	switch {
	case math.IsNaN(costUSD):
		return errors.New("cost is NaN — every budget comparison against NaN is false, which would disable the cost cap entirely")
	case math.IsInf(costUSD, 0):
		return errors.New("cost is infinite — refusing to accrue it into the run budget")
	case costUSD < 0:
		return fmt.Errorf("cost cannot be negative (%v) — it would subtract from the run's budget", costUSD)
	}
	return nil
}

// payloadHash fingerprints WHAT the host is reporting: result, session, usage and
// artifacts. It goes into the op fingerprint, which binds an op-id to its payload.
//
// Without it, the same op-id resent with DIFFERENT evidence is treated as a retry: before
// the commit it would quietly overwrite the evidence, and after the commit it would answer
// "already done" while discarding what the host just sent. An op-id identifies one
// operation — and an operation is its payload, not just its target.
func (o WorkerOutcome) payloadHash() string {
	h := sha256.New()
	// LENGTH-PREFIXED, not separator-delimited. A separator (even NUL) can appear inside
	// a result or an artifact, and then two different payloads can hash the same — which
	// is precisely the collision that would let a changed payload masquerade as a retry.
	// Length prefixes make the encoding unambiguous.
	field := func(s string) { fmt.Fprintf(h, "%d:%s", len(s), s) }
	field(o.Result)
	field(o.SessionID)
	fmt.Fprintf(h, "|%d|%d|%v|%t|%t|", o.TokensIn, o.TokensOut, o.CostUSD, o.TokensReported, o.CostReported)
	names := make([]string, 0, len(o.Artifacts))
	for n := range o.Artifacts {
		names = append(names, n)
	}
	sort.Strings(names) // map order is random; the fingerprint must not be
	fmt.Fprintf(h, "artifacts=%d|", len(names))
	for _, n := range names {
		field(n)
		field(o.Artifacts[n])
	}
	return hex.EncodeToString(h.Sum(nil)) // full digest — this guards a state transition
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

// resumeIfCommitted resumes a worker-close operation that already reached its
// COMMIT POINT (the worker carries THIS op-id) but whose later steps — applying the
// decision to run state, writing the op record — may never have landed.
//
// It does NOT assume the operation finished: it RE-APPLIES the recorded outcome
// from the journal (`apply`), which is safe because every step reads durable
// evidence. "The worker is done" is not "the operation is done" — conflating the
// two is how a crash between the two writes turns into a phantom success.
//
// Only a retry of the SAME op-id resumes. Without an op-id (idempotency disabled),
// or with a different one, this returns false and the caller falls through to
// loadActiveWorker, which rejects a closed worker.
func (e *Engine) resumeIfCommitted(state *core.State, workerID, opID, fingerprint string, apply closeApplier) (opRecord, bool) {
	if opID == "" {
		return opRecord{}, false
	}
	ws, err := e.store.LoadWorkerStatus(state.RunID, workerID)
	if err != nil || ws.Status != core.WorkerDone || ws.CloseOpID != opID {
		return opRecord{}, false
	}
	// Same op-id, DIFFERENT payload: not a retry — the host is sending new evidence under
	// an id it already used. Fall through and let it fail rather than answer "already
	// done" while dropping what it just sent.
	//
	// An EMPTY recorded fingerprint is not a wildcard: it means the worker was closed by
	// a build that did not record one, so we cannot verify the payload matches, so we
	// must not claim it does. It falls through and fails loudly.
	if ws.CloseFingerprint != fingerprint {
		return opRecord{}, false
	}
	return opRecord{BlockReason: apply(state, ws, ws.CloseBlockReason)}, true
}

// closeApplier applies a committed close's outcome to the run state and returns the
// resulting block reason (empty if the run continues). It runs once on the happy
// path and again on every recovery, so it must be idempotent.
type closeApplier func(*core.State, *core.WorkerStatus, string) string

// WorkerComplete closes a host-first worker: it reloads the BEFORE snapshot, diffs
// the tree to attribute mutations (writing mutations.json and emitting sensitive /
// out-of-scope events), persists the result, marks the worker done, and clears
// ActiveWorker. It rejects a stale/wrong worker and blocks the run if the mutation
// policy is violated. A retry with the same op-id after the op record failed to
// write recovers from the already-applied state instead of erroring.
// auditWorkerClose gathers and persists a closing worker's EVIDENCE — its result +
// usage, its artifacts (plain workers only), and the mutation audit — and returns
// what that evidence decided (the mutation-policy block reason). It does NOT close
// the worker: the caller commits, so that a crash here leaves the worker open and
// the whole operation replayable. requireReview rejects a non-review stage.
func (e *Engine) auditWorkerClose(state *core.State, runID, workerID string, out WorkerOutcome, requireReview bool) (*core.WorkerStatus, workflows.Stage, string, error) {
	ws, before, lerr := e.loadActiveWorker(runID, workerID, state.ActiveWorker)
	if lerr != nil {
		return nil, workflows.Stage{}, "", lerr
	}
	stage, ok := e.stageOf(state, ws.Stage)
	if !ok {
		stage = workflows.Stage{Name: ws.Stage}
	}
	if requireReview && stage.Kind != workflows.KindReview {
		return nil, stage, "", fmt.Errorf("worker %s is on stage %q, which is not a review stage — use `worker complete`", workerID, ws.Stage)
	}
	// Validate the ENTIRE payload before a single byte is written (I2 in the plan).
	// Validating as we go would leave a rejected call half-persisted: the result saved,
	// the first artifact saved, and the second one rejected — and since Go's map order is
	// random, not even the same half twice.
	if verr := out.validate(); verr != nil {
		return nil, stage, "", verr
	}
	if !requireReview {
		if verr := validateArtifacts(ws.Stage, out.Artifacts); verr != nil {
			return nil, stage, "", verr
		}
	}

	e.persistWorkerResult(runID, workerID, out.result())
	e.applyUsage(state, ws, workerID, out)
	if !requireReview {
		if aerr := e.materializeArtifacts(state, ws.Stage, workerID, out.Result, out.Artifacts); aerr != nil {
			return nil, stage, "", aerr
		}
	}
	if perr := e.persistFailed(); perr != nil {
		return nil, stage, "", perr // result/artifacts not persisted → don't commit
	}
	tracker, terr := e.repo.ResumeTracking(before)
	if terr != nil {
		return nil, stage, "", terr // we cannot attribute this worker's changes — do not commit
	}
	blockReason := e.trackMutations(state, stage, workerID, e.runBaseSHA(runID), tracker)
	if perr := e.persistFailed(); perr != nil {
		return nil, stage, "", perr // mutations.json not written → don't commit
	}
	return ws, stage, blockReason, nil
}

func (e *Engine) WorkerComplete(runID, workerID, opID, token string, out WorkerOutcome) (string, error) {
	fp := opFingerprint(runID, workerID, out.payloadHash())
	rec, err := e.runOp(runID, opID, "worker.complete", fp, func(state *core.State) (opRecord, error) {
		if derr := requireDriver(state, token); derr != nil {
			return opRecord{}, derr
		}

		if r, ok := e.resumeIfCommitted(state, workerID, opID, fp, e.applyWorkerOutcome); ok {
			return r, nil
		}
		ws, _, blockReason, cerr := e.auditWorkerClose(state, runID, workerID, out, false)
		if cerr != nil {
			return opRecord{}, cerr
		}
		if perr := e.commitWorkerClose(state, ws, opID, fp, blockReason); perr != nil {
			return opRecord{}, perr
		}
		return opRecord{BlockReason: e.applyWorkerOutcome(state, ws, blockReason)}, nil
	})
	return rec.BlockReason, err
}

// validateArtifacts checks EVERY artifact name — against the allowlist
// (core.ArtifactCatalog) and against what THIS stage may produce
// (core.ArtifactAllowedForStage), so a `propose` worker can never write a `plan`: a
// stage's evidence must be its own. It writes nothing. Validating the whole batch up
// front is what lets a rejected call leave the runtime untouched.
func validateArtifacts(stage string, artifacts map[string]string) error {
	for name := range artifacts {
		if _, ok := core.ArtifactFilename(name); !ok {
			return fmt.Errorf("unknown artifact %q (allowed: proposal, plan, test_intent)", name)
		}
		if !core.ArtifactAllowedForStage(stage, name) {
			return fmt.Errorf("stage %q cannot produce a %q artifact — a stage's evidence must be its own", stage, name)
		}
	}
	return nil
}

// materializeArtifacts writes a worker's named artifacts under the run's artifacts/
// dir. Names were already checked by validateArtifacts — this only writes. Each
// artifact is stamped with provenance metadata (stage, iteration, hash). When a stage
// has a default artifact (e.g. propose → proposal) and the host did not pass it
// explicitly, the worker's result becomes that artifact.
func (e *Engine) materializeArtifacts(state *core.State, stage, workerID, resultText string, artifacts map[string]string) error {
	// Sorted, NOT map order. Each saveArtifact emits an artifact_saved event, and the
	// retry-dedup in emit() matches events by POSITION (op seq vs. how many already landed).
	// That is only sound if a replay emits the same events in the same order — but a stage
	// can carry two artifacts (plan + test_intent), and Go map iteration is random, so a
	// retry after a partial failure could emit them in the other order: the dedup would then
	// skip the one that never wrote (losing it) and re-emit the one already on disk
	// (duplicating it). A fixed order makes the position-based dedup exact.
	for _, name := range sortedKeys(artifacts) {
		if err := e.saveArtifact(state, stage, workerID, name, artifacts[name]); err != nil {
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

// sortedKeys returns a map's keys in a stable order, so callers that emit one event per
// entry produce a replay-stable sequence (see materializeArtifacts).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
func (e *Engine) ReviewComplete(runID, workerID, opID, token string, out WorkerOutcome) (string, error) {
	fp := opFingerprint(runID, workerID, out.payloadHash())
	rec, err := e.runOp(runID, opID, "review.complete", fp, func(state *core.State) (opRecord, error) {
		if derr := requireDriver(state, token); derr != nil {
			return opRecord{}, derr
		}

		if r, ok := e.resumeIfCommitted(state, workerID, opID, fp, e.applyReviewOutcome); ok {
			return r, nil
		}
		// Check the verdict envelope BEFORE touching anything. In host-first the envelope
		// is built by the HOST, so a malformed one is a protocol error, not review
		// evidence: rejecting it here writes nothing, so the reviewer stays OPEN and the
		// host simply retries with a well-formed verdict. (Headless is the opposite case:
		// there the AGENT wrote it, so garbage IS the finding and it blocks the run.)
		if _, verr := core.ParseVerdictFromResult(out.result()); verr != nil {
			return opRecord{}, fmt.Errorf("reviewer verdict is not usable: %w", verr)
		}
		ws, stage, policyBlock, cerr := e.auditWorkerClose(state, runID, workerID, out, true)
		if cerr != nil {
			return opRecord{}, cerr
		}
		// The verdict is EVIDENCE: persist it before the commit, so the branch can be
		// recomputed from it on the happy path and on any recovery alike.
		if policyBlock == "" {
			if verr := e.persistReviewVerdict(state, stage, out.result()); verr != nil {
				return opRecord{}, verr
			}
		}
		if perr := e.commitWorkerClose(state, ws, opID, fp, policyBlock); perr != nil {
			return opRecord{}, perr
		}
		return opRecord{BlockReason: e.applyReviewOutcome(state, ws, policyBlock)}, nil
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
	case stage.Kind == workflows.KindWorker && state.Stages[stageName] != core.StageActive:
		// Closing a WORKER stage that was never entered via `worker start` is a host PROTOCOL
		// error — a call out of order — not evidence that the run cannot continue. Per I1
		// (reject ≠ block), REJECT it here, before any write: it must not flip the run to
		// blocked, emit run_blocked, or write an op record. (Gate stages need no worker, and a
		// missing/invalid artifact AFTER a real worker is genuine evidence and still blocks.)
		return workflows.Stage{}, fmt.Errorf("stage %q has no worker evidence — run `worker start`/`worker complete` before closing it", stageName)
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
		if perr := e.persistFailed(); perr != nil {
			return false, perr // stage_started did not persist — do not run the gate (it may have effects)
		}
		return e.runGateStage(ctx, state, nil, stage)
	}
	// The "no worker evidence" case is now REJECTED earlier, in validateStageClose (before any
	// write), because it is a host protocol error, not a block. By here the worker stage is
	// active; a missing/invalid REQUIRED artifact after a real worker is genuine evidence.
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
	// Confined, no-follow read: an artifact is evidence the kernel gates on, so it must be the
	// regular file the store holds — a symlink to external data (or through a symlinked parent)
	// is refused here, which blocks and asks for the stage to be re-run rather than consuming
	// bytes from outside the runtime as if they were ours.
	data, err := e.store.LoadArtifact(state.RunID, filename)
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
func (e *Engine) StageClose(runID, stageName, opID, token string) (string, error) {
	rec, err := e.runOpHeartbeat(runID, opID, "stage.close", opFingerprint(runID, stageName), func(ctx context.Context, state *core.State) (opRecord, error) {
		if derr := requireDriver(state, token); derr != nil {
			return opRecord{}, derr
		}
		if rec, handled, rerr := e.stageCloseRecovery(state, stageName, opID); handled {
			return rec, rerr
		}
		// VALIDATE the call FIRST: an out-of-order close (wrong stage, no worker evidence) must be
		// REJECTED without writing anything (I1), not turned into a run BLOCK just because the
		// budget also happens to be exhausted. Only a legit close reconciles the budget.
		stage, verr := e.validateStageClose(state, stageName)
		if verr != nil {
			return opRecord{}, verr
		}
		// Now reconcile the (native) wall-clock and any other budget, and BLOCK before advancing or
		// completing if it is exhausted: a run that ran past maxWallClock while the agent worked
		// must not reach the terminal stage and report `completed`. Native cannot preempt a
		// subagent mid-run, but it can refuse the transition when the command returns.
		if e.hostBudgetBlock(state) {
			return stageOutcome(state), nil
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

// cachedOpResult returns a previously-run operation's result when THIS op-id already ran. done=true
// means the caller should return (rec, err) immediately: either the cached result, an error reading
// the record, or a mismatch (the same op-id used for a DIFFERENT operation). done=false means run.
func (e *Engine) cachedOpResult(runID, opID, kind, fp string) (opRecord, bool, error) {
	rec, cached, err := e.cachedOp(runID, opID)
	if err != nil {
		return opRecord{}, true, err
	}
	if !cached {
		return opRecord{}, false, nil
	}
	if rec.Kind != kind || rec.Fingerprint != fp {
		return opRecord{}, true, fmt.Errorf("--op-id %q was already used for a different operation (%s) — use a fresh op-id per operation", opID, rec.Kind)
	}
	return rec, true, nil
}

// newOpEventScope counts, from the ALREADY-VERIFIED event snapshot, how many events THIS op-id has
// appended — so a replay re-emits none. It reads the same bytes runOpCtx proved coherent (no second
// file read), so the count can never come from a snapshot other than the one that was validated.
func newOpEventScope(events []core.Event, opID, fp string) *opEventScope {
	already := 0
	for _, ev := range events {
		if ev.OpID == opID && ev.OpFP == fp {
			already++
		}
	}
	return &opEventScope{opID: opID, opFP: fp, alreadyWritten: already}
}

// stageCloseRecovery handles a `stage close` on a stage that is ALREADY done: the close took
// effect but the op record never wrote. handled=false means "proceed normally". Recovery is
// reconstructed ONLY for the op-id that actually closed it — proven by that op-id already having
// events in the log (alreadyWritten>0). A DIFFERENT op-id trying to "close" an already-closed
// stage is out-of-order and is rejected WITHOUT writing a record or event: otherwise a fresh
// op-id would receive the result of an operation it never ran (breaking op-id/payload idempotency).
func (e *Engine) stageCloseRecovery(state *core.State, stageName, opID string) (rec opRecord, handled bool, err error) {
	if opID == "" || state.Stages[stageName] != core.StageDone {
		return opRecord{}, false, nil
	}
	if e.opEvents != nil && e.opEvents.alreadyWritten > 0 {
		return stageOutcome(state), true, nil
	}
	return opRecord{}, true, fmt.Errorf("stage %q is already closed by another operation — a new op-id cannot re-close it; check `status`", stageName)
}

// runBaseSHA reads the run's baseline id from its persisted workspace snapshot.
func (e *Engine) runBaseSHA(runID string) string {
	if snap, err := e.store.LoadWorkspace(runID); err == nil {
		return snap.BaseSHA
	}
	return ""
}
