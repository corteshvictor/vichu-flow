package main

import (
	"flag"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// cmdObserve is read-only observability over a run: a summary plus the recent
// event tail. It never takes the lock or writes state — safe to run against a
// live run. (Rich TUI/web observability lands in a later milestone; this is the
// basic view the host pack and the user rely on now.)
func cmdObserve(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	positionals, err := parseArgsAnyOrder(fs, args)
	if err != nil {
		return err
	}
	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(firstArg(positionals))
	if err != nil {
		return err
	}
	if *jsonOut {
		return printStatusJSON(proj, runID)
	}
	return printStatus(proj, runID)
}
