// Package engine drives a workflow as a persistent state machine. It is the
// only component that mutates a run's status, and every transition it makes is
// authorized by verified evidence (worker results, gate verdicts) — never by an
// agent's self-report. All state lives on disk, so a run survives a crash and
// resumes from where it stopped.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/contextpack"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/gates"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/security"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// Engine executes workflows over a single repository.
type Engine struct {
	store    *runtime.Store
	registry *adapters.Registry
	cfg      *config.Config
	repo     workspace.Provider
	gates    *gates.Runner
	policy   security.Policy
	log      func(string)
	// strict, when set (during a host-first transactional command), makes
	// must-succeed persistence failures abort the command instead of warning —
	// so a host never gets "success" while the runtime is left corrupt. nil for
	// the full runner, which degrades loudly but keeps the loop going.
	strict *strictScope
	// opEvents is set while a host-first transactional command runs, so its events can be
	// made exactly-once across retries (see emit).
	opEvents *opEventScope
}

// strictScope collects the first must-succeed write failure within a host-first
// operation. The engine is single-goroutine per command process, so no locking.
type strictScope struct{ err error }

// Options configures a new Engine.
type Options struct {
	Store    *runtime.Store
	Registry *adapters.Registry
	Config   *config.Config
	Repo     workspace.Provider
	Log      func(string) // optional progress sink for the CLI
}

// New builds an Engine.
func New(opts Options) *Engine {
	logFn := opts.Log
	if logFn == nil {
		logFn = func(string) {
			// no-op: progress logging is optional (the CLI supplies a sink).
		}
	}
	return &Engine{
		store:    opts.Store,
		registry: opts.Registry,
		cfg:      opts.Config,
		repo:     opts.Repo,
		gates:    gates.NewRunner(opts.Store),
		policy:   security.New(opts.Config.Security),
		log:      logFn,
	}
}

// runState holds the per-run context the loop threads between stages.
type runState struct {
	wf          *workflows.Workflow
	pack        string
	baseSHA     string
	lastSummary string
	startedAt   time.Time
	// spentBefore is the wall-clock already consumed by previous sessions of
	// this run (resume accumulates spend; it never resets the meter).
	spentBefore float64
	// resumeSession maps a stage to an agent session id to continue when the run
	// re-enters it after a resume. Seeded once at Resume time (see
	// sessionsToResume) and consumed on first use; nil/empty for a fresh Start.
	resumeSession map[string]string
	// lockLost is set by the heartbeat when another process reclaims this run's
	// lock. The run loop stops promptly instead of working without ownership.
	lockLost atomic.Bool
}

// wallClockSpent is the run's cumulative wall-clock in seconds.
func (rs *runState) wallClockSpent() float64 {
	return rs.spentBefore + time.Since(rs.startedAt).Seconds()
}

// Start creates a new run and executes it until it completes or blocks.
func (e *Engine) Start(ctx context.Context, task, workflowName string) (*core.State, error) {
	cr, err := e.createRun(task, workflowName, "")
	if err != nil {
		return nil, err
	}
	// A pre-stage block (requireCleanTree=block on a dirty tree) leaves the run
	// blocked before a single stage ran — return it without acquiring the lock.
	if cr.state.Status != core.StatusActive {
		return cr.state, nil
	}

	handle, err := e.store.AcquireLock(cr.state.RunID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()

	rs := &runState{wf: cr.wf, pack: cr.pack.Markdown, baseSHA: cr.snap.BaseSHA, startedAt: time.Now()}
	return e.runWithHeartbeat(ctx, handle, cr.state, rs)
}

// StartRun materializes a new run and returns its persisted initial state WITHOUT
// executing any stage — the host-first lifecycle entry point (`vichu run start`).
// The host then drives the workers itself and calls the transactional commands to
// audit and advance the run. If requireCleanTree=block on a dirty tree, the run is
// returned already blocked.
func (e *Engine) StartRun(task, workflowName, opID string) (*core.State, string, error) {
	if opID == "" {
		cr, err := e.createRun(task, workflowName, "")
		if err != nil {
			return nil, "", err
		}
		return e.issueDriverToken(cr.state)
	}
	if !validOpID(opID) {
		return nil, "", fmt.Errorf("invalid --op-id %q (use letters, digits, '.', '_', '-')", opID)
	}

	// Idempotency: run start happens BEFORE a run exists. We RESERVE the op-id
	// atomically (O_EXCL) with a pre-generated run id BEFORE creating the run, so a
	// crash/retry maps to the same run instead of duplicating. Fingerprint uses the
	// NORMALIZED workflow, so "" and the default name are the same operation.
	const scope = "run-start"
	fp := opFingerprint(e.normalizeWorkflow(workflowName), task)
	reservedID := runtime.NewRunID(time.Now())
	rec := opRecord{Kind: scope, Fingerprint: fp, RunID: reservedID}
	var prev opRecord
	reserved, err := e.store.ReserveGlobalOperation(scope, opID, rec, &prev)
	if err != nil {
		return nil, "", err
	}
	if !reserved {
		if prev.Kind != scope || prev.Fingerprint != fp {
			return nil, "", fmt.Errorf("--op-id %q was already used for a different run start — use a fresh op-id", opID)
		}
		// Already reserved for this exact operation. Return the run only if it was
		// FULLY materialized (state + workspace + context pack + config); a prior
		// attempt that wrote state.json but crashed before the rest is incomplete —
		// re-create with the reserved id so resume/drift/context have all the pieces.
		//
		// A retry ROTATES the driver token. The token is a capability, not a result: we
		// cannot hand back one we never stored (only its hash is on disk), and re-issuing
		// invalidates whatever the lost response carried — which is the safe direction.
		if st, ok := e.completeRun(prev.RunID); ok {
			return e.issueDriverToken(st)
		}
		reservedID = prev.RunID
	}
	cr, err := e.createRun(task, workflowName, reservedID)
	if err != nil {
		return nil, "", err
	}
	return e.issueDriverToken(cr.state)
}

// issueDriverToken mints the run's driver capability, persists its HASH, and returns the
// token to the caller — the one and only time it exists outside memory. See driver.go.
func (e *Engine) issueDriverToken(state *core.State) (*core.State, string, error) {
	tok, err := mintDriverToken(state)
	if err != nil {
		return nil, "", err
	}
	if err := e.store.SaveState(state); err != nil {
		return nil, "", fmt.Errorf("cannot persist the run's driver token hash: %w", err)
	}
	return state, tok, nil
}

// completeRun returns a run's state only if it was fully materialized by
// createRun — state.json AND the auditable inputs (workspace, context pack,
// config snapshot). A run with state.json but missing pieces is incomplete (a
// crashed run start) and must be re-created, not returned.
func (e *Engine) completeRun(runID string) (*core.State, bool) {
	st, err := e.store.LoadState(runID)
	if err != nil {
		return nil, false
	}
	if _, werr := e.store.LoadWorkspace(runID); werr != nil {
		return nil, false
	}
	if e.store.ContextPack(runID) == "" {
		return nil, false
	}
	if !e.store.ConfigSnapshotExists(runID) {
		return nil, false
	}
	return st, true
}

// normalizeWorkflow resolves an empty workflow name to the configured default, so
// "" and the default name are the SAME operation for run-start idempotency.
func (e *Engine) normalizeWorkflow(name string) string {
	if name == "" {
		return e.cfg.Workflow.Default
	}
	return name
}

// createdRun is a run materialized on disk — snapshot, state, workspace, context
// pack, config snapshot — but not yet executed and not locked.
type createdRun struct {
	state *core.State
	wf    *workflows.Workflow
	pack  *contextpack.Pack
	snap  *core.Workspace
}

// createRun materializes a new run WITHOUT acquiring the lock or executing any
// stage. With an empty runID it generates one; with a non-empty runID it uses it
// (so `run start --op-id` can reserve the id atomically before creating the run).
// It is the lifecycle primitive shared by Start (which then runs the loop) and the
// host-first `run start` command (which stops here). If requireCleanTree=block on
// a dirty tree, the returned run is already blocked.
func (e *Engine) createRun(task, workflowName, runID string) (*createdRun, error) {
	wf, err := workflows.Get(e.normalizeWorkflow(workflowName))
	if err != nil {
		return nil, err
	}

	snap, err := e.repo.Snapshot(e.cfg.Workspace.Isolation)
	if err != nil {
		return nil, fmt.Errorf("capturing workspace snapshot: %w", err)
	}

	if runID == "" {
		var idErr error
		runID, idErr = e.freshRunID()
		if idErr != nil {
			return nil, idErr
		}
	}
	state := &core.State{
		RunID:        runID,
		Status:       core.StatusActive,
		Workflow:     wf.Name,
		Provider:     e.cfg.Workflow.Provider,
		Task:         task,
		CurrentStage: wf.Start,
		Stages:       pendingStages(wf),
		Iterations:   map[string]int{},
	}
	if err := e.store.CreateRun(state); err != nil {
		return nil, err
	}
	// run_created MUST land: `run start` reports the run (and issues a driver token) only if this
	// event is durable — a created run with no run_created in the audit is the kernel lying.
	if err := e.emitDurable(state, "", "", core.EventRunCreated, map[string]any{"workflow": wf.Name, "task": task}); err != nil {
		return nil, fmt.Errorf("cannot record run_created (%w) — refusing to report the run as created", err)
	}

	// Persist the auditable run inputs.
	if err := e.store.SaveWorkspace(runID, snap); err != nil {
		return nil, err
	}
	pack, err := contextpack.Build(e.repo.Root(), e.cfg)
	if err != nil {
		return nil, err
	}
	if err := e.store.SaveContextPack(runID, []byte(pack.Markdown)); err != nil {
		return nil, err
	}
	// The config snapshot is part of the auditable run inputs — a run is not
	// complete without it, so a failure here fails run creation (not a warn).
	if err := e.snapshotConfig(runID); err != nil {
		return nil, fmt.Errorf("saving config snapshot: %w", err)
	}

	e.applyCleanTreePolicy(state, snap)
	return &createdRun{state: state, wf: wf, pack: pack, snap: snap}, nil
}

// freshRunID mints a run id that is not already taken. NewRunID's 48-bit random suffix makes
// a clash with an existing run astronomically unlikely, but "unlikely" is not "never" under
// heavy concurrency, and a clash would have CreateRun overwrite an unrelated run's state
// (CreateRun does not reject an existing id — the reserved-op-id retry path re-materializes on
// purpose). So on the vanishing chance of a collision we mint another.
//
// Five collisions in a row is not chance — it is a clock stuck in the same second or a
// crypto/rand fault (which makes NewRunID's suffix a fixed, colliding value). We ERROR rather
// than return an id that would clobber an unrelated run. Refusing to start is strictly safer
// than silently overwriting someone else's state.
func (e *Engine) freshRunID() (string, error) {
	for range 5 {
		id := runtime.NewRunID(time.Now())
		if !e.store.RunExists(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not mint a unique run id after 5 attempts — a clock stuck in the same second or a crypto/rand fault; refusing to reuse an id that would overwrite an existing run")
}

// applyCleanTreePolicy enforces requireCleanTree against the snapshot's dirty
// set: block the run, warn, or allow silently.
func (e *Engine) applyCleanTreePolicy(state *core.State, snap *core.Workspace) {
	if len(snap.DirtyFiles) == 0 {
		return
	}
	switch e.cfg.Workspace.RequireCleanTree {
	case "block":
		e.block(state, fmt.Sprintf("working tree has %d uncommitted change(s); requireCleanTree=block", len(snap.DirtyFiles)))
	case "warn":
		e.emit(state, "", "", "dirty_tree_warning", map[string]any{"dirty_files": len(snap.DirtyFiles)})
		e.log(i18n.T("engine.dirty_warning", len(snap.DirtyFiles)))
	}
}

// runWithHeartbeat runs the stage loop while keeping the lock heartbeat fresh. If
// the lock is lost to another process, it cancels the run so this process stops
// promptly instead of working — and writing state — without ownership.
func (e *Engine) runWithHeartbeat(ctx context.Context, handle *runtime.Handle, state *core.State, rs *runState) (*core.State, error) {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go handle.StartHeartbeat(hbCtx, func() {
		rs.lockLost.Store(true)
		cancelRun()
	})
	return e.run(runCtx, state, rs)
}

// ResumeOptions controls how a run is resumed.
type ResumeOptions struct {
	// AcceptChanges re-baselines the workspace snapshot when drift is detected,
	// explicitly accepting external changes (e.g. the user fixed code by hand
	// after a gate failure) instead of blocking.
	AcceptChanges bool
}

// Resume continues an existing run, guarding against workspace drift.
func (e *Engine) Resume(ctx context.Context, runID string, opts ResumeOptions) (*core.State, error) {
	handle, err := e.acquireForResume(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()

	state, err := e.store.LoadState(runID)
	if err != nil {
		return nil, err
	}
	// Validate the audit UNDER THE LOCK, before deciding terminal or mutating: a corrupt/missing
	// log means the run's history is unreadable, so neither "already done" nor a new event can be
	// trusted on top of it. Fail closed.
	if verr := e.store.ValidateEventLog(runID); verr != nil {
		return nil, fmt.Errorf("refusing to resume: the run's audit is unreadable (%w)", verr)
	}
	if state.Status.Terminal() {
		return state, nil
	}
	wf, err := workflows.Get(state.Workflow)
	if err != nil {
		return nil, err
	}

	// VALIDATE before mutating: load and check the workspace, provider and drift WITHOUT
	// touching the config snapshot. Overwriting the snapshot first meant a later validation
	// failure (corrupt workspace.json, provider open error, blocked drift) left the run's
	// frozen policy already replaced by the live one — a partial effect from a failed resume.
	snap, err := e.store.LoadWorkspace(runID)
	if err != nil {
		return nil, fmt.Errorf("loading workspace snapshot: %w", err)
	}
	if err := e.reopenProviderForResume(snap); err != nil {
		return nil, err
	}
	snap, blocked, err := e.resolveDrift(state, runID, snap, opts)
	if err != nil {
		return nil, err
	}
	if blocked {
		return state, nil
	}

	state.Status = core.StatusActive
	state.BlockedReason = ""

	// Seed the agent session to continue BEFORE reconciling: reconcile clears
	// active-worker bookkeeping for interrupted workers, while the session seed
	// comes from the latest COMPLETED worker of the stage being re-entered.
	// Reconcile decides interrupted workers by the OLD frozen config — that is why the
	// re-snapshot below happens AFTER it, not before.
	resumeSession := e.sessionsToResume(state)
	if rerr := e.reconcileInterruptedWorkers(state); rerr != nil {
		return nil, rerr
	}

	// Persist the new config snapshot, THEN announce the resume. The run_resumed event used to
	// be emitted before this write, so a failed snapshot write left the audit claiming a resume
	// the command then reported as failed — the kernel lying. The event only lands once the
	// resume's durable effect (the re-frozen config) is on disk.
	if ferr := e.reSnapshotConfigForResume(runID); ferr != nil {
		return nil, ferr
	}
	e.emit(state, state.CurrentStage, "", core.EventRunResumed, nil)

	if state.Iterations == nil {
		state.Iterations = map[string]int{}
	}
	rs := &runState{
		wf:            wf,
		pack:          e.store.ContextPack(runID),
		baseSHA:       snap.BaseSHA,
		lastSummary:   e.lastSummaryOnDisk(wf, state),
		startedAt:     time.Now(),
		spentBefore:   state.Budgets.WallClockSpentSeconds,
		resumeSession: resumeSession,
	}
	return e.runWithHeartbeat(ctx, handle, state, rs)
}

// ReopenRun is the HOST-FIRST resume: it validates a run can be continued —
// re-opens the original workspace provider and checks for drift — and returns its
// state WITHOUT executing any stage or agent. The host then keeps driving via the
// transactional commands. (The headless loop-running resume is Engine.Resume,
// used by the deprecated `vichu resume` / CI fallback.)
func (e *Engine) ReopenRun(runID string, opts ResumeOptions) (*core.State, string, error) {
	handle, err := e.acquireForResume(runID)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = handle.Release() }()

	state, err := e.store.LoadState(runID)
	if err != nil {
		return nil, "", err
	}
	// Validate the audit UNDER THE LOCK, before deciding terminal or touching snapshots, config, the
	// token or the timeline: resuming on a corrupt/missing log would rotate the token and append
	// run_resumed on top of an unreadable history, and even reporting a terminal run as "done" leans
	// on a history we cannot read. Fail closed — a damaged run stays un-resumable until an explicit
	// repair (cancel is the escape hatch); the kernel must not build on an audit it cannot read.
	if verr := e.store.ValidateEventLog(runID); verr != nil {
		return nil, "", fmt.Errorf("refusing to resume: the run's audit is unreadable (%w)", verr)
	}
	if state.Status.Terminal() {
		return state, "", nil
	}

	// VALIDATE before mutating the snapshot — see Resume. Load and check workspace, provider
	// and drift first; a failure here must leave the frozen config untouched.
	snap, err := e.store.LoadWorkspace(runID)
	if err != nil {
		return nil, "", fmt.Errorf("loading workspace snapshot: %w", err)
	}
	if err := e.reopenProviderForResume(snap); err != nil {
		return nil, "", err
	}
	_, blocked, err := e.resolveDrift(state, runID, snap, opts)
	if err != nil {
		return nil, "", err
	}
	if blocked {
		return state, "", nil
	}
	// Mark active and reconcile interrupted workers, but DO NOT run the loop — the
	// host owns execution. Reconcile uses the OLD frozen config; re-snapshot after it.
	if state.Status == core.StatusBlocked {
		state.Status = core.StatusActive
		state.BlockedReason = ""
	}
	ensureMaps(state)
	if rerr := e.reconcileInterruptedWorkers(state); rerr != nil {
		return nil, "", rerr
	}
	// Past validation and reconcile: re-freeze the config from the current vichu.yaml.
	if ferr := e.reSnapshotConfigForResume(runID); ferr != nil {
		return nil, "", ferr
	}
	if state.Status == core.StatusBlocked {
		// Reconciliation found evidence it could not verify. That block must LAND — a
		// resume that reports "blocked" while state.json still says active is the kernel
		// lying in the other direction.
		if serr := e.store.SaveState(state); serr != nil {
			return nil, "", fmt.Errorf("cannot persist the block reconciliation found (%w): %s", serr, state.BlockedReason)
		}
		return state, "", nil
	}
	tok, err := e.rotateTokenAndAnnounceResume(state)
	if err != nil {
		return nil, "", err
	}
	return state, tok, nil
}

// rotateTokenAndAnnounceResume mints a fresh driver token, persists its hash, and records
// run_resumed — the tail of a host-first resume. Resume is the human's command (not pre-authorized
// to the pack), so it is the moment a leaked or lost token dies and a fresh one is issued. Both the
// hash write and the run_resumed event MUST land before the token is handed back: returning a token
// while state.json still carries the OLD hash hands out a key to nothing, and reporting success with
// no run_resumed in the audit is the kernel lying. On either failure it returns an error and NO
// token — the previous token stays valid, the honest recoverable outcome.
func (e *Engine) rotateTokenAndAnnounceResume(state *core.State) (string, error) {
	tok, err := mintDriverToken(state) // sets the NEW token hash on state, in memory only
	if err != nil {
		return "", err
	}
	// ORDER IS LOAD-BEARING: announce, THEN persist the rotated hash. The new hash is what
	// invalidates the old token, and it only reaches disk in SaveState below. Emitting first means
	// a failure at EITHER step leaves the old hash on disk — so "your previous token is still
	// valid" is TRUE, and the outcome is recoverable. Persisting the hash first and then failing
	// the event killed the old token while the message claimed it still worked (the kernel lying).
	if eerr := e.emitDurable(state, state.CurrentStage, "", core.EventRunResumed, map[string]any{"host": true}); eerr != nil {
		return "", fmt.Errorf("cannot record run_resumed (%w) — the run was NOT resumed; your previous token is still valid, fix the storage problem and retry", eerr)
	}
	if serr := e.store.SaveState(state); serr != nil {
		return "", fmt.Errorf("cannot persist the rotated driver token (%w) — the run was NOT resumed and your previous token is still the valid one; fix the storage problem and retry", serr)
	}
	return tok, nil
}

// acquireForResume takes the run lock, translating a live-owner conflict into an
// actionable message. It uses the EXISTING-only acquire: resuming an id with no run must return
// "not found", never MATERIALIZE the run's directory as a side effect of a rejected command (I2).
func (e *Engine) acquireForResume(runID string) (*runtime.Handle, error) {
	handle, err := e.store.AcquireLockExisting(runID)
	if err != nil {
		if errors.Is(err, runtime.ErrLocked) {
			return nil, fmt.Errorf("run %s is already being executed by a live process — cancel it with `vichu cancel %s` or wait for it to finish", runID, runID)
		}
		return nil, err
	}
	return handle, nil
}

// reopenProviderForResume switches e.repo back to the backend the run started
// with, so a folder that gained (or lost) a .git since the run began can't flip
// the `auto` provider and report avoidable drift against a different baseline.
// It reopens at the project root (where .vichu lives), not e.repo.Root(): when
// `auto` resolves git, the Repo re-roots to the git top level, which can sit
// ABOVE the project root — reopening there would miss the filesystem baseline.
func (e *Engine) reopenProviderForResume(snap *core.Workspace) error {
	if snap.Provider == "" || snap.Provider == e.repo.Kind() {
		return nil
	}
	reopened, err := workspace.Open(e.store.ProjectRoot(), snap.Provider)
	if err != nil {
		return fmt.Errorf("reopening %s workspace for resume: %w", snap.Provider, err)
	}
	e.repo = reopened
	return nil
}

// resolveDrift checks for workspace drift on resume. If the live workspace
// diverged and the caller did not accept it, it blocks the run and returns
// blocked=true. If accepted, it re-baselines and returns the fresh snapshot.
func (e *Engine) resolveDrift(state *core.State, runID string, snap *core.Workspace, opts ResumeOptions) (*core.Workspace, bool, error) {
	drifted, reason := e.checkDrift(runID, snap)
	if !drifted {
		return snap, false, nil
	}
	if !opts.AcceptChanges {
		e.emit(state, "", "", core.EventWorkspaceDrift, map[string]any{"reason": reason})
		e.block(state, "workspace_drift: "+reason)
		e.log(i18n.T("engine.drift_hint", runID))
		return snap, true, nil
	}
	// Explicitly accepted: re-baseline the snapshot to the current tree.
	fresh, err := e.repo.Snapshot(snap.Isolation)
	if err != nil {
		return nil, false, fmt.Errorf("re-baselining workspace: %w", err)
	}
	if err := e.store.SaveWorkspace(runID, fresh); err != nil {
		return nil, false, err
	}
	e.emit(state, "", "", "workspace_rebaselined", map[string]any{"reason": reason})
	e.log(i18n.T("engine.rebaselined"))
	return fresh, false, nil
}

// lastSummaryOnDisk restores prompt continuity across resumes: the summary of
// the most recent completed stage, read back from summaries/<stage>.md.
func (e *Engine) lastSummaryOnDisk(wf *workflows.Workflow, state *core.State) string {
	last := ""
	for _, st := range wf.Stages {
		if state.Stages[st.Name] != core.StageDone {
			continue
		}
		if md := e.store.StageSummary(state.RunID, st.Name); md != "" {
			last = md
		}
	}
	return last
}

// run is the shared stage loop used by both Start and Resume. It steps until the
// run reaches a terminal state; every transition is decided by verified evidence
// inside step, never by an agent's claim.
func (e *Engine) run(ctx context.Context, state *core.State, rs *runState) (*core.State, error) {
	// Make must-succeed writes fatal in headless too. A host-first op already runs under a
	// strict scope; the headless loop (Start/Resume) had none, so criticalWrite degraded to a
	// warning and the run could report a terminal status — "completed" — that it never
	// persisted (state.json stayed `active`). That is the kernel lying about the run's
	// outcome. Under a strict scope the first must-succeed failure is recorded; step() stops
	// on it, and run returns it, so the CLI exits non-zero and prints no success.
	if e.strict == nil {
		e.strict = &strictScope{}
		defer func() { e.strict = nil }()
	}
	for e.step(ctx, state, rs) { //nolint:revive // empty body: step does the work, returns whether to continue
	}
	if err := e.persistFailed(); err != nil {
		return state, err
	}
	return state, nil
}

// step runs one iteration of the stage loop and reports whether to continue.
// It returns false once the run reaches a terminal state (completed, blocked,
// failed, canceled) or cannot advance on valid evidence.
func (e *Engine) step(ctx context.Context, state *core.State, rs *runState) bool {
	if e.persistFailed() != nil {
		return false // a must-succeed write failed; stop before touching more state
	}
	if rs.lockLost.Load() {
		e.log("lock ownership lost — another process took over this run; stopping without modifying its state")
		return false
	}
	if e.finalizeIfCanceled(state) || state.Status.Terminal() {
		return false
	}
	// Budgets are HARD limits: an over-budget run blocks here even if the only
	// thing left is the terminal stage — it never completes over budget.
	if e.budgetBlocked(state) {
		return false
	}
	stage, ok := rs.wf.Stage(state.CurrentStage)
	if !ok {
		e.fail(state, "unknown stage "+state.CurrentStage)
		return false
	}
	if stage.Kind == workflows.KindTerminal {
		e.complete(state)
		return false
	}
	if e.stageIterationsBlocked(state, stage) {
		return false
	}
	if !e.executeStage(ctx, state, rs, stage) {
		return false // the stage reached a terminal state
	}
	// Enforce a stage's required-artifact contract before advancing — the same gate
	// host-first runs in `stage close`, so `vichu exec` and a host pack hold SDD to
	// the identical bar (e.g. the `plan` stage must produce a `plan` artifact with a
	// `## Tests` section). No-ops for stages without a RequiresArtifact.
	if reason := e.checkRequiredArtifact(state, stage); reason != "" {
		e.block(state, reason)
		return false
	}
	return e.advanceStage(state, stage) // false ⇒ could not decide from evidence
}

// budgetBlocked blocks the run if a run-level budget is exhausted.
func (e *Engine) budgetBlocked(state *core.State) bool {
	reason := e.checkBudgets(state)
	if reason == "" {
		return false
	}
	e.emit(state, state.CurrentStage, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
	e.block(state, reason)
	return true
}

// stageIterationsBlocked counts this entry into the stage and blocks if the
// stage's iteration budget (re-entries via resume or future loops) is exceeded.
func (e *Engine) stageIterationsBlocked(state *core.State, stage workflows.Stage) bool {
	state.Iterations[stage.Name]++
	sb, ok := e.cfg.Budgets.Stage[stage.Name]
	if !ok || sb.MaxIterations <= 0 || state.Iterations[stage.Name] <= sb.MaxIterations {
		return false
	}
	reason := fmt.Sprintf("stage %q exceeded its iteration budget (%d)", stage.Name, sb.MaxIterations)
	e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
	e.block(state, reason)
	return true
}

// executeStage runs one worker or gate stage under cancellation and budget
// deadlines. It returns true only when the stage finished cleanly and the run
// should advance; otherwise it has already set the terminal state.
func (e *Engine) executeStage(ctx context.Context, state *core.State, rs *runState, stage workflows.Stage) bool {
	state.Stages[stage.Name] = core.StageActive
	state.Status = core.StatusActive
	if !e.saveStateOK(state) {
		return false // stage-start did not persist — do not emit stage_started or run the worker
	}
	e.emit(state, stage.Name, "", core.EventStageStarted, nil)
	e.log(i18n.T("engine.stage", stage.Name))

	// A watcher cancels the stage context — killing the worker or gate process —
	// when `vichu cancel` marks the run canceled on disk; the budget deadline
	// does the same when wall-clock runs out mid-stage.
	stageCtx, stopWatch := e.watchCancel(ctx, state.RunID)
	stageCtx, stopDeadline, deadlineReason := e.withBudgetDeadline(stageCtx, stage, rs)
	if deadlineReason != "" {
		stopDeadline()
		stopWatch()
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": deadlineReason})
		e.block(state, deadlineReason)
		return false
	}

	advance, err := e.dispatchStage(stageCtx, state, rs, stage)
	deadlineHit := stageCtx.Err() == context.DeadlineExceeded
	stopDeadline()
	stopWatch()

	// Lock lost mid-stage: another process owns the run now. Stop WITHOUT writing
	// a terminal state — the worker was killed by the canceled context; do not
	// clobber the new owner's state with a fail/block from this process.
	if rs.lockLost.Load() {
		return false
	}

	// Wall-clock spend updates after every stage, success or not.
	state.Budgets.WallClockSpentSeconds = rs.wallClockSpent()

	switch {
	case e.finalizeIfCanceled(state):
		return false
	case deadlineHit || errors.Is(err, context.DeadlineExceeded):
		reason := fmt.Sprintf("wall-clock budget exhausted during stage %q (%.0fs spent)", stage.Name, rs.wallClockSpent())
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
		e.block(state, reason)
		return false
	case err != nil:
		e.fail(state, err.Error())
		return false
	default:
		return advance // false here means the stage already blocked the run
	}
}

// dispatchStage runs the stage body appropriate to its kind.
func (e *Engine) dispatchStage(ctx context.Context, state *core.State, rs *runState, stage workflows.Stage) (bool, error) {
	switch stage.Kind {
	case workflows.KindWorker, workflows.KindReview:
		// A review runs an agent exactly like a worker; runWorkerStage then
		// parses the verdict and picks the branch (see applyVerdict).
		return e.runWorkerStage(ctx, state, rs, stage)
	case workflows.KindGate:
		return e.runGateStage(ctx, state, rs, stage)
	default:
		return true, nil
	}
}

// advanceStage records a stage as done and transitions to the next, returning
// false if it blocked the run instead. A review stage's branch is recomputed
// from its persisted verdict (crash-safe — the verdict is on disk before this
// runs); other stages use their static Next.
func (e *Engine) advanceStage(state *core.State, stage workflows.Stage) bool {
	next := stage.Next
	if stage.Kind == workflows.KindReview {
		branch, ok := e.reviewBranch(state, stage)
		if !ok {
			e.block(state, fmt.Sprintf("cannot read the persisted verdict for review stage %q — refusing to transition without verifiable evidence", stage.Name))
			return false
		}
		next = branch
	}
	// Mark the stage done AND move to the next stage in a SINGLE persisted write.
	// Two writes could leave a crash window where a stage is marked done while it
	// is still current_stage, which would re-run a completed stage on resume.
	state.Stages[stage.Name] = core.StageDone
	state.CurrentStage = next
	if !e.saveStateOK(state) {
		return false // the transition did not persist — do not emit stage_completed/transition
	}
	e.emit(state, stage.Name, "", core.EventStageCompleted, nil)
	e.emit(state, "", "", core.EventStageTransition, map[string]any{"from": stage.Name, "to": next})
	return true
}

// reviewBranch picks a review stage's next stage from its persisted verdict.
// advanceStage is only reached for approved/needs_fixes (a blocked verdict or an
// exhausted auto-fix budget stops the run earlier in applyVerdict), so the
// choice is binary: approved advances, anything else loops to the fix stage.
// ok=false means the verdict could not be read — the caller must block rather
// than guess a branch, so a lost verdict never silently routes to fix.
func (e *Engine) reviewBranch(state *core.State, stage workflows.Stage) (string, bool) {
	v, err := e.store.LoadReviewVerdict(state.RunID, stage.Name, state.Iterations[stage.Name])
	if err != nil {
		return "", false
	}
	if v.Status == core.VerdictApproved {
		return stage.NextOnApproved, true
	}
	return stage.NextOnNeedsFixes, true
}

// withBudgetDeadline derives the stage context's deadline from the remaining
// run wall-clock budget and the stage's own MaxWallClock, whichever is sooner.
// A non-empty reason means the budget is already exhausted.
func (e *Engine) withBudgetDeadline(ctx context.Context, stage workflows.Stage, rs *runState) (context.Context, context.CancelFunc, string) {
	noop := context.CancelFunc(func() {
		// no-op cancel: returned when no budget deadline applies.
	})
	var deadline time.Time

	if b := e.cfg.Budgets.Run.MaxWallClock; b > 0 {
		remaining := b.Std() - time.Duration(rs.wallClockSpent()*float64(time.Second))
		if remaining <= 0 {
			return ctx, noop, fmt.Sprintf("wall-clock budget exhausted (%s)", b.Std())
		}
		deadline = time.Now().Add(remaining)
	}
	if sb, ok := e.cfg.Budgets.Stage[stage.Name]; ok && sb.MaxWallClock > 0 {
		stageDeadline := time.Now().Add(sb.MaxWallClock.Std())
		if deadline.IsZero() || stageDeadline.Before(deadline) {
			deadline = stageDeadline
		}
	}
	if deadline.IsZero() {
		return ctx, noop, ""
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	return dctx, cancel, ""
}

// saveState persists state but never clobbers a terminal status written
// externally (e.g. `vichu cancel` from another process). If the run is already
// terminal on disk, the in-memory state adopts it instead.
func (e *Engine) saveState(state *core.State) {
	if disk, err := e.store.LoadState(state.RunID); err == nil &&
		disk.Status.Terminal() && !state.Status.Terminal() {
		*state = *disk
		return
	}
	e.criticalWrite(e.store.SaveState(state), "persist run state")
}

// saveStateOK persists state and reports whether it durably landed. A false means a
// must-succeed write failed (recorded in the strict scope, so the run will stop). Callers use
// it to avoid the "kernel lies" trap: never emit an event, dispatch a worker, or transition on
// the strength of a state change that state.json does not actually reflect. Persist first,
// check, THEN announce — see executeStage and advanceStage.
func (e *Engine) saveStateOK(state *core.State) bool {
	e.saveState(state)
	return e.persistFailed() == nil
}

// warn surfaces a non-fatal persistence/evidence failure through the log so it
// is never silent. A failed audit write means the run's evidence is incomplete,
// but does not by itself stop the run; the safety-critical paths (mutation
// tracking and gate backup) block the run instead of degrading quietly.
func (e *Engine) warn(err error, what string) {
	if err != nil {
		e.log(fmt.Sprintf("warning: could not %s: %v", what, err))
	}
}

// criticalWrite routes a must-succeed persistence error. In a host-first strict
// scope it records the FIRST failure so the transactional command returns an
// error (and the operation is NOT marked completed) — a reported success never
// leaves the runtime missing state/status/mutations. Outside a strict scope (the
// full runner) it degrades to a warn, as before.
func (e *Engine) criticalWrite(err error, what string) {
	if err == nil {
		return
	}
	if e.strict != nil {
		if e.strict.err == nil {
			e.strict.err = fmt.Errorf("%s: %w", what, err)
		}
		return
	}
	e.warn(err, what)
}

// canceledOnDisk reports whether another process marked the run canceled.
func (e *Engine) canceledOnDisk(runID string) bool {
	disk, err := e.store.LoadState(runID)
	return err == nil && disk.Status == core.StatusCanceled
}

// finalizeIfCanceled adopts an external cancellation: clears transient fields
// and stops the loop without overwriting the canceled status.
func (e *Engine) finalizeIfCanceled(state *core.State) bool {
	if !e.canceledOnDisk(state.RunID) {
		return false
	}
	state.Status = core.StatusCanceled
	state.ActiveWorker = ""
	state.NextAction = ""
	e.criticalWrite(e.store.SaveState(state), "persist canceled state")
	e.log(i18n.T("engine.canceled"))
	return true
}

// watchCancel derives a context that is canceled when the run is marked
// canceled on disk, so in-flight worker and gate processes are killed instead
// of running to completion.
func (e *Engine) watchCancel(ctx context.Context, runID string) (context.Context, context.CancelFunc) {
	wctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if e.canceledOnDisk(runID) {
					cancel()
					return
				}
			}
		}
	}()
	return wctx, cancel
}

func (e *Engine) complete(state *core.State) {
	if e.finalizeIfCanceled(state) {
		return
	}
	state.Status = core.StatusCompleted
	state.CurrentStage = "done"
	state.ActiveWorker = ""
	state.NextAction = ""
	if state.Stages != nil {
		state.Stages["done"] = core.StageDone
	}
	e.criticalWrite(e.store.SaveState(state), "persist completed state")
	if e.persistFailed() != nil {
		return // the completion did not persist — do not announce it in the log or the timeline
	}
	e.emit(state, "", "", core.EventRunCompleted, nil)
	e.log(i18n.T("engine.completed"))
}

func (e *Engine) block(state *core.State, reason string) {
	if e.finalizeIfCanceled(state) {
		return
	}
	state.Status = core.StatusBlocked
	state.BlockedReason = reason
	// No worker is active once the run is blocked — clear the transient before
	// setting the resolution hint, so the observable state never points at a
	// worker that already finished, failed, or was canceled.
	state.ActiveWorker = ""
	state.NextAction = "resolve and `vichu resume " + state.RunID + "`"
	e.criticalWrite(e.store.SaveState(state), "persist blocked state")
	if e.persistFailed() != nil {
		return // the block did not persist — do not announce it
	}
	e.emit(state, state.CurrentStage, "", core.EventRunBlocked, map[string]any{"reason": reason})
	e.log(i18n.T("engine.blocked", reason))
}

func (e *Engine) fail(state *core.State, reason string) {
	if e.finalizeIfCanceled(state) {
		return
	}
	state.Status = core.StatusFailed
	state.BlockedReason = reason
	state.ActiveWorker = ""
	state.NextAction = ""
	e.criticalWrite(e.store.SaveState(state), "persist failed state")
	if e.persistFailed() != nil {
		return // the failure did not persist — do not announce it
	}
	e.emit(state, state.CurrentStage, "", core.EventRunFailed, map[string]any{"reason": reason})
	e.log(i18n.T("engine.failed", reason))
}

// emitDurable appends an event that MUST land before the caller reports success — run_created
// and run_resumed, on which a driver token and an exit-0 depend. Plain emit only WARNS outside a
// strict scope (StartRun/ReopenRun have none), so a failed events.ndjson append would let the
// command hand out a token and exit 0 with no event in the audit — the kernel lying. This scopes
// a strict window around the emit (respecting an outer one) and returns the append failure so the
// caller can refuse to report success.
func (e *Engine) emitDurable(state *core.State, stage, worker, event string, detail map[string]any) error {
	if e.strict != nil {
		before := e.strict.err
		e.emit(state, stage, worker, event, detail)
		if before == nil && e.strict.err != nil {
			return e.strict.err
		}
		return nil
	}
	e.strict = &strictScope{}
	e.emit(state, stage, worker, event, detail)
	err := e.strict.err
	e.strict = nil
	return err
}

// emit appends a normalized event to the run's timeline.
func (e *Engine) emit(state *core.State, stage, worker, event string, detail map[string]any) {
	ev := core.Event{
		Run:    state.RunID,
		Stage:  stage,
		Worker: worker,
		Event:  event,
		Detail: detail,
	}
	// Inside a transactional command, stamp the event with (op_id, seq) and drop it if
	// this operation already wrote it. A retry replays the operation from the top — that
	// is how it recovers — so without this, every recovered operation would double its
	// entries in events.ndjson, the file people read to know what happened.
	if op := e.opEvents; op != nil {
		op.seq++
		if op.seq <= op.alreadyWritten {
			return // a previous attempt of THIS op already appended this event
		}
		ev.OpID, ev.OpFP, ev.Seq = op.opID, op.opFP, op.seq
	}
	// criticalWrite, not warn. Inside a host-first operation this ABORTS: events.ndjson is the
	// public audit trail, and an operation that reports success while its evidence never
	// reached the log is the kernel lying about what happened. (Outside an operation — the
	// headless runner — criticalWrite still only warns, so the loop degrades loudly rather
	// than dying mid-run.)
	e.criticalWrite(e.store.AppendEvent(ev), "record event "+event)
}

// opEventScope counts the events a transactional command emits, so a replay can skip the
// ones a previous attempt already appended. Events within an operation are emitted in a
// deterministic order (the operation is deterministic — that is what makes replay safe),
// so the Nth event of op X is always the same event.
type opEventScope struct {
	opID           string
	opFP           string
	seq            int
	alreadyWritten int
}

func (e *Engine) snapshotConfig(runID string) error {
	data, err := e.cfg.MarshalYAML()
	if err != nil {
		return err
	}
	return e.store.SaveConfigSnapshot(runID, data)
}

func pendingStages(wf *workflows.Workflow) map[string]core.StageStatus {
	m := map[string]core.StageStatus{}
	for _, name := range wf.StageNames() {
		m[name] = core.StagePending
	}
	return m
}
