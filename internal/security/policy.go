// Package security is the central policy engine (PLAN.md §9): one source of
// policy evaluated everywhere a command is about to run — gates, shell workers,
// and the permission configuration generated for real agents. Dangerous actions
// are stopped BEFORE execution; in a headless runtime, "requires confirmation"
// means the run blocks awaiting a human.
package security

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/shellwords"
)

// Command classes referenced by security.requireConfirmationFor.
const (
	ClassGitPush          = "git_push"
	ClassDestructiveShell = "destructive_shell"
	ClassPackageInstall   = "package_install"
)

// Policy evaluates commands against the project's security configuration.
type Policy struct {
	cfg config.SecurityConfig
}

// New builds a Policy from config.
func New(cfg config.SecurityConfig) Policy {
	return Policy{cfg: cfg}
}

// Classify maps an argv to a command class, or "" for unclassified (allowed)
// commands. It sees through shell wrappers (`sh -c '...'`, `cmd /c ...`,
// `pwsh -Command ...`), classifying the most dangerous command inside, so
// wrapping does not defeat the policy. Classification is conservative: it
// matches well-known dangerous shapes, not every possible disguise — mutation
// tracking and gates remain the backstop after execution.
func Classify(argv []string) string {
	return classify(argv, 0)
}

func classify(argv []string, depth int) string {
	if len(argv) == 0 || depth > 8 {
		return ""
	}
	// A shell wrapper carries its real command in a payload string; classify
	// each command inside it (recursively, to catch nested wrappers).
	if payload, ok := shellWrapperPayload(argv); ok {
		for _, seg := range shellwords.SplitScript(payload) {
			if c := classify(seg, depth+1); c != "" {
				return c
			}
		}
		return ""
	}
	return classifyDirect(argv)
}

// classifyDirect classifies a non-wrapper argv by its executable and arguments,
// trying each category of dangerous command in turn. Splitting it this way keeps
// each classifier small and independently testable.
func classifyDirect(argv []string) string {
	base := strings.ToLower(filepath.Base(argv[0]))
	for _, classify := range []func(base string, argv []string) string{
		classifyGit, classifyInterpreter, classifyDestructiveShell, classifyPackageInstall,
	} {
		if c := classify(base, argv); c != "" {
			return c
		}
	}
	return ""
}

// classifyGit classifies git invocations, skipping global options (-C <path>,
// -c <k=v>, --git-dir <path>, …) so `git -C . clean` is judged on `clean`.
func classifyGit(base string, argv []string) string {
	if base != "git" {
		return ""
	}
	sub, args := gitSubcommand(argv[1:])
	switch sub {
	case "push":
		return ClassGitPush
	case "clean", "rm":
		return ClassDestructiveShell
	case "reset":
		if hasArg(args, "--hard") {
			return ClassDestructiveShell
		}
	case "checkout", "restore":
		if hasArg(args, "--") || hasArg(args, ".") {
			return ClassDestructiveShell // discards working-tree changes
		}
	}
	return ""
}

// inlineCodeInterpreters maps an interpreter to the flags that make it run
// inline code (arbitrary execution the policy cannot introspect).
var inlineCodeInterpreters = map[string][]string{
	"python":  {"-c"},
	"python3": {"-c"},
	"python2": {"-c"},
	"node":    {"-e", "--eval", "-p", "--print"},
	"nodejs":  {"-e", "--eval", "-p", "--print"},
	"deno":    {"-e", "--eval", "-p", "--print"},
	"ruby":    {"-e"},
	"perl":    {"-e", "-E"},
	"php":     {"-r"},
}

var powershellNames = map[string]struct{}{
	"powershell": {}, "powershell.exe": {}, "pwsh": {}, "pwsh.exe": {},
}

// classifyInterpreter flags interpreters asked to run inline code.
func classifyInterpreter(base string, argv []string) string {
	if flags, ok := inlineCodeInterpreters[base]; ok && hasAnyArg(argv, flags...) {
		return ClassDestructiveShell
	}
	if _, ok := powershellNames[base]; ok && hasEncodedCommand(argv) {
		return ClassDestructiveShell
	}
	return ""
}

// hasEncodedCommand reports a PowerShell -EncodedCommand (or its prefixes -e,
// -enc, …), whose opaque base64 payload cannot be inspected.
func hasEncodedCommand(argv []string) bool {
	for _, a := range argv[1:] {
		la := strings.ToLower(a)
		if len(la) >= 2 && strings.HasPrefix("-encodedcommand", la) {
			return true
		}
	}
	return false
}

// classifyDestructiveShell flags commands that delete files or escalate.
func classifyDestructiveShell(base string, argv []string) string {
	switch base {
	case "sudo", "doas", "dd", "mkfs", "shred", "format":
		return ClassDestructiveShell
	case "rm":
		if hasRecursiveOrForce(argv) {
			return ClassDestructiveShell
		}
	case "rmdir", "rd":
		if hasWinFlag(argv, "s") {
			return ClassDestructiveShell
		}
	case "del", "erase":
		if hasWinFlag(argv, "s") || hasWinFlag(argv, "q") || hasWinFlag(argv, "f") {
			return ClassDestructiveShell
		}
	case "remove-item", "ri": // PowerShell
		if hasPwshFlag(argv, "recurse") || hasPwshFlag(argv, "force") {
			return ClassDestructiveShell
		}
	}
	return ""
}

// pkgManager describes how to recognize a package manager's install commands:
// the install/add verbs, the global options that take a value (so
// `npm --prefix . install` resolves to `install`, not `.`), and any subcommand
// selectors that may precede the verb (e.g. cargo's `+toolchain`).
type pkgManager struct {
	verbs     map[string]struct{}
	globals   map[string]struct{}
	selectors []string
}

var pkgManagers = map[string]pkgManager{
	"npm":     {verbs: setOf("install", "add", "i"), globals: setOf("--prefix", "-C", "--cache", "--registry", "--userconfig", "--globalconfig", "-w", "--workspace")},
	"pnpm":    {verbs: setOf("install", "add", "i"), globals: setOf("--dir", "-C", "--prefix", "--filter", "--store-dir", "--workspace-root")},
	"yarn":    {verbs: setOf("install", "add"), globals: setOf("--cwd")},
	"bun":     {verbs: setOf("install", "add", "i"), globals: setOf("--cwd")},
	"pip":     {verbs: setOf("install"), globals: setOf("--log", "--proxy", "--cache-dir", "--timeout", "--retries")},
	"pip3":    {verbs: setOf("install"), globals: setOf("--log", "--proxy", "--cache-dir", "--timeout", "--retries")},
	"pipx":    {verbs: setOf("install"), globals: setOf("--cache-dir")},
	"uv":      {verbs: setOf("add", "install"), globals: setOf("--directory", "--project", "--cache-dir", "--config-file")},
	"go":      {verbs: setOf("install", "get"), globals: setOf("-C")},
	"cargo":   {verbs: setOf("install"), globals: setOf("--manifest-path", "--config", "-Z"), selectors: []string{"+"}},
	"brew":    {verbs: setOf("install", "add"), globals: setOf("--cache")},
	"apt":     {verbs: setOf("install")},
	"apt-get": {verbs: setOf("install")},
	"dnf":     {verbs: setOf("install")},
	"yum":     {verbs: setOf("install")},
	"pacman":  {verbs: setOf("-s")},
	"choco":   {verbs: setOf("install", "add")},
	"scoop":   {verbs: setOf("install", "add")},
	"winget":  {verbs: setOf("install", "add")},
}

// classifyPackageInstall flags package-manager install/add commands, resolving
// the subcommand past any global options so flagged forms cannot slip through.
func classifyPackageInstall(base string, argv []string) string {
	pm, ok := pkgManagers[base]
	if !ok {
		return ""
	}
	sub, _ := subcommandAfterGlobals(argv[1:], pm.globals)
	if _, dangerous := pm.verbs[sub]; dangerous {
		return ClassPackageInstall
	}
	return ""
}

var posixShells = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ash": {}, "ksh": {},
}

// shellWrapperPayload returns the script a shell wrapper would execute and true
// when argv is such a wrapper (sh/bash/zsh/dash/ksh -c, cmd /c|/k,
// powershell/pwsh -Command|-c).
func shellWrapperPayload(argv []string) (string, bool) {
	base := strings.ToLower(filepath.Base(argv[0]))
	rest := argv[1:]
	switch {
	case isInSet(base, posixShells):
		// POSIX shells accept the command option combined with other flags:
		// `-c`, `-ec`, `-lc`, `-euxc`, … all take the script as the next word.
		return findValueArg(rest, isPosixCommandFlag)
	case base == "cmd" || base == "cmd.exe":
		return joinAfterFlag(rest, "/c", "/k")
	case isInSet(base, powershellNames):
		return joinAfterFlag(rest, "-c", "-command")
	}
	return "", false
}

func isInSet(s string, set map[string]struct{}) bool {
	_, ok := set[s]
	return ok
}

// findValueArg returns the argument following the first one matching pred.
func findValueArg(args []string, pred func(string) bool) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if pred(args[i]) {
			return args[i+1], true
		}
	}
	return "", false
}

// joinAfterFlag returns everything after the first occurrence of any flag
// (case-insensitive), joined by spaces.
func joinAfterFlag(args []string, flags ...string) (string, bool) {
	for i, a := range args {
		la := strings.ToLower(a)
		for _, f := range flags {
			if la == f {
				return strings.Join(args[i+1:], " "), true
			}
		}
	}
	return "", false
}

// CheckCommand decides whether an argv may run. nil means allowed; a non-nil
// error carries the actionable reason the run blocks with.
func (p Policy) CheckCommand(argv []string) error {
	class := Classify(argv)
	if class == "" {
		return nil
	}
	if class == ClassGitPush && !p.cfg.AllowGitMutations {
		return fmt.Errorf("policy: `%s` is blocked (security.allowGitMutations: false)", strings.Join(argv, " "))
	}
	for _, c := range p.cfg.RequireConfirmationFor {
		if c == class {
			return fmt.Errorf("policy: `%s` requires human confirmation (security.requireConfirmationFor: %s)", strings.Join(argv, " "), class)
		}
	}
	return nil
}

// ClaudeDisallowedTools translates the policy into Claude Code tool-permission
// rules, so the same policy that guards vichu's own executions also constrains
// what a claude-code worker may do.
func (p Policy) ClaudeDisallowedTools() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(rules ...string) {
		for _, r := range rules {
			if _, ok := seen[r]; ok {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}

	if !p.cfg.AllowGitMutations {
		add("Bash(git push:*)")
	}
	for _, c := range p.cfg.RequireConfirmationFor {
		switch c {
		case ClassGitPush:
			add("Bash(git push:*)")
		case ClassDestructiveShell:
			add("Bash(rm -rf:*)", "Bash(rm -fr:*)", "Bash(sudo:*)", "Bash(git reset --hard:*)", "Bash(git clean:*)", "Bash(dd:*)", "Bash(mkfs:*)")
			// A shell wrapper can smuggle any destructive command under any flag
			// combination (-c, -lc, -ec, …), so deny the shell executables
			// themselves — not specific flag forms — when destructive shell
			// needs confirmation.
			add("Bash(sh:*)", "Bash(bash:*)", "Bash(zsh:*)", "Bash(dash:*)", "Bash(ksh:*)")
		case ClassPackageInstall:
			// Generated from the SAME pkgManagers table the classifier uses, so
			// the two layers cannot desync: one rule per install verb, and one
			// per global flag (a global like `--prefix`/`-C`/`--filter` can carry
			// an install, so it is denied too). This deliberately over-blocks a
			// few dual-use `<pm> <global> <verb>` forms but never the plain
			// `npm test` / `go test` commands (no broad binary ban). Claude's
			// prefix-based rules can't glob the subcommand after a global flag;
			// vichu's own CheckCommand is the authoritative, fully-parsed layer.
			add(packageInstallClaudeRules()...)
		}
	}
	return out
}

// packageInstallClaudeRules derives Claude --disallowedTools rules from the
// pkgManagers table (sorted for deterministic output): one rule per install
// verb and per global flag of every package manager.
func packageInstallClaudeRules() []string {
	var rules []string
	for base, pm := range pkgManagers {
		for v := range pm.verbs {
			rules = append(rules, fmt.Sprintf("Bash(%s %s:*)", base, v))
		}
		for g := range pm.globals {
			rules = append(rules, fmt.Sprintf("Bash(%s %s:*)", base, g))
		}
		// A selector (e.g. `cargo +nightly`) precedes the verb, so the plain
		// `<base> <verb>` rule misses it — deny the selector prefix too.
		for _, s := range pm.selectors {
			rules = append(rules, fmt.Sprintf("Bash(%s %s:*)", base, s))
		}
	}
	sort.Strings(rules)
	return rules
}

func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func hasAnyArg(argv []string, wants ...string) bool {
	for _, w := range wants {
		if hasArg(argv[1:], w) {
			return true
		}
	}
	return false
}

// gitGlobalsWithValue are git global options that consume the following token.
var gitGlobalsWithValue = map[string]struct{}{
	"-C": {}, "-c": {}, "--git-dir": {}, "--work-tree": {},
	"--namespace": {}, "--super-prefix": {}, "--config-env": {}, "--exec-path": {},
}

// gitSubcommand finds git's actual subcommand, skipping global options (and
// their values) that precede it. So `git -C . clean -fd build` →
// ("clean", ["-fd","build"]).
func gitSubcommand(args []string) (string, []string) {
	return subcommandAfterGlobals(args, gitGlobalsWithValue)
}

// subcommandAfterGlobals returns the first non-flag token (the subcommand) and
// the arguments after it, skipping leading global options. Options listed in
// valued also consume the following token (e.g. `-C <path>`); `--opt=value`
// forms and `+toolchain` selectors (cargo) are skipped as single tokens. This
// is what prevents `npm --prefix . install` from reading `.` as the subcommand.
func subcommandAfterGlobals(args []string, valued map[string]struct{}) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			continue
		}
		if a[0] != '-' && a[0] != '+' {
			return strings.ToLower(a), args[i+1:]
		}
		if _, ok := valued[a]; ok {
			i++ // also skip the option's value
		}
		// else: value-less global, --opt=value form, or +toolchain — skip alone.
	}
	return "", nil
}

// setOf builds a set from the given keys.
func setOf(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// hasRecursiveOrForce detects rm flags that make deletion destructive
// (recursive and/or force, in any combined form like -rf, -fr, -r -f).
func hasRecursiveOrForce(argv []string) bool {
	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			if a == "--recursive" || a == "--force" {
				return true
			}
			continue
		}
		if strings.ContainsAny(a[1:], "rRf") {
			return true
		}
	}
	return false
}

// isPosixCommandFlag reports whether a shell argument is a single-dash option
// bundle that includes the command flag `c` (e.g. -c, -ec, -lc, -euxc), whose
// next word is the script to run. Conservative on purpose: any short-flag
// bundle containing 'c' is treated as a command wrapper.
func isPosixCommandFlag(a string) bool {
	if len(a) < 2 || a[0] != '-' || strings.HasPrefix(a, "--") {
		return false
	}
	return strings.Contains(a[1:], "c")
}

// hasWinFlag detects a Windows-style /flag (e.g. /s, /q), case-insensitive.
func hasWinFlag(argv []string, flag string) bool {
	want := "/" + strings.ToLower(flag)
	for _, a := range argv[1:] {
		if strings.ToLower(a) == want {
			return true
		}
	}
	return false
}

// hasPwshFlag detects a PowerShell -Flag (e.g. -Recurse), allowing the common
// abbreviated forms, case-insensitive.
func hasPwshFlag(argv []string, flag string) bool {
	flag = strings.ToLower(flag)
	for _, a := range argv[1:] {
		a = strings.ToLower(strings.TrimPrefix(a, "-"))
		if a != "" && strings.HasPrefix(flag, a) {
			return true
		}
	}
	return false
}
