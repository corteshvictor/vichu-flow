package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, i18n.T("init.flag_force"))
	provider := fs.String("provider", config.WorkspaceAuto, i18n.T("init.flag_provider"))
	templateName := fs.String("template", "", i18n.T("init.flag_template"))
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
	if config.Exists(cfgPath) && !*force {
		return fmt.Errorf(i18n.T("init.exists"), config.FileName)
	}

	projectName := filepath.Base(root)
	detected, seeded, err := seedOrDetect(root, projectName, *templateName, *force)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML(detected, projectName)), 0o644); err != nil {
		return err
	}
	// Always ignore the runtime dir, even with no .git yet: it holds prompts,
	// results, and the filesystem provider's full baseline copy of the tree. A
	// later `git init` would otherwise risk committing all of it. Writing a
	// .gitignore does not force Git on anyone.
	gitignoreAdded, err := ensureGitignore(root)
	if err != nil {
		return err
	}
	printInitSummary(root, detected, seeded, gitignoreAdded)
	return nil
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

// ensureGitignore makes sure the runtime directory is ignored. Runs may contain
// code fragments and prompts and must never be committed.
func ensureGitignore(root string) (bool, error) {
	entry := runtime.DirName + "/"
	path := filepath.Join(root, ".gitignore")

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry || strings.TrimSpace(line) == runtime.DirName {
			return false, nil
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + "\n# VichuFlow runtime (contains code fragments and prompts)\n" + entry + "\n"); err != nil {
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
