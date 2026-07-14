package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/corteshvictor/vichu-flow/internal/engine"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// reviewSubcommands maps `vichu review <verb>` to its handler. `review complete`
// closes a host-first reviewer worker: it audits the reviewer, persists the
// structured verdict, and branches the run (approved → verify, needs_fixes → fix).
var reviewSubcommands = map[string]func([]string) error{
	"complete": cmdReviewComplete,
}

func cmdReview(args []string) error {
	if len(args) > 0 {
		if h, ok := reviewSubcommands[args[0]]; ok {
			return h(args[1:])
		}
		return fmt.Errorf(i18n.T("review.unknown"), args[0])
	}
	return errors.New(i18n.T("review.need_subcommand"))
}

// cmdReviewComplete records the reviewer's verdict and transitions the run on it.
// `--verdict <file>` points OUTSIDE the runtime (single-writer); or `--verdict-stdin`.
func cmdReviewComplete(args []string) error {
	fs := flag.NewFlagSet("review complete", flag.ContinueOnError)
	run := fs.String("run", "", i18n.T("worker.flag_run"))
	worker := fs.String("worker", "", i18n.T("worker.flag_worker"))
	verdict := fs.String("verdict", "", i18n.T("review.flag_verdict"))
	verdictStdin := fs.Bool("verdict-stdin", false, i18n.T("review.flag_verdict_stdin"))
	opID := fs.String("op-id", "", i18n.T("op.flag_id"))
	tokenR := driverTokenFlags(fs)
	session := fs.String("session", "", i18n.T("worker.flag_session"))
	tokensIn := fs.Int("tokens-in", 0, i18n.T("worker.flag_tokens_in"))
	tokensOut := fs.Int("tokens-out", 0, i18n.T("worker.flag_tokens_out"))
	costUSD := fs.Float64("cost-usd", 0, i18n.T("worker.flag_cost"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *run == "" || *worker == "" {
		return errors.New(i18n.T("review.need_flags"))
	}
	token, err := tokenR.resolve(*verdictStdin)
	if err != nil {
		return err
	}

	proj, err := openWorkerProject()
	if err != nil {
		return err
	}
	content, err := readHostFile(*verdict, *verdictStdin, proj.store.Root())
	if err != nil {
		return err
	}
	if content == "" {
		return errors.New(i18n.T("review.need_verdict"))
	}

	tokensReported, costReported := usageFlags(fs)
	out := engine.WorkerOutcome{
		Result: content, SessionID: *session,
		TokensIn: *tokensIn, TokensOut: *tokensOut, CostUSD: *costUSD,
		TokensReported: tokensReported, CostReported: costReported,
	}
	blockReason, err := proj.engineForOutput(*jsonOut).ReviewComplete(*run, *worker, *opID, token, out)
	if err != nil {
		return err
	}

	state, _ := proj.store.LoadState(*run)
	if *jsonOut {
		out := map[string]any{"worker": *worker, "blocked": blockReason != "", "block_reason": blockReason}
		if state != nil {
			out["status"] = string(state.Status)
			out["stage"] = state.CurrentStage
		}
		return printJSON(out)
	}
	if blockReason != "" {
		fmt.Printf(i18n.T("review.blocked")+"\n", blockReason)
		return nil
	}
	if state != nil {
		fmt.Printf(i18n.T("review.done")+"\n", state.CurrentStage)
	}
	return nil
}
