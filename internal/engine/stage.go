package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/gates"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// warnSaveWorkerStatus is the warn label used wherever a worker status write may
// fail, kept as one constant so the message stays consistent.
const warnSaveWorkerStatus = "save worker status"

// buildWorkerPrompt assembles the worker's prompt: the base instruction plus, for a review
// stage, the diff-only change set (so it judges the work without re-reading the whole repo).
// It returns blocked=true when the change set cannot be assembled SAFELY — a changed file that
// is a symlink (never followed) or unreadable — having blocked the run, so the caller stops
// BEFORE the prompt is persisted or the reviewer is invoked, never sending external bytes or a
// half-truth.
func (e *Engine) buildWorkerPrompt(state *core.State, stage workflows.Stage, rs *runState) (prompt string, blocked bool) {
	prompt = buildPrompt(rs.pack, stage.Instruction, state.Task, rs.lastSummary, e.cfg)
	prompt, err := e.withReviewContext(prompt, state, stage.Name, stage.Kind == workflows.KindReview)
	if err != nil {
		e.block(state, fmt.Sprintf("cannot assemble the review change set: %v", err))
		return "", true
	}
	return prompt, false
}

// runWorkerStage invokes an agent for a stage, capturing its events, result,
// and the exact set of files it mutated. It returns advance=false (without
// error) when a budget guard blocks the run.
func (e *Engine) runWorkerStage(ctx context.Context, state *core.State, rs *runState, stage workflows.Stage) (bool, error) {
	// Budget gates that must pass BEFORE starting an agent — run-level, agent
	// invocation (so a budget of N permits exactly N agents and still lets
	// verify/done run), and the per-stage token cap (stops a review→fix loop that
	// already burned its budget). Any one blocks the run.
	if e.preWorkerBudgetBlocked(state, stage) {
		return false, nil
	}

	agentCfg := e.cfg.Agent(stage.Role)
	adapter, err := e.registry.Get(agentCfg.Provider)
	if err != nil {
		return false, err
	}

	// Compute the NEXT worker's ordinal WITHOUT consuming the budget yet. A worker that never
	// reaches the adapter — a policy-blocked command, a tracking failure — must not spend an
	// invocation slot, or a `resume` after fixing the config finds the budget already gone though
	// no provider was ever called. The slot is reserved below, at the commit point.
	nextInvocation := state.Budgets.AgentInvocations + 1
	workerID := fmt.Sprintf("%s-%02d", stage.Name, nextInvocation)
	inv, prompt, blocked := e.prepareInvocation(state, stage, workerID, agentCfg, rs)
	if blocked {
		return false, nil
	}

	// Start mutation tracking BEFORE the worker runs — refuse to run an agent we
	// cannot audit. Without tracking there is no mutations.json, and the
	// read-only / scope / sensitive-file policy checks would silently not apply.
	tracker, err := e.repo.BeginTracking()
	if err != nil {
		e.block(state, fmt.Sprintf("cannot track worker mutations: %v — refusing to run an agent without an audit trail", err))
		return false, nil
	}

	// Committed to dispatch: consume the invocation slot now, so announceWorkerStart persists the
	// bumped counter together with the worker's own record — one durable step, not a count that
	// leaks ahead of a worker that may never have started.
	state.Budgets.AgentInvocations = nextInvocation
	ws := &core.WorkerStatus{
		ID: workerID, Role: stage.Role, Adapter: adapter.Name(),
		Stage: stage.Name, Status: core.WorkerRunning, StartedAt: time.Now().UTC(),
	}
	if ok, err := e.announceWorkerStart(state, stage, ws, prompt); !ok {
		return false, err
	}

	sess, err := e.startSession(ctx, adapter, state, rs, stage, inv)
	if err != nil {
		e.markWorkerFailed(state, ws)
		return false, fmt.Errorf("starting worker %s: %w", workerID, err)
	}
	for ev := range sess.Events() {
		e.emit(state, stage.Name, workerID, agentEventName(ev.Kind), agentEventDetail(ev, adapter.Name()))
	}
	result, err := sess.Result(ctx)
	// Persist whatever the worker produced — failed and canceled workers keep
	// their captured output in the audit trail too.
	e.persistWorkerResult(state.RunID, workerID, result)
	// Finalize the mutation audit for ANY worker that started — even one that
	// failed or was canceled. A worker that modified files and THEN failed must
	// still leave a mutations.json (and sensitive/out-of-scope events): auditing
	// what an agent touched is non-negotiable, independent of the run's outcome.
	policyBlock := e.trackMutations(state, stage, workerID, rs.baseSHA, tracker)

	if ok, uerr := e.accrueUsageOrBlock(state, stage, ws, workerID, result, rs); !ok {
		return false, uerr
	}

	if err != nil {
		return e.handleWorkerError(ctx, state, ws, workerID, err)
	}
	if ok, ferr := e.finishWorker(state, stage, ws, workerID, result, rs); !ok {
		return false, ferr
	}

	if policyBlock != "" {
		e.block(state, policyBlock)
		return false, nil
	}
	// Materialize the stage's default artifact (propose → proposal.md, plan →
	// plan.md) from the worker's result — the same kernel-owned step host-first does
	// in `worker complete`, so SDD artifacts exist for the required-artifact gate
	// whether the run is driven headless or by a host. For a stage that requires its
	// artifact this is contract evidence, so a persist failure BLOCKS rather than
	// silently advancing. No-op for stages with no default artifact (explore,
	// implement, fix) and for review stages.
	if stage.Kind == workflows.KindWorker {
		if aerr := e.materializeArtifacts(state, stage.Name, workerID, result.Markdown, nil); aerr != nil {
			e.block(state, fmt.Sprintf("stage %q: cannot persist its artifact: %v", stage.Name, aerr))
			return false, nil
		}
	}
	if stage.Kind == workflows.KindReview {
		return e.applyVerdict(state, stage, result), nil
	}
	return true, nil
}

// prepareInvocation builds the adapter invocation for a stage and, for a shell worker, runs
// the pre-execution command policy. blocked=true means a shell command that needs human
// confirmation (or is denied) blocked the run, and the caller must halt without dispatching.
func (e *Engine) prepareInvocation(state *core.State, stage workflows.Stage, workerID string, agentCfg config.AgentConfig, rs *runState) (inv adapters.Invocation, prompt string, blocked bool) {
	prompt, blocked = e.buildWorkerPrompt(state, stage, rs)
	if blocked {
		return adapters.Invocation{}, "", true
	}
	inv = adapters.Invocation{
		Role:             stage.Role,
		Prompt:           prompt,
		WorkDir:          e.repo.Root(),
		Model:            agentCfg.Model,
		Effort:           agentCfg.Effort,
		Iteration:        state.Iterations[stage.Name],
		ReadOnly:         stage.ReadOnly,
		AllowNonZeroExit: agentCfg.AllowNonZeroExit,
		DisallowedTools:  e.policy.ClaudeDisallowedTools(),
	}
	if agentCfg.Provider == adapters.ShellName {
		inv.Command = splitCommand(agentCfg.Command)
		if cerr := e.policy.CheckCommand(inv.Command); cerr != nil {
			e.emit(state, stage.Name, workerID, "policy_blocked", map[string]any{"reason": cerr.Error()})
			e.block(state, cerr.Error())
			return inv, prompt, true
		}
	}
	return inv, prompt, false
}

// announceWorkerStart persists the worker record and state, then emits worker_started —
// PERSIST → CHECK → ANNOUNCE, so a dispatched agent always has a durable record it began.
// ok=false means a must-succeed write failed; the caller must NOT launch the adapter.
//
// status.json is the worker's very record of existing, so it is must-succeed (not warn): if
// it did not land, the agent would run with no auditable worker at all. prompt.md stays
// best-effort — a copy of the prompt for humans, not a safety record. The worker is left
// `running` on a failure on purpose: reconcileInterruptedWorkers cancels it on resume,
// rather than us cleaning up while the store is failing.
func (e *Engine) announceWorkerStart(state *core.State, stage workflows.Stage, ws *core.WorkerStatus, prompt string) (ok bool, err error) {
	e.criticalWrite(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
	e.warn(e.store.WriteWorkerFile(state.RunID, ws.ID, "prompt.md", []byte(prompt)), "write worker prompt")
	state.ActiveWorker = ws.ID
	state.NextAction = "running " + stage.Role
	if !e.saveStateOK(state) {
		return false, nil // worker record or worker-start state did not persist — do not dispatch
	}
	e.emit(state, stage.Name, ws.ID, core.EventWorkerStarted, map[string]any{"adapter": ws.Adapter, "role": stage.Role})
	if perr := e.persistFailed(); perr != nil {
		return false, perr // the worker_started event did not persist — do not dispatch
	}
	return true, nil
}

// accrueUsageOrBlock validates the worker's reported usage, then accrues it into the run
// budget. ok=false means the usage was invalid (negative/NaN cost or tokens) and the run is
// now blocked — the caller must halt (with the returned error if a must-succeed write failed).
// Evidence (result + mutations) is already persisted, so blocking here never loses the record
// of what ran. Cost/tokens are accrued for ANY worker, even one that errored.
func (e *Engine) accrueUsageOrBlock(state *core.State, stage workflows.Stage, ws *core.WorkerStatus, workerID string, result core.Result, rs *runState) (ok bool, err error) {
	if uerr := validateReportedUsage(result.TokensIn, result.TokensOut, result.CostReported, result.CostUSD); uerr != nil {
		// The worker DID run and produce evidence, but its numbers can't be trusted. Give it a
		// TERMINAL status BEFORE blocking: leaving it `running` while block clears active_worker
		// makes resume mis-classify it as interrupted and CANCEL it, though it actually finished.
		// Critical write — if the terminal status does not land, do not announce the block.
		fin := time.Now().UTC()
		ws.Status = core.WorkerFailed
		ws.FinishedAt = &fin
		e.criticalWrite(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
		if perr := e.persistFailed(); perr != nil {
			return false, perr
		}
		e.block(state, fmt.Sprintf("worker %s reported invalid usage and the run cannot trust its budget: %v", workerID, uerr))
		return false, nil
	}
	e.accrueReportedUsage(state, stage.Name, workerID, result)
	state.Budgets.WallClockSpentSeconds = rs.wallClockSpent()
	return true, nil
}

// finishWorker records a SUCCESSFUL worker's terminal state and announces it — PERSIST →
// CHECK → ANNOUNCE, so the run never reaches `completed` with a worker whose done-status
// never landed. ok=false means a must-succeed write failed and the caller must halt.
func (e *Engine) finishWorker(state *core.State, stage workflows.Stage, ws *core.WorkerStatus, workerID string, result core.Result, rs *runState) (ok bool, err error) {
	fin := time.Now().UTC()
	ws.Status = core.WorkerDone
	ws.FinishedAt = &fin
	ws.SessionID = result.SessionID
	e.criticalWrite(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
	if perr := e.persistFailed(); perr != nil {
		return false, perr
	}
	e.emit(state, stage.Name, workerID, core.EventWorkerFinished, map[string]any{"cost_usd": result.CostUSD})
	rs.lastSummary = truncate(result.Markdown, 2000)
	e.warn(e.store.SaveStageSummary(state.RunID, stage.Name, []byte(rs.lastSummary)), "save stage summary")
	state.ActiveWorker = ""
	e.saveState(state)
	return true, nil
}

// applyVerdict parses the reviewer's structured verdict, persists it as the
// review's evidence, emits events, and decides the branch. A missing or invalid
// verdict, or a "blocked" status, stops the run with an explicit reason — it
// NEVER falls through to "approved". "needs_fixes" loops to the fix stage until
// the auto-fix budget (workflow.maxAutoIterations) is spent.
func (e *Engine) applyVerdict(state *core.State, stage workflows.Stage, result core.Result) bool {
	if err := e.persistReviewVerdict(state, stage, result); err != nil {
		e.block(state, err.Error())
		return false
	}
	return e.decideFromVerdict(state, stage)
}

// persistReviewVerdict parses the reviewer's structured verdict and persists it as
// the review's EVIDENCE, emitting the verdict + findings events. It only records —
// the branch is decided separately (decideFromVerdict), by reading this back. That
// split is what lets host-first persist the evidence BEFORE committing the reviewer's
// close and still recompute the same branch on a recovery.
func (e *Engine) persistReviewVerdict(state *core.State, stage workflows.Stage, result core.Result) error {
	iteration := state.Iterations[stage.Name]
	v, err := core.ParseVerdictFromResult(result)
	if err != nil {
		return fmt.Errorf("review stage %q produced no valid verdict: %v", stage.Name, err)
	}
	v.Stage, v.Iteration, v.CapturedAt = stage.Name, iteration, time.Now().UTC()
	// The verdict is the public evidence that justifies the transition — and the
	// engine reads it back to choose the branch. If it cannot be persisted, refuse
	// to proceed rather than transition on evidence that was never recorded.
	if err := e.store.SaveReviewVerdict(state.RunID, stage.Name, iteration, &v); err != nil {
		return fmt.Errorf("cannot persist review verdict for %q (iteration %d): %v — refusing to transition on unrecorded evidence", stage.Name, iteration, err)
	}
	e.emit(state, stage.Name, "", core.EventReviewCompleted, map[string]any{
		"status": string(v.Status), "findings": len(v.Findings), "iteration": iteration,
	})
	for _, f := range v.Findings {
		e.emit(state, stage.Name, "", core.EventReviewFindings, map[string]any{
			"severity": string(f.Severity), "file": f.File, "message": f.Message,
		})
	}
	return nil
}

// decideFromVerdict routes the run from the PERSISTED verdict — never from the
// prompt, and never from an in-memory value the caller happened to hold. Reading it
// back from disk is what makes the decision reproducible on a retry: the same
// evidence always yields the same branch. "blocked", an exhausted auto-fix budget,
// or an unreadable verdict all stop the run; it NEVER falls through to "approved".
func (e *Engine) decideFromVerdict(state *core.State, stage workflows.Stage) bool {
	iteration := state.Iterations[stage.Name]
	v, err := e.store.LoadReviewVerdict(state.RunID, stage.Name, iteration)
	if err != nil {
		e.block(state, fmt.Sprintf("cannot read the persisted verdict for review stage %q (iteration %d) — refusing to transition without verifiable evidence", stage.Name, iteration))
		return false
	}

	switch v.Status {
	case core.VerdictApproved:
		return true // advanceStage recomputes the branch from the persisted verdict
	case core.VerdictNeedsFixes:
		if budget := e.reviewIterationBudget(stage); budget > 0 && iteration >= budget {
			e.block(state, fmt.Sprintf("review loop reached its iteration budget (%d reviews) without approval", budget))
			return false
		}
		return true
	default: // core.VerdictBlocked
		e.block(state, fmt.Sprintf("reviewer blocked the run: %s", v.Summary))
		return false
	}
}

// reviewIterationBudget is the single source of truth for how many review
// iterations the auto-fix loop may run: a per-stage budget
// (budgets.stage.<review>.maxIterations) overrides the workflow-wide default
// (workflow.maxAutoIterations). It counts REVIEWS — N reviews allow up to N-1
// auto-fixes, and the Nth review can still approve.
func (e *Engine) reviewIterationBudget(stage workflows.Stage) int {
	if sb, ok := e.cfg.Budgets.Stage[stage.Name]; ok && sb.MaxIterations > 0 {
		return sb.MaxIterations
	}
	return e.cfg.Workflow.MaxAutoIterations
}

// runGateStage runs each configured verification command. A failing gate blocks
// the run; this verdict — not any agent claim — is what gates the transition.
func (e *Engine) runGateStage(ctx context.Context, state *core.State, _ *runState, stage workflows.Stage) (bool, error) {
	ran := 0
	for _, name := range stage.Gates {
		cmdStr := e.cfg.CommandFor(name)
		if cmdStr == "" {
			continue // not configured / "auto" — gate disabled
		}
		ran++
		advance, err := e.runGate(ctx, state, stage, name, cmdStr, ran)
		if err != nil {
			return false, err
		}
		if !advance {
			return false, nil // the gate blocked the run
		}
	}
	if ran == 0 {
		// The stage WANTED verification (it declares gates) but none were
		// configured. With requireGates on, refuse to report a "completed" run that
		// verified nothing; otherwise warn loudly and continue (demo/fake friendly).
		if len(stage.Gates) > 0 && e.cfg.Workflow.GatesRequired() {
			reason := fmt.Sprintf("stage %q has no verification gates configured — nothing was verified (set commands.%s in vichu.yaml, or workflow.requireGates: false)", stage.Name, strings.Join(stage.Gates, "/"))
			e.emit(state, stage.Name, "", "no_gates_configured", map[string]any{"required": true})
			e.block(state, reason)
			return false, nil
		}
		e.emit(state, stage.Name, "", "no_gates_configured", nil)
		e.log(i18n.T("engine.no_gates"))
	}
	return true, nil
}

// runGate runs one verification gate: policy check, execution, mutation
// backstop, then verdict. advance=false means the gate blocked the run.
func (e *Engine) runGate(ctx context.Context, state *core.State, stage workflows.Stage, name, cmdStr string, n int) (bool, error) {
	spec := gates.Spec{Name: name, Command: splitCommand(cmdStr), Dir: e.repo.Root()}
	// Policy gate BEFORE execution — a configured gate command the policy
	// classifies as dangerous blocks the run instead of running.
	if err := e.policy.CheckCommand(spec.Command); err != nil {
		e.emit(state, stage.Name, "", "policy_blocked", map[string]any{"gate": name, "reason": err.Error()})
		e.block(state, err.Error())
		return false, nil
	}
	e.emit(state, stage.Name, "", core.EventGateStarted, map[string]any{"gate": name, "command": cmdStr})
	state.NextAction = "gate: " + name
	e.saveState(state)
	// A gate can have real, non-idempotent effects (network, a build step). If announcing it
	// did not persist, do NOT run it: a retry would re-run the whole operation, so an effect
	// with no durable record of the attempt could happen twice.
	if perr := e.persistFailed(); perr != nil {
		return false, perr
	}

	// Track what the gate touches: a verification command should not mutate the
	// tree (the backstop for gates that mutate via an interpreter the policy can't
	// introspect, e.g. `python -c '...'`). Set this up BEFORE running the gate and
	// refuse to run if it can't be established: without a backup (block mode) a
	// damaging gate could not be rolled back, and without tracking the backstop
	// can't detect it.
	var backup *workspace.Backup
	var tracker *workspace.Tracker
	if e.cfg.Security.GateMutations != "allow" {
		if e.cfg.Security.GateMutations == "block" {
			b, err := e.repo.BackupChanged()
			if err != nil {
				e.block(state, fmt.Sprintf("cannot back up the working tree before gate %q: %v", name, err))
				return false, nil
			}
			backup = b
		}
		t, err := e.repo.BeginTracking()
		if err != nil {
			e.block(state, fmt.Sprintf("cannot track gate %q mutations: %v", name, err))
			return false, nil
		}
		tracker = t
	}
	v, err := e.gates.Run(ctx, state.RunID, stage.Name, n, spec)
	if err != nil {
		return false, err
	}
	if reason, muts := e.checkGateMutations(state, stage.Name, n, tracker); reason != "" {
		e.rollbackGate(state, stage.Name, backup, muts)
		e.block(state, reason)
		return false, nil
	}
	e.emit(state, stage.Name, "", core.EventGateCompleted, map[string]any{
		"gate": name, "passed": v.Passed, "exit_code": v.ExitCode,
	})
	if v.Passed {
		return true, nil
	}
	// A gate killed by the budget deadline is a budget stop, not a verification
	// failure — the run loop maps it to a budget block.
	if ctx.Err() == context.DeadlineExceeded {
		return false, ctx.Err()
	}
	e.recordGateExcerpt(state, stage.Name, name, n, v)
	e.block(state, fmt.Sprintf("gate %q failed (exit %d) — see %s", name, v.ExitCode, v.OutputPath))
	return false, nil
}

// rollbackGate restores every existing file a blocking gate modified or
// deleted, preventing loss of real user work. Dirty/untracked files come from
// the content backup; tracked-and-clean files (not in the backup) are restored
// from HEAD via git.
func (e *Engine) rollbackGate(state *core.State, stage string, backup *workspace.Backup, muts []core.Mutation) {
	restored := 0
	if backup != nil {
		if n, err := backup.Restore(); err != nil {
			e.emit(state, stage, "", "gate_rollback_failed", map[string]any{"error": err.Error(), "restored": n})
		} else {
			restored += n
		}
	}

	// Tracked-and-clean files the gate changed are not in the backup; recover
	// them from the last commit.
	var fromHEAD []string
	for _, m := range muts {
		if m.Kind != core.MutationModified && m.Kind != core.MutationDeleted {
			continue
		}
		if backup == nil || !backup.Has(m.Path) {
			fromHEAD = append(fromHEAD, m.Path)
		}
	}
	if n, err := e.repo.RestoreBaseline(fromHEAD); err != nil {
		e.emit(state, stage, "", "gate_rollback_failed", map[string]any{"error": err.Error(), "paths": fromHEAD})
	} else {
		restored += n
	}

	if restored > 0 {
		e.emit(state, stage, "", "gate_rolled_back", map[string]any{"restored": restored})
	}
}

// recordGateExcerpt persists a bounded excerpt of a failed gate's output
// (context budget: agents and views consume this, never the full log) and
// records any truncation — silent truncation is forbidden.
func (e *Engine) recordGateExcerpt(state *core.State, stage, name string, n int, v *core.GateVerdict) {
	maxBytes := e.cfg.Budgets.Context.MaxLogExcerptKB * 1024
	text, truncated, err := gates.Excerpt(v.OutputPath, maxBytes)
	if err != nil {
		return
	}
	e.warn(e.store.SaveGateExcerpt(state.RunID, stage, n, []byte(text)), "save gate excerpt")
	if truncated {
		e.emit(state, stage, "", core.EventOutputTruncated, map[string]any{
			"gate": name, "full_bytes": v.OutputBytes, "excerpt_bytes": maxBytes,
		})
	}
}

// trackMutations diffs the working tree, flags out-of-scope and sensitive
// changes, persists mutations.json, emits events, and returns a non-empty
// blocking reason when the security policy says the run must not proceed.
func (e *Engine) trackMutations(state *core.State, stage workflows.Stage, workerID, baseSHA string, tracker *workspace.Tracker) string {
	if tracker == nil {
		return ""
	}
	muts, err := tracker.Finish()
	if err != nil {
		// Cannot finalize the mutation audit: we no longer know what the worker
		// changed, so the read-only / scope / sensitive-file policy can't be
		// applied. Block rather than proceed without evidence.
		return fmt.Sprintf("cannot finalize mutation audit for worker %s: %v", workerID, err)
	}
	for i := range muts {
		if !workspace.InScope(muts[i].Path, stage.Scope) {
			muts[i].OutOfScope = true
		}
	}
	report := &core.MutationReport{
		Worker: workerID, Stage: stage.Name, BaseSHA: baseSHA,
		Mutations: muts, CapturedAt: time.Now().UTC(),
	}
	e.criticalWrite(e.store.SaveMutationReport(state.RunID, workerID, report), "save mutation report")
	e.emit(state, stage.Name, workerID, core.EventMutationTracked, map[string]any{"count": len(muts)})
	for _, m := range muts {
		if m.HostBookkeeping {
			// Not attributed to the worker, but not hidden either: this is the host's own
			// permission allowlist, so a change here is worth seeing even when we don't block.
			e.emit(state, stage.Name, workerID, "host_bookkeeping_mutation", map[string]any{
				"path": m.Path, "hash": m.Hash,
			})
			continue
		}
		if m.OutOfScope {
			e.emit(state, stage.Name, workerID, core.EventOutOfScopeMut, map[string]any{"path": m.Path})
		}
		if m.Sensitive {
			e.emit(state, stage.Name, workerID, "sensitive_mutation", map[string]any{"path": m.Path})
		}
	}
	return mutationPolicyVerdict(stage, muts, e.cfg.Security)
}

// checkGateMutations records any files a gate changed and decides whether that
// is allowed. Gates are verification commands: modifying or deleting an existing
// tracked OR pre-existing untracked file blocks the run by default
// (security.gateMutations), while new untracked files the gate creates (test
// caches, coverage) only produce an event — gitignored artifacts never appear
// here. Returns a blocking reason (and the mutations), or "" to proceed.
func (e *Engine) checkGateMutations(state *core.State, stage string, n int, tracker *workspace.Tracker) (string, []core.Mutation) {
	if tracker == nil || e.cfg.Security.GateMutations == "allow" {
		return "", nil
	}
	muts, err := tracker.Finish()
	if err != nil {
		// Cannot tell whether the gate mutated the tree — never treat it as safe.
		return fmt.Sprintf("cannot verify gate %q mutations (run %d): %v", stage, n, err), nil
	}
	if len(muts) == 0 {
		return "", nil
	}
	report := &core.MutationReport{
		Worker: fmt.Sprintf("gate:%s:%d", stage, n), Stage: stage,
		BaseSHA: e.repo.BaseID(), Mutations: muts, CapturedAt: time.Now().UTC(),
	}
	// The gate's mutation report is the PROMISED public audit of what the gate changed — it is
	// must-succeed, not warn. If it does not persist, the run must not complete having recorded
	// a gate_mutation event with no report behind it.
	e.criticalWrite(e.store.SaveGateMutationReport(state.RunID, stage, n, report), "save gate mutation report")
	e.emit(state, stage, "", "gate_mutation", map[string]any{"gate_n": n, "count": len(muts)})

	if e.cfg.Security.GateMutations == "warn" {
		return "", muts
	}
	// block mode: a gate that modifies or deletes a file that ALREADY EXISTED stops the
	// run — tracked, untracked, or ignored, because none of those tell you the file is
	// disposable. A file the gate genuinely CREATED is recorded and allowed through.
	for _, m := range muts {
		if gateOutputAllowed(m, e.cfg.Security.GateOutputs) {
			continue
		}
		switch m.Kind {
		case core.MutationDeleted:
			return fmt.Sprintf("gate deleted %s — gates must only verify, not change the tree%s", m.Path, gateOutputHint(m)), muts
		case core.MutationModified:
			return fmt.Sprintf("gate modified %s — gates must only verify, not change the tree%s", m.Path, gateOutputHint(m)), muts
		}
	}
	return "", muts
}

// gateOutputAllowed reports a pre-existing file a gate may rewrite: one the project has
// EXPLICITLY declared as a gate output in `security.gateOutputs`.
//
// It is declared and not inferred. The tempting shortcut is "the file is gitignored, so it
// must be build output" — but an ignored file can just as easily be a private note, a
// credential, a certificate, or a local config, and a global gitignore can make it a file
// the project never mentioned at all. Guessing there means a gate can quietly overwrite
// something irreplaceable. So the project says which paths are disposable, or none are.
//
// A sensitive path is never allowed, whatever the allowlist says: `.env` is gitignored
// precisely because it holds secrets, and no gate has business rewriting it.
func gateOutputAllowed(m core.Mutation, allowed []string) bool {
	if m.Sensitive {
		return false
	}
	return workspace.InScope(m.Path, allowed) && len(allowed) > 0
}

// gateOutputHint tells the user how to allow a path they *meant* their gate to write —
// a coverage profile, a log — without which the message is just a wall.
func gateOutputHint(m core.Mutation) string {
	if m.Sensitive {
		return " (security.gateMutations: block). This path is sensitive and cannot be allowlisted"
	}
	return fmt.Sprintf(" (security.gateMutations: block). If your gate is meant to write this, declare it:\n    security:\n      gateOutputs: [%q]", m.Path)
}

// mutationPolicyVerdict applies the security policy to a worker's mutations and
// returns a blocking reason, or "" when the run may proceed. Pure function so
// every branch is unit-testable.
func mutationPolicyVerdict(stage workflows.Stage, muts []core.Mutation, sec config.SecurityConfig) string {
	// The coding host's machine-local permission file (settings.local.json) is the one
	// mutation VichuFlow cannot attribute: the HOST rewrites it every time the user
	// approves a command (normal, constant, and the agent touched nothing), and an AGENT
	// could write it to grant itself tools (an escalation). On disk the two are identical
	// — host-first means we do not launch the agent, so we cannot see which happened.
	//
	// So we do not pretend. `security.hostLocalState` decides, and the DEFAULT is warn:
	// blocking by default would kill a run the first time the user clicks "approve",
	// which is the bug that motivated all of this. Users who pre-authorize every command
	// their agents need can set `block` and get the guarantee back. Either way the change
	// is RECORDED with its hash and announced with its own event — the escalation cannot
	// be silent, and it cannot fool the kernel, which still runs the gates itself.
	//
	// Everything else under .claude/ and .agents/ is a normal — and sensitive — mutation.
	if hostState := hostLocalStateVerdict(muts, sec); hostState != "" {
		return hostState
	}
	// HostBookkeeping is the ONLY exemption. Being ignored is not one: a read-only stage
	// that mutated anything blocks, whatever your .gitignore says about the path. An
	// ignored file can be a private note or a credential, and `security.gateOutputs` — the
	// escape hatch for build output — is deliberately scoped to GATES, which is where the
	// legitimate need is. A worker has no business rewriting your coverage file either.
	attributed := make([]core.Mutation, 0, len(muts))
	for _, m := range muts {
		if !m.HostBookkeeping {
			attributed = append(attributed, m)
		}
	}
	if stage.ReadOnly && len(attributed) > 0 {
		return fmt.Sprintf("stage %q is read-only but the worker modified %d file(s), starting with %s", stage.Name, len(attributed), attributed[0].Path)
	}
	return attributedMutationVerdict(attributed, sec)
}

// attributedMutationVerdict applies the sensitive / out-of-scope policy to the mutations
// actually attributed to the worker (host bookkeeping already filtered out).
func attributedMutationVerdict(muts []core.Mutation, sec config.SecurityConfig) string {
	for _, m := range muts {
		if m.Sensitive && sec.SensitiveMutations != "warn" {
			return fmt.Sprintf("worker modified sensitive file %s (security.sensitiveMutations: block)", m.Path)
		}
	}
	if sec.OutOfScopeMutations != "block" {
		return ""
	}
	for _, m := range muts {
		if m.OutOfScope {
			return fmt.Sprintf("worker modified %s outside the stage's declared scope (security.outOfScopeMutations: block)", m.Path)
		}
	}
	return ""
}

// hostLocalStateVerdict applies security.hostLocalState to a change in the coding host's
// permission file. With `block`, ANY change stops the run — the setting exists precisely
// for people who have pre-authorized what their agents need, so the file moving mid-run
// means something they did not expect wrote it.
func hostLocalStateVerdict(muts []core.Mutation, sec config.SecurityConfig) string {
	if sec.HostLocalState != "block" {
		return ""
	}
	for _, m := range muts {
		if m.HostBookkeeping {
			return fmt.Sprintf("the coding host's permission file %s changed during this worker — VichuFlow cannot tell whether the host wrote it (you approved a command) or the agent did (granting itself tools), so with security.hostLocalState: block it stops the run. Inspect the file, then `vichu run resume --accept-changes` if it was you", m.Path)
		}
	}
	return ""
}

func (e *Engine) persistWorkerResult(runID, workerID string, result core.Result) {
	e.criticalWrite(e.store.WriteWorkerFile(runID, workerID, "result.md", []byte(result.Markdown)), "write worker result")
	if data, err := json.MarshalIndent(result, "", "  "); err == nil {
		e.criticalWrite(e.store.WriteWorkerFile(runID, workerID, "result.json", append(data, '\n')), "write worker result json")
	}
	if result.SessionID != "" {
		session := map[string]string{"session_id": result.SessionID}
		if data, err := json.MarshalIndent(session, "", "  "); err == nil {
			e.criticalWrite(e.store.WriteWorkerFile(runID, workerID, "session.json", append(data, '\n')), "write worker session")
		}
	}
}

// handleWorkerError maps a worker session error to the right terminal outcome —
// canceled (external cancel or a budget deadline) vs failed — and returns what
// runWorkerStage should propagate to the run loop.
func (e *Engine) handleWorkerError(ctx context.Context, state *core.State, ws *core.WorkerStatus, workerID string, err error) (bool, error) {
	if e.canceledOnDisk(state.RunID) {
		e.markWorker(state, ws, core.WorkerCanceled)
		return false, nil // the run loop finalizes the canceled state
	}
	if ctx.Err() != nil {
		e.markWorker(state, ws, core.WorkerCanceled)
		return false, ctx.Err() // the run loop maps deadlines to a budget block
	}
	e.markWorker(state, ws, core.WorkerFailed)
	return false, fmt.Errorf("worker %s: %w", workerID, err)
}

func (e *Engine) markWorkerFailed(state *core.State, ws *core.WorkerStatus) {
	e.markWorker(state, ws, core.WorkerFailed)
}

func (e *Engine) markWorker(state *core.State, ws *core.WorkerStatus, status core.WorkerState) {
	fin := time.Now().UTC()
	ws.Status = status
	ws.FinishedAt = &fin
	e.warn(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
}

// addStageTokens accumulates a worker's token spend into its stage's running
// total, so per-stage token budgets bound a stage's CUMULATIVE cost across all
// its iterations (a review→fix loop is the motivating case).
func addStageTokens(state *core.State, stage string, in, out int) {
	if in == 0 && out == 0 {
		return
	}
	if state.Budgets.StageTokensIn == nil {
		state.Budgets.StageTokensIn = map[string]int{}
	}
	if state.Budgets.StageTokensOut == nil {
		state.Budgets.StageTokensOut = map[string]int{}
	}
	state.Budgets.StageTokensIn[stage] = addClamped(state.Budgets.StageTokensIn[stage], in)
	state.Budgets.StageTokensOut[stage] = addClamped(state.Budgets.StageTokensOut[stage], out)
}

// addClamped adds without wrapping. Signed overflow in Go wraps to a large NEGATIVE
// number, which would RESET a run's token budget to below zero — the one number a
// runaway agent must never be able to reset. Saturating at MaxInt keeps every
// `spent >= max` comparison true, so the cap holds.
func addClamped(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// accrueReportedUsage adds the cost/tokens a worker actually reported to the run
// budget and emits a token_usage event. Cost and tokens are independent (codex
// reports tokens but not USD cost), so an unreported dimension is left untouched —
// it stays "unknown" rather than accruing a fake zero. Shared by the headless
// runner (stage.go) and the host-first close path (applyUsage) so both account
// usage identically.
func (e *Engine) accrueReportedUsage(state *core.State, stageName, workerID string, r core.Result) {
	if r.CostReported {
		state.Budgets.CostReported = true
		state.Budgets.CostUSDSpent += r.CostUSD
	}
	if !r.TokensReported {
		return
	}
	state.Budgets.TokensReported = true
	state.Budgets.TokensInSpent = addClamped(state.Budgets.TokensInSpent, r.TokensIn)
	state.Budgets.TokensOutSpent = addClamped(state.Budgets.TokensOutSpent, r.TokensOut)
	addStageTokens(state, stageName, r.TokensIn, r.TokensOut)
	if r.TokensIn > 0 || r.TokensOut > 0 {
		e.emit(state, stageName, workerID, "token_usage", map[string]any{
			"tokens_in": r.TokensIn, "tokens_out": r.TokensOut,
			"run_tokens_total": state.Budgets.TokensTotalSpent(),
		})
	}
}

// preWorkerBudgetBlocked runs the budget gates that must pass before starting an
// agent (run-level, agent-invocation, per-stage tokens) in order; it emits the
// budget event, blocks the run, and returns true on the first one exhausted.
func (e *Engine) preWorkerBudgetBlocked(state *core.State, stage workflows.Stage) bool {
	gates := []func() string{
		func() string { return e.checkBudgets(state) },
		func() string { return e.agentBudgetExceeded(state) },
		func() string { return e.stageTokenBudgetExceeded(state, stage) },
	}
	for _, gate := range gates {
		if reason := gate(); reason != "" {
			e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
			e.block(state, reason)
			return true
		}
	}
	return false
}

// stageTokenBudgetExceeded returns a non-empty reason when a stage has already
// spent its configured token budget (cumulative across iterations). It gates
// re-entry into the stage — so a runaway review loop stops instead of burning
// more tokens. (A single over-budget call can't be pre-empted without adapter
// support; this caps the LOOP, and reviewContext:diff-only caps the single call.)
func (e *Engine) stageTokenBudgetExceeded(state *core.State, stage workflows.Stage) string {
	sb, ok := e.cfg.Budgets.Stage[stage.Name]
	if !ok {
		return ""
	}
	if sb.MaxTotalTokens > 0 && state.Budgets.StageTokensTotal(stage.Name) >= sb.MaxTotalTokens {
		return fmt.Sprintf("stage %q exceeded its token budget (%d total tokens)", stage.Name, sb.MaxTotalTokens)
	}
	if sb.MaxInputTokens > 0 && state.Budgets.StageTokensIn[stage.Name] >= sb.MaxInputTokens {
		return fmt.Sprintf("stage %q exceeded its input-token budget (%d)", stage.Name, sb.MaxInputTokens)
	}
	if sb.MaxOutputTokens > 0 && state.Budgets.StageTokensOut[stage.Name] >= sb.MaxOutputTokens {
		return fmt.Sprintf("stage %q exceeded its output-token budget (%d)", stage.Name, sb.MaxOutputTokens)
	}
	return ""
}

// agentBudgetExceeded returns a non-empty reason when starting another agent
// would exceed the run's agent-invocation budget. It is checked only before a
// worker/review stage starts an agent, so a budget of N allows exactly N agents
// while still permitting the gate and terminal stages to run.
func (e *Engine) agentBudgetExceeded(state *core.State) string {
	b := e.cfg.Budgets.Run
	if b.MaxAgentInvocations > 0 && state.Budgets.AgentInvocations >= b.MaxAgentInvocations {
		return fmt.Sprintf("agent invocation budget exhausted (%d)", b.MaxAgentInvocations)
	}
	return ""
}

// checkBudgets returns a non-empty reason if a run-level RESOURCE budget
// (wall-clock, cost, tokens) is exhausted. These are hard limits that gate every
// stage — including gates and completion — so a run never finishes over budget.
// The agent-invocation budget is NOT here: it gates only the START of an agent
// (see agentBudgetExceeded), so spending it does not block verify/done.
func (e *Engine) checkBudgets(state *core.State) string {
	b := e.cfg.Budgets.Run
	if b.MaxWallClock > 0 && state.Budgets.WallClockSpentSeconds >= b.MaxWallClock.Std().Seconds() {
		return fmt.Sprintf("wall-clock budget exhausted (%s)", b.MaxWallClock.Std())
	}
	if b.MaxCostUSD > 0 && state.Budgets.CostUSDSpent >= b.MaxCostUSD {
		return fmt.Sprintf("cost budget exhausted ($%.2f)", b.MaxCostUSD)
	}
	if b.MaxInputTokens > 0 && state.Budgets.TokensInSpent >= b.MaxInputTokens {
		return fmt.Sprintf("input-token budget exhausted (%d)", b.MaxInputTokens)
	}
	if b.MaxOutputTokens > 0 && state.Budgets.TokensOutSpent >= b.MaxOutputTokens {
		return fmt.Sprintf("output-token budget exhausted (%d)", b.MaxOutputTokens)
	}
	if b.MaxTotalTokens > 0 && state.Budgets.TokensTotalSpent() >= b.MaxTotalTokens {
		return fmt.Sprintf("total-token budget exhausted (%d)", b.MaxTotalTokens)
	}
	return ""
}

// checkDrift reports whether the live repo diverged from the run's snapshot in
// a way that makes resuming unsafe. It compares content fingerprints, not just
// paths: the expected state is the snapshot's fingerprints overlaid with the
// run's own recorded mutations (workers are chronological by id); anything in
// the working tree that differs from that — a new file, an external edit to a
// file the run touched, or a vanished change — is drift.
func (e *Engine) checkDrift(runID string, snap *core.Workspace) (bool, string) {
	if head := e.repo.BaseID(); head != snap.BaseSHA {
		return true, "base commit changed since the run started"
	}
	current, err := e.repo.FingerprintChanged()
	if err != nil {
		return false, ""
	}
	expected := e.expectedFingerprints(runID, snap)
	if p := legacySymlinkPath(snap, current, expected); p != "" {
		return true, fmt.Sprintf("this run was started by an older VichuFlow that fingerprinted the symlink %s by following it and hashing the file it pointed at. "+
			"It is now fingerprinted by its target TEXT, so the two cannot be compared and we will not guess whether the link changed. "+
			"Review the diff, then resume with --accept-changes to re-baseline this run", p)
	}
	return driftReason(current, expected)
}

// legacySymlinkPath returns the first path whose persisted fingerprint cannot be compared
// with the one we compute today, or "" when they are comparable.
//
// The symlink fingerprint format changed: older versions followed the link and hashed the
// content it pointed at; a link is now hashed by its target text. A run snapshotted before
// that has a bare hex hash where we now produce a "symlink:" one — which reads as drift and
// blocked the run with "external modification", a message that is simply false. Nothing
// external changed. Rather than read through the link to reconstruct the old value (which
// is the escape this whole layer exists to prevent), the run is failed closed and told what
// actually happened.
func legacySymlinkPath(snap *core.Workspace, current, expected map[string]string) string {
	if snap.FingerprintVersion == core.FingerprintSymlinkTarget {
		return ""
	}
	for _, p := range sortedPaths(current) {
		if !workspace.IsSymlinkFingerprint(current[p]) {
			continue
		}
		if eh, ok := expected[p]; ok && !workspace.IsSymlinkFingerprint(eh) {
			return p
		}
	}
	return ""
}

// sortedPaths keeps the reported path stable when several would qualify.
func sortedPaths(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// expectedFingerprints is the working-tree state the run itself produced: the
// snapshot's fingerprints overlaid with the run's own recorded mutations.
func (e *Engine) expectedFingerprints(runID string, snap *core.Workspace) map[string]string {
	expected := make(map[string]string, len(snap.Fingerprints))
	for p, h := range snap.Fingerprints {
		expected[p] = h
	}
	workers, err := e.store.ListWorkers(runID)
	if err != nil {
		return expected
	}
	for _, w := range workers { // sorted = chronological; later workers win
		r, err := e.store.LoadMutationReport(runID, w)
		// Only overlay mutations newer than the snapshot: after an explicit
		// re-baseline, the fresh snapshot already reflects older mutations
		// (possibly hand-edited since) and must win over their stale hashes.
		if err != nil || r.CapturedAt.Before(snap.CapturedAt) {
			continue
		}
		for _, m := range r.Mutations {
			expected[m.Path] = m.Hash
		}
	}
	return expected
}

// driftReason compares the live working tree against the expected state and
// returns the first divergence, or false when they match.
func driftReason(current, expected map[string]string) (bool, string) {
	for p, h := range current {
		switch eh, ok := expected[p]; {
		case !ok:
			return true, "external change to " + p
		case eh != h:
			return true, "external modification to " + p
		}
	}
	for p, h := range expected {
		if h == "" {
			continue // the run deleted this path; its absence now is expected
		}
		if _, ok := current[p]; !ok {
			return true, "change to " + p + " disappeared since the run's last checkpoint"
		}
	}
	return false, ""
}

func agentEventName(kind string) string {
	switch kind {
	case adapters.EventToolUse:
		return core.EventToolUse
	case adapters.EventText:
		return core.EventAgentText
	default:
		return kind
	}
}

func agentEventDetail(ev adapters.AgentEvent, adapterName string) map[string]any {
	d := map[string]any{"adapter": adapterName}
	if ev.Text != "" {
		d["text"] = truncate(ev.Text, 500)
	}
	for k, v := range ev.Detail {
		d[k] = v
	}
	return d
}
