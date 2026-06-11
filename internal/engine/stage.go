package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/gates"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// runWorkerStage invokes an agent for a stage, capturing its events, result,
// and the exact set of files it mutated. It returns advance=false (without
// error) when a budget guard blocks the run.
func (e *Engine) runWorkerStage(ctx context.Context, state *core.State, rs *runState, stage workflows.Stage) (bool, error) {
	if blocked := e.checkBudgets(state); blocked != "" {
		e.emit(state, stage.Name, "", core.EventBudgetExceeded, map[string]any{"reason": blocked})
		e.block(state, blocked)
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

	inv := adapters.Invocation{
		Role:             stage.Role,
		Prompt:           prompt,
		WorkDir:          e.repo.Root(),
		Model:            agentCfg.Model,
		Effort:           agentCfg.Effort,
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

	now := time.Now().UTC()
	ws := &core.WorkerStatus{
		ID: workerID, Role: stage.Role, Adapter: adapter.Name(),
		Stage: stage.Name, Status: core.WorkerRunning, StartedAt: now,
	}
	_ = e.store.SaveWorkerStatus(state.RunID, ws)
	_ = e.store.WriteWorkerFile(state.RunID, workerID, "prompt.md", []byte(prompt))
	state.ActiveWorker = workerID
	state.NextAction = "running " + stage.Role
	e.saveState(state)
	e.emit(state, stage.Name, workerID, core.EventWorkerStarted, map[string]any{"adapter": adapter.Name(), "role": stage.Role})

	tracker, _ := e.repo.BeginTracking()

	sess, err := adapter.Start(ctx, inv)
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
	if err != nil {
		// A worker killed by cancellation or a budget deadline is canceled, not
		// done/failed: the audit must reflect what actually happened.
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
	policyBlock := e.trackMutations(state, stage, workerID, rs.baseSHA, tracker)

	// Aggregate per-worker usage into the run-level totals — the central
	// accounting a multi-agent run needs to know it is saving, not burning.
	state.Budgets.CostUSDSpent += result.CostUSD
	state.Budgets.TokensInSpent += result.TokensIn
	state.Budgets.TokensOutSpent += result.TokensOut
	state.Budgets.WallClockSpentSeconds = rs.wallClockSpent()

	fin := time.Now().UTC()
	ws.Status = core.WorkerDone
	ws.FinishedAt = &fin
	ws.SessionID = result.SessionID
	_ = e.store.SaveWorkerStatus(state.RunID, ws)
	e.emit(state, stage.Name, workerID, core.EventWorkerFinished, map[string]any{"cost_usd": result.CostUSD})
	if result.TokensIn > 0 || result.TokensOut > 0 {
		e.emit(state, stage.Name, workerID, "token_usage", map[string]any{
			"tokens_in": result.TokensIn, "tokens_out": result.TokensOut,
			"run_tokens_total": state.Budgets.TokensTotalSpent(),
		})
	}

	rs.lastSummary = truncate(result.Markdown, 2000)
	_ = e.store.SaveStageSummary(state.RunID, stage.Name, []byte(rs.lastSummary))
	state.ActiveWorker = ""
	e.saveState(state)

	if policyBlock != "" {
		e.block(state, policyBlock)
		return false, nil
	}
	return true, nil
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
	// tree. This is the backstop for gates that mutate via an interpreter the
	// policy can't introspect (e.g. `python -c '...'`). In block mode we also
	// back up at-risk user work first, so the gate's damage can be rolled back.
	var backup *workspace.Backup
	if e.cfg.Security.GateMutations == "block" {
		backup, _ = e.repo.BackupChanged()
	}
	tracker, _ := e.repo.BeginTracking()
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
	_ = e.store.SaveGateExcerpt(state.RunID, stage, n, []byte(text))
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
		return ""
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
	_ = e.store.SaveMutationReport(state.RunID, workerID, report)
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
	if err != nil || len(muts) == 0 {
		return "", nil
	}
	report := &core.MutationReport{
		Worker: fmt.Sprintf("gate:%s:%d", stage, n), Stage: stage,
		BaseSHA: e.repo.HeadSHA(), Mutations: muts, CapturedAt: time.Now().UTC(),
	}
	_ = e.store.SaveGateMutationReport(state.RunID, stage, n, report)
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
	_ = e.store.WriteWorkerFile(runID, workerID, "result.md", []byte(result.Markdown))
	if data, err := json.MarshalIndent(result, "", "  "); err == nil {
		_ = e.store.WriteWorkerFile(runID, workerID, "result.json", append(data, '\n'))
	}
	if result.SessionID != "" {
		session := map[string]string{"session_id": result.SessionID}
		if data, err := json.MarshalIndent(session, "", "  "); err == nil {
			_ = e.store.WriteWorkerFile(runID, workerID, "session.json", append(data, '\n'))
		}
	}
}

func (e *Engine) markWorkerFailed(state *core.State, ws *core.WorkerStatus) {
	e.markWorker(state, ws, core.WorkerFailed)
}

func (e *Engine) markWorker(state *core.State, ws *core.WorkerStatus, status core.WorkerState) {
	fin := time.Now().UTC()
	ws.Status = status
	ws.FinishedAt = &fin
	_ = e.store.SaveWorkerStatus(state.RunID, ws)
}

// checkBudgets returns a non-empty reason if a run-level budget is exhausted.
func (e *Engine) checkBudgets(state *core.State) string {
	b := e.cfg.Budgets.Run
	if b.MaxAgentInvocations > 0 && state.Budgets.AgentInvocations >= b.MaxAgentInvocations {
		return fmt.Sprintf("agent invocation budget exhausted (%d)", b.MaxAgentInvocations)
	}
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
