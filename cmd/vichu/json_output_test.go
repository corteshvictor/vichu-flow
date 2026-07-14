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

// reRunID extracts a run id from a command's human output ("Created run run-… ").
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
	// blocked verify close. The run id is parsed from `run start`'s output and used
	// explicitly throughout — NOT resolved via LatestRun, which is ambiguous once a
	// second run shares the same timestamp-second (the macOS-CI flake).
	drive := func(task string, createFile bool) (string, string) {
		rid, tok := startRunWithToken(t, "--workflow", "quick", task)
		// activeWorker resolves the worker from THIS run's state (explicit rid).
		activeWorker := func() string {
			st, err := store.LoadState(rid)
			if err != nil || st.ActiveWorker == "" {
				t.Fatalf("no active worker for %s: %v", rid, err)
			}
			return st.ActiveWorker
		}
		cli(cmdWorker, "start", "--run", rid, "--stage", "explore", "--role", "explorer", "--driver-token", tok)
		cli(cmdWorker, "complete", "--run", rid, "--worker", activeWorker(), "--driver-token", tok)
		cli(cmdStage, "close", "--run", rid, "--stage", "explore", "--driver-token", tok)
		cli(cmdWorker, "start", "--run", rid, "--stage", "implement", "--role", "implementer", "--driver-token", tok)
		if createFile {
			mustWrite(t, feat, "package main\n")
		} else {
			_ = os.Remove(feat)
		}
		cli(cmdWorker, "complete", "--run", rid, "--worker", activeWorker(), "--driver-token", tok)
		cli(cmdStage, "close", "--run", rid, "--stage", "implement", "--driver-token", tok)
		return rid, tok
	}

	// --- blocked: the verify gate fails (no feature.go) ---
	ridB, tokB := drive("blocked-run", false)
	out := captureStdout(t, func() {
		cli(cmdStage, "close", "--run", ridB, "--stage", "verify", "--json", "--driver-token", tokB)
	})
	assertPureJSON(t, out, "blocked")

	// --- completed: the verify gate passes (feature.go present) ---
	ridC, tokC := drive("ok-run", true)
	out = captureStdout(t, func() {
		cli(cmdStage, "close", "--run", ridC, "--stage", "verify", "--json", "--driver-token", tokC)
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

// startRunWithToken runs `run start --json` and returns the run id and the DRIVER TOKEN,
// exactly as a host pack's orchestrator would: the token is printed once and never written
// to disk, so this is the only place to get it.
func startRunWithToken(t *testing.T, args ...string) (runID, token string) {
	t.Helper()
	out := captureStdout(t, func() {
		if err := cmdRunStart(append([]string{"--json"}, args...)); err != nil {
			t.Fatalf("run start: %v", err)
		}
	})
	var got struct {
		RunID string `json:"run_id"`
		Token string `json:"driver_token"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("run start --json: %v (%s)", err, out)
	}
	if got.Token == "" {
		t.Fatal("run start must issue a driver token — without it the orchestrator cannot drive the run")
	}
	return got.RunID, got.Token
}
