package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHostPack(t *testing.T) {
	root := t.TempDir()
	written, err := installHostPack(root, "claude-code", false, false)
	if err != nil {
		t.Fatalf("installHostPack: %v", err)
	}
	if len(written) == 0 {
		t.Fatal("expected files written")
	}
	// Every recorded file exists and the install record was written.
	for _, f := range written {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Errorf("expected %s: %v", f, err)
		}
	}
	if loadInstalledHost(root) == nil {
		t.Fatal(".vichu/host.json install record missing")
	}
	// The orchestrator skill must be present — it is the heart of the pack.
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); err != nil {
		t.Fatalf("orchestrator skill missing: %v", err)
	}
}

func TestInstallHostPackIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Re-install over our own unchanged files must succeed (no clobber error).
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatalf("re-install of our own files must succeed: %v", err)
	}
}

func TestInstallHostPackRefusesToClobberUserFile(t *testing.T) {
	root := t.TempDir()
	// A user file at a pack destination that VichuFlow did NOT install.
	dst := filepath.Join(root, ".claude/commands/vichu.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("MY OWN COMMAND"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("must refuse to overwrite a user file without --force")
	}
	// The user's file is untouched.
	if data, _ := os.ReadFile(dst); string(data) != "MY OWN COMMAND" {
		t.Fatal("a refused install must not modify the user's file")
	}
	// --force overwrites it.
	if _, err := installHostPack(root, "claude-code", true, false); err != nil {
		t.Fatalf("--force install: %v", err)
	}
	if data, _ := os.ReadFile(dst); string(data) == "MY OWN COMMAND" {
		t.Fatal("--force must overwrite the file")
	}
}

func TestInstallHostPackDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	written, err := installHostPack(root, "claude-code", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) == 0 {
		t.Fatal("dry-run should report the files it would write")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write any files")
	}
}

func TestUnknownHostErrors(t *testing.T) {
	if _, err := installHostPack(t.TempDir(), "nope", false, false); err == nil {
		t.Fatal("unknown host must error")
	}
}

func TestUninstallRemovesOnlyVichuFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// The user edits one installed file — it must survive uninstall.
	edited := filepath.Join(root, ".claude/commands/vichu.md")
	if err := os.WriteFile(edited, []byte("MY EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, kept, err := uninstallHostPack(root, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) == 0 {
		t.Fatal("uninstall should remove the unmodified pack files")
	}
	if len(kept) != 1 || kept[0] != ".claude/commands/vichu.md" {
		t.Fatalf("the user-modified file must be kept, got kept=%v", kept)
	}
	// The edited file is untouched; an unmodified one is gone.
	if data, _ := os.ReadFile(edited); string(data) != "MY EDIT" {
		t.Fatal("uninstall must not delete a user-modified file")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("an unmodified pack file should have been removed")
	}
	// Even though a file was kept, the install record must be gone — the kept file
	// is now the user's, so doctor must stop treating the host pack as installed.
	if loadInstalledHost(root) != nil {
		t.Fatal("uninstall must drop .vichu/host.json even when a file is kept")
	}
}

func TestUninstallWithNoPackErrors(t *testing.T) {
	if _, _, err := uninstallHostPack(t.TempDir(), "claude-code"); err == nil {
		t.Fatal("uninstall with no installed pack must error")
	}
}

// TestHostRecordIsPortableNotInVichu: the install record must live WITH the pack
// (.claude/vichu-host.json), not under the gitignored .vichu/ — otherwise it never
// reaches a teammate's clone.
func TestHostRecordIsPortableNotInVichu(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err != nil {
		t.Fatalf("install must write the portable record %s: %v", portableHostRecordPath(root), err)
	}
	if _, err := os.Stat(legacyHostRecordPath(root)); err == nil {
		t.Fatal("install must NOT write the legacy .vichu/host.json")
	}
}

// TestHostPackRecordSurvivesCloneWithoutVichu: a clone where .vichu/ (gitignored) is
// absent but .claude/ (committed, with the record) is present must still validate —
// doctor sees the pack and re-install is idempotent, no false clobber.
func TestHostPackRecordSurvivesCloneWithoutVichu(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate the clone: drop the gitignored runtime dir entirely.
	if err := os.RemoveAll(filepath.Join(root, ".vichu")); err != nil {
		t.Fatal(err)
	}
	if loadInstalledHost(root) == nil {
		t.Fatal("the record must survive a clone that has .claude/ but not .vichu/")
	}
	// Re-install must be idempotent — the record proves VichuFlow owns these files.
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatalf("re-install in a clone must not error as a clobber: %v", err)
	}
}

// TestLegacyHostRecordMigratesOnReinstall: an install left by an older binary (record
// under .vichu/host.json) is still readable, and re-installing migrates it to the
// portable location and drops the legacy copy.
func TestLegacyHostRecordMigratesOnReinstall(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Forge the OLD layout: move the record to the legacy spot.
	data, err := os.ReadFile(portableHostRecordPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".vichu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyHostRecordPath(root), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(portableHostRecordPath(root)); err != nil {
		t.Fatal(err)
	}
	// Precondition: only the legacy record exists, and it is still readable.
	if loadInstalledHost(root) == nil {
		t.Fatal("legacy record must remain readable for migration")
	}
	// Re-install migrates to portable and removes legacy.
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err != nil {
		t.Fatalf("re-install must migrate to the portable record: %v", err)
	}
	if _, err := os.Stat(legacyHostRecordPath(root)); err == nil {
		t.Fatal("re-install must remove the legacy record")
	}
}

// TestHostPackFilesWithoutRecordRefusesClobber: pack-named files present but NO
// install record (e.g. a hand-placed file) must give an actionable clobber error,
// not be silently overwritten.
func TestHostPackFilesWithoutRecordRefusesClobber(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("pack files with no record must refuse to clobber without --force")
	}
}

// TestUninstallUnknownHostDoesNotDelete: the reviewer's case — `uninstall --host
// nope` on a project with claude-code installed must error and delete NOTHING.
func TestUninstallUnknownHostDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "nope"); err == nil {
		t.Fatal("an unknown host must error, not delete the installed pack")
	}
	// The installed pack and its record are untouched.
	if loadInstalledHost(root) == nil {
		t.Fatal("a rejected uninstall must not drop the install record")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); err != nil {
		t.Fatalf("a rejected uninstall must not delete pack files: %v", err)
	}
}

// TestUninstallMismatchedHostDoesNotDelete: when the install record names a
// different host than the one requested (the multi-host hazard), refuse and delete
// nothing — even though the requested host is itself a valid pack.
func TestUninstallMismatchedHostDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate a record that belongs to a different (future) host.
	ih := loadInstalledHost(root)
	ih.Host = "codex"
	if err := saveInstalledHost(root, ih); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code"); err == nil {
		t.Fatal("a host mismatch must error, not delete the wrong pack")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); err != nil {
		t.Fatalf("a mismatched uninstall must not delete pack files: %v", err)
	}
}

// TestInitHostDryRunWritesNothing: `init --host --dry-run` in a fresh project must
// not write vichu.yaml, .gitignore, .claude, or .vichu — it only previews.
func TestInitHostDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit([]string{"--host", "claude-code", "--dry-run"}); err != nil {
		t.Fatalf("init --host --dry-run: %v", err)
	}
	for _, p := range []string{"vichu.yaml", ".gitignore", ".claude", ".vichu"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("--dry-run must not create %s", p)
		}
	}
}

// TestInitHostOnExistingProjectAddsGitignore: adding a pack to an already-init'd
// project keeps vichu.yaml and ensures .vichu/ is gitignored.
func TestInitHostOnExistingProjectAddsGitignore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil { // normal init first
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, ".gitignore")) // simulate a project without it
	if err := cmdInit([]string{"--host", "claude-code"}); err != nil {
		t.Fatalf("init --host on existing project: %v", err)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), ".vichu/") {
		t.Fatalf("init --host must ensure .vichu/ is gitignored, got %q (%v)", gi, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude/skills/vichu-orchestrator/SKILL.md")); err != nil {
		t.Fatalf("pack must be installed: %v", err)
	}
}
