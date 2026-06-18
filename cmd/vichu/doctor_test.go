package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeClaudeOnPath puts a POSIX `claude` shim on PATH that answers `--version` and
// `auth status` with the given values, so doctor's host-agent probe can be driven
// deterministically. Skips on Windows (shell script).
func fakeClaudeOnPath(t *testing.T, version string, loggedIn bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shim is a POSIX shell script")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n--version) echo '%s (Claude Code)';;\nauth) echo '{\"loggedIn\":%t}';;\nesac\n", version, loggedIn)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICHU_CLAUDE_BIN", "") // use the PATH-resolved `claude` (our shim)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// projectWithHostPack creates a fresh initialized project with the claude-code host
// pack installed, and chdirs into it.
func projectWithHostPack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := installHostPack(dir, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDoctorFailsOnInvalidConfig: `vichu doctor` must exit non-zero when vichu.yaml
// exists but does not parse — it is the setup gate, not a "file exists" check.
func TestDoctorFailsOnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// A vichu.yaml that exists but is not valid YAML.
	if err := os.WriteFile(filepath.Join(dir, "vichu.yaml"), []byte("project: {name: x\n  broken: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor(nil); err == nil {
		t.Fatal("doctor must fail (non-zero) on an invalid vichu.yaml, not report green")
	}
}

// TestDoctorPassesOnValidConfig: a valid config doctor's checks pass (sanity).
func TestDoctorPassesOnValidConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor(nil); err != nil {
		t.Fatalf("doctor should pass on a valid project: %v", err)
	}
}

// TestDoctorFailsWhenHostAgentUnsupportedVersion: the host pack's binary is on PATH
// but reports an unsupported CLI version — doctor must FAIL, not pass green. The
// pack is only usable if the agent behind it can actually run.
func TestDoctorFailsWhenHostAgentUnsupportedVersion(t *testing.T) {
	fakeClaudeOnPath(t, "99.0.0", true)
	projectWithHostPack(t)
	if err := cmdDoctor(nil); err == nil {
		t.Fatal("doctor must fail when the host agent CLI is an unsupported version")
	}
}

// TestDoctorFailsWhenHostAgentUnauthenticated: a supported version but not logged in
// is still unusable — doctor must fail.
func TestDoctorFailsWhenHostAgentUnauthenticated(t *testing.T) {
	fakeClaudeOnPath(t, "1.5.0", false)
	projectWithHostPack(t)
	if err := cmdDoctor(nil); err == nil {
		t.Fatal("doctor must fail when the host agent CLI is not authenticated")
	}
}

// TestDoctorPassesWhenHostAgentHealthy: supported version + authenticated → doctor
// passes (the host pack is genuinely usable).
func TestDoctorPassesWhenHostAgentHealthy(t *testing.T) {
	fakeClaudeOnPath(t, "1.5.0", true)
	projectWithHostPack(t)
	if err := cmdDoctor(nil); err != nil {
		t.Fatalf("doctor should pass with a healthy host agent: %v", err)
	}
}

// TestDoctorValidatesHostPackFromClone: in a fresh clone the gitignored .vichu/ is
// absent but the committed .claude/ carries the pack AND its record — doctor must
// still validate the pack (record travels with .claude/, not under .vichu/).
func TestDoctorValidatesHostPackFromClone(t *testing.T) {
	fakeClaudeOnPath(t, "1.5.0", true)
	dir := projectWithHostPack(t)
	if err := os.RemoveAll(filepath.Join(dir, ".vichu")); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor(nil); err != nil {
		t.Fatalf("doctor must validate the host pack from a clone without .vichu/: %v", err)
	}
}

// TestHostPackOutdatedDetected: a fresh install is not outdated, but once the
// recorded pack_hash no longer matches the binary's embedded pack (simulating a
// binary upgrade that changed the pack), doctor flags it as outdated.
func TestHostPackOutdatedDetected(t *testing.T) {
	root := t.TempDir()
	if _, err := installHostPack(root, "claude-code", false, false); err != nil {
		t.Fatal(err)
	}
	ih := loadInstalledHost(root)
	if ih == nil || ih.PackHash == "" {
		t.Fatal("install must record a pack_hash")
	}
	d := &doctorReport{ok: true}
	if d.hostPackOutdated(ih) {
		t.Fatal("a fresh install must not be reported outdated")
	}
	// Simulate the binary's embedded pack having moved on since install.
	ih.PackHash = "stale-different-hash"
	if !d.hostPackOutdated(ih) {
		t.Fatal("a pack_hash mismatch must be reported outdated")
	}
}

// TestHostPackPrePackHashNotOutdated: an install from before pack_hash existed
// (empty PackHash) must not be falsely flagged — we can't tell, so we don't cry wolf.
func TestHostPackPrePackHashNotOutdated(t *testing.T) {
	d := &doctorReport{ok: true}
	if d.hostPackOutdated(&installedHost{Host: "claude-code", PackHash: ""}) {
		t.Fatal("a pre-pack_hash install must not be reported outdated")
	}
}
