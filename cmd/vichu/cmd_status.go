package main

import (
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(fs.Arg(0))
	if err != nil {
		return err
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
	row("status.budget", fmt.Sprintf(i18n.T("status.budget_line"),
		state.Budgets.AgentInvocations, state.Budgets.CostUSDSpent,
		state.Budgets.WallClockSpentSeconds, state.Budgets.TokensTotalSpent()))
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
