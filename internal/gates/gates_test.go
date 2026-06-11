package gates

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
)

func sh(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", script}
	}
	return []string{"sh", "-c", script}
}

func TestGatePasses(t *testing.T) {
	store := rt.Open(t.TempDir())
	r := NewRunner(store)
	v, err := r.Run(context.Background(), "run-1", "verify", 1, Spec{
		Name:    "test",
		Command: sh("echo all good"),
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !v.Passed || v.ExitCode != 0 {
		t.Fatalf("want pass/0, got passed=%v code=%d", v.Passed, v.ExitCode)
	}
	data, err := os.ReadFile(v.OutputPath)
	if err != nil {
		t.Fatalf("reading output.log: %v", err)
	}
	if !strings.Contains(string(data), "all good") {
		t.Fatalf("output.log missing captured text: %q", data)
	}
	// verdict.json must be persisted on disk.
	if _, err := os.Stat(store.GateDir("run-1", "verify", 1) + "/verdict.json"); err != nil {
		t.Fatalf("verdict.json not written: %v", err)
	}
}

func TestGateFails(t *testing.T) {
	store := rt.Open(t.TempDir())
	r := NewRunner(store)
	v, err := r.Run(context.Background(), "run-1", "verify", 1, Spec{
		Name:    "test",
		Command: sh("exit 3"),
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Passed {
		t.Fatal("failing command must not pass")
	}
	if v.ExitCode != 3 {
		t.Fatalf("want exit code 3, got %d", v.ExitCode)
	}
}

func TestGateMissingBinary(t *testing.T) {
	store := rt.Open(t.TempDir())
	r := NewRunner(store)
	v, err := r.Run(context.Background(), "run-1", "verify", 1, Spec{
		Name:    "test",
		Command: []string{"this-binary-does-not-exist-vichu"},
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run should not hard-error on missing binary: %v", err)
	}
	if v.Passed || v.ExitCode != -1 {
		t.Fatalf("missing binary should fail with code -1, got passed=%v code=%d", v.Passed, v.ExitCode)
	}
}

func TestExcerptTruncates(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/big.log"
	big := strings.Repeat("x", 5000) + "TAIL_MARKER"
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := Excerpt(path, 1024)
	if err != nil {
		t.Fatalf("Excerpt: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(text, "TAIL_MARKER") {
		t.Fatal("excerpt should keep the tail where failures surface")
	}
	if !strings.Contains(text, "truncated") {
		t.Fatal("excerpt should include a truncation notice")
	}
}
