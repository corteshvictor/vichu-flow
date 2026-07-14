package workspace

import "testing"

// TestHostLocalStateIsAnExactAllowlist guards the narrowest, most dangerous thing in
// this package: the list of files the mutation audit is allowed to NOT see.
//
// This used to be the pattern `*.local.*` under the host's config dir. That was a
// hole. A coding host loads its subagents with a glob (`.claude/agents/*.md`), so an
// agent could write `.claude/agents/backdoor.local.md` — a file the host would load as
// a subagent on the next session and that mutations.json would never report. An agent
// could install its own future instructions, invisibly, in a product whose entire
// promise is "I audit every mutation".
//
// So: exactly two paths are excluded, and nothing else. Every entry here is a blind
// spot, and a blind spot has to be named and justified one file at a time.
func TestHostLocalStateIsAnExactAllowlist(t *testing.T) {
	excluded := []string{
		".claude/settings.local.json", // the allowlist the host rewrites on every approval
		".agents/settings.local.json", // .claude is often a symlink to .agents
	}
	audited := []string{
		".claude/agents/backdoor.local.md",   // would be LOADED as a subagent
		".claude/skills/evil/SKILL.local.md", // ditto, as a skill
		".claude/commands/sneaky.local.md",   // ditto, as a slash command
		".claude/settings.json",              // the SHARED settings — the user's
		".claude/vichu-host.json",            // our own install record
		".agents/tool.local.yaml",            // not the file we excluded
		"src/config.local.json",              // `.local.` outside a host dir
		"settings.local.json",                // at the repo root, not under .claude/
		".claude/nested/settings.local.json", // right name, wrong place
		"src/main.go",                        // ordinary code
	}

	for _, p := range excluded {
		if !IsHostBookkeeping(p) && !isRuntimePath(p) {
			t.Errorf("%s is the host's own bookkeeping and must not be a worker mutation", p)
		}
	}
	for _, p := range audited {
		if IsHostBookkeeping(p) || isRuntimePath(p) {
			t.Errorf("%s MUST be audited — excluding it lets an agent change what the next agent runs, invisibly", p)
		}
	}
}

// TestHostConfigIsSensitive: a worker that writes into the host's config is not
// changing your code, it is changing what the agent that runs NEXT is allowed and
// instructed to do. That is the highest-leverage write in the tree, so every touch is
// surfaced — including the standing instruction files every future agent reads.
func TestHostConfigIsSensitive(t *testing.T) {
	sensitive := []string{
		".claude/agents/vichu-reviewer.md",
		".claude/settings.json",
		".agents/skills/x/SKILL.md",
		"CLAUDE.md",
		"AGENTS.md",
		"docs/AGENTS.md",
	}
	for _, p := range sensitive {
		if !IsSensitive(p) {
			t.Errorf("%s must be flagged sensitive: it steers every future agent, not just this run", p)
		}
	}
	if IsSensitive("src/main.go") {
		t.Error("ordinary source files must not be flagged sensitive — the signal would be worthless")
	}
}
