package engine

import (
	"context"
	"errors"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
)

// reconcileInterruptedWorkers makes the audit trail honest after a crash. A run
// that died mid-stage leaves a worker marked "running" whose process is gone; on
// resume we mark every such worker canceled and clear the run's active-worker
// pointer, so observable state never claims a worker that is no longer alive.
func (e *Engine) reconcileInterruptedWorkers(state *core.State) {
	workers, err := e.store.ListWorkers(state.RunID)
	if err != nil {
		return
	}
	for _, id := range workers {
		ws, err := e.store.LoadWorkerStatus(state.RunID, id)
		if err != nil || ws.Status != core.WorkerRunning {
			continue
		}
		fin := time.Now().UTC()
		ws.Status = core.WorkerCanceled
		ws.FinishedAt = &fin
		e.warn(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
		e.emit(state, ws.Stage, ws.ID, core.EventWorkerInterrupted, map[string]any{"role": ws.Role})
	}
	state.ActiveWorker = ""
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
