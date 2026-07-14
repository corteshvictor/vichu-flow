package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
	rt "github.com/corteshvictor/vichu-flow/internal/runtime"
)

// TestReinstallIdenticalPackPreservesModeAndInode (#3): reinstalling the SAME pack must change
// nothing — the installer reports "unchanged", so it must not rewrite a byte-identical file
// (churning inode/mtime) nor reset a mode the user set (0600 → 0644).
func TestReinstallIdenticalPackPreservesModeAndInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes / inode identity do not round-trip on Windows")
	}
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, ".claude", "agents", "vichu-worker.md")
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("reinstall reset the mode to %o (should preserve 0600)", after.Mode().Perm())
	}
	if !os.SameFile(before, after) {
		t.Fatal("reinstall rewrote a byte-identical file (new inode) though it reports 'unchanged'")
	}
}

// TestInstallRefusesSymlinkedRecord (#4): a `.claude/vichu-host.json` the user symlinked to a
// shared record must not be read through and then replaced with a regular file. The target is
// INSIDE the project so the OLD confined-but-following read would resolve it (an escaping link
// os.Root already refused); the fix reads no-follow and refuses it.
func TestInstallRefusesSymlinkedRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(root, ".claude", "vichu-host.json")
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, ".claude", "shared-record.json")
	if err := os.WriteFile(shared, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared-record.json", rec); err != nil { // relative → internal, os.Root follows it
		t.Fatal(err)
	}

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("install must refuse a symlinked install record, not read through and replace it")
	}
	if fi, err := os.Lstat(rec); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlinked record must be left in place (err=%v)", err)
	}
}

// TestReinstallFailsWhenLegacyRecordCannotBeRemoved (#7): the migration deletes the legacy
// .vichu/host.json. If it cannot be removed, the install must NOT report a completed migration —
// it must fail (and roll back). A non-empty directory at that path makes the remove fail.
func TestReinstallFailsWhenLegacyRecordCannotBeRemoved(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// A directory (non-empty) where the legacy record would be — Remove refuses it.
	legacy := filepath.Join(root, ".vichu", "host.json")
	if err := os.MkdirAll(filepath.Join(legacy, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("reinstall must fail when the legacy record cannot be removed, not silently claim migration succeeded")
	}
}

// TestStatusFailsOnCorruptEventLog (#6): `status` must not report a corrupt audit log as
// "recent_events": null — a broken timeline is a hard error, not an empty one.
func TestStatusFailsOnCorruptEventLog(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)
	ev := filepath.Join(store.RunDir(rid), "events.ndjson")
	if err := os.WriteFile(ev, []byte("{not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdStatus([]string{"--json", rid}); err == nil {
		t.Fatal("status --json must fail on a corrupt event log, not report recent_events: null with exit 0")
	}
	if err := cmdStatus([]string{rid}); err == nil {
		t.Fatal("status (human) must fail on a corrupt event log")
	}
}

// TestAdoptIdenticalPackDoesNotRewrite (#P2b, ronda 17): adopting a byte-identical pack file (no
// install record → unvouched) must NOT rewrite it — that churns the inode/mtime and strips
// xattrs/ACLs. Only a genuine content change (upgrade/replace) writes.
func TestAdoptIdenticalPackDoesNotRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode identity does not round-trip on Windows")
	}
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Drop the record so the identical files are seen as UNVOUCHED (adopted) on the next install.
	if err := os.Remove(filepath.Join(root, ".claude", "vichu-host.json")); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, ".claude", "agents", "vichu-worker.md")
	before, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("adopting a byte-identical file rewrote it (new inode) — xattrs/ACLs/timestamps lost")
	}
}

// TestStatusRejectsImpossibleProvider (ronda 21): status --json must not publish a workspace
// provider the runtime never produces — a corrupt/forged snapshot value is a hard error.
func TestStatusRejectsImpossibleProvider(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)
	ws := filepath.Join(store.RunDir(rid), "workspace.json")
	if err := os.WriteFile(ws, []byte(`{"provider":"evil-provider"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdStatus([]string{"--json", rid}); err == nil {
		t.Fatal("status --json accepted an impossible workspace provider")
	}
}

// TestDoctorFailsOnRetiredRuleWithoutRecord (ronda 21): a project left with only the retired,
// insecure `Bash(vichu *)` rule and no record must fail doctor — discovery keys off permissions,
// not only files, and a retired rule is a live danger.
func TestDoctorFailsOnRetiredRuleWithoutRecord(t *testing.T) {
	fakeClaudeOnPath(t, "1.5.0", true)
	dir := projectWithHostPack(t)
	if err := os.Remove(filepath.Join(dir, ".claude", "vichu-host.json")); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"skills", "agents", "commands"} {
		_ = os.RemoveAll(filepath.Join(dir, ".claude", sub))
	}
	writeSettingsAllow(t, dir, []string{"Bash(vichu *)"})
	if err := cmdDoctor(nil); err == nil {
		t.Fatal("doctor passed with only a retired insecure rule and no record")
	}
}

// TestStatusFailsOnCorruptLock (ronda 21, self-audit): a corrupt/unreadable run lock must fail
// status in BOTH modes — reporting "no lock held" when we could not read it would tell a host
// nobody owns the run. (The --json path was fixed first; this pins the human path too.)
func TestStatusFailsOnCorruptLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)
	if err := os.WriteFile(filepath.Join(store.RunDir(rid), "lock.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdStatus([]string{"--json", rid}); err == nil {
		t.Fatal("status --json reported a corrupt lock as absent")
	}
	if err := cmdStatus([]string{rid}); err == nil {
		t.Fatal("status (human) reported a corrupt lock as absent")
	}
}

// TestStatusAndCancelOnMissingAudit (ronda 23): a deleted audit must not be presented as an empty
// timeline. status fails; cancel still stops the run (escape hatch) but reports the loss non-zero
// instead of forging a fresh log with only run_canceled.
func TestStatusAndCancelOnMissingAudit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)
	ev := filepath.Join(store.RunDir(rid), "events.ndjson")
	if err := os.Remove(ev); err != nil {
		t.Fatal(err)
	}
	if err := cmdStatus([]string{"--json", rid}); err == nil {
		t.Fatal("status --json presented a missing audit as an empty timeline")
	}
	// cancel is the escape hatch: it must report the audit loss (non-zero) AND not create a new
	// log, but the state must end canceled.
	if err := cmdCancel([]string{rid}); err == nil {
		t.Fatal("cancel must report the lost audit non-zero")
	}
	st, _ := store.LoadState(rid)
	if st.Status != core.StatusCanceled {
		t.Fatalf("cancel must still stop the run, got status %s", st.Status)
	}
	if _, err := os.Stat(ev); err == nil {
		t.Fatal("cancel forged a new events.ndjson over a missing audit")
	}
}

// TestCancelValidatesTerminalAudit (ronda 25): cancel on an ALREADY-TERMINAL run must not report a
// clean "already completed" (exit 0) while the audit is corrupt — a run whose history cannot be read
// is not a confirmed finish. The terminal short-circuit used to return before validating the log.
func TestCancelValidatesTerminalAudit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, filepath.Join(dir, "vichu.yaml"),
		"project: {name: f, language: go}\nworkflow: {default: quick}\nagents: {default: {provider: fake}}\ncommands: {test: \"true\"}\n")
	store := rt.Open(dir)
	if err := cmdRunStart([]string{"--workflow", "quick", "task"}); err != nil {
		t.Fatal(err)
	}
	rid := runID(t, store)
	// Drive the run to a terminal state, then corrupt (not delete) its audit.
	st, _ := store.LoadState(rid)
	st.Status = core.StatusCompleted
	if err := store.SaveState(st); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(store.RunDir(rid), "events.ndjson")
	if err := os.WriteFile(ev, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdCancel([]string{rid}); err == nil {
		t.Fatal("cancel reported a terminal run as a clean finish over a corrupt audit")
	}
}
