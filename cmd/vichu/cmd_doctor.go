package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf(i18n.T("doctor.header")+"\n\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

	ok := true
	check := func(label string, pass bool, detail string) {
		mark := "✓"
		if !pass {
			mark = "✗"
			ok = false
		}
		fmt.Printf("  %s %-22s %s\n", mark, label, detail)
	}
	// warn surfaces an advisory (e.g. an unbounded budget) without failing doctor.
	warn := func(label, detail string) {
		fmt.Printf("  ! %-22s %s\n", label, detail)
	}

	// Git is the hard requirement.
	gitOK := workspace.GitAvailable()
	check("git", gitOK, gitDetail(gitOK))

	// Project config + repo.
	root, rootErr := findRoot()
	if rootErr != nil {
		check("vichu.yaml", false, i18n.T("doctor.no_config"))
	} else {
		check("vichu.yaml", true, filepath.Join(root, config.FileName))
		if _, err := workspace.Detect(root); err != nil {
			check("git repository", false, err.Error())
		} else {
			check("git repository", true, root)
		}
		// Nudge older configs (pre-v0.2.1) whose token budget is still unlimited.
		if cfg, err := config.Load(filepath.Join(root, config.FileName)); err == nil && cfg.Budgets.Run.MaxTotalTokens == 0 {
			warn("token budget", i18n.T("doctor.tokens_unlimited"))
		}
	}

	// Adapters.
	fmt.Println("\n  " + i18n.T("doctor.adapters"))
	reg := adapters.DefaultRegistry()
	for _, name := range sortedNames(reg) {
		a, err := reg.Get(name)
		if err != nil {
			fmt.Printf("    ✗ %-12s %s\n", name, err.Error())
			continue
		}
		av, _ := a.Probe(context.Background())
		mark := "✓"
		detail := av.Version
		if !av.Available {
			mark = "—"
			detail = av.Reason
		}
		fmt.Printf("    %s %-12s %s\n", mark, name, detail)
	}

	fmt.Println()
	if ok {
		fmt.Println(i18n.T("doctor.all_ok"))
	} else {
		fmt.Println(i18n.T("doctor.failures"))
	}
	return nil
}

func gitDetail(ok bool) string {
	if ok {
		return i18n.T("doctor.git_ok")
	}
	return i18n.T("doctor.git_missing")
}

func sortedNames(reg *adapters.Registry) []string {
	names := reg.Names()
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}
