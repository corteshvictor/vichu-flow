package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	workflow := fs.String("workflow", "", i18n.T("run.flag_workflow"))
	provider := fs.String("provider", "", i18n.T("run.flag_provider"))
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
	if *provider != "" {
		proj.cfg.Workflow.Provider = *provider
	}

	fmt.Printf(i18n.T("run.running")+"\n", workflowName(proj, *workflow), task)
	state, err := proj.newEngine().Start(context.Background(), task, *workflow)
	if err != nil {
		return err
	}

	fmt.Println()
	printStateSummary(state)
	fmt.Printf("\n"+i18n.T("run.observe")+"\n", state.RunID)
	return nil
}

func workflowName(p *project, override string) string {
	if override != "" {
		return override
	}
	return p.cfg.Workflow.Default
}
