package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// gitignoreFile is the project's ignore file that `init`/`new` append the runtime dir to.
const gitignoreFile = ".gitignore"

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, i18n.T("init.flag_force"))
	provider := fs.String("provider", config.WorkspaceAuto, i18n.T("init.flag_provider"))
	templateName := fs.String("template", "", i18n.T("init.flag_template"))
	host := fs.String("host", "", i18n.T("init.flag_host"))
	dryRun := fs.Bool("dry-run", false, i18n.T("init.flag_dry_run"))
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	prov, err := openWorkspaceForInit(cwd, *provider)
	if err != nil {
		return err
	}
	root := prov.Root()
	cfgPath := filepath.Join(root, config.FileName)

	// --host on an already-initialized project, or a fresh --host --dry-run, only
	// touches the pack (never the config) — handled and returned by the helper.
	if done, err := maybeHostOnly(root, cfgPath, *host, *force, *dryRun); done {
		return err
	}
	if config.Exists(cfgPath) && !*force {
		return fmt.Errorf(i18n.T("init.exists"), config.FileName)
	}

	projectName := filepath.Base(root)
	detected, seeded, err := seedOrDetect(root, projectName, *templateName, *force)
	if err != nil {
		return err
	}
	// .gitignore BEFORE vichu.yaml: vichu.yaml is the COMMIT point. If .gitignore cannot be
	// written (e.g. it is a hostile symlink and the confined write refuses it), we fail with
	// nothing committed — so a retry is not trapped by a half-written vichu.yaml reporting
	// "already exists". Always ignore the runtime dir even with no .git yet: it holds prompts,
	// results, and the filesystem baseline; a later `git init` must not commit it.
	gitignoreAdded, err := ensureGitignore(root)
	if err != nil {
		return err
	}
	yaml := config.DefaultYAML(config.DefaultOptions{Detected: detected, ProjectName: projectName, WorkspaceProvider: *provider})
	if err := confinedProjectWrite(root, config.FileName, []byte(yaml), 0o644); err != nil {
		return err
	}
	printInitSummary(root, detected, seeded, gitignoreAdded)
	if *host != "" {
		fmt.Println()
		return installHostAndReport(root, *host, *force, *dryRun)
	}
	return nil
}

// maybeHostOnly handles the two `--host` paths that do NOT run a full init:
// adding the pack to an already-initialized project (keep vichu.yaml, ensure
// .vichu/ gitignored), and a fresh `--host --dry-run` (preview only, write
// nothing). It returns done=true when it handled the command.
func maybeHostOnly(root, cfgPath, host string, force, dryRun bool) (done bool, err error) {
	if host == "" {
		return false, nil
	}
	if config.Exists(cfgPath) {
		if !dryRun {
			if _, gerr := ensureGitignore(root); gerr != nil {
				return true, gerr
			}
		}
		return true, installHostAndReport(root, host, force, dryRun)
	}
	if dryRun {
		return true, installHostAndReport(root, host, force, true)
	}
	return false, nil // fresh install: fall through to normal init + pack at the end
}

// openWorkspaceForInit resolves the workspace provider, translating the git-only
// failures into actionable messages. Git is recommended but not required: auto
// and filesystem run in any folder, so an agent's work is still snapshotted and
// reversible without a VCS. Only --provider git hard-requires a repo.
func openWorkspaceForInit(cwd, provider string) (workspace.Provider, error) {
	prov, err := workspace.Open(cwd, provider)
	if err != nil {
		switch {
		case errors.Is(err, workspace.ErrNoGit):
			return nil, errors.New(i18n.T("init.no_git"))
		case errors.Is(err, workspace.ErrNotRepo):
			return nil, errors.New(i18n.T("init.not_repo"))
		}
		return nil, fmt.Errorf("%w", err)
	}
	return prov, nil
}

// seedOrDetect returns the stack config for the new vichu.yaml: with --template
// it seeds a ready-to-run project and uses that template's config; otherwise it
// detects the existing stack in place. The returned slice is the seeded files.
func seedOrDetect(root, projectName, templateName string, force bool) (config.Detected, []string, error) {
	if templateName == "" {
		return config.Detect(root), nil, nil
	}
	tpl, err := resolveTemplate(templateName)
	if err != nil {
		return config.Detected{}, nil, err
	}
	seeded, err := writeTemplate(root, tpl, projectName, force)
	if err != nil {
		return config.Detected{}, nil, err
	}
	return tpl.Detected, seeded, nil
}

// printInitSummary reports what `vichu init` wrote.
func printInitSummary(root string, detected config.Detected, seeded []string, gitignoreAdded bool) {
	row := func(label, value string) { fmt.Printf("  %-10s %s\n", label, value) }
	fmt.Printf(i18n.T("init.done")+"\n\n", root)
	row(i18n.T("init.language")+":", orUnknown(detected.Language))
	if detected.TestCmd != "" {
		row(i18n.T("init.test")+":", detected.TestCmd)
	}
	if detected.LintCmd != "" {
		row(i18n.T("init.lint")+":", detected.LintCmd)
	}
	fmt.Println()
	row(i18n.T("init.wrote"), config.FileName)
	for _, f := range seeded {
		row(i18n.T("init.wrote"), f)
	}
	if gitignoreAdded {
		fmt.Printf("  "+i18n.T("init.updated_gi")+"\n", runtime.DirName)
	}
	fmt.Println("\n" + i18n.T("init.next"))
}

// confinedProjectWrite writes rel under root atomically and confined, so a symlink an
// attacker left in the project (a cloned repo, a hostile checkout) cannot redirect an init
// or scaffold write to a file outside it — the rename replaces the symlink rather than
// following it. Init runs before any agent, but a project's existing contents are still
// untrusted input.
func confinedProjectWrite(root, rel string, data []byte, mode os.FileMode) error {
	pr, err := openProjectRoot(root)
	if err != nil {
		return err
	}
	defer pr.Close()
	return pr.writeFileAtomic(rel, data, mode)
}

// ensureGitignore makes sure the runtime directory is ignored. Runs may contain
// code fragments and prompts and must never be committed.
func ensureGitignore(root string) (bool, error) {
	entry := runtime.DirName + "/"

	pr, err := openProjectRoot(root)
	if err != nil {
		return false, err
	}
	defer pr.Close()

	// Lstat FIRST and refuse a symlink. writeFileAtomic replaces the destination with a regular
	// file, so a `.gitignore` the user symlinked to a shared ignore file would be silently turned
	// into a plain local file — breaking the sharing, and leaving the shared target stale — while
	// os.Root would still FOLLOW an internal link on the read. And keep the file's existing mode:
	// appending one line must not widen a 0600 .gitignore to 0644.
	mode := fs.FileMode(0o644)
	switch info, lerr := pr.lstat(gitignoreFile); {
	case errors.Is(lerr, fs.ErrNotExist):
		// no file yet — a fresh 0644 is correct
	case lerr != nil:
		return false, lerr
	case info.Mode()&fs.ModeSymlink != 0:
		return false, fmt.Errorf("%s is a symlink — refusing to append through it, because saving would replace the link with a regular file and break your shared config. Edit its target directly, then re-run", gitignoreFile)
	default:
		mode = info.Mode().Perm()
	}

	// Read WITHOUT following (the Lstat above already refused a symlink; this closes the TOCTOU).
	// A missing one is an empty baseline.
	data, err := pr.readFileNoFollow(gitignoreFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry || strings.TrimSpace(line) == runtime.DirName {
			return false, nil
		}
	}

	// Read-modify-write ATOMICALLY (not append-in-place): writeFileAtomic replaces a symlink
	// at the path rather than appending through it to whatever it points at.
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	next := string(data) + prefix + "\n# VichuFlow runtime (contains code fragments and prompts)\n" + entry + "\n"
	if err := pr.writeFileAtomic(gitignoreFile, []byte(next), mode); err != nil {
		return false, err
	}
	return true, nil
}

func orUnknown(s string) string {
	if s == "" {
		return i18n.T("common.unknown_val")
	}
	return s
}
