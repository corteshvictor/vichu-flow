package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	workflow := fs.String("workflow", "", i18n.T("run.flag_workflow"))
	// Note: this is the workflow provider label, not the workspace provider — the
	// latter is project-level config (workspace.provider / `vichu init --provider`).
	workflowProvider := fs.String("workflow-provider", "", i18n.T("run.flag_workflow_provider"))
	// Deprecated alias kept so v0.2 automation that passed --provider doesn't break
	// with "flag provided but not defined". It still sets the workflow label.
	deprecatedProvider := fs.String("provider", "", i18n.T("run.flag_provider_deprecated"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return errors.New(i18n.T("run.need_task"))
	}

	proj, err := openProject()
	if err != nil {
		if config.IsNotFound(err) {
			return errors.New(i18n.T("run.no_config"))
		}
		return err
	}
	wfProvider := *workflowProvider
	if *deprecatedProvider != "" {
		fmt.Fprintln(os.Stderr, i18n.T("run.provider_renamed"))
		if wfProvider == "" {
			wfProvider = *deprecatedProvider
		}
	}
	if wfProvider != "" {
		proj.cfg.Workflow.Provider = wfProvider
	}

	fmt.Printf(i18n.T("run.running")+"\n", workflowName(proj, *workflow), task)
	state, err := proj.newEngine().Start(context.Background(), task, *workflow)
	if err != nil {
		return err
	}

	fmt.Println()
	printStateSummary(state)
	fmt.Printf("\n"+i18n.T("run.observe")+"\n", state.RunID)
	return runStatusError(state)
}

// runStatusError turns a non-success terminal run state into a non-zero exit, so
// automation never reads a blocked/failed/canceled run as shell success. The
// human-readable summary is already printed; this only sets the exit code.
func runStatusError(state *core.State) error {
	if state.Status == core.StatusCompleted {
		return nil
	}
	if state.BlockedReason != "" {
		return fmt.Errorf("run %s: %s", state.Status, state.BlockedReason)
	}
	return fmt.Errorf("run %s", state.Status)
}

func workflowName(p *project, override string) string {
	if override != "" {
		return override
	}
	return p.cfg.Workflow.Default
}
