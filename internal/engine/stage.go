package engine

import (
	"context"
	"encoding/json"
	"fmt"
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

// runWorkerStage invokes an agent for a stage, capturing its events, result,
// and the exact set of files it mutated. It returns advance=false (without
// error) when a budget guard blocks the run.
func (e *Engine) runWorkerStage(ctx context.Context, state *core.State, rs *runState, stage workflows.Stage) (bool, error) {
	if blocked := e.checkBudgets(state); blocked != "" {
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": blocked})
		e.block(state, blocked)
		return false, nil
	}

	// The agent-invocation budget gates STARTING an agent — not gates or the
	// terminal stage. Check it here, before incrementing, so a budget of N permits
	// exactly N agents and still lets verify/done run.
	if reason := e.agentBudgetExceeded(state); reason != "" {
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
		e.block(state, reason)
		return false, nil
	}

	// Per-stage token budget: if this stage already burned its token cap across
	// previous iterations (e.g. a review→fix loop), stop before spending more.
	if reason := e.stageTokenBudgetExceeded(state, stage); reason != "" {
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": reason})
		e.block(state, reason)
		return false, nil
	}

	agentCfg := e.cfg.Agent(stage.Role)
	adapter, err := e.registry.Get(agentCfg.Provider)
	if err != nil {
		return false, err
	}

	state.Budgets.AgentInvocations++
	workerID := fmt.Sprintf("%s-%02d", stage.Name, state.Budgets.AgentInvocations)
	prompt := buildPrompt(rs.pack, stage.Instruction, state.Task, rs.lastSummary, e.cfg)
	// A review stage gets the change set inline (diff-only) so it judges the work
	// without re-reading the whole repo — fewer tokens, less free exploration.
	prompt = e.withReviewContext(prompt, state, stage.Name, stage.Kind == workflows.KindReview)

	inv := adapters.Invocation{
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
		// Policy gate BEFORE execution: a worker command that needs human
		// confirmation (or is denied) blocks the run instead of ever running.
		if err := e.policy.CheckCommand(inv.Command); err != nil {
			e.emit(state, stage.Name, workerID, "policy_blocked", map[string]any{"reason": err.Error()})
			e.block(state, err.Error())
			return false, nil
		}
	}

	// Start mutation tracking BEFORE the worker runs — refuse to run an agent we
	// cannot audit. Without tracking there is no mutations.json, and the
	// read-only / scope / sensitive-file policy checks would silently not apply.
	tracker, err := e.repo.BeginTracking()
	if err != nil {
		e.block(state, fmt.Sprintf("cannot track worker mutations: %v — refusing to run an agent without an audit trail", err))
		return false, nil
	}

	now := time.Now().UTC()
	ws := &core.WorkerStatus{
		ID: workerID, Role: stage.Role, Adapter: adapter.Name(),
		Stage: stage.Name, Status: core.WorkerRunning, StartedAt: now,
	}
	e.warn(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
	e.warn(e.store.WriteWorkerFile(state.RunID, workerID, "prompt.md", []byte(prompt)), "write worker prompt")
	state.ActiveWorker = workerID
	state.NextAction = "running " + stage.Role
	e.saveState(state)
	e.emit(state, stage.Name, workerID, core.EventWorkerStarted, map[string]any{"adapter": adapter.Name(), "role": stage.Role})

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

	// Aggregate usage/cost for ANY worker that ran — an adapter can report real
	// cost/tokens even on an error result (claude-code does), so the run's budget
	// must reflect what was actually burned, not just successful workers. Emit
	// token_usage too: a failed worker's spend is still part of the timeline.
	state.Budgets.CostUSDSpent += result.CostUSD
	state.Budgets.TokensInSpent += result.TokensIn
	state.Budgets.TokensOutSpent += result.TokensOut
	state.Budgets.WallClockSpentSeconds = rs.wallClockSpent()
	addStageTokens(state, stage.Name, result.TokensIn, result.TokensOut)
	if result.TokensIn > 0 || result.TokensOut > 0 {
		e.emit(state, stage.Name, workerID, "token_usage", map[string]any{
			"tokens_in": result.TokensIn, "tokens_out": result.TokensOut,
			"run_tokens_total": state.Budgets.TokensTotalSpent(),
		})
	}

	if err != nil {
		return e.handleWorkerError(ctx, state, ws, workerID, err)
	}

	fin := time.Now().UTC()
	ws.Status = core.WorkerDone
	ws.FinishedAt = &fin
	ws.SessionID = result.SessionID
	e.warn(e.store.SaveWorkerStatus(state.RunID, ws), warnSaveWorkerStatus)
	e.emit(state, stage.Name, workerID, core.EventWorkerFinished, map[string]any{"cost_usd": result.CostUSD})

	rs.lastSummary = truncate(result.Markdown, 2000)
	e.warn(e.store.SaveStageSummary(state.RunID, stage.Name, []byte(rs.lastSummary)), "save stage summary")
	state.ActiveWorker = ""
	e.saveState(state)

	if policyBlock != "" {
		e.block(state, policyBlock)
		return false, nil
	}
	if stage.Kind == workflows.KindReview {
		return e.applyVerdict(state, stage, result), nil
	}
	return true, nil
}

// applyVerdict parses the reviewer's structured verdict, persists it as the
// review's evidence, emits events, and decides the branch. A missing or invalid
// verdict, or a "blocked" status, stops the run with an explicit reason — it
// NEVER falls through to "approved". "needs_fixes" loops to the fix stage until
// the auto-fix budget (workflow.maxAutoIterations) is spent.
func (e *Engine) applyVerdict(state *core.State, stage workflows.Stage, result core.Result) bool {
	iteration := state.Iterations[stage.Name]
	v, err := core.ParseVerdictFromResult(result)
	if err != nil {
		e.block(state, fmt.Sprintf("review stage %q produced no valid verdict: %v", stage.Name, err))
		return false
	}
	v.Stage, v.Iteration, v.CapturedAt = stage.Name, iteration, time.Now().UTC()
	// The verdict is the public evidence that justifies the transition — and the
	// engine reads it back to choose the branch. If it cannot be persisted, refuse
	// to proceed rather than transition on evidence that was never recorded.
	if err := e.store.SaveReviewVerdict(state.RunID, stage.Name, iteration, &v); err != nil {
		e.block(state, fmt.Sprintf("cannot persist review verdict for %q (iteration %d): %v — refusing to transition on unrecorded evidence", stage.Name, iteration, err))
		return false
	}
	e.emit(state, stage.Name, "", core.EventReviewCompleted, map[string]any{
		"status": string(v.Status), "findings": len(v.Findings), "iteration": iteration,
	})
	for _, f := range v.Findings {
		e.emit(state, stage.Name, "", core.EventReviewFindings, map[string]any{
			"severity": string(f.Severity), "file": f.File, "message": f.Message,
		})
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
	if n, err := e.repo.RestoreFromHEAD(fromHEAD); err != nil {
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
	e.warn(e.store.SaveMutationReport(state.RunID, workerID, report), "save mutation report")
	e.emit(state, stage.Name, workerID, core.EventMutationTracked, map[string]any{"count": len(muts)})
	for _, m := range muts {
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
		BaseSHA: e.repo.HeadSHA(), Mutations: muts, CapturedAt: time.Now().UTC(),
	}
	e.warn(e.store.SaveGateMutationReport(state.RunID, stage, n, report), "save gate mutation report")
	e.emit(state, stage, "", "gate_mutation", map[string]any{"gate_n": n, "count": len(muts)})

	if e.cfg.Security.GateMutations == "warn" {
		return "", muts
	}
	// block mode: a gate that modifies or deletes an existing file (tracked or
	// real untracked work) stops the run. New untracked files are recorded only.
	for _, m := range muts {
		switch m.Kind {
		case core.MutationDeleted:
			return fmt.Sprintf("gate deleted %s — gates must only verify, not change the tree (security.gateMutations: block)", m.Path), muts
		case core.MutationModified:
			return fmt.Sprintf("gate modified %s — gates must only verify, not change the tree (security.gateMutations: block)", m.Path), muts
		}
	}
	return "", muts
}

// mutationPolicyVerdict applies the security policy to a worker's mutations and
// returns a blocking reason, or "" when the run may proceed. Pure function so
// every branch is unit-testable.
func mutationPolicyVerdict(stage workflows.Stage, muts []core.Mutation, sec config.SecurityConfig) string {
	if stage.ReadOnly && len(muts) > 0 {
		return fmt.Sprintf("stage %q is read-only but the worker modified %d file(s), starting with %s", stage.Name, len(muts), muts[0].Path)
	}
	for _, m := range muts {
		if m.Sensitive && sec.SensitiveMutations != "warn" {
			return fmt.Sprintf("worker modified sensitive file %s (security.sensitiveMutations: block)", m.Path)
		}
	}
	if sec.OutOfScopeMutations == "block" {
		for _, m := range muts {
			if m.OutOfScope {
				return fmt.Sprintf("worker modified %s outside the stage's declared scope (security.outOfScopeMutations: block)", m.Path)
			}
		}
	}
	return ""
}

func (e *Engine) persistWorkerResult(runID, workerID string, result core.Result) {
	e.warn(e.store.WriteWorkerFile(runID, workerID, "result.md", []byte(result.Markdown)), "write worker result")
	if data, err := json.MarshalIndent(result, "", "  "); err == nil {
		e.warn(e.store.WriteWorkerFile(runID, workerID, "result.json", append(data, '\n')), "write worker result json")
	}
	if result.SessionID != "" {
		session := map[string]string{"session_id": result.SessionID}
		if data, err := json.MarshalIndent(session, "", "  "); err == nil {
			e.warn(e.store.WriteWorkerFile(runID, workerID, "session.json", append(data, '\n')), "write worker session")
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
	state.Budgets.StageTokensIn[stage] += in
	state.Budgets.StageTokensOut[stage] += out
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
	if head := e.repo.HeadSHA(); head != snap.BaseSHA {
		return true, "base commit changed since the run started"
	}
	current, err := e.repo.FingerprintChanged()
	if err != nil {
		return false, ""
	}
	return driftReason(current, e.expectedFingerprints(runID, snap))
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
