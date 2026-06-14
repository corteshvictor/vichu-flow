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

	d := &doctorReport{ok: true}
	d.gitCheck()
	d.projectChecks()
	printAdapters()

	fmt.Println()
	if d.ok {
		fmt.Println(i18n.T("doctor.all_ok"))
	} else {
		fmt.Println(i18n.T("doctor.failures"))
	}
	return nil
}

// doctorReport tracks whether all required checks passed and renders each line.
type doctorReport struct{ ok bool }

// check renders a pass/fail line and records a failure on the report.
func (d *doctorReport) check(label string, pass bool, detail string) {
	mark := "✓"
	if !pass {
		mark = "✗"
		d.ok = false
	}
	fmt.Printf("  %s %-22s %s\n", mark, label, detail)
}

// warn renders an advisory (e.g. an unbounded budget) without failing doctor.
func (d *doctorReport) warn(label, detail string) {
	fmt.Printf("  ! %-22s %s\n", label, detail)
}

// gitCheck reports git as a recommendation, not a requirement: the filesystem
// provider gives the same undo guarantees without a VCS, so a missing git is an
// advisory, not a failure.
func (d *doctorReport) gitCheck() {
	if workspace.GitAvailable() {
		d.check("git", true, gitDetail(true))
		return
	}
	d.warn("git", i18n.T("doctor.git_missing"))
}

// projectChecks validates the project config and resolves its workspace provider.
func (d *doctorReport) projectChecks() {
	root, err := findRoot()
	if err != nil {
		d.check("vichu.yaml", false, i18n.T("doctor.no_config"))
		return
	}
	d.check("vichu.yaml", true, filepath.Join(root, config.FileName))

	cfg, cfgErr := config.Load(filepath.Join(root, config.FileName))
	mode := config.WorkspaceAuto
	if cfgErr == nil && cfg.Workspace.Provider != "" {
		mode = cfg.Workspace.Provider
	}
	if prov, err := workspace.Open(root, mode); err != nil {
		d.check("workspace", false, err.Error())
	} else {
		d.check("workspace", true, fmt.Sprintf("%s (%s)", prov.Kind(), prov.Root()))
	}
	// Nudge older configs (pre-v0.2.1) whose token budget is still unlimited.
	if cfgErr == nil && cfg.Budgets.Run.MaxTotalTokens == 0 {
		d.warn("token budget", i18n.T("doctor.tokens_unlimited"))
	}
}

// printAdapters probes each registered adapter and reports its availability.
func printAdapters() {
	fmt.Println("\n  " + i18n.T("doctor.adapters"))
	reg := adapters.DefaultRegistry()
	for _, name := range sortedNames(reg) {
		a, err := reg.Get(name)
		if err != nil {
			fmt.Printf("    ✗ %-12s %s\n", name, err.Error())
			continue
		}
		av, _ := a.Probe(context.Background())
		mark, detail := "✓", av.Version
		if !av.Available {
			mark, detail = "—", av.Reason
		}
		fmt.Printf("    %s %-12s %s\n", mark, name, detail)
	}
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
