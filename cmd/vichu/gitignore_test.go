package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEnsureGitignoreRefusesSymlink: a `.gitignore` the user symlinked to a shared ignore file
// must not be silently turned into a plain local file. ensureGitignore refuses it and leaves the
// link and its target intact — the same rule already applied to settings.json and pack files
// (os.Root FOLLOWS an internal link, and the atomic write would replace it with a regular file).
func TestEnsureGitignoreRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.gitignore")
	if err := os.WriteFile(shared, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared.gitignore", filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureGitignore(dir); err == nil {
		t.Fatal("ensureGitignore must refuse a symlinked .gitignore, not replace it")
	}
	fi, err := os.Lstat(filepath.Join(dir, ".gitignore"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".gitignore must stay a symlink (err=%v)", err)
	}
	if data, _ := os.ReadFile(shared); string(data) != "node_modules/\n" {
		t.Fatalf("the shared target must be untouched, got %q", data)
	}
}

// TestEnsureGitignorePreservesMode: appending the runtime-dir rule to an existing .gitignore must
// keep its permissions — a 0600 file the user tightened must not be widened to 0644.
func TestEnsureGitignorePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not round-trip on Windows")
	}
	dir := t.TempDir()
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte("build/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(gi)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf(".gitignore mode widened from 0600 to %o", fi.Mode().Perm())
	}
	if data, _ := os.ReadFile(gi); !strings.Contains(string(data), ".vichu/") {
		t.Fatal("the runtime dir rule should have been appended")
	}
}
