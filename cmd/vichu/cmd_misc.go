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
	"github.com/corteshvictor/vichu-flow/internal/runtime"
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
	state, token, err := proj.engineForOutput(*jsonOut).ReopenRun(runID, engine.ResumeOptions{AcceptChanges: *accept})
	if err != nil {
		return err
	}
	// Resume ROTATES the driver token: it is the human's command, so it is the moment a
	// leaked or lost capability dies and a fresh one is issued.
	if *jsonOut {
		return printResumeJSON(proj, runID, token)
	}
	printStateSummary(state)
	if token != "" {
		fmt.Printf("\n"+i18n.T("run.driver_token")+"\n", token)
	}
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
	alreadyCanceled := state.Status == core.StatusCanceled

	// Open the audit ONCE and HOLD the descriptor across validation → state save → append, so the
	// run_canceled event lands on the EXACT file that was validated. Two separate opens (validate,
	// then append) can see different files if the log is repointed in between — deleted OR replaced by
	// another regular file — and the event would be absorbed by the replacement while the real history
	// is lost, with cancel still exiting 0. One descriptor closes that; Append's identity check reports
	// a repointed path instead. A missing/corrupt log yields a nil handle and auditErr (escape hatch).
	appender, verifiedEvents, auditErr := proj.store.OpenVerifiedAudit(runID)
	defer func() { _ = appender.Close() }()

	if state.Status.Terminal() && !alreadyCanceled {
		// completed or failed — nothing to cancel and nothing to record, BUT do not claim so over a
		// corrupt audit: a run whose history cannot be read is not a confirmed clean finish.
		if auditErr != nil {
			return fmt.Errorf("run %s is %s, but its audit is unreadable so its history cannot be confirmed: %w", runID, state.Status, auditErr)
		}
		fmt.Printf(i18n.T("cancel.already")+"\n", runID, state.Status)
		return nil
	}

	// Two writes have to land: the run's STATE (authoritative — it is what stops the engine)
	// and the audit EVENT. There is no transaction here yet, so one of them can land alone,
	// and the ORDER decides which lie you tell when that happens.
	//
	// State first. A crash after it leaves a run that IS canceled with no record of it yet —
	// an INCOMPLETE audit, which the retry below repairs. Recording the event first leaves
	// the opposite: an audit asserting a cancel that never took effect, while the state says
	// the run is still active. That is not an incomplete record, it is a FALSE one, and the
	// kernel does not get to make those. Under-report, never over-report.
	// cancel is the ESCAPE HATCH: it must stop even a run whose audit is corrupt. So it still marks
	// the state canceled, but it must NOT then forge a misleading timeline. If the audit was
	// unreadable at open (auditErr), cancel the state but report the loss non-zero.
	if !alreadyCanceled {
		// Cooperative cancel: mark the run canceled on disk. An engine actively owning the
		// run observes this within a second (its cancel watcher) and kills the in-flight
		// worker or gate process.
		state.Status = core.StatusCanceled
		state.NextAction = ""
		if err := proj.store.SaveState(state); err != nil {
			return err
		}
	}
	if auditErr != nil {
		return fmt.Errorf("run %s canceled (its state now stops the engine), but its audit is unreadable so the cancel was NOT recorded — the timeline is lost, do not trust it: %w", runID, auditErr)
	}
	// The event, and the repair. Dropping this error used to print "canceled" over a failed
	// write; returning it was not enough either, because the state was already saved, so the
	// RETRY hit "already canceled" and exited 0 — the run stayed canceled with zero record
	// of it, permanently, while reporting success. Now the retry finishes the job.
	if err := ensureCancelEvent(appender, runID, verifiedEvents, alreadyCanceled); err != nil {
		return err
	}
	if alreadyCanceled {
		fmt.Printf(i18n.T("cancel.already")+"\n", runID, state.Status)
		return nil
	}
	fmt.Printf(i18n.T("cancel.done")+"\n", runID)
	return nil
}

// ensureCancelEvent appends the run_canceled event unless the run already carries one.
//
// `late` marks an event written after the fact: the run was already canceled on disk when we
// got here, so an earlier attempt saved the state and then failed to record it. The event's
// timestamp is when we WROTE it, not when the cancel happened, and the trail says so rather
// than passing it off as contemporaneous.
//
// `events` is the ALREADY-VERIFIED snapshot the caller loaded (OpenVerifiedAudit) — passed in rather
// than re-read so the "does a run_canceled already exist?" check runs against the exact bytes that
// were validated, not a fresh read that could differ.
//
// The append goes THROUGH the appender's held descriptor: it lands on the exact inode that was
// validated (not a file the path was repointed to since), and the appender confirms afterward that the
// path still resolves to it. If the log was deleted OR replaced in between, the append is reported as
// failed and the caller keeps the state canceled but returns non-zero — under-report, never over.
//
// KNOWN LIMIT — this is check-then-append, so it is idempotent across RETRIES but not across
// CONCURRENT cancels: two `vichu cancel` processes racing on the same run can both see an
// empty trail and both append, leaving two run_canceled events. Exactly-once emission needs
// the durable outbox tracked as H8, and the outbox is where this belongs — a private lock
// here would be a second mechanism solving the same problem badly. A duplicate event is
// visible and explainable; the failures above were neither.
func ensureCancelEvent(appender *runtime.AuditAppender, runID string, events []core.Event, late bool) error {
	for _, ev := range events {
		if ev.Event == core.EventRunCanceled {
			return nil
		}
	}
	ev := core.Event{Run: runID, Event: core.EventRunCanceled}
	if late {
		ev.Detail = map[string]any{"recorded_late": true}
	}
	if err := appender.Append(ev); err != nil {
		return fmt.Errorf("run %s is marked canceled, but recording it in the audit trail failed: %w — run `vichu cancel` again once the problem is fixed and it will record it", runID, err)
	}
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

// printResumeJSON emits the run's status plus the freshly rotated driver token. The token
// is never persisted, so this is the only place the caller can get it.
func printResumeJSON(proj *project, runID, token string) error {
	state, err := proj.store.LoadState(runID)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"run_id":       state.RunID,
		"status":       string(state.Status),
		"stage":        state.CurrentStage,
		"blocked":      state.Status == core.StatusBlocked,
		"block_reason": state.BlockedReason,
		"driver_token": token,
	})
}
