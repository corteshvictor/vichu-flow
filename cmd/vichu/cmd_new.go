package main

import (
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/safeio"
)

// cmdNew scaffolds a brand-new project in its own directory from a template:
// minimal source, a real verification gate, vichu.yaml, and .gitignore — so the
// very first `vichu run` can reach `completed` with no manual config and no Git.
func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	templateName := fs.String("template", "empty", i18n.T("new.flag_template"))
	force := fs.Bool("force", false, i18n.T("new.flag_force"))

	// Go's flag package stops at the first positional, so `vichu new my-app
	// --template go` would ignore the flag. Pull a leading name out first, so the
	// name works both before and after the flags.
	name := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, parseArgs = args[0], args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if name == "" {
		name = strings.TrimSpace(fs.Arg(0))
	}
	if err := validateProjectName(name); err != nil {
		return err
	}

	tpl, err := resolveTemplate(*templateName)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := filepath.Join(cwd, name)
	if err := prepareNewRoot(cwd, name, *force); err != nil {
		return err
	}

	written, err := writeTemplate(root, tpl, name, *force)
	if err != nil {
		return err
	}
	yaml := config.DefaultYAML(config.DefaultOptions{Detected: tpl.Detected, ProjectName: name})
	if err := confinedProjectWrite(root, config.FileName, []byte(yaml), 0o644); err != nil {
		return err
	}
	if _, err := ensureGitignore(root); err != nil {
		return err
	}

	fmt.Printf(i18n.T("new.done")+"\n\n", name, tpl.Name)
	for _, f := range written {
		fmt.Printf("  %s\n", f)
	}
	fmt.Printf("  %s\n", config.FileName)
	fmt.Printf("\n"+i18n.T("new.next")+"\n", name)
	return nil
}

// prepareNewRoot validates and creates the target directory for `vichu new`, through the
// confined parent. It refuses a target that is a SYMLINK or a plain file — even with --force:
// --force replaces files INSIDE a project, it does not authorize writing a whole project
// through a symlinked root to somewhere outside the current directory. A dangling symlink
// (Stat would say "not there") is rejected too, via Lstat.
func prepareNewRoot(cwd, name string, force bool) error {
	parent, err := safeio.Open(cwd)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if fi, lerr := parent.Lstat(name); lerr == nil {
		if fi.Mode()&iofs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s already exists and is not a real directory (a symlink or a file) — refusing to create a project through it", name)
		}
	} else if !errors.Is(lerr, iofs.ErrNotExist) {
		return lerr
	}
	if entries, _ := os.ReadDir(filepath.Join(cwd, name)); len(entries) > 0 && !force {
		return fmt.Errorf(i18n.T("new.exists"), name)
	}
	return parent.MkdirAll(name, 0o755)
}

// validateProjectName requires a single safe directory name: not empty, not a
// path (no separators), and not "."/"..". This keeps `vichu new` from creating a
// directory outside the working directory or in an unexpected place.
func validateProjectName(name string) error {
	if name == "" {
		return errors.New(i18n.T("new.need_name"))
	}
	if name == "." || name == ".." || filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf(i18n.T("new.bad_name"), name)
	}
	return nil
}
