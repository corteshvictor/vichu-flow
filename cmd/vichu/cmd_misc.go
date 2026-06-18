package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/engine"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// cmdResume is the top-level `vichu resume` — a deprecated alias for the
// host-first `vichu run resume` (reopen/validate only, does NOT execute). The
// headless loop-running resume lives at `vichu exec resume`.
func cmdResume(args []string) error {
	fmt.Fprintln(os.Stderr, i18n.T("resume.renamed"))
	return cmdRunResume(args)
}

// cmdRunResume is `vichu run resume [--run <id>]` — the HOST-FIRST resume: it
// reopens and validates the run (provider, drift) and reports state, WITHOUT
// executing any stage. The host keeps driving via the transactional commands.
func cmdRunResume(args []string) error {
	fs := flag.NewFlagSet("run resume", flag.ContinueOnError)
	run := fs.String("run", "", i18n.T("worker.flag_run"))
	accept := fs.Bool("accept-changes", false, i18n.T("resume.flag_accept"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	positionals, err := parseArgsAnyOrder(fs, args)
	if err != nil {
		return err
	}
	id := *run // --run takes precedence; a positional id still works
	if id == "" {
		id = firstArg(positionals)
	}
	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(id)
	if err != nil {
		return err
	}
	state, err := proj.engineForOutput(*jsonOut).ReopenRun(runID, engine.ResumeOptions{AcceptChanges: *accept})
	if err != nil {
		return err
	}
	if *jsonOut {
		return printStatusJSON(proj, runID)
	}
	printStateSummary(state)
	// An active run is the SUCCESS case for host-first resume (the host keeps
	// driving). Only a terminal-but-not-completed state is a non-zero exit.
	if state.Status == core.StatusActive {
		return nil
	}
	return runStatusError(state)
}

// cmdExecResume is `vichu exec resume [id]` — the HEADLESS resume: it reopens the
// run AND runs the loop to completion (the kernel drives agents via adapters). For
// CI/automation; the host-first experience uses `vichu run resume` instead.
func cmdExecResume(args []string) error {
	fs := flag.NewFlagSet("exec resume", flag.ContinueOnError)
	accept := fs.Bool("accept-changes", false, i18n.T("resume.flag_accept"))
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
	fmt.Printf(i18n.T("resume.resuming")+"\n", runID)
	state, err := proj.newEngine().Resume(context.Background(), runID, engine.ResumeOptions{AcceptChanges: *accept})
	if err != nil {
		return err
	}
	fmt.Println()
	printStateSummary(state)
	return runStatusError(state)
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
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

	state, err := proj.store.LoadState(runID)
	if err != nil {
		return err
	}
	if state.Status.Terminal() {
		fmt.Printf(i18n.T("cancel.already")+"\n", runID, state.Status)
		return nil
	}

	// Cooperative cancel: mark the run canceled on disk. An engine actively
	// owning the run observes this within a second (its cancel watcher) and
	// kills the in-flight worker or gate process.
	state.Status = core.StatusCanceled
	state.NextAction = ""
	if err := proj.store.SaveState(state); err != nil {
		return err
	}
	_ = proj.store.AppendEvent(core.Event{Run: runID, Event: core.EventRunCanceled})
	fmt.Printf(i18n.T("cancel.done")+"\n", runID)
	return nil
}

func cmdAdapters(args []string) error {
	fs := flag.NewFlagSet("adapters", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg := adapters.DefaultRegistry()
	fmt.Println(i18n.T("adapters.header"))
	for _, name := range sortedNames(reg) {
		a, err := reg.Get(name)
		if err != nil {
			fmt.Printf("  ✗ %-12s %s\n", name, err.Error())
			continue
		}
		av, _ := a.Probe(context.Background())
		caps := a.Capabilities()
		status := i18n.T("adapters.available")
		if !av.Available {
			status = av.Reason
		}
		fmt.Printf("  %-12s %-22s [stream=%v resume=%v cost=%v]\n",
			name, status, caps.Streaming, caps.Resume, caps.CostReporting)
	}
	return nil
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	proj, err := openProject()
	if err != nil {
		return err
	}
	path := filepath.Join(proj.root, config.FileName)
	fmt.Printf(i18n.T("config.header")+"\n\n", path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	os.Stdout.Write(data)
	return nil
}
