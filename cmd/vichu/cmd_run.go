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

// runSubcommands maps `vichu run <verb>` lifecycle verbs to their handlers. The
// `run` namespace operates a run's lifecycle (host-first); running a whole
// workflow headless is `vichu exec`.
var runSubcommands = map[string]func([]string) error{
	"start":  cmdRunStart,
	"resume": cmdRunResume,
}

// cmdRun dispatches the `run` namespace. `vichu run <verb>` (e.g. `run start`)
// operates a run's lifecycle; the bare `vichu run "task"` form is a deprecated
// alias for `vichu exec` (run a full workflow headless).
func cmdRun(args []string) error {
	if len(args) > 0 {
		if h, ok := runSubcommands[args[0]]; ok {
			return h(args[1:])
		}
	}
	fmt.Fprintln(os.Stderr, i18n.T("run.exec_renamed"))
	return cmdExec(args)
}

// cmdExec runs a full workflow headless to completion — the fallback / CLI mode.
// The host-first experience drives runs from inside the agent instead (§ host
// packs); this is for CI, automation, and hosts without integration. `exec
// resume` continues an existing run headless.
func cmdExec(args []string) error {
	if len(args) > 0 && args[0] == "resume" {
		return cmdExecResume(args[1:])
	}
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
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

// cmdRunStart materializes a run WITHOUT executing it — the host-first lifecycle
// entry point. It prints the new run id (or JSON) so the host can then drive the
// workers and call the transactional commands to audit and advance the run.
func cmdRunStart(args []string) error {
	fs := flag.NewFlagSet("run start", flag.ContinueOnError)
	workflow := fs.String("workflow", "", i18n.T("run.flag_workflow"))
	taskFlag := fs.String("task", "", i18n.T("run.flag_task"))
	opID := fs.String("op-id", "", i18n.T("op.flag_id"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	// parseArgsAnyOrder, NOT plain fs.Parse: the task is a positional, and Go's flag package
	// stops parsing at the first positional — so `run start "task" --op-id X` would silently
	// drop --op-id, and a retry that lost its idempotency key creates a DUPLICATE run doing
	// the work twice. Accept flags on either side of the task.
	positionals, err := parseArgsAnyOrder(fs, args)
	if err != nil {
		return err
	}
	positional := strings.TrimSpace(strings.Join(positionals, " "))
	if *taskFlag != "" && positional != "" {
		return errors.New(i18n.T("run.task_both"))
	}
	task := *taskFlag
	if task == "" {
		task = positional
	}
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

	state, token, err := proj.engineForOutput(*jsonOut).StartRun(task, *workflow, *opID)
	if err != nil {
		return err
	}

	// The driver token is printed ONCE, here, and never written under .vichu/. Only its
	// hash is persisted, so a subagent that can read the runtime cannot drive the run.
	if *jsonOut {
		return printJSON(map[string]string{
			"run_id":       state.RunID,
			"status":       string(state.Status),
			"stage":        state.CurrentStage,
			"driver_token": token,
		})
	}
	fmt.Printf(i18n.T("run.started")+"\n", state.RunID, state.CurrentStage)
	if token != "" {
		fmt.Printf("\n"+i18n.T("run.driver_token")+"\n", token)
	}
	return nil
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
