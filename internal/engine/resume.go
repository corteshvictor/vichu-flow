package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
)

// reconcileInterruptedWorkers makes the audit trail honest after a crash. A run
// that died mid-stage leaves a worker marked "running" whose process is gone; on
// resume we mark every such worker canceled and clear the run's active-worker
// pointer, so observable state never claims a worker that is no longer alive.
//
// It also handles the UPGRADE case. A run from a build with no operation journal could
// crash after the worker was marked `done` and its mutations.json written, but BEFORE the
// resulting block reached state.json. Such a worker is not `running`, so the loop below
// skips it — and then ActiveWorker gets cleared and the run carries on, having silently
// dropped a security block that its own evidence still proves. Clearing the active worker
// without re-deciding from the evidence is how a read-only violation survives an upgrade.
func (e *Engine) reconcileInterruptedWorkers(state *core.State) error {
	workers, err := e.store.ListWorkers(state.RunID)
	if err != nil {
		return fmt.Errorf("cannot list this run's workers (%w) — refusing to resume without knowing what was left running", err)
	}
	for _, id := range workers {
		ws, err := e.store.LoadWorkerStatus(state.RunID, id)
		if err != nil {
			// A worker whose status we cannot read is a hole in the audit. If it is the
			// ACTIVE one, we are about to clear it and let the run advance on evidence we
			// never saw — so block instead. (A stale, non-active worker's unreadable status
			// cannot affect this run's decisions; it is noted and skipped.)
			if id == state.ActiveWorker {
				e.block(state, fmt.Sprintf("cannot read the status of the active worker %s (%v) — refusing to resume a run whose evidence is unreadable; inspect .vichu/runs/%s/workers/%s/", id, err, state.RunID, id))
				return nil
			}
			e.warn(err, "read worker status "+id)
			continue
		}
		if ws.Status != core.WorkerRunning {
			e.reconcileLegacyClose(state, ws)
			continue
		}
		fin := time.Now().UTC()
		ws.Status = core.WorkerCanceled
		ws.FinishedAt = &fin
		// The cancel must LAND before we clear the active pointer. `criticalWrite` only
		// aborts inside a host-first operation; resume is not one, so it would merely WARN —
		// and then we would clear ActiveWorker and report success while the worker is still
		// `running` on disk. Fail the resume instead: nothing is lost, and the retry is safe.
		if serr := e.store.SaveWorkerStatus(state.RunID, ws); serr != nil {
			return fmt.Errorf("cannot record worker %s as canceled (%w) — refusing to resume a run whose worker is still marked running", id, serr)
		}
		e.emit(state, ws.Stage, ws.ID, core.EventWorkerInterrupted, map[string]any{"role": ws.Role})
	}
	if state.Status != core.StatusBlocked {
		state.ActiveWorker = ""
	}
	return nil
}

// reconcileLegacyClose re-decides a worker that a PRE-JOURNAL build closed but whose
// outcome never reached the run state — the crash window between "worker done" and "state
// applied". Modern closes carry the outcome on the worker (CloseOpID), so they need
// nothing; only a worker that is done, still named as the run's active worker, and carries
// NO journal can be in that window.
//
// The decision is recomputed from the evidence already on disk (mutations.json + the run's
// frozen config), never from a guess. If the evidence cannot be read, the run BLOCKS: an
// audit we cannot re-read is not one we get to assume was clean.
func (e *Engine) reconcileLegacyClose(state *core.State, ws *core.WorkerStatus) {
	if ws.Status != core.WorkerDone || ws.CloseOpID != "" || state.ActiveWorker != ws.ID {
		return // not the legacy crash window
	}
	stage, ok := e.stageOf(state, ws.Stage)
	if !ok {
		stage = workflows.Stage{Name: ws.Stage}
	}
	// The run's FROZEN policy, not the project's current one. The worker ran under the rules
	// that were in force then, and those rules are what its evidence must be judged against.
	// Reading `e.cfg` here would let someone relax `sensitiveMutations` to `warn` today and
	// have yesterday's blocked run quietly resume — the config becomes a way to un-decide a
	// verdict after the fact.
	sec, err := e.frozenSecurity(state.RunID)
	if err != nil {
		e.block(state, fmt.Sprintf("worker %s was closed by an older VichuFlow, and the run's frozen policy (config.snapshot.yaml) cannot be read (%v) — refusing to re-judge its evidence under different rules", ws.ID, err))
		return
	}
	report, err := e.store.LoadMutationReport(state.RunID, ws.ID)
	if err != nil {
		e.block(state, fmt.Sprintf("worker %s was closed by an older VichuFlow and its mutation audit cannot be read (%v) — refusing to resume a run whose evidence is unverifiable", ws.ID, err))
		return
	}
	e.emit(state, ws.Stage, ws.ID, "legacy_worker_reconciled", map[string]any{
		"mutations": len(report.Mutations),
	})
	if reason := mutationPolicyVerdict(stage, report.Mutations, sec); reason != "" {
		e.block(state, reason)
	}
}

// frozenSecurity loads the security policy the run was STARTED with, from its config
// snapshot. A run's verdicts must be reproducible: the same evidence, judged by the same
// rules, forever. A missing snapshot is an error, not a license to use today's config.
func (e *Engine) frozenSecurity(runID string) (config.SecurityConfig, error) {
	cfg, err := e.loadFrozenConfig(runID)
	if err != nil {
		return config.SecurityConfig{}, err
	}
	return cfg.Security, nil
}

// loadFrozenConfig reads the run's frozen config.snapshot.yaml through the confined Store
// (so a symlinked snapshot is refused, not followed) and parses it. A missing or unreadable
// snapshot is an error the caller must surface — never a silent fall back to the live config.
func (e *Engine) loadFrozenConfig(runID string) (*config.Config, error) {
	data, err := e.store.ReadConfigSnapshot(runID)
	if err != nil {
		return nil, fmt.Errorf("cannot read the run's frozen config (config.snapshot.yaml) — the run cannot be judged by a config it did not start with: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("the run's frozen config is unreadable: %w", err)
	}
	return cfg, nil
}

// freezeConfigForRun replaces the engine's live config with the run's FROZEN snapshot, so
// every host-first command judges the run by the policy, gates, budgets and workflow it
// started with — not a vichu.yaml the agent rewrote mid-run. Called once per command, under
// the lock, right after loading state.
func (e *Engine) freezeConfigForRun(runID string) error {
	cfg, err := e.loadFrozenConfig(runID)
	if err != nil {
		return err
	}
	e.cfg = cfg
	return nil
}

// pinFrozenRun makes a host-first command judge the run by the state it STARTED with, not the
// live vichu.yaml: the config is frozen from config.snapshot.yaml, and the workspace provider
// is pinned from workspace.json. Without this an agent could rewrite vichu.yaml mid-run —
// flipping a failing gate to `true`, a block policy to warn, or the provider — and change the
// very rules that judge it. A missing/tampered snapshot blocks rather than falling back.
func (e *Engine) pinFrozenRun(runID string) error {
	if err := e.freezeConfigForRun(runID); err != nil {
		return err
	}
	snap, err := e.store.LoadWorkspace(runID)
	if err != nil {
		return fmt.Errorf("cannot read the run's workspace snapshot (workspace.json) to pin its provider: %w", err)
	}
	return e.reopenProviderForResume(snap)
}

// reSnapshotConfigForResume records the CURRENT vichu.yaml (e.cfg, loaded live) as the run's
// new frozen config. Resume is a human/CI action — the one legitimate moment to fix a broken
// gate or policy and continue — so it re-freezes the baseline rather than silently keeping
// the old snapshot. Mid-run host-first commands never do this: only an explicit resume can
// change a run's frozen config, and resume is not pre-authorized to a subagent.
func (e *Engine) reSnapshotConfigForResume(runID string) error {
	if err := e.snapshotConfig(runID); err != nil {
		return fmt.Errorf("re-freezing the run's config on resume: %w", err)
	}
	return nil
}

// sessionsToResume finds the agent session to continue for the stage the run is
// re-entering. It returns a stage→sessionID map seeded only for the current
// stage: if its most recent worker recorded a session id (the worker completed
// but the run blocked or stopped at that stage), the next entry continues that
// agent session instead of starting cold. A worker interrupted mid-flight never
// recorded a session, so that case correctly falls back to a fresh start.
func (e *Engine) sessionsToResume(state *core.State) map[string]string {
	sessions := map[string]string{}
	workers, err := e.store.ListWorkers(state.RunID)
	if err != nil {
		return sessions
	}
	for _, id := range workers { // sorted = chronological; the latest wins
		ws, err := e.store.LoadWorkerStatus(state.RunID, id)
		if err != nil {
			continue
		}
		// Only a COMPLETED worker recorded a usable session; an interrupted
		// (running/failed/canceled) worker never finished its session and must be
		// restarted fresh, not resumed.
		if ws.Stage == state.CurrentStage && ws.Status == core.WorkerDone && ws.SessionID != "" {
			sessions[ws.Stage] = ws.SessionID
		}
	}
	return sessions
}

// startSession begins a worker, continuing a prior agent session when one was
// seeded for this stage (see sessionsToResume) and the adapter supports resume.
// The seed is consumed on first use, so only the re-entered stage resumes; later
// normal re-entries (e.g. the review→fix loop) start fresh. A resume that errors
// for any reason (unsupported, expired session) falls back to a fresh start
// rather than failing the run — the fallback is recorded, never silent.
func (e *Engine) startSession(ctx context.Context, adapter adapters.Adapter, state *core.State, rs *runState, stage workflows.Stage, inv adapters.Invocation) (adapters.Session, error) {
	sid := rs.resumeSession[stage.Name]
	if sid == "" || !adapter.Capabilities().Resume {
		return adapter.Start(ctx, inv)
	}
	delete(rs.resumeSession, stage.Name) // resume the interrupted entry only once

	sess, err := adapter.Resume(ctx, sid, inv)
	if err == nil {
		e.emit(state, stage.Name, state.ActiveWorker, core.EventWorkerResumed, map[string]any{"session_id": sid})
		return sess, nil
	}
	detail := map[string]any{"session_id": sid, "error": err.Error()}
	if errors.Is(err, adapters.ErrResumeUnsupported) {
		detail["error"] = "adapter does not support resume"
	}
	e.emit(state, stage.Name, state.ActiveWorker, core.EventWorkerResumeFailed, detail)
	return adapter.Start(ctx, inv)
}
