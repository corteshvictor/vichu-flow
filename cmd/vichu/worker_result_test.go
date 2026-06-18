package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadResultRejectsRuntimePath: a --result file inside .vichu/runs breaks
// single-writer (the kernel owns the runtime), so it must be rejected.
func TestReadResultRejectsRuntimePath(t *testing.T) {
	dir := t.TempDir()
	runtimeRoot := filepath.Join(dir, ".vichu")

	// A file OUTSIDE the runtime is read fine.
	outside := filepath.Join(dir, "result.md")
	if err := os.WriteFile(outside, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readHostFile(outside, false, runtimeRoot); err != nil || got != "ok" {
		t.Fatalf("outside file should read: got %q err %v", got, err)
	}

	// A file INSIDE the runtime is rejected.
	inside := filepath.Join(runtimeRoot, "runs", "r1", "sneaky")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readHostFile(inside, false, runtimeRoot); err == nil {
		t.Fatal("a --result path inside the runtime must be rejected")
	}

	// No path and no stdin → empty result, no error.
	if got, err := readHostFile("", false, runtimeRoot); err != nil || got != "" {
		t.Fatalf("empty result should be allowed: got %q err %v", got, err)
	}

	// A symlink OUTSIDE the runtime that POINTS INSIDE it must also be rejected —
	// the check resolves symlinks, not just the literal path.
	link := filepath.Join(dir, "sneaky-link")
	if err := os.Symlink(inside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readHostFile(link, false, runtimeRoot); err == nil {
		t.Fatal("a symlink resolving into the runtime must be rejected")
	}
}
