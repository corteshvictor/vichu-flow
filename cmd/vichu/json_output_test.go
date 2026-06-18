package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was
// written. Engine progress logs go to stderr (not captured), so this proves a
// `--json` command emits ONLY the JSON object on stdout — the host-pack contract.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		out <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-out
}

// assertPureJSON fails unless out is a single JSON object (no human text before it)
// whose "status" equals wantStatus.
func assertPureJSON(t *testing.T, out, wantStatus string) {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		t.Fatalf("stdout must be a pure JSON object, got:\n%q", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		t.Fatalf("parse: %v\n%q", err, out)
	}
	if m["status"] != wantStatus {
		t.Fatalf("status = %v, want %q\n%q", m["status"], wantStatus, out)
	}
}

// TestStageCloseJSONIsPure: `stage close --json` must print ONLY a JSON object on
// stdout for both a terminal (completed) transition and a blocked one — the engine's
// completion/block log must go to stderr, never contaminate the contract stream.
func TestStageCloseJSONIsPure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a unix shell gate")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: j, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"test -f feature.go\"}\n")
	store := rt.Open(dir)

	cli := func(fn func([]string) error, args ...string) {
		t.Helper()
		if err := fn(args); err != nil {
			t.Fatalf("cmd %v: %v", args, err)
		}
	}
	feat := filepath.Join(dir, "feature.go")

	// drive a quick run up to (but not closing) verify. createFile toggles whether the
	// `test -f feature.go` gate will pass, so we can reach both a completed and a
	// blocked verify close.
	drive := func(task string, createFile bool) string {
		cli(cmdRunStart, "--workflow", "quick", task)
		rid := runID(t, store)
		cli(cmdWorker, "start", "--run", rid, "--stage", "explore", "--role", "explorer")
		cli(cmdWorker, "complete", "--run", rid, "--worker", activeWorker(t, store))
		cli(cmdStage, "close", "--run", rid, "--stage", "explore")
		cli(cmdWorker, "start", "--run", rid, "--stage", "implement", "--role", "implementer")
		if createFile {
			mustWrite(t, feat, "package main\n")
		} else {
			_ = os.Remove(feat)
		}
		cli(cmdWorker, "complete", "--run", rid, "--worker", activeWorker(t, store))
		cli(cmdStage, "close", "--run", rid, "--stage", "implement")
		return rid
	}

	// --- blocked: the verify gate fails (no feature.go) ---
	ridB := drive("blocked-run", false)
	out := captureStdout(t, func() {
		cli(cmdStage, "close", "--run", ridB, "--stage", "verify", "--json")
	})
	assertPureJSON(t, out, "blocked")

	// --- completed: the verify gate passes (feature.go present) ---
	ridC := drive("ok-run", true)
	out = captureStdout(t, func() {
		cli(cmdStage, "close", "--run", ridC, "--stage", "verify", "--json")
	})
	assertPureJSON(t, out, "completed")
}

// TestStatusFlagOrderBothWork: `--json` must be honored whether it comes before or
// after the run id — Go's flag parser stops at the first positional, so the bare
// `status <id> --json` used to silently print human text.
func TestStatusFlagOrderBothWork(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)

	for _, args := range [][]string{
		{"--json", rid}, // flag first
		{rid, "--json"}, // flag AFTER the positional — the regression
	} {
		out := captureStdout(t, func() {
			if err := cmdStatus(args); err != nil {
				t.Fatalf("status %v: %v", args, err)
			}
		})
		trimmed := strings.TrimSpace(out)
		if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
			t.Fatalf("status %v must print JSON regardless of flag order, got:\n%q", args, out)
		}
	}
}
