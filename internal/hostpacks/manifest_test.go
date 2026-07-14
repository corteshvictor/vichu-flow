package hostpacks

import (
	"runtime"
	"testing"
)

// TestValidateDestsRejectsAnythingThatEscapes: every consumer of a manifest turns these strings
// into paths it WRITES to, and there is now more than one — the installer, and the
// release-history tool. A check that lives inside one of them is a check the next consumer
// forgets, which is exactly what happened: `packhistory` took `dest` straight to filepath.Join,
// so a manifest declaring `../../../escaped` wrote outside its directory and exited 0.
//
// The manifest is our own file, so this is a typo-class bug rather than an attack. A typo that
// silently overwrites a repo file is still not one we get to shrug at.
func TestValidateDestsRejectsAnythingThatEscapes(t *testing.T) {
	bad := map[string][]string{
		"parent traversal":  {"../escaped"},
		"deep traversal":    {"../../../../../../escaped"},
		"nested traversal":  {".claude/../../escaped"},
		"absolute unix":     {"/etc/passwd"},
		"bare dotdot":       {".."},
		"empty":             {""},
		"duplicate targets": {".claude/a.md", ".claude/a.md"},
	}
	if runtime.GOOS == "windows" {
		bad["windows volume"] = []string{`C:\Windows\System32\drivers\etc\hosts`}
		bad["unc path"] = []string{`\\server\share\x`}
	}
	for name, dests := range bad {
		if err := ValidateDests(dests); err == nil {
			t.Errorf("%s: %v must be rejected — a manifest destination is a path we WRITE to", name, dests)
		}
	}

	good := []string{
		".claude/skills/vichu-orchestrator/SKILL.md",
		".claude/agents/vichu-worker.md",
		".claude/commands/vichu.md",
	}
	if err := ValidateDests(good); err != nil {
		t.Fatalf("a normal pack manifest must validate: %v", err)
	}
}
