package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// stageSubcommands maps `vichu stage <verb>` to its handler. `stage close` is the
// host-first transactional command that validates the current stage's evidence
// (running its gates if any) and transitions the run to the next stage.
var stageSubcommands = map[string]func([]string) error{
	"close": cmdStageClose,
}

func cmdStage(args []string) error {
	if len(args) > 0 {
		if h, ok := stageSubcommands[args[0]]; ok {
			return h(args[1:])
		}
		return fmt.Errorf(i18n.T("stage.unknown"), args[0])
	}
	return errors.New(i18n.T("stage.need_subcommand"))
}

// cmdStageClose verifies the current stage and advances the run. For a gate stage
// it runs the configured gates; a failing gate blocks. After it, the run is at
// the next stage (or completed).
func cmdStageClose(args []string) error {
	fs := flag.NewFlagSet("stage close", flag.ContinueOnError)
	run := fs.String("run", "", i18n.T("worker.flag_run"))
	stage := fs.String("stage", "", i18n.T("worker.flag_stage"))
	opID := fs.String("op-id", "", i18n.T("op.flag_id"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *run == "" || *stage == "" {
		return errors.New(i18n.T("stage.need_close_flags"))
	}

	proj, err := openWorkerProject()
	if err != nil {
		return err
	}
	blockReason, err := proj.engineForOutput(*jsonOut).StageClose(*run, *stage, *opID)
	if err != nil {
		return err
	}

	// Re-read the run to report where it landed.
	state, _ := proj.store.LoadState(*run)
	if *jsonOut {
		out := map[string]any{"closed": *stage, "blocked": blockReason != "", "block_reason": blockReason}
		if state != nil {
			out["status"] = string(state.Status)
			out["stage"] = state.CurrentStage
		}
		return printJSON(out)
	}
	if blockReason != "" {
		fmt.Printf(i18n.T("stage.blocked")+"\n", *stage, blockReason)
		return nil
	}
	if state != nil {
		fmt.Printf(i18n.T("stage.closed")+"\n", *stage, state.CurrentStage, state.Status)
	}
	return nil
}
