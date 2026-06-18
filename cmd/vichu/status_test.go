package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// TestOrderedStageNamesFollowsWorkflow verifies stages render in workflow order
// (explore → implement → verify → done), not alphabetically — `done` must not
// sort to the front.
func TestOrderedStageNamesFollowsWorkflow(t *testing.T) {
	state := &core.State{
		Workflow: "quick",
		Stages: map[string]core.StageStatus{
			"done":      core.StagePending,
			"explore":   core.StageDone,
			"verify":    core.StageActive,
			"implement": core.StageDone,
		},
	}
	got := orderedStageNames(state)
	want := []string{"explore", "implement", "verify", "done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage order = %v, want %v", got, want)
	}

	// The rendered line must start with the first workflow stage, not "done".
	line := stageLine(state)
	if !strings.HasPrefix(line, "✓explore") {
		t.Fatalf("stage line should start with explore, got %q", line)
	}
}

// TestBudgetsJSONUsageUnknownVsKnown: cost and tokens are independent in the JSON
// snapshot. Unreported → null (unknown, not zero); reported → the real number.
// A codex-style worker reports tokens but not cost, so cost stays null.
func TestBudgetsJSONUsageUnknownVsKnown(t *testing.T) {
	// Nothing reported: both null, both flags false.
	none := budgetsJSON(core.BudgetState{AgentInvocations: 2, WallClockSpentSeconds: 3})
	if none["cost_usd"] != nil || none["tokens_total"] != nil {
		t.Fatalf("unreported usage must be null, got cost=%v tokens=%v", none["cost_usd"], none["tokens_total"])
	}
	if none["cost_reported"] != false || none["tokens_reported"] != false {
		t.Fatal("unreported flags must be false")
	}

	// Tokens reported, cost not (codex): tokens is a number, cost stays null.
	tok := budgetsJSON(core.BudgetState{
		TokensReported: true, TokensInSpent: 100, TokensOutSpent: 50,
	})
	if tok["tokens_total"] != 150 {
		t.Fatalf("reported tokens must surface, got %v", tok["tokens_total"])
	}
	if tok["cost_usd"] != nil || tok["cost_reported"] != false {
		t.Fatalf("unreported cost must stay null/false, got %v", tok["cost_usd"])
	}

	// Both reported: both surface as real numbers.
	both := budgetsJSON(core.BudgetState{
		CostReported: true, CostUSDSpent: 1.25, TokensReported: true, TokensInSpent: 10, TokensOutSpent: 5,
	})
	if both["cost_usd"] != 1.25 || both["tokens_total"] != 15 {
		t.Fatalf("reported usage must surface, got cost=%v tokens=%v", both["cost_usd"], both["tokens_total"])
	}
}

// TestBudgetUsageStringsIndependent: the human budget line renders cost and tokens
// independently — one can be a value while the other reads "unknown".
func TestBudgetUsageStringsIndependent(t *testing.T) {
	cost, tokens := budgetUsageStrings(core.BudgetState{
		TokensReported: true, TokensInSpent: 100, TokensOutSpent: 50,
	})
	if !strings.Contains(cost, "unknown") {
		t.Fatalf("unreported cost must read unknown, got %q", cost)
	}
	if !strings.Contains(tokens, "150") {
		t.Fatalf("reported tokens must show the count, got %q", tokens)
	}
}

// TestOrderedStageNamesUnknownWorkflowFallsBack ensures a custom/unknown
// workflow still renders deterministically (sorted) rather than panicking.
func TestOrderedStageNamesUnknownWorkflowFallsBack(t *testing.T) {
	state := &core.State{
		Workflow: "custom-not-registered",
		Stages: map[string]core.StageStatus{
			"b": core.StageDone,
			"a": core.StageActive,
		},
	}
	got := orderedStageNames(state)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("fallback order = %v, want sorted [a b]", got)
	}
}
