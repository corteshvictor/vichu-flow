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
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Git is a hard requirement: agents writing code without version control
	// have no undo.
	repo, err := workspace.Detect(cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNoGit) {
			return errors.New(i18n.T("init.no_git"))
		}
		return fmt.Errorf("%w", err)
	}
	root := repo.Root()
	cfgPath := filepath.Join(root, config.FileName)

	if config.Exists(cfgPath) && !*force {
		return fmt.Errorf(i18n.T("init.exists"), config.FileName)
	}

	detected := config.Detect(root)
	projectName := filepath.Base(root)
	if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML(detected, projectName)), 0o644); err != nil {
		return err
	}

	gitignoreAdded, err := ensureGitignore(root)
	if err != nil {
		return err
	}

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
	if gitignoreAdded {
		fmt.Printf("  "+i18n.T("init.updated_gi")+"\n", runtime.DirName)
	}
	fmt.Println("\n" + i18n.T("init.next"))
	return nil
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
