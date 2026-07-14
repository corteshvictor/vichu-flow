package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/hostpacks"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

func TestInstallHostPack(t *testing.T) {
	root := t.TempDir()
	_, written, err := installHostPack(root, "claude-code", false, false)
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
	if installRecord(t, root) == nil {
		t.Fatal(".vichu/host.json install record missing")
	}
	// The orchestrator skill must be present — it is the heart of the pack.
	if _, err := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); err != nil {
		t.Fatalf("orchestrator skill missing: %v", err)
	}
}

func TestInstallHostPackIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Re-install over our own unchanged files must succeed (no clobber error).
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
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
	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("must refuse to overwrite a user file without --force")
	}
	// The user's file is untouched.
	if data, _ := os.ReadFile(dst); string(data) != "MY OWN COMMAND" {
		t.Fatal("a refused install must not modify the user's file")
	}
	// --force overwrites it.
	if _, _, err := installHostPack(root, "claude-code", true, false); err != nil {
		t.Fatalf("--force install: %v", err)
	}
	if data, _ := os.ReadFile(dst); string(data) == "MY OWN COMMAND" {
		t.Fatal("--force must overwrite the file")
	}
}

func TestInstallHostPackDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	_, written, err := installHostPack(root, "claude-code", false, true)
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
	if _, _, err := installHostPack(t.TempDir(), "nope", false, false); err == nil {
		t.Fatal("unknown host must error")
	}
}

// TestUninstallIsAllOrNothing: a file at a pack destination that we cannot prove is ours — you
// edited it, or it was never ours — is not ours to destroy. And half-uninstalling is worse than
// not starting: the old behavior deleted the files it recognized, kept the rest, dropped the
// install record, and printed "Uninstalled" — leaving a pack on disk that a retry could no
// longer even see.
//
// So: name the files, change NOTHING, and let the human decide with --force.
func TestUninstallIsAllOrNothing(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(root, ".claude/commands/vichu.md")
	mustWrite(t, edited, "MY EDIT")

	_, _, err := uninstallHostPack(root, "claude-code", false, false)
	if err == nil {
		t.Fatal("uninstall must refuse rather than half-remove a pack it cannot fully account for")
	}
	if !strings.Contains(err.Error(), ".claude/commands/vichu.md") {
		t.Fatalf("the error must NAME the file it refused to touch: %v", err)
	}
	// Nothing changed: not the edited file, not the rest of the pack, not the record.
	if got := readFileString(t, edited); got != "MY EDIT" {
		t.Fatalf("the refused uninstall touched the user's file: %q", got)
	}
	if _, serr := os.Stat(filepath.Join(root, ".claude/skills/vichu-orchestrator/SKILL.md")); serr != nil {
		t.Fatal("a refused uninstall must not remove any pack file")
	}
	if installRecord(t, root) == nil {
		t.Fatal("a refused uninstall must keep the install record — a retry has to be able to see the install")
	}

	// --force is the human saying "yes, delete it anyway".
	removed, _, ferr := uninstallHostPack(root, "claude-code", false, true)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(removed) == 0 {
		t.Fatal("--force must remove the pack")
	}
	if _, serr := os.Stat(edited); serr == nil {
		t.Fatal("--force must remove the edited file too — that is what the user asked for")
	}
	if installRecord(t, root) != nil {
		t.Fatal("a completed uninstall must drop the record")
	}
}

func TestUninstallWithNoPackErrors(t *testing.T) {
	if _, _, err := uninstallHostPack(t.TempDir(), "claude-code", true, false); err == nil {
		t.Fatal("uninstall with no installed pack must error")
	}
}

// settingsPath is the host's shared settings file inside a project.
func settingsPath(root string) string {
	return filepath.Join(root, ".claude", "settings.json")
}

// projectWithSettings creates a project whose host settings already hold `content` —
// the settings file is the USER's, and every test here is about not damaging it.
func projectWithSettings(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, settingsPath(root), content)
	return root
}

// projectWithUserSettings creates a project whose host settings already carry the
// user's own model + permission rule, so a merge can be shown to preserve them.
func projectWithUserSettings(t *testing.T) string {
	t.Helper()
	return projectWithSettings(t, `{"model":"opus","permissions":{"allow":["Bash(go test *)"]}}`)
}

// allowRules reads the permission rules currently in the host's shared settings.
func allowRules(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings.json must stay valid JSON: %v", err)
	}
	perms, _ := m["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	out := make([]string, 0, len(allow))
	for _, a := range allow {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestInstallPreAuthorizesKernelCommands: the pack pre-authorizes the kernel commands
// the orchestrator calls on every run, MERGING into the host's shared settings —
// otherwise the host prompts the user on each `vichu` call and a run is unusable. The
// merge must leave the user's own settings and rules untouched.
func TestInstallPreAuthorizesKernelCommands(t *testing.T) {
	root := projectWithUserSettings(t)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	got := allowRules(t, root)
	if !slices.Contains(got, aPackRule(t)) {
		t.Fatalf("the pack must pre-authorize the kernel commands, got %v", got)
	}
	if !slices.Contains(got, "Bash(go test *)") {
		t.Fatalf("the merge must preserve the user's own rules, got %v", got)
	}
	// Everything else the user had must survive the merge.
	data, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "opus" {
		t.Fatalf("the merge must preserve the user's other settings, got %v", m)
	}
}

// TestPreAuthorizeIsIdempotent: re-installing must not duplicate the rules we added.
func TestPreAuthorizeIsIdempotent(t *testing.T) {
	root := projectWithUserSettings(t)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	first := allowRules(t, root)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if second := allowRules(t, root); len(second) != len(first) {
		t.Fatalf("re-install must not duplicate permission rules: %v → %v", first, second)
	}
}

// TestUninstallDropsOnlyOurPermissions: uninstall removes the rules the pack added and
// keeps the user's own — the settings file is theirs, we only merged into it.
func TestUninstallDropsOnlyOurPermissions(t *testing.T) {
	root := projectWithUserSettings(t)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	left := allowRules(t, root)
	if slices.Contains(left, aPackRule(t)) {
		t.Fatalf("uninstall must drop the rules the pack added, got %v", left)
	}
	if !slices.Contains(left, "Bash(go test *)") {
		t.Fatalf("uninstall must keep the user's own rules, got %v", left)
	}
}

// TestUninstallKeepsAPermissionTheUserAlreadyHad: the collision case. If the user
// ALREADY had `Bash(vichu *)` before installing, the install added nothing — so the
// uninstall must take nothing away. Removing the manifest's full rule list (rather
// than what we actually added) deletes the user's own rule, and they never get it back.
func TestUninstallKeepsAPermissionTheUserAlreadyHad(t *testing.T) {
	// The user got here first — this rule is THEIRS, not ours.
	root := projectWithSettings(t, `{"permissions":{"allow":["Bash(vichu *)"]}}`)

	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	if left := allowRules(t, root); !slices.Contains(left, "Bash(vichu *)") {
		t.Fatalf("uninstall deleted a rule the user already had before installing, leaving %v", left)
	}
}

// TestInstallRejectsUnsafeSettingsWithoutWriting: if the host's settings.json is
// unreadable or shaped in a way we cannot safely edit, the install must fail having
// touched NOTHING — no pack files, no install record, and above all no overwrite of the
// user's value. Copying first and validating after leaves a half-installed project and,
// worse, silently replaces a `permissions` key we failed to parse.
func TestInstallRejectsUnsafeSettingsWithoutWriting(t *testing.T) {
	cases := map[string]string{
		"invalid JSON":                 `{"permissions":`,
		"permissions is not an object": `{"permissions":"user-value"}`,
		"allow is not a list":          `{"permissions":{"allow":"Bash(vichu *)"}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := projectWithSettings(t, content)
			if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
				t.Fatal("install must refuse a settings.json it cannot safely edit")
			}
			assertInstalledNothing(t, root, content)
		})
	}
}

// assertInstalledNothing asserts a failed install left the project exactly as it found
// it: the user's settings byte-for-byte, no pack files, no install record.
func assertInstalledNothing(t *testing.T, root, settings string) {
	t.Helper()
	if got := readFileString(t, settingsPath(root)); got != settings {
		t.Fatalf("the user's settings must survive byte-for-byte\n want: %s\n  got: %s", settings, got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "vichu-orchestrator", "SKILL.md")); err == nil {
		t.Fatal("a failed install must not leave pack files behind")
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err == nil {
		t.Fatal("a failed install must not leave an install record behind")
	}
}

// TestUninstallRefusesToEscapeTheProject: `uninstall` is a delete loop driven by
// .claude/vichu-host.json — a file DESIGNED to be committed (it travels with the pack so
// a clone can validate it). On any repo you cloned, it is untrusted input.
//
// A record listing `../victim.txt` made `vichu uninstall` delete a file outside the
// project and exit zero. Every path is now checked BEFORE anything is removed, and one
// bad path aborts the whole command — no files, no permissions, no record.
func TestUninstallRefusesToEscapeTheProject(t *testing.T) {
	hostile := map[string]string{
		"parent traversal": "../victim.txt",
		"nested traversal": ".claude/../../victim.txt",
		"absolute path":    absoluteVictimPath(),
		"bare dotdot":      "..",
	}
	for name, dest := range hostile {
		t.Run(name, func(t *testing.T) {
			_, root, victim := projectBesideAVictimFile(t)
			if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
				t.Fatal(err)
			}
			// A hostile record, exactly as it would arrive in a cloned repo.
			ih := installRecord(t, root)
			ih.Files[dest] = hashBytes([]byte(victimContent))
			writeInstallRecord(t, root, ih)

			if _, _, err := uninstallHostPack(root, "claude-code", true, false); err == nil {
				t.Fatalf("uninstall must refuse a record listing %q", dest)
			}
			assertUninstallDidNothing(t, root, victim)
		})
	}
}

const victimContent = "outside the project"

// projectBesideAVictimFile builds parent/{victim.txt, project/} so a `../` in the record
// has something real to destroy.
func projectBesideAVictimFile(t *testing.T) (parent, root, victim string) {
	t.Helper()
	parent = t.TempDir()
	victim = filepath.Join(parent, "victim.txt")
	mustWrite(t, victim, victimContent)
	root = filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return parent, root, victim
}

// assertUninstallDidNothing: a refused uninstall must leave the outside world alone AND
// not have half-done the job inside the project.
func assertUninstallDidNothing(t *testing.T, root, victim string) {
	t.Helper()
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("uninstall deleted a file OUTSIDE the project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "vichu-orchestrator", "SKILL.md")); err != nil {
		t.Fatal("a refused uninstall must not remove pack files")
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err != nil {
		t.Fatal("a refused uninstall must not remove the install record")
	}
}

func absoluteVictimPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\drivers\etc\hosts`
	}
	return "/etc/hosts"
}

// TestUninstallFailsLoudlyOnAResidue: if a file we own cannot be removed, uninstall must
// FAIL and keep the record. Reporting success while SKILL.md is still installed — and
// deleting the record on the way out — leaves a residue nothing can recognize as ours:
// the record is the ONLY thing that knows which files and permissions belong to us, so
// doctor can then neither report the residue nor repair it.
func TestUninstallFailsLoudlyOnAResidue(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("relies on POSIX directory permissions denying removal")
	}
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, ".claude", "skills", "vichu-orchestrator")
	if err := os.Chmod(locked, 0o555); err != nil { // revoke write → its file cannot be removed
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err == nil {
		t.Fatal("uninstall must fail when a file it owns cannot be removed")
	}
	if _, serr := os.Stat(portableHostRecordPath(root)); serr != nil {
		t.Fatal("the record must be KEPT on a failed uninstall — without it nothing can see the residue")
	}
	if !slices.Contains(allowRules(t, root), aPackRule(t)) {
		t.Fatal("permissions must not be withdrawn while our files are still installed — the pack is still there and still needs them")
	}
	// Once the user fixes the obstacle, a second attempt completes.
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatalf("the retry must succeed once the obstacle is gone: %v", err)
	}
	if _, serr := os.Stat(portableHostRecordPath(root)); serr == nil {
		t.Fatal("a successful uninstall must drop the record")
	}
}

// TestFailedInstallRollsBack: the install is ALL-OR-NOTHING. If a copy fails partway,
// the project must come back exactly as we found it — no half-written pack file, no
// permission left authorized, and no install record. A failed install that still changed
// the project is worse than none: the user is told it failed, so they never look.
func TestFailedInstallRollsBack(t *testing.T) {
	root := projectWithSettings(t, `{"model":"opus"}`)
	// Make one destination un-writable by turning its parent into a FILE: the copy of
	// .claude/agents/* cannot succeed, and it fails midway through the batch.
	mustWrite(t, filepath.Join(root, ".claude", "agents"), "not a directory")

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("a failing copy must fail the install")
	}
	if got := readFileString(t, settingsPath(root)); got != `{"model":"opus"}` {
		t.Fatalf("the rollback must restore the user's settings, got: %s", got)
	}
	if rules := allowRules(t, root); slices.Contains(rules, aPackRule(t)) {
		t.Fatalf("a failed install must not leave a permission authorized, got %v", rules)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err == nil {
		t.Fatal("a failed install must not leave an install record")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "vichu-orchestrator", "SKILL.md")); err == nil {
		t.Fatal("the rollback must remove the files the failed install had already written")
	}
}

// TestInstallKeepsSettingsFileMode: a settings.json the user chmod'ed to 0600 may hold
// host configuration they do not want other local accounts reading. Widening it to 0644
// because we appended a permission rule is a privacy regression nobody asked for.
func TestInstallKeepsSettingsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	root := projectWithSettings(t, `{"model":"opus"}`)
	if err := os.Chmod(settingsPath(root), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(settingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("the install widened the user's settings from 0600 to %#o", got)
	}
	if !slices.Contains(allowRules(t, root), aPackRule(t)) {
		t.Fatal("the rule must still have been added")
	}
}

// TestDryRunWritesNothing: --dry-run must not authorize anything. It used to print
// "Pre-authorized in .claude/settings.json" while writing nothing — a tool lying about
// what it did, in a product whose entire pitch is that it does not.
func TestDryRunWritesNothing(t *testing.T) {
	root := projectWithUserSettings(t)
	plan, _, err := installHostPack(root, "claude-code", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.addRules, aPackRule(t)) {
		t.Fatalf("the dry run must report the rule it WOULD add, got %v", plan.addRules)
	}
	if got := allowRules(t, root); slices.Contains(got, aPackRule(t)) {
		t.Fatalf("--dry-run must not write permissions, got %v", got)
	}
}

// TestPackHashCoversPermissions: embeddedPackHash folds the pack's permission rules into its
// DIAGNOSTIC fingerprint, so two records differ when a release changes only which commands the
// pack pre-authorizes. This is inventory/inspection only — `doctor` detects a permission change by
// comparing settings.json to the manifest (hostPermissionsCheck), NOT via this hash — but the hash
// must still cover permissions so the diagnostic record reflects them. Hashing only files misses it.
func TestPackHashCoversPermissions(t *testing.T) {
	m, err := loadHostManifest("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	before := embeddedPackHash("claude-code", m)
	m.Permissions = append(m.Permissions, "Bash(something-new *)")
	if after := embeddedPackHash("claude-code", m); after == before {
		t.Fatal("pack_hash must change when the pack's permission rules change")
	}
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestHostRecordIsPortableNotInVichu: the install record must live WITH the pack
// (.claude/vichu-host.json), not under the gitignored .vichu/ — otherwise it never
// reaches a teammate's clone.
func TestHostRecordIsPortableNotInVichu(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err != nil {
		t.Fatalf("install must write the portable record %s: %v", portableHostRecordPath(root), err)
	}
	if _, err := os.Stat(filepath.Join(root, relLegacyRecord)); err == nil {
		t.Fatal("install must NOT write the legacy .vichu/host.json")
	}
}

// TestHostPackRecordSurvivesCloneWithoutVichu: a clone where .vichu/ (gitignored) is
// absent but .claude/ (committed, with the record) is present must still validate —
// doctor sees the pack and re-install is idempotent, no false clobber.
func TestHostPackRecordSurvivesCloneWithoutVichu(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate the clone: drop the gitignored runtime dir entirely.
	if err := os.RemoveAll(filepath.Join(root, ".vichu")); err != nil {
		t.Fatal(err)
	}
	if installRecord(t, root) == nil {
		t.Fatal("the record must survive a clone that has .claude/ but not .vichu/")
	}
	// Re-install must be idempotent — the record proves VichuFlow owns these files.
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatalf("re-install in a clone must not error as a clobber: %v", err)
	}
}

// TestLegacyHostRecordMigratesOnReinstall: an install left by an older binary (record
// under .vichu/host.json) is still readable, and re-installing migrates it to the
// portable location and drops the legacy copy.
func TestLegacyHostRecordMigratesOnReinstall(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, relLegacyRecord), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(portableHostRecordPath(root)); err != nil {
		t.Fatal(err)
	}
	// Precondition: only the legacy record exists, and it is still readable.
	if installRecord(t, root) == nil {
		t.Fatal("legacy record must remain readable for migration")
	}
	// Re-install migrates to portable and removes legacy.
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err != nil {
		t.Fatalf("re-install must migrate to the portable record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, relLegacyRecord)); err == nil {
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
	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("pack files with no record must refuse to clobber without --force")
	}
}

// TestUninstallUnknownHostDoesNotDelete: the reviewer's case — `uninstall --host
// nope` on a project with claude-code installed must error and delete NOTHING.
func TestUninstallUnknownHostDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "nope", true, false); err == nil {
		t.Fatal("an unknown host must error, not delete the installed pack")
	}
	// The installed pack and its record are untouched.
	if installRecord(t, root) == nil {
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
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate a record that belongs to a different (future) host.
	ih := installRecord(t, root)
	ih.Host = "codex"
	writeInstallRecord(t, root, ih)
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err == nil {
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

// installRecord reads a project's portable install record through a confined root, the
// same way the commands do.
func installRecord(t *testing.T, root string) *installedHost {
	t.Helper()
	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	ih, err := loadInstalledHost(pr)
	if err != nil {
		t.Fatal(err)
	}
	return ih
}

// writeInstallRecord persists a (possibly hostile) record, as a cloned repo would carry it.
func writeInstallRecord(t *testing.T, root string, ih *installedHost) {
	t.Helper()
	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if err := saveInstalledHost(pr, ih, nil); err != nil {
		t.Fatal(err)
	}
}

// TestInstallRefusesToFollowASymlinkOutOfTheProject: plain filepath.Join + os.WriteFile
// FOLLOW SYMLINKS. Make `.claude` a symlink to a directory outside the repo and the
// installer used to write the whole pack — skill, subagents, settings, record — out there
// and report success. An installer that a symlink in the tree can redirect is an installer
// that writes wherever the repo tells it to, and a host pack is code your agent then runs.
func TestInstallRefusesToFollowASymlinkOutOfTheProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The repo ships `.claude` as a symlink pointing out of the project.
	if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("install must refuse to write through a symlink that escapes the project")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the installer wrote %d entries OUTSIDE the project: %v", len(entries), entries)
	}
}

// symlinkPackFileToInternalCopy replaces an installed pack file with a symlink to ANOTHER file
// INSIDE the project holding its EXACT current bytes — a user sharing one subagent across
// their own `.claude/` layout. The target must be internal: os.Root refuses a symlink that
// ESCAPES the project outright (so the old code errored on those anyway), but FOLLOWS an
// internal one — which is exactly the byte-identical link the content-based check adopted as
// its own and then replaced or deleted.
func symlinkPackFileToInternalCopy(t *testing.T, root, rel string) (link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	link = filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, ".claude", "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(shared, filepath.Base(rel))
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	// RELATIVE target: os.Root refuses an ABSOLUTE symlink target outright, but follows a
	// relative one that stays inside the root — so a relative link is what actually reproduces
	// the old follow-and-adopt behavior.
	relTarget, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

// TestInstallDoesNotAdoptUserSymlinkPackFile: a pack destination the user turned into a symlink
// (to share one skill/subagent across projects) is theirs, even when its target's bytes match
// the pack exactly. The installer used to follow the link, see its own bytes, and adopt it —
// then the atomic write replaced the link with a regular file, silently breaking the sharing.
// Without --force, install must refuse and leave the link (and its target) untouched.
func TestInstallDoesNotAdoptUserSymlinkPackFile(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	link, target := symlinkPackFileToInternalCopy(t, root, ".claude/agents/vichu-worker.md")

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("install adopted a user's symlink whose target matched the pack — it must refuse without --force")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced the user's symlink with a regular file (err=%v)", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target must survive untouched: %v", err)
	}
}

// TestUninstallDoesNotDeleteUserSymlinkPackFile: the mirror image at uninstall — a byte-identical
// symlink is the user's construct, not ours to delete. Uninstall used to follow it, recognize the
// pack's bytes, and unlink it. Without --force it must treat the link as a stranger and abort,
// leaving it in place.
func TestUninstallDoesNotDeleteUserSymlinkPackFile(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	link, target := symlinkPackFileToInternalCopy(t, root, ".claude/agents/vichu-worker.md")

	if _, _, err := uninstallHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("uninstall deleted a user's symlink whose target matched the pack — it must refuse without --force")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("uninstall must leave the user's symlink in place (err=%v)", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target must survive untouched: %v", err)
	}
}

// TestUninstallWithdrawsNothingWithoutALedgerClaim: `added_permissions` used to live in the
// COMMITTED record, so a cloned repo could claim the user's own `Bash(user-owned *)` as ours
// and `vichu uninstall` would strip it.
//
// The ledger is where those claims live now — but it is a CLAIM, not proof (plan §9.5): it
// sits under `.vichu/`, which an agent can write. With no claim at all (a clone), uninstall
// withdraws nothing and says so. With one, it still needs `--withdraw-permissions`.
func TestUninstallWithdrawsNothingWithoutALedgerClaim(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":["Bash(user-owned *)"]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// The ledger is the only place permission-ownership CLAIMS live — and they are claims, not
	// proof (plan §9.5): it sits under `.vichu/`, which an agent can write. Delete it and there
	// is no claim at all, so uninstall withdraws nothing. A fresh clone looks exactly like this.
	if err := os.RemoveAll(filepath.Join(root, ".vichu")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	left := allowRules(t, root)
	if !slices.Contains(left, "Bash(user-owned *)") {
		t.Fatalf("uninstall removed a rule the user wrote themselves, got %v", left)
	}
	// With no ledger we cannot know what we added, so we withdraw NOTHING — including our
	// own rule. Leaving a rule behind is the safe failure; deleting the user's is not.
	if !slices.Contains(left, aPackRule(t)) {
		t.Fatalf("with no ownership ledger, uninstall must withdraw nothing, got %v", left)
	}
}

// TestUninstallWithdrawsOnlyWhatTheLedgerSays: the happy path — an install on THIS machine
// leaves a ledger, and uninstall withdraws exactly the rules in it.
func TestUninstallWithdrawsOnlyWhatTheLedgerSays(t *testing.T) {
	root := projectWithUserSettings(t) // already has Bash(go test *)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	left := allowRules(t, root)
	if slices.Contains(left, aPackRule(t)) {
		t.Fatalf("uninstall must withdraw the rule its own install added, got %v", left)
	}
	if !slices.Contains(left, "Bash(go test *)") {
		t.Fatalf("uninstall must keep the user's own rules, got %v", left)
	}
}

// TestAtomicWriteDoesNotDestroyANeighbour: the temp name used to be predictable
// (".vichu-tmp-" + basename). A user file that happened to carry that name was TRUNCATED
// by the create and then DELETED by the cleanup — we destroyed a file we never even looked
// at, and reported success.
func TestAtomicWriteDoesNotDestroyANeighbour(t *testing.T) {
	root := projectWithSettings(t, `{"model":"opus"}`)
	victim := filepath.Join(root, ".claude", ".vichu-tmp-settings.json")
	mustWrite(t, victim, "the user's file, which happens to have this name")

	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, victim); got != "the user's file, which happens to have this name" {
		t.Fatalf("the installer destroyed an unrelated file it never validated, leaving: %q", got)
	}
}

// TestUninstallKeepsTheCommitMarkerWhenTheLedgerSurvives: the portable record IS the commit
// marker — while it exists, uninstall work is still owed. Deleting it after failing to
// remove the ledger left an orphaned ledger and a retry that answered "no host pack is
// installed"; the stale ledger then went on claiming rules the user had since re-added.
func TestUninstallKeepsTheCommitMarkerWhenTheLedgerSurvives(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("relies on POSIX directory permissions denying removal")
	}
	root := projectWithUserSettings(t)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, ".vichu", "hosts", "claude-code")
	if err := os.Chmod(locked, 0o555); err != nil { // the ledger cannot be removed
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err == nil {
		t.Fatal("uninstall must fail when it cannot remove the ownership ledger")
	}
	if installRecord(t, root) == nil {
		t.Fatal("the portable record is the commit marker — it must survive so the retry can finish the job")
	}
	// And the retry completes once the obstacle is gone.
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatalf("the retry must complete: %v", err)
	}
	if installRecord(t, root) != nil {
		t.Fatal("a successful uninstall must drop the record")
	}
}

// TestLedgerSurvivesACrashedInstall: a ledger with no install record comes from an install
// that died before writing the commit marker. It lives under `.vichu/` (local, gitignored) so —
// unlike the portable record — a CLONE never carries it, so it cannot import a stranger's claims.
// It is NOT proof, though: `.vichu/` is agent-writable (H11), so the ledger is a PROPOSAL, and
// uninstall only ACTS on its claims under the §9.5 restrictions (never withdrawing by default).
// Still, discarding it as "orphaned" made the recovered install forget which rules it had added —
// stranding them in the user's settings forever — and made `init` report them as the USER's own,
// which is a lie about the one fact that tells them whether uninstall will clean up.
func TestLedgerSurvivesACrashedInstall(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":[]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// A hard kill before the commit marker: everything written except the record.
	if err := os.Remove(portableHostRecordPath(root)); err != nil {
		t.Fatal(err)
	}
	plan, _, err := installHostPack(root, "claude-code", false, false)
	if err != nil {
		t.Fatal(err)
	}
	// The recovered install must still know these rules are ours — not report them as the
	// user's, and not forget them.
	if !slices.Contains(plan.ledgerClaimedOurs, aPackRule(t)) {
		t.Fatalf("the recovered install must still claim its own rules, got ours=%v users=%v", plan.ledgerClaimedOurs, plan.ledgerClaimedUsers)
	}
	if slices.Contains(plan.ledgerClaimedUsers, aPackRule(t)) {
		t.Fatal("a rule WE added must never be reported as the user's own")
	}
	// And uninstall withdraws them, as it always should have.
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	if left := allowRules(t, root); slices.Contains(left, aPackRule(t)) {
		t.Fatalf("uninstall must withdraw the rules the ledger records as ours, leaving %v", left)
	}
}

// TestConcurrentInstallsAreSerialized: two `vichu init --host` processes in one project
// edit the same .claude/settings.json. Without a lock they read the same allow-list, each
// append their rules, and the last write wins — silently dropping the other's. They also
// fought over a shared temp filename, each deleting the other's file mid-write.
//
// The lock makes the whole operation single-writer: one wins, the other is told to wait.
// Whichever way it lands, the settings file is intact and the pack is fully installed.
func TestConcurrentInstallsAreSerialized(t *testing.T) {
	root := projectWithUserSettings(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = installHostPack(root, "claude-code", false, false)
		}()
	}
	wg.Wait()

	// At most one may be turned away, and only with the "busy" message — never with a
	// corrupted file or a half-written pack.
	for _, err := range errs {
		if err != nil && !strings.Contains(err.Error(), i18n.T("host.locked")) {
			t.Fatalf("a concurrent install must either succeed or report the lock, got: %v", err)
		}
	}
	got := allowRules(t, root)
	if !slices.Contains(got, aPackRule(t)) || !slices.Contains(got, "Bash(go test *)") {
		t.Fatalf("settings.json lost a rule to a concurrent write: %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "vichu-orchestrator", "SKILL.md")); err != nil {
		t.Fatalf("the pack must be fully installed: %v", err)
	}
}

// packRules is what the claude-code pack actually declares, so the tests track the manifest
// instead of hard-coding a rule that a security fix might have to narrow.
func packRules(t *testing.T) []string {
	t.Helper()
	m, err := loadHostManifest("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	return m.Permissions
}

// aPackRule is one representative rule the pack authorizes — enough to assert "ours was
// added / withdrawn / left alone" without restating the whole list.
func aPackRule(t *testing.T) string {
	t.Helper()
	rules := packRules(t)
	if len(rules) == 0 {
		t.Fatal("the pack must declare at least one permission rule")
	}
	return rules[0]
}

// TestPackNeverAuthorizesAnOverrideCommand: the pack must never pre-authorize a command
// that can move a run past the kernel's verdict.
//
// It once authorized `Bash(vichu *)` — a wildcard over EVERY subcommand, including
// `vichu run resume`, which clears a block. A subagent inherits the parent's Bash
// permissions, so a worker whose read-only violation blocked the run could simply unblock
// itself and carry on. The one property this product sells is that the agent cannot
// advance the run without evidence the kernel verified; a permission rule that hands it the
// override turns the whole thing into theater.
//
// `run resume`, `cancel`, `init`, `uninstall` and `exec` stay un-authorized on purpose:
// the host stops and asks a human, and that prompt IS the control.
func TestPackNeverAuthorizesAnOverrideCommand(t *testing.T) {
	// `run start` belongs here too, and that is not obvious. A driver token protects a run
	// that EXISTS — but anyone who can call `run start` gets a brand-new run with a brand-new
	// token. On the filesystem provider that second run RE-BASELINES the tree, so a file the
	// first run's worker created stops looking like a mutation: the implementer opens its own
	// run and its changes vanish from the original run's audit. Starting a run is an act of
	// human intent, and it costs one approval per task, not per command.
	forbidden := []string{"run start", "run resume", "cancel", "init", "uninstall", "exec"}
	for _, rule := range packRules(t) {
		if strings.Contains(rule, "vichu *") || strings.Contains(rule, "vichu:*") {
			t.Fatalf("%q is a wildcard over every vichu subcommand — it authorizes `run resume`, which clears a block", rule)
		}
		for _, f := range forbidden {
			if strings.Contains(rule, f) {
				t.Fatalf("%q pre-authorizes %q, an override a human must approve", rule, f)
			}
		}
	}
}

// TestReadOnlySubagentsHaveNoBash: `tools` in a subagent's frontmatter is a security
// boundary. A subagent inherits the parent's tools unless it restricts them — so a
// read-only worker or a reviewer with Bash inherits every pre-authorized `vichu` command
// too. Neither has any legitimate need for it: they read code and reason about it.
func TestReadOnlySubagentsHaveNoBash(t *testing.T) {
	for _, agent := range []string{"vichu-worker", "vichu-reviewer"} {
		data, err := hostpacks.FS.ReadFile("packs/claude-code/agents/" + agent + ".md")
		if err != nil {
			t.Fatal(err)
		}
		front, _, ok := strings.Cut(strings.TrimPrefix(string(data), "---\n"), "\n---")
		if !ok {
			t.Fatalf("%s has no frontmatter", agent)
		}
		if !strings.Contains(front, "tools:") {
			t.Fatalf("%s must restrict `tools` — without it, it inherits Bash and every pre-authorized vichu command", agent)
		}
		if strings.Contains(front, "Bash") || strings.Contains(front, "Write") || strings.Contains(front, "Edit") {
			t.Fatalf("%s is a read-only stage and must not have Bash/Write/Edit, got: %s", agent, front)
		}
	}
}

// TestRefreshWithdrawsOurStaleRule: narrowing the manifest is not enough — the old over-broad
// rule is already sitting in the user's settings.json, and an upgrade that leaves it there
// fixes nothing. The refresh withdraws it because the ledger CLAIMS we added it — and what
// makes acting on that claim safe is the BOUND, not the claim: only rules from the published
// catalog can be withdrawn, and removing a grant can only ever reduce what an agent may do.
func TestRefreshWithdrawsOurStaleRule(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":[]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate an install from the release that authorized the wildcard: the rule is in the
	// user's settings AND the ledger records it as ours.
	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadHostSettings(pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.addAndSave(pr, []string{"Bash(vichu *)"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	ow := &hostOwnership{Host: "claude-code", AddedPermissions: append(packRules(t), "Bash(vichu *)")}
	if err := saveHostOwnership(pr, ow, nil); err != nil {
		t.Fatal(err)
	}
	pr.Close()

	// The user upgrades and refreshes the pack.
	plan, _, err := installHostPack(root, "claude-code", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.staleRules, "Bash(vichu *)") {
		t.Fatalf("the refresh must plan to withdraw the stale wildcard, got %v", plan.staleRules)
	}
	left := allowRules(t, root)
	if slices.Contains(left, "Bash(vichu *)") {
		t.Fatalf("the refresh left the wildcard authorized — an upgrade that does not withdraw it fixes nothing: %v", left)
	}
	if !slices.Contains(left, aPackRule(t)) {
		t.Fatalf("the refresh must still authorize the pack's own commands: %v", left)
	}
}

// TestRefreshLeavesTheUsersOwnWildcardAlone: if the LEDGER does not say we added it, the
// rule is the user's — even one that looks exactly like ours.
func TestRefreshLeavesTheUsersOwnWildcardAlone(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":["Bash(vichu *)"]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if left := allowRules(t, root); !slices.Contains(left, "Bash(vichu *)") {
		t.Fatalf("the user wrote that rule themselves — we never added it, so we must not take it away: %v", left)
	}
}

// TestInstallRecoversFromACrashBeforeTheCommit: the install record is the commit marker and
// is written LAST. A process killed before it (SIGKILL, power loss — not a Go error, so the
// rollback never runs) leaves the pack files on disk with no record to vouch for them.
//
// The retry used to refuse them as "the user's" and demand --force, which is a terrible
// thing to tell someone whose only crime was losing power: --force also discards edits they
// may have made elsewhere. The files are byte-for-byte ours; comparing CONTENT, instead of
// trusting a record we never got to write, is the whole recovery.
func TestInstallRecoversFromACrashBeforeTheCommit(t *testing.T) {
	root := projectWithSettings(t, `{"model":"opus"}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// The state a hard kill leaves behind: everything written except the commit marker.
	if err := os.Remove(portableHostRecordPath(root)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatalf("the retry after a crash must complete, not demand --force: %v", err)
	}
	if installRecord(t, root) == nil {
		t.Fatal("the recovered install must write its commit marker")
	}
	if !slices.Contains(allowRules(t, root), aPackRule(t)) {
		t.Fatal("the recovered install must leave the permissions in place")
	}
}

// TestARealUserFileIsStillRefused: content-equality is not a license to overwrite. A file at
// a pack destination whose content DIFFERS is the user's, crash or no crash.
func TestARealUserFileIsStillRefused(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, ".claude", "commands", "vichu.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dst, "MY OWN COMMAND")

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("a file whose content is not ours must still be refused without --force")
	}
	if got := readFileString(t, dst); got != "MY OWN COMMAND" {
		t.Fatalf("the user's file must be untouched, got %q", got)
	}
}

// TestHostileRecordCannotDeleteArbitraryFiles: `.claude/vichu-host.json` is DESIGNED to be
// committed, so on a cloned repo it is attacker-controlled. It used to supply the PATHS that
// `vichu uninstall` deletes, and the hash check proved nothing — the attacker writes both the
// file and the hash. A record listing `README.md` with README's real hash made uninstall
// delete it and exit 0.
//
// The set of paths uninstall may touch now comes from the MANIFEST, embedded in this binary.
// The record supplies hashes only, to tell "ours, untouched" from "the user edited it".
func TestHostileRecordCannotDeleteArbitraryFiles(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "README.md")
	mustWrite(t, victim, "# the user's README")

	// A hostile record, exactly as it arrives in a cloned repo: a real path, a real hash.
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeInstallRecord(t, root, &installedHost{
		Host: "claude-code", Mode: "native",
		Files: map[string]string{"README.md": hashBytes([]byte("# the user's README"))},
	})

	// Whether it errors or "succeeds", the one thing that must not happen is the deletion.
	_, _, _ = uninstallHostPack(root, "claude-code", true, false)
	if got := readFileString(t, victim); got != "# the user's README" {
		t.Fatalf("uninstall deleted a file the pack never owned, leaving %q", got)
	}
}

// TestForgedLedgerCannotWithdrawTheUsersRules: the ownership ledger lives under `.vichu/`,
// which is EXCLUDED from the mutation audit (it is the kernel's own runtime, so a worker
// writing there must never count as a mutation). An agent with a write tool can therefore
// forge it — claim `Bash(user-owned *)` is ours — and the next `vichu uninstall` strips a
// rule the user wrote themselves. That is the kernel presenting manipulable data as proof of
// ownership.
//
// A claim about a rule VichuFlow never authored is now refused, loudly.
func TestForgedLedgerCannotWithdrawTheUsersRules(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":["Bash(user-owned *)"]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// The agent forges the ledger.
	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	ow, err := loadHostOwnership(pr, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	ow.AddedPermissions = append(ow.AddedPermissions, "Bash(user-owned *)")
	if err := saveHostOwnership(pr, ow, nil); err != nil {
		t.Fatal(err)
	}
	pr.Close()

	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err != nil {
		t.Fatal(err)
	}
	if left := allowRules(t, root); !slices.Contains(left, "Bash(user-owned *)") {
		t.Fatalf("a forged ledger claim stripped a rule the user wrote themselves, leaving %v", left)
	}
}

// TestInstallSaysWhatActuallyHappened: one blanket "adopted" line for every recognized file
// told three separate lies — it claimed there was no install record when there was, it said
// "VichuFlow now manages them" during a --dry-run that wrote nothing, and it lumped an
// untouched refresh in with a genuine new claim. The file operations were safe; the story
// told about them was false, which is I6 broken in the only place a user can see.
func TestInstallSaysWhatActuallyHappened(t *testing.T) {
	root := t.TempDir()

	// 1. Fresh install: nothing existed, so nothing is "already current" or "adopted".
	out := captureStdout(t, func() {
		if err := installHostAndReport(root, "claude-code", false, false); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, i18n.T("host.files_adopted")) {
		t.Fatalf("a fresh install adopts nothing:\n%s", out)
	}

	// 2. Re-install of the very same pack: unchanged, and it must say so — not "adopted".
	out = captureStdout(t, func() {
		if err := installHostAndReport(root, "claude-code", false, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, i18n.T("host.files_current")) {
		t.Fatalf("re-installing the same pack must report it as already current:\n%s", out)
	}
	if strings.Contains(out, i18n.T("host.files_adopted")) {
		t.Fatalf("re-installing our own recorded pack is not an adoption:\n%s", out)
	}

	// 3. A clone: same files, no install record. THAT is an adoption, and it is announced.
	clone := t.TempDir()
	for _, f := range packDests(t) {
		src := filepath.Join(root, filepath.FromSlash(f))
		dst := filepath.Join(clone, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, dst, readFileString(t, src))
	}
	out = captureStdout(t, func() {
		if err := installHostAndReport(clone, "claude-code", false, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, i18n.T("host.files_adopted")) {
		t.Fatalf("claiming files nothing vouched for MUST be announced:\n%s", out)
	}
}

// TestDryRunOnlySpeaksInWoulds: a preview that writes nothing must not describe the world as
// though it changed it.
func TestDryRunOnlySpeaksInWoulds(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(portableHostRecordPath(root)); err != nil { // now an adoption
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := installHostAndReport(root, "claude-code", false, true); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, i18n.T("host.files_adopted")) || strings.Contains(out, i18n.T("host.preauthorized")) {
		t.Fatalf("--dry-run must speak only in \"would\":\n%s", out)
	}
	if !strings.Contains(out, i18n.T("host.files_would_adopt")) {
		t.Fatalf("--dry-run must still say what it WOULD do:\n%s", out)
	}
}

func packDests(t *testing.T) []string {
	t.Helper()
	m, err := loadHostManifest("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Dest)
	}
	return out
}

// TestForgedRecordCannotOverwriteTheUsersFile: `.claude/vichu-host.json` is committed, so on
// a cloned repo it is the attacker's to write — and its hashes used to AUTHORIZE an
// overwrite. A record claiming the user's customized `vichu-implementer.md`, with its real
// hash, made `init` treat the file as ours and replace it with the embedded pack.
//
// (The attacker cannot inject content — we write OUR pack. The harm is the user's edit
// silently reverted: data loss, not code execution. It is still not ours to do.)
//
// Overwrite authority comes from the pack EMBEDDED IN THIS BINARY (current + released
// versions) — the one reference a repo cannot forge. No record, portable or local, authorizes
// it: see plan §9.5, and TestForgedLedgerCannotDestroyAFile for why the ledger cannot either.
func TestForgedRecordCannotOverwriteTheUsersFile(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, ".claude", "agents", "vichu-implementer.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dst, "MY CUSTOMIZED IMPLEMENTER")
	writeInstallRecord(t, root, &installedHost{
		Host: "claude-code", Mode: "native",
		Files: map[string]string{".claude/agents/vichu-implementer.md": hashBytes([]byte("MY CUSTOMIZED IMPLEMENTER"))},
	})

	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("a committed record must not authorize overwriting a file whose content is not ours")
	}
	if got := readFileString(t, dst); got != "MY CUSTOMIZED IMPLEMENTER" {
		t.Fatalf("the user's customization was overwritten on a hostile record's say-so, leaving: %q", got)
	}
}

// TestUninstallDoesNotWithdrawPermissionsByDefault: the ledger that says which rules are ours
// lives inside the workspace, under `.vichu/`, which is excluded from the mutation audit — so
// an agent with a write tool can forge it. The authored-rules catalog stops it claiming a rule
// we never shipped, but the user may have written one of OURS themselves.
//
// So the ledger's claims are a PROPOSAL. Leaving a permission behind is one edit to undo;
// deleting one the user wrote is not.
func TestUninstallDoesNotWithdrawPermissionsByDefault(t *testing.T) {
	root := projectWithSettings(t, `{"permissions":{"allow":[]}}`)
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uninstallHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	if left := allowRules(t, root); !slices.Contains(left, aPackRule(t)) {
		t.Fatalf("uninstall must NOT withdraw permissions without --withdraw-permissions, got %v", left)
	}
}

// TestForgedLedgerCannotDestroyAFile is the finding that forced a redesign.
//
// Moving overwrite authority from the committed record to the local ledger only changed WHO
// forges the proof: `.vichu/` is excluded from the mutation audit precisely so the KERNEL can
// write there — which means an agent with a write tool can too. Ownership metadata stored
// inside a workspace an agent can write to is not evidence, full stop.
//
// The only unforgeable reference is the pack embedded in this binary. A file is ours iff its
// bytes are our bytes.
func TestForgedLedgerCannotDestroyAFile(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, ".claude", "agents", "vichu-implementer.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dst, "MY CUSTOMIZED IMPLEMENTER")

	// The agent forges BOTH records to claim the file.
	writeInstallRecord(t, root, &installedHost{
		Host: "claude-code", Mode: "native",
		Files: map[string]string{".claude/agents/vichu-implementer.md": hashBytes([]byte("MY CUSTOMIZED IMPLEMENTER"))},
	})
	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveHostOwnership(pr, &hostOwnership{Host: "claude-code"}, nil); err != nil {
		t.Fatal(err)
	}
	pr.Close()

	// init must not overwrite it.
	if _, _, err := installHostPack(root, "claude-code", false, false); err == nil {
		t.Fatal("no record — forged or not — may authorize overwriting content that is not ours")
	}
	if got := readFileString(t, dst); got != "MY CUSTOMIZED IMPLEMENTER" {
		t.Fatalf("init overwrote the user's file on a forged record, leaving %q", got)
	}
	// uninstall must refuse — a forged record does not make the file ours to delete.
	if _, _, err := uninstallHostPack(root, "claude-code", true, false); err == nil {
		t.Fatal("no record — forged or not — may authorize deleting content that is not ours")
	}
	if got := readFileString(t, dst); got != "MY CUSTOMIZED IMPLEMENTER" {
		t.Fatalf("uninstall deleted the user's file on a forged record, leaving %q", got)
	}
}

// TestCloneCanUninstallCleanly: the previous design refused to delete anything without a local
// ledger, then deleted the install record and printed "Uninstalled" — leaving the whole pack
// on disk and no longer recognizable as a pack. A clone has no `.vichu/` and never will.
//
// Ownership by CONTENT needs no ledger, so this just works.
func TestCloneCanUninstallCleanly(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	// What a teammate's clone looks like: committed .claude/, no .vichu/.
	if err := os.RemoveAll(filepath.Join(root, ".vichu")); err != nil {
		t.Fatal(err)
	}

	removed, kept, err := uninstallHostPack(root, "claude-code", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) == 0 {
		t.Fatalf("a clone must be able to uninstall the pack it carries, removed=%v kept=%v", removed, kept)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "skills", "vichu-orchestrator", "SKILL.md")); err == nil {
		t.Fatal("uninstall reported success but left the pack on disk")
	}
}

// TestKnownHashesCatalogIsTruthful is the gate that keeps upgrades working, and it must never
// SKIP. It used to reconstruct the released pack from `git show`, and skipped when git was
// missing — so a source distribution, a shallow checkout, or a forgotten catalog entry would go
// green while silently breaking every upgrade from that release.
//
// Both directions are checked, over EVERY released version: each fixture's hash must be in the
// catalog (or that release cannot upgrade), and each catalog hash must have a fixture (or the
// catalog claims a version we cannot prove we shipped).
func TestKnownHashesCatalogIsTruthful(t *testing.T) {
	m, err := loadHostManifest("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	own, err := loadPackOwnership("claude-code", m)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := releasedFixtures(t, "claude-code")
	if len(fixtures) == 0 {
		t.Fatal("no released-pack fixtures — the catalog cannot be verified, so upgrades cannot be trusted")
	}
	cataloged := hashIndex(own.shipped)
	fixtured := map[string]map[string]bool{}
	for dest, bodies := range fixtures {
		fixtured[dest] = map[string]bool{}
		for _, body := range bodies {
			fixtured[dest][hashBytes(body)] = true
		}
	}

	// Every released file must be IN the catalog, or an upgrade from that release refuses.
	assertEachIn(t, fixtured, cataloged,
		"known-hashes.json does not list %s for %s — an upgrade from the release that shipped it will REFUSE to proceed. Add it (see RELEASING.md).")
	// And every catalog entry must have a fixture, or it claims a version we cannot prove.
	assertEachIn(t, cataloged, fixtured,
		"known-hashes.json lists %s for %s, but no fixture matches it — the catalog claims a version we cannot prove we shipped")

	for dest, bodies := range fixtures {
		for _, body := range bodies {
			if !own.owns(dest, body) {
				t.Errorf("a released %s is not recognized as ours", dest)
			}
		}
	}
}

// assertEachIn reports every (dest, hash) present in `have` but missing from `want`.
func assertEachIn(t *testing.T, have, want map[string]map[string]bool, format string) {
	t.Helper()
	for dest, hashes := range have {
		for h := range hashes {
			if !want[dest][h] {
				t.Errorf(format, h, dest)
			}
		}
	}
}

// hashIndex turns dest → []hash into dest → set(hash).
func hashIndex(m map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for dest, hashes := range m {
		out[dest] = map[string]bool{}
		for _, h := range hashes {
			out[dest][h] = true
		}
	}
	return out
}

// TestUpgradeFromAReleasedPackNeedsNoForce: a v0.4.0 pack file, untouched, is unmistakably
// ours — but its bytes differ from what this binary ships. Without the released-versions
// catalog it looked like a stranger's file: `doctor` said "run vichu init --host", and that
// command then refused. The official upgrade path led straight into a wall.
func TestUpgradeFromAReleasedPackNeedsNoForce(t *testing.T) {
	fixtures := map[string][]byte{}
	for dest, bodies := range releasedFixtures(t, "claude-code") {
		fixtures[dest] = bodies[0]
	}

	root := t.TempDir()
	rec := &installedHost{Host: "claude-code", Mode: "native", Files: map[string]string{}}
	for dest, body := range fixtures {
		dst := filepath.Join(root, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, dst, string(body))
		rec.Files[dest] = hashBytes(body)
	}
	writeInstallRecord(t, root, rec) // what a real v0.4.0 install left behind

	// The plain refresh — exactly what `doctor` tells the user to run — must work.
	plan, _, err := installHostPack(root, "claude-code", false, false)
	if err != nil {
		t.Fatalf("refreshing an untouched pack from an earlier RELEASE must not need --force: %v", err)
	}
	if len(plan.fromRelease) == 0 {
		t.Fatalf("the refresh must report these as upgraded from an earlier release, got %+v", plan)
	}

	// And uninstall must recognize an earlier release as ours.
	root2 := t.TempDir()
	for dest, body := range fixtures {
		dst := filepath.Join(root2, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, dst, string(body))
	}
	writeInstallRecord(t, root2, rec)
	removed, _, err := uninstallHostPack(root2, "claude-code", false, false)
	if err != nil {
		t.Fatalf("uninstalling an earlier RELEASE must not need --force: %v", err)
	}
	if len(removed) != len(fixtures) {
		t.Fatalf("uninstall must remove every file of the earlier release, removed=%v", removed)
	}
}

// releasedFixtures reads the checked-in copy of EVERY released pack under
// testdata/packs/<host>/<version>/, keyed dest → the set of bodies we ever shipped there.
//
// Every version, not just the newest historical one: the catalog must grow with each release,
// and a checker that only reads one version would pass the first time and then fail forever —
// the release ritual would work exactly once.
//
// Checked in, not fetched from git: a test that can skip is a test that has stopped guarding.
func releasedFixtures(t *testing.T, host string) map[string][][]byte {
	t.Helper()
	base := filepath.Join("testdata", "packs", host)
	versions, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("cannot read the released-pack fixtures at %s: %v — without them the released-versions catalog is unverifiable, and every upgrade depends on it", base, err)
	}
	out := map[string][][]byte{}
	for _, v := range versions {
		if v.IsDir() {
			readVersionFixtures(t, filepath.Join(base, v.Name()), out)
		}
	}
	return out
}

// readVersionFixtures adds one released version's files to the dest → bodies index.
func readVersionFixtures(t *testing.T, root string, out map[string][][]byte) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		dest := filepath.ToSlash(rel)
		out[dest] = append(out[dest], body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDryRunForceNamesEveryDestructiveReplacement: a --dry-run exists so someone can weigh the
// risk of --force BEFORE running it. It listed what it would install and what it would adopt,
// and said nothing about the one file --force would destroy — the only part of the preview
// that could actually hurt them.
func TestDryRunForceNamesEveryDestructiveReplacement(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, ".claude", "agents", "vichu-worker.md")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, victim, "MY USER FILE")

	out := captureStdout(t, func() {
		if err := installHostAndReport(root, "claude-code", true, true); err != nil { // --force --dry-run
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, i18n.T("host.files_would_replace")) {
		t.Fatalf("--force --dry-run must warn about what it would DESTROY:\n%s", out)
	}
	if !strings.Contains(out, ".claude/agents/vichu-worker.md") {
		t.Fatalf("the warning must NAME the file:\n%s", out)
	}
	// And the preview must have written nothing.
	if got := readFileString(t, victim); got != "MY USER FILE" {
		t.Fatalf("--dry-run wrote to the user's file: %q", got)
	}
	if _, err := os.Stat(portableHostRecordPath(root)); err == nil {
		t.Fatal("--dry-run must not create an install record")
	}
}

// TestEveryReleasedPackIsRecorded closes the loop the catalog gate cannot.
//
// TestKnownHashesCatalogIsTruthful only checks that the fixtures and the catalog AGREE. It
// cannot notice that a release was never recorded — or was recorded HALF. Both sides would
// simply be missing the file, and CI would stay green while the one hash that never made it in
// turns a future user's untouched pack into "you edited this" and refuses to upgrade them.
//
// So this compares against the source of truth: the manifest AT EACH TAG. Every destination it
// declared must exist as a fixture, with the exact bytes, and its hash must be in the catalog.
func TestEveryReleasedPackIsRecorded(t *testing.T) {
	own, err := loadPackOwnership("claude-code", mustManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range releasedTags(t) {
		raw, gerr := gitShow(tag + ":internal/hostpacks/packs/claude-code/manifest.json")
		if gerr != nil {
			continue // a release that predates host packs — nothing to record
		}
		var m hostManifest
		if uerr := json.Unmarshal(raw, &m); uerr != nil {
			t.Errorf("the manifest at %s is not valid JSON: %v", tag, uerr)
			continue
		}
		assertReleaseRecorded(t, tag, &m, own)
	}
}

// assertReleaseRecorded checks one released tag against its fixtures and the catalog.
func assertReleaseRecorded(t *testing.T, tag string, m *hostManifest, own packOwnership) {
	t.Helper()
	dir := filepath.Join("testdata", "packs", "claude-code", tag)
	fix := "go run ./tools/packhistory --host claude-code --tag " + tag
	for _, f := range m.Files {
		want, gerr := gitShow(tag + ":internal/hostpacks/packs/claude-code/" + f.Src)
		if gerr != nil {
			t.Errorf("cannot read %s at %s: %v", f.Src, tag, gerr)
			continue
		}
		got, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Dest)))
		if rerr != nil {
			t.Errorf("release %s shipped %s, but there is no fixture for it (%v). A half-recorded release passes every other gate and then breaks the upgrade from %s. Run: %s", tag, f.Dest, rerr, tag, fix)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the fixture for %s at %s does not match what that release actually shipped. Run: %s", f.Dest, tag, fix)
			continue
		}
		if !own.owns(f.Dest, want) {
			t.Errorf("known-hashes.json does not list %s as shipped at %s — an upgrade from that release will REFUSE to proceed. Run: %s", f.Dest, tag, fix)
		}
	}
}

func mustManifest(t *testing.T) *hostManifest {
	t.Helper()
	m, err := loadHostManifest("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// releasedTags lists the repo's release tags. Without git (a source tarball) there is nothing
// to check against, which is honest — but CI fetches tags (fetch-depth: 0), and CI is where
// this must bite.
func releasedTags(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		t.Skip("git not available — cannot cross-check released packs (CI fetches tags; this gate runs there)")
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		t.Skip("no release tags in this checkout (shallow clone?) — CI fetches them with fetch-depth: 0")
	}
	return tags
}

func gitShow(ref string) ([]byte, error) { return exec.Command("git", "show", ref).Output() }

// TestUndoLogRestoresSymlinkType: the installer's undo log must restore a symlink AS a
// symlink. It used to Lstat correctly but then read THROUGH the link and record a
// regular-file restore, so a failed --force install turned a shared-config symlink into a
// plain file — a "rollback" that permanently changed the project structure.
func TestUndoLogRestoresSymlinkType(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	root := t.TempDir()
	// A shared target and a versioned symlink pointing at it, the shape a repo uses to share
	// a pack file across worktrees.
	if err := os.WriteFile(filepath.Join(root, "shared.md"), []byte("SHARED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".claude", "agents", "vichu-worker.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../shared.md", link); err != nil {
		t.Fatal(err)
	}

	pr, err := openProjectRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	u := &undoLog{pr: pr}
	rel := ".claude/agents/vichu-worker.md"
	if err := u.before(rel); err != nil {
		t.Fatalf("before: %v", err)
	}
	// The install overwrites the link with a regular file (as --force would).
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("NEW PACK CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A later step fails → rollback.
	if err := u.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rollback restored the symlink as a regular file — the project structure was permanently changed")
	}
	target, err := os.Readlink(link)
	if err != nil || target != "../../shared.md" {
		t.Fatalf("symlink target = %q (%v), want ../../shared.md", target, err)
	}
}

// TestInitWritesConfinedThroughSymlinks: init/scaffold must not write through a symlink an
// untrusted checkout left in the project. Covers vichu.yaml, .gitignore, and a template file.
func TestInitWritesConfinedThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(external, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// vichu.yaml planted as a symlink to the external victim.
	if err := os.Symlink(external, filepath.Join(root, "vichu.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := confinedProjectWrite(root, "vichu.yaml", []byte("new: config\n"), 0o644); err != nil {
		t.Fatalf("confinedProjectWrite: %v", err)
	}
	if data, _ := os.ReadFile(external); string(data) != "ORIGINAL\n" {
		t.Fatalf("init wrote through the vichu.yaml symlink onto an external file: %q", data)
	}
	fi, _ := os.Lstat(filepath.Join(root, "vichu.yaml"))
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("vichu.yaml is still a symlink — the write followed it instead of replacing it")
	}
}

// TestWriteTemplateRetryIsIdempotent: a template file already on disk with IDENTICAL content
// (a prior init that wrote templates then failed) must not dead-end the retry with "file
// exists" — it is recognized as already applied. A DIFFERING file still refuses without force.
func TestWriteTemplateRetryIsIdempotent(t *testing.T) {
	tpl, err := resolveTemplate("node")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	// First write.
	if _, err := writeTemplate(root, tpl, "app", false); err != nil {
		t.Fatalf("first writeTemplate: %v", err)
	}
	// Retry without --force: identical files → no error (already applied).
	if _, err := writeTemplate(root, tpl, "app", false); err != nil {
		t.Fatalf("retry with identical files must succeed, got: %v", err)
	}
	// A file that DIFFERS still refuses without force.
	files := tpl.Files("app")
	if len(files) > 0 {
		if err := os.WriteFile(filepath.Join(root, files[0].Path), []byte("EDITED BY USER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := writeTemplate(root, tpl, "app", false); err == nil {
			t.Fatal("a differing existing file must refuse without --force")
		}
	}
}
