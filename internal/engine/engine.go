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
}

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
	if workflowName == "" {
		workflowName = e.cfg.Workflow.Default
	}
	wf, err := workflows.Get(workflowName)
	if err != nil {
		return nil, err
	}

	snap, err := e.repo.Snapshot(e.cfg.Workspace.Isolation)
	if err != nil {
		return nil, fmt.Errorf("capturing workspace snapshot: %w", err)
	}

	runID := runtime.NewRunID(time.Now())
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
	e.emit(state, "", "", core.EventRunCreated, map[string]any{"workflow": wf.Name, "task": task})

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
	e.warn(e.snapshotConfig(runID), "save config snapshot")

	// requireCleanTree policy.
	if len(snap.DirtyFiles) > 0 {
		switch e.cfg.Workspace.RequireCleanTree {
		case "block":
			e.block(state, fmt.Sprintf("working tree has %d uncommitted change(s); requireCleanTree=block", len(snap.DirtyFiles)))
			return state, nil
		case "warn":
			e.emit(state, "", "", "dirty_tree_warning", map[string]any{"dirty_files": len(snap.DirtyFiles)})
			e.log(i18n.T("engine.dirty_warning", len(snap.DirtyFiles)))
		}
	}

	handle, err := e.store.AcquireLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()

	rs := &runState{wf: wf, pack: pack.Markdown, baseSHA: snap.BaseSHA, startedAt: time.Now()}
	return e.runWithHeartbeat(ctx, handle, state, rs)
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
	state, err := e.store.LoadState(runID)
	if err != nil {
		return nil, err
	}
	if state.Status.Terminal() {
		return state, nil
	}
	wf, err := workflows.Get(state.Workflow)
	if err != nil {
		return nil, err
	}

	handle, err := e.acquireForResume(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()

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
	e.emit(state, state.CurrentStage, "", core.EventRunResumed, nil)

	// Seed the agent session to continue BEFORE reconciling: reconcile clears
	// active-worker bookkeeping for interrupted workers, while the session seed
	// comes from the latest COMPLETED worker of the stage being re-entered.
	resumeSession := e.sessionsToResume(state)
	e.reconcileInterruptedWorkers(state)

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

// acquireForResume takes the run lock, translating a live-owner conflict into an
// actionable message.
func (e *Engine) acquireForResume(runID string) (*runtime.Handle, error) {
	handle, err := e.store.AcquireLock(runID)
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
	for e.step(ctx, state, rs) { //nolint:revive // empty body: step does the work, returns whether to continue
	}
	return state, nil
}

// step runs one iteration of the stage loop and reports whether to continue.
// It returns false once the run reaches a terminal state (completed, blocked,
// failed, canceled) or cannot advance on valid evidence.
func (e *Engine) step(ctx context.Context, state *core.State, rs *runState) bool {
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
	e.saveState(state)
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
	e.saveState(state)
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
	e.warn(e.store.SaveState(state), "persist run state")
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
	e.warn(e.store.SaveState(state), "persist canceled state")
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
	e.warn(e.store.SaveState(state), "persist completed state")
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
	e.warn(e.store.SaveState(state), "persist blocked state")
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
	e.warn(e.store.SaveState(state), "persist failed state")
	e.emit(state, state.CurrentStage, "", core.EventRunFailed, map[string]any{"reason": reason})
	e.log(i18n.T("engine.failed", reason))
}

// emit appends a normalized event to the run's timeline.
func (e *Engine) emit(state *core.State, stage, worker, event string, detail map[string]any) {
	e.warn(e.store.AppendEvent(core.Event{
		Run:    state.RunID,
		Stage:  stage,
		Worker: worker,
		Event:  event,
		Detail: detail,
	}), "record event "+event)
}

func (e *Engine) snapshotConfig(runID string) error {
	return e.cfg.Save(e.store.ConfigSnapshotPath(runID))
}

func pendingStages(wf *workflows.Workflow) map[string]core.StageStatus {
	m := map[string]core.StageStatus{}
	for _, name := range wf.StageNames() {
		m[name] = core.StagePending
	}
	return m
}
