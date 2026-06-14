package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

func fsWorkspace(t *testing.T) (*FilesystemWorkspace, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "hello\n")
	w, err := OpenFilesystem(dir)
	if err != nil {
		t.Fatalf("OpenFilesystem: %v", err)
	}
	return w, dir
}

func TestFilesystemSnapshotEmptyDirtyAndStableBaseID(t *testing.T) {
	w, _ := fsWorkspace(t)
	snap, err := w.Snapshot("")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.BaseSHA == "" {
		t.Fatal("expected a non-empty baseline id")
	}
	if snap.Isolation != core.IsolationCurrentWorktree {
		t.Fatalf("want default isolation, got %q", snap.Isolation)
	}
	if len(snap.DirtyFiles) != 0 {
		t.Fatalf("a fresh snapshot equals its baseline, dirty=%v", snap.DirtyFiles)
	}
	// BaseID is persisted and stable across reads (drift checks rely on this).
	if w.BaseID() != snap.BaseSHA {
		t.Fatalf("BaseID %q != snapshot id %q", w.BaseID(), snap.BaseSHA)
	}
	// No change since snapshot → nothing fingerprinted as changed.
	fp, err := w.FingerprintChanged()
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 0 {
		t.Fatalf("expected no changes right after snapshot, got %v", fp)
	}
}

func TestFilesystemMutationTracking(t *testing.T) {
	w, dir := fsWorkspace(t)
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}

	tracker, err := w.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}
	writeFile(t, dir, "src/new.go", "package main\nfunc main(){}\n")
	writeFile(t, dir, "README.md", "hello\nworld\n") // modify baseline file
	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	byPath := map[string]core.Mutation{}
	for _, m := range muts {
		byPath[m.Path] = m
	}
	if m, ok := byPath["src/new.go"]; !ok || m.Kind != core.MutationUntracked {
		t.Fatalf("new file should be untracked, got %+v (all: %v)", m, muts)
	}
	if m, ok := byPath["README.md"]; !ok || m.Kind != core.MutationModified {
		t.Fatalf("README.md should be modified, got %+v", m)
	}
	if byPath["README.md"].Added == 0 {
		t.Fatalf("modified file should report added lines, got %+v", byPath["README.md"])
	}
}

func TestFilesystemDetectsDeletion(t *testing.T) {
	w, dir := fsWorkspace(t)
	writeFile(t, dir, "keep/data.txt", "user work\n")
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}

	tracker, err := w.BeginTracking()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "keep", "data.txt")); err != nil {
		t.Fatal(err)
	}
	muts, err := tracker.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(muts) != 1 || muts[0].Path != "keep/data.txt" || muts[0].Kind != core.MutationDeleted {
		t.Fatalf("deletion must be reported, got %v", muts)
	}
}

func TestFilesystemRestoreBaseline(t *testing.T) {
	w, dir := fsWorkspace(t)
	writeFile(t, dir, "app.go", "v1\n")
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}

	// Damage the tree: modify one baseline file, delete another.
	writeFile(t, dir, "app.go", "CORRUPT\n")
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}

	n, err := w.RestoreBaseline([]string{"app.go", "README.md"})
	if err != nil {
		t.Fatalf("RestoreBaseline: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 paths restored, got %d", n)
	}
	if got := readFile(t, dir, "app.go"); got != "v1\n" {
		t.Fatalf("app.go not reverted, got %q", got)
	}
	if got := readFile(t, dir, "README.md"); got != "hello\n" {
		t.Fatalf("deleted README.md not recreated, got %q", got)
	}
}

func TestFilesystemBackupRestore(t *testing.T) {
	w, dir := fsWorkspace(t)
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}
	// The run produces work; back it up before a gate runs.
	writeFile(t, dir, "work.txt", "agent work\n")
	backup, err := w.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}
	if !backup.Has("work.txt") {
		t.Fatal("backup should hold the changed file")
	}
	// A misbehaving gate clobbers it; Restore brings it back.
	writeFile(t, dir, "work.txt", "gate damage\n")
	if _, err := backup.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, dir, "work.txt"); got != "agent work\n" {
		t.Fatalf("backup did not restore content, got %q", got)
	}
}

func TestBackupRestorePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes not meaningful on windows")
	}
	w, dir := fsWorkspace(t)
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}
	// The run writes an executable script; back it up before a gate runs.
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil { // defeat umask
		t.Fatal(err)
	}
	backup, err := w.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}
	// A gate clobbers it with different content and loses the exec bit.
	if err := os.WriteFile(script, []byte("damaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("restore must preserve mode 0755, got %o", info.Mode().Perm())
	}
}

func TestFilesystemIgnoresRuntimeAndGitDirs(t *testing.T) {
	w, dir := fsWorkspace(t)
	if _, err := w.Snapshot(""); err != nil {
		t.Fatal(err)
	}
	// Files inside .git and .vichu must never count as worker mutations.
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, ".vichu/runs/x/state.json", "{}\n")
	fp, err := w.FingerprintChanged()
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 0 {
		t.Fatalf(".git/.vichu changes must be ignored, got %v", fp)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
