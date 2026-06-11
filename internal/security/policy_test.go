package security

import (
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/config"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		argv  []string
		class string
	}{
		{[]string{"git", "push"}, ClassGitPush},
		{[]string{"git", "push", "--force", "origin", "main"}, ClassGitPush},
		{[]string{"git", "reset", "--hard", "HEAD~1"}, ClassDestructiveShell},
		{[]string{"git", "clean", "-fd"}, ClassDestructiveShell},
		{[]string{"git", "status"}, ""},
		{[]string{"git", "reset", "HEAD~1"}, ""}, // soft reset is not destructive
		{[]string{"rm", "-rf", "/tmp/x"}, ClassDestructiveShell},
		{[]string{"rm", "-fr", "dir"}, ClassDestructiveShell},
		{[]string{"rm", "file.txt"}, ""},
		{[]string{"sudo", "anything"}, ClassDestructiveShell},
		{[]string{"npm", "install", "leftpad"}, ClassPackageInstall},
		{[]string{"pnpm", "add", "x"}, ClassPackageInstall},
		{[]string{"pip", "install", "requests"}, ClassPackageInstall},
		{[]string{"go", "install", "x"}, ClassPackageInstall},
		{[]string{"go", "test", "./..."}, ""},
		{[]string{"npm", "test"}, ""},
		{[]string{"pytest"}, ""},
		{nil, ""},
		// Package install hidden behind global options must still be caught.
		{[]string{"npm", "--prefix", ".", "install"}, ClassPackageInstall},
		{[]string{"pnpm", "--dir", ".", "add", "x"}, ClassPackageInstall},
		{[]string{"yarn", "--cwd", ".", "add", "x"}, ClassPackageInstall},
		{[]string{"go", "-C", ".", "get", "./..."}, ClassPackageInstall},
		{[]string{"pip", "--cache-dir", "/tmp", "install", "x"}, ClassPackageInstall},
		{[]string{"cargo", "+nightly", "install", "ripgrep"}, ClassPackageInstall},
		{[]string{"npm", "--prefix", ".", "run", "build"}, ""}, // global option, safe subcommand
		// Wrapped + global-option install (both bypasses combined).
		{[]string{"sh", "-c", "npm --prefix . install"}, ClassPackageInstall},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.class {
			t.Errorf("Classify(%v) = %q, want %q", c.argv, got, c.class)
		}
	}
}

func TestClassifySeesThroughShellWrappers(t *testing.T) {
	cases := []struct {
		argv  []string
		class string
	}{
		// The reviewer's bypass: dangerous command wrapped in sh -c.
		{[]string{"sh", "-c", "rm -rf build"}, ClassDestructiveShell},
		{[]string{"bash", "-c", "git push origin main"}, ClassGitPush},
		{[]string{"zsh", "-c", "sudo reboot"}, ClassDestructiveShell},
		{[]string{"sh", "-c", "npm install left-pad"}, ClassPackageInstall},
		// Compound payloads: the dangerous segment must be found.
		{[]string{"sh", "-c", "echo hi && rm -rf build"}, ClassDestructiveShell},
		{[]string{"bash", "-c", "make; git push"}, ClassGitPush},
		// Combined short-flag bundles that include the command flag.
		{[]string{"sh", "-ec", "rm -rf build"}, ClassDestructiveShell},
		{[]string{"bash", "-lc", "rm -rf build"}, ClassDestructiveShell},
		{[]string{"zsh", "-fc", "git push"}, ClassGitPush},
		{[]string{"bash", "-euxc", "sudo reboot"}, ClassDestructiveShell},
		{[]string{"sh", "-lc", "go test ./..."}, ""}, // combined flags, safe payload
		// A bundle without the command flag is not a wrapper (treated directly).
		{[]string{"bash", "-l"}, ""},
		// Nested wrappers, including combined flags.
		{[]string{"sh", "-c", "bash -c 'rm -rf build'"}, ClassDestructiveShell},
		{[]string{"sh", "-ec", "bash -lc 'git push'"}, ClassGitPush},
		// Windows wrappers.
		{[]string{"cmd", "/c", "rmdir", "/s", "/q", "build"}, ClassDestructiveShell},
		{[]string{"cmd.exe", "/c", "del", "/f", "/q", "x"}, ClassDestructiveShell},
		{[]string{"pwsh", "-Command", "Remove-Item -Recurse -Force build"}, ClassDestructiveShell},
		{[]string{"powershell", "-Command", "Remove-Item -Force x"}, ClassDestructiveShell},
		// Safe wrapped commands stay unclassified.
		{[]string{"sh", "-c", "echo hello"}, ""},
		{[]string{"sh", "-c", "pytest -k 'not slow'"}, ""},
		{[]string{"bash", "-c", "go test ./..."}, ""},
		// Git global options must not hide the real subcommand.
		{[]string{"git", "-C", ".", "clean", "-fd", "build"}, ClassDestructiveShell},
		{[]string{"git", "-C", ".", "push"}, ClassGitPush},
		{[]string{"git", "-c", "user.name=x", "reset", "--hard"}, ClassDestructiveShell},
		{[]string{"git", "--git-dir", ".git", "push"}, ClassGitPush},
		{[]string{"git", "-C", ".", "rm", "-r", "src"}, ClassDestructiveShell},
		{[]string{"git", "-C", ".", "status"}, ""}, // global option, safe subcommand
		// Inline-code interpreters = arbitrary execution.
		{[]string{"python3", "-c", "import shutil; shutil.rmtree('build')"}, ClassDestructiveShell},
		{[]string{"node", "-e", "require('fs').rmSync('x',{recursive:true})"}, ClassDestructiveShell},
		{[]string{"ruby", "-e", "FileUtils.rm_rf('x')"}, ClassDestructiveShell},
		{[]string{"perl", "-e", "unlink glob('*')"}, ClassDestructiveShell},
		{[]string{"pwsh", "-EncodedCommand", "ZQBjAGgAbwA="}, ClassDestructiveShell},
		{[]string{"pwsh", "-enc", "ZQBjAGgAbwA="}, ClassDestructiveShell},
		// Interpreters running a real script file are not inline execution.
		{[]string{"python3", "scripts/check.py"}, ""},
		{[]string{"node", "app.js"}, ""},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.class {
			t.Errorf("Classify(%v) = %q, want %q", c.argv, got, c.class)
		}
	}
}

func TestCheckCommandBlocksWrappedDanger(t *testing.T) {
	cfg := config.Default().Security // requireConfirmationFor has the defaults
	cfg.RequireConfirmationFor = []string{ClassGitPush, ClassDestructiveShell, ClassPackageInstall}
	p := New(cfg)

	wrapped := [][]string{
		{"sh", "-c", "rm -rf build"},
		{"sh", "-ec", "rm -rf build"},
		{"bash", "-lc", "rm -rf build"},
		{"bash", "-c", "git push origin main"},
		{"zsh", "-fc", "git push"},
		{"cmd", "/c", "rmdir", "/s", "/q", "build"},
	}
	for _, argv := range wrapped {
		if err := p.CheckCommand(argv); err == nil {
			t.Errorf("wrapped dangerous command must be blocked: %v", argv)
		}
	}
	// A wrapped safe command still passes.
	if err := p.CheckCommand([]string{"sh", "-c", "go test ./..."}); err != nil {
		t.Errorf("wrapped safe command should pass, got %v", err)
	}
}

func TestCheckCommand(t *testing.T) {
	cfg := config.Default().Security // sensible defaults, but requireConfirmationFor empty by default
	cfg.RequireConfirmationFor = []string{ClassGitPush, ClassDestructiveShell, ClassPackageInstall}
	p := New(cfg)

	// git push: blocked by allowGitMutations=false before confirmation even matters.
	if err := p.CheckCommand([]string{"git", "push"}); err == nil || !strings.Contains(err.Error(), "allowGitMutations") {
		t.Errorf("git push must be policy-blocked, got %v", err)
	}
	// destructive shell: requires confirmation → blocked in headless runs.
	if err := p.CheckCommand([]string{"rm", "-rf", "build"}); err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Errorf("rm -rf must require confirmation, got %v", err)
	}
	if err := p.CheckCommand([]string{"npm", "install"}); err == nil {
		t.Error("package install must require confirmation")
	}
	// Normal commands pass.
	for _, argv := range [][]string{{"go", "test", "./..."}, {"pytest"}, {"git", "status"}} {
		if err := p.CheckCommand(argv); err != nil {
			t.Errorf("CheckCommand(%v) should pass, got %v", argv, err)
		}
	}

	// With confirmations disabled and git mutations allowed, everything passes.
	open := New(config.SecurityConfig{AllowGitMutations: true})
	if err := open.CheckCommand([]string{"git", "push"}); err != nil {
		t.Errorf("explicitly allowed git push should pass, got %v", err)
	}
}

func TestClaudeDisallowedTools(t *testing.T) {
	cfg := config.SecurityConfig{
		AllowGitMutations:      false,
		RequireConfirmationFor: []string{ClassGitPush, ClassDestructiveShell, ClassPackageInstall},
	}
	rules := New(cfg).ClaudeDisallowedTools()
	joined := strings.Join(rules, ",")
	for _, want := range []string{"Bash(git push:*)", "Bash(rm -rf:*)", "Bash(sudo:*)", "Bash(npm install:*)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing rule %q in %v", want, rules)
		}
	}
	// Shells are denied broadly (any flag form), not as specific -c rules, so a
	// wrapper like `bash -lc` cannot slip past.
	for _, want := range []string{"Bash(sh:*)", "Bash(bash:*)", "Bash(zsh:*)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing broad shell rule %q in %v", want, rules)
		}
	}
	// Package-install rules are GENERATED from the classifier's own table, so
	// every global the classifier knows is covered on the Claude side too —
	// including the variants the classifier recognizes (pnpm --filter,
	// pip --cache-dir, npm -w, …), not just a hand-picked few.
	for _, want := range []string{
		"Bash(npm --prefix:*)", "Bash(npm -w:*)", "Bash(pnpm --dir:*)", "Bash(pnpm --filter:*)",
		"Bash(yarn --cwd:*)", "Bash(go -C:*)", "Bash(pip --cache-dir:*)", "Bash(go get:*)",
		"Bash(cargo +:*)", // cargo +nightly install must be covered
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing package-install rule %q in %v", want, rules)
		}
	}
	// Dual-use commands must still be runnable (no broad npm/go binary ban).
	for _, unwanted := range []string{"Bash(npm:*)", "Bash(go:*)", "Bash(pnpm:*)"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("must not broadly ban dual-use tool %q (breaks npm test / go test)", unwanted)
		}
	}
}

// TestClaudeRulesCoverClassifierGlobals enforces parity: every global flag the
// classifier uses for a package manager must produce a Claude disallow rule, so
// the two layers can never drift apart.
func TestClaudeRulesCoverClassifierGlobals(t *testing.T) {
	cfg := config.SecurityConfig{RequireConfirmationFor: []string{ClassPackageInstall}}
	rules := New(cfg).ClaudeDisallowedTools()
	joined := strings.Join(rules, ",")
	for base, pm := range pkgManagers {
		for g := range pm.globals {
			want := "Bash(" + base + " " + g + ":*)"
			if !strings.Contains(joined, want) {
				t.Errorf("classifier knows %s %s but Claude rules omit %q", base, g, want)
			}
		}
		for _, s := range pm.selectors {
			want := "Bash(" + base + " " + s + ":*)"
			if !strings.Contains(joined, want) {
				t.Errorf("classifier skips %s selector %q but Claude rules omit %q", base, s, want)
			}
		}
	}
	// No duplicate rules even though several managers share global flag names.
	seen := map[string]int{}
	for _, r := range rules {
		seen[r]++
		if seen[r] > 1 {
			t.Errorf("duplicate rule %q", r)
		}
	}
}
