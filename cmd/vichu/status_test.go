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
