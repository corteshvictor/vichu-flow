package workspace

import (
	"encoding/json"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// TestTrackingSurvivesProcessBoundary is the load-bearing test for host-first
// mutation auditing: `worker start` and `worker complete` run in SEPARATE
// processes, so the "before" snapshot must round-trip through disk (JSON) and a
// fresh provider must still attribute exactly what the agent changed.
func TestTrackingSurvivesProcessBoundary(t *testing.T) {
	w, dir := fsWorkspace(t)
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}

	// "Process 1" — worker start: capture the before-snapshot and serialize it.
	tracker, err := w.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}
	blob, err := json.Marshal(tracker.Before())
	if err != nil {
		t.Fatalf("marshal before-snapshot: %v", err)
	}

	// The host runs its native subagent, which changes files.
	writeFile(t, dir, "src/new.go", "package main\n")
	writeFile(t, dir, "README.md", "hello\nworld\n") // modify the baseline file

	// "Process 2" — worker complete: a FRESH provider + the deserialized snapshot
	// must reconstruct the tracker and attribute the mutations correctly.
	var before map[string]core.FileSig
	if err := json.Unmarshal(blob, &before); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	w2, err := OpenFilesystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := w2.ResumeTracking(before)
	if err != nil {
		t.Fatalf("ResumeTracking: %v", err)
	}
	muts, err := resumed.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	byPath := map[string]core.Mutation{}
	for _, m := range muts {
		byPath[m.Path] = m
	}
	if m, ok := byPath["src/new.go"]; !ok || m.Kind != core.MutationUntracked {
		t.Fatalf("new file must be attributed across the process boundary, got %+v (all: %v)", m, muts)
	}
	if m, ok := byPath["README.md"]; !ok || m.Kind != core.MutationModified {
		t.Fatalf("modified file must be attributed, got %+v", m)
	}
}
