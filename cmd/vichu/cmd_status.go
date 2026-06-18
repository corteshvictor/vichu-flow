package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workflows"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	watch := fs.Bool("watch", false, i18n.T("status.flag_watch"))
	interval := fs.Duration("interval", 2*time.Second, i18n.T("status.flag_interval"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	positionals, err := parseArgsAnyOrder(fs, args)
	if err != nil {
		return err
	}
	if *jsonOut && *watch {
		return errors.New(i18n.T("status.json_watch"))
	}

	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(firstArg(positionals))
	if err != nil {
		return err
	}

	if *jsonOut {
		return printStatusJSON(proj, runID)
	}
	if !*watch {
		return printStatus(proj, runID)
	}

	for {
		fmt.Print("\033[2J\033[H") // clear screen
		if err := printStatus(proj, runID); err != nil {
			return err
		}
		state, err := proj.store.LoadState(runID)
		if err != nil {
			return err
		}
		// Stop following once the run settles: terminal, or blocked/paused
		// awaiting a human — only an active run keeps changing.
		if state.Status.Settled() {
			return nil
		}
		time.Sleep(*interval)
	}
}

// printStatusJSON emits a stable machine-readable snapshot for host packs: the
// fields they need to decide the next step (drive a worker, close a stage, resume,
// observe). The schema is a contract — add fields, don't rename.
func printStatusJSON(proj *project, runID string) error {
	state, err := proj.store.LoadState(runID)
	if err != nil {
		return err
	}
	out := map[string]any{
		"run_id":         state.RunID,
		"status":         string(state.Status),
		"workflow":       state.Workflow,
		"current_stage":  state.CurrentStage,
		"stages":         orderedStages(state),
		"active_worker":  state.ActiveWorker,
		"blocked_reason": state.BlockedReason,
		"next_action":    state.NextAction,
		"budgets":        budgetsJSON(state.Budgets),
	}
	if lk, err := proj.store.InspectLock(runID); err == nil && lk.Present {
		out["lock"] = map[string]any{"present": true, "orphaned": lk.Orphaned, "pid": lk.Lock.PID}
	} else {
		out["lock"] = map[string]any{"present": false}
	}
	out["recent_events"] = recentEventsJSON(proj, runID)
	return printJSON(out)
}

// budgetsJSON renders the budget snapshot. Invocations and wall-clock are always
// kernel-measured; cost and tokens are independent and each is null when its kind
// was not reported (a host may expose tokens but not cost). cost_reported /
// tokens_reported tell consumers which case they are in — null means "unknown",
// not zero.
func budgetsJSON(b core.BudgetState) map[string]any {
	out := map[string]any{
		"agent_invocations": b.AgentInvocations,
		"wall_clock_sec":    b.WallClockSpentSeconds,
		"cost_reported":     b.CostReported,
		"tokens_reported":   b.TokensReported,
		"cost_usd":          nil,
		"tokens_total":      nil,
	}
	if b.CostReported {
		out["cost_usd"] = b.CostUSDSpent
	}
	if b.TokensReported {
		out["tokens_total"] = b.TokensTotalSpent()
	}
	return out
}

// orderedStages returns [{name, status}] in workflow order — a list, not a map,
// so consumers get a stable ordering.
func orderedStages(state *core.State) []map[string]string {
	names := orderedStageNames(state)
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n, "status": string(state.Stages[n])})
	}
	return out
}

func recentEventsJSON(proj *project, runID string) []map[string]any {
	events, err := proj.store.ReadEvents(runID)
	if err != nil {
		return nil
	}
	start := 0
	if len(events) > 12 {
		start = len(events) - 12
	}
	out := make([]map[string]any, 0, len(events)-start)
	for _, ev := range events[start:] {
		out = append(out, map[string]any{"ts": ev.TS.Format(time.RFC3339), "event": ev.Event, "detail": ev.Detail})
	}
	return out
}

func printStatus(proj *project, runID string) error {
	state, err := proj.store.LoadState(runID)
	if err != nil {
		return err
	}
	printStateSummary(state)

	// Surface an orphaned lock so the user knows the run can be resumed.
	if lk, err := proj.store.InspectLock(runID); err == nil && lk.Present {
		if lk.Orphaned {
			fmt.Printf("  "+i18n.T("status.lock_orphaned")+"\n", lk.Lock.PID, runID)
		} else {
			fmt.Printf("  "+i18n.T("status.lock_held")+"\n", lk.Lock.PID)
		}
	}

	// Recent timeline.
	events, err := proj.store.ReadEvents(runID)
	if err == nil && len(events) > 0 {
		fmt.Println("\n  " + i18n.T("status.recent"))
		start := 0
		if len(events) > 8 {
			start = len(events) - 8
		}
		for _, ev := range events[start:] {
			fmt.Printf("    %s  %-20s %s\n", ev.TS.Format("15:04:05"), ev.Event, eventHint(ev))
		}
	}
	return nil
}

func printStateSummary(state *core.State) {
	// row prints one "  <label>: <value>" line; centralizing it avoids
	// repeating the format string and keeps alignment consistent.
	row := func(labelKey, value string) {
		fmt.Printf("  %-9s %s\n", i18n.T(labelKey)+":", value)
	}
	fmt.Printf(i18n.T("status.run")+"\n", state.RunID)
	row("status.status", string(state.Status))
	row("status.workflow", state.Workflow)
	row("status.stage", stageLine(state))
	if state.ActiveWorker != "" {
		row("status.worker", state.ActiveWorker)
	}
	if state.NextAction != "" {
		row("status.next", state.NextAction)
	}
	if state.BlockedReason != "" {
		row("status.blocked", state.BlockedReason)
	}
	cost, tokens := budgetUsageStrings(state.Budgets)
	row("status.budget", fmt.Sprintf(i18n.T("status.budget_line"),
		state.Budgets.AgentInvocations, cost,
		state.Budgets.WallClockSpentSeconds, tokens))
}

// budgetUsageStrings renders the cost and token parts of the budget line. Cost and
// tokens are independent: a host may report tokens but not cost, so each reads
// "unknown" on its own when unreported — never a misleading $0.00 / 0 tokens.
// Invocations and wall-clock are always kernel-measured, rendered by the caller.
func budgetUsageStrings(b core.BudgetState) (cost, tokens string) {
	cost = i18n.T("status.cost_unknown")
	if b.CostReported {
		cost = fmt.Sprintf(i18n.T("status.cost_value"), b.CostUSDSpent)
	}
	tokens = i18n.T("status.tokens_unknown")
	if b.TokensReported {
		tokens = fmt.Sprintf(i18n.T("status.tokens_value"), b.TokensTotalSpent())
	}
	return cost, tokens
}

// stageLine renders the stage progress in WORKFLOW order (explore → implement →
// verify → done), not alphabetically, so observability matches the docs.
func stageLine(state *core.State) string {
	if len(state.Stages) == 0 {
		return state.CurrentStage
	}
	out := ""
	for _, n := range orderedStageNames(state) {
		marker := "·"
		switch state.Stages[n] {
		case core.StageDone:
			marker = "✓"
		case core.StageActive:
			marker = "▶"
		case core.StageFailed:
			marker = "✗"
		}
		out += fmt.Sprintf("%s%s ", marker, n)
	}
	return out
}

// orderedStageNames returns the run's stages in the order its workflow defines.
// Falls back to sorted keys for an unknown (e.g. custom) workflow, and appends
// any stage present in state but not in the workflow definition.
func orderedStageNames(state *core.State) []string {
	wf, err := workflows.Get(state.Workflow)
	if err != nil {
		names := make([]string, 0, len(state.Stages))
		for n := range state.Stages {
			names = append(names, n)
		}
		sort.Strings(names)
		return names
	}
	var ordered []string
	seen := map[string]bool{}
	for _, n := range wf.StageNames() {
		if _, ok := state.Stages[n]; ok {
			ordered = append(ordered, n)
			seen[n] = true
		}
	}
	var extra []string
	for n := range state.Stages {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	return append(ordered, extra...)
}

func eventHint(ev core.Event) string {
	if ev.Detail == nil {
		return ""
	}
	for _, k := range []string{"reason", "gate", "path", "to", "role", "text"} {
		if v, ok := ev.Detail[k]; ok {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return ""
}
