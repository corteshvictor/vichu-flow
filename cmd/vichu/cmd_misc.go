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

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	accept := fs.Bool("accept-changes", false, i18n.T("resume.flag_accept"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(fs.Arg(0))
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
	return nil
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	proj, err := openProject()
	if err != nil {
		return err
	}
	runID, err := proj.resolveRunID(fs.Arg(0))
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
