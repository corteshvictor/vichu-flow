package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/engine"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// workerSubcommands maps `vichu worker <verb>` to its handler. These are the
// host-first transactional commands a host pack calls around its native subagent:
// `worker start` (capture the BEFORE snapshot) → the host runs the agent →
// `worker complete` (attribute mutations, persist result).
var workerSubcommands = map[string]func([]string) error{
	"start":    cmdWorkerStart,
	"complete": cmdWorkerComplete,
}

func cmdWorker(args []string) error {
	if len(args) > 0 {
		if h, ok := workerSubcommands[args[0]]; ok {
			return h(args[1:])
		}
		return fmt.Errorf(i18n.T("worker.unknown"), args[0])
	}
	return errors.New(i18n.T("worker.need_subcommand"))
}

// cmdWorkerStart opens a worker BEFORE the host launches its subagent: it
// captures and persists the pre-worker mutation snapshot so the kernel can later
// attribute exactly what the agent changed. Prints the new worker id.
func cmdWorkerStart(args []string) error {
	fs := flag.NewFlagSet("worker start", flag.ContinueOnError)
	run := fs.String("run", "", i18n.T("worker.flag_run"))
	stage := fs.String("stage", "", i18n.T("worker.flag_stage"))
	role := fs.String("role", "", i18n.T("worker.flag_role"))
	opID := fs.String("op-id", "", i18n.T("op.flag_id"))
	tokenR := driverTokenFlags(fs)
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := tokenR.resolve(false)
	if err != nil {
		return err
	}
	if *run == "" || *stage == "" || *role == "" {
		return errors.New(i18n.T("worker.need_start_flags"))
	}

	proj, err := openWorkerProject()
	if err != nil {
		return err
	}
	workerID, blockReason, err := proj.engineForOutput(*jsonOut).WorkerStart(*run, *stage, *role, *opID, token)
	if err != nil {
		return err
	}
	if blockReason != "" {
		// The run was blocked (e.g. budget) and NO worker was opened — the host
		// must not launch a subagent.
		if *jsonOut {
			return printJSON(map[string]any{"worker_id": "", "blocked": true, "block_reason": blockReason})
		}
		fmt.Printf(i18n.T("worker.start_blocked")+"\n", blockReason)
		return nil
	}
	if *jsonOut {
		return printJSON(map[string]any{"worker_id": workerID, "blocked": false})
	}
	fmt.Printf(i18n.T("worker.started")+"\n", workerID)
	return nil
}

// cmdWorkerComplete closes a worker AFTER the host's subagent ran: it diffs the
// tree against the BEFORE snapshot, writes mutations.json, persists the result,
// and blocks the run if the mutation policy is violated. `--result <file>` points
// to a file OUTSIDE the runtime (the kernel copies it in — single-writer).
func cmdWorkerComplete(args []string) error {
	fs := flag.NewFlagSet("worker complete", flag.ContinueOnError)
	run := fs.String("run", "", i18n.T("worker.flag_run"))
	worker := fs.String("worker", "", i18n.T("worker.flag_worker"))
	result := fs.String("result", "", i18n.T("worker.flag_result"))
	resultStdin := fs.Bool("result-stdin", false, i18n.T("worker.flag_result_stdin"))
	opID := fs.String("op-id", "", i18n.T("op.flag_id"))
	tokenR := driverTokenFlags(fs)
	session := fs.String("session", "", i18n.T("worker.flag_session"))
	tokensIn := fs.Int("tokens-in", 0, i18n.T("worker.flag_tokens_in"))
	tokensOut := fs.Int("tokens-out", 0, i18n.T("worker.flag_tokens_out"))
	costUSD := fs.Float64("cost-usd", 0, i18n.T("worker.flag_cost"))
	var artifactArgs artifactFlag
	fs.Var(&artifactArgs, "artifact", i18n.T("worker.flag_artifact"))
	jsonOut := fs.Bool("json", false, i18n.T("run.flag_json"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *run == "" || *worker == "" {
		return errors.New(i18n.T("worker.need_complete_flags"))
	}
	token, err := tokenR.resolve(*resultStdin)
	if err != nil {
		return err
	}

	proj, err := openWorkerProject()
	if err != nil {
		return err
	}
	resultText, err := readHostFile(*result, *resultStdin, proj.store.Root())
	if err != nil {
		return err
	}
	artifacts, err := artifactArgs.load(proj.store.Root())
	if err != nil {
		return err
	}
	tokensReported, costReported := usageFlags(fs)
	out := engine.WorkerOutcome{
		Result: resultText, SessionID: *session,
		TokensIn: *tokensIn, TokensOut: *tokensOut, CostUSD: *costUSD,
		TokensReported: tokensReported, CostReported: costReported,
		Artifacts: artifacts,
	}
	blockReason, err := proj.engineForOutput(*jsonOut).WorkerComplete(*run, *worker, *opID, token, out)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"worker":       *worker,
			"blocked":      blockReason != "",
			"block_reason": blockReason,
		})
	}
	if blockReason != "" {
		fmt.Printf(i18n.T("worker.done_blocked")+"\n", *worker, blockReason)
		return nil
	}
	fmt.Printf(i18n.T("worker.done")+"\n", *worker)
	return nil
}

// usageFlags reports which kinds of usage the host actually exposed on this close,
// by whether the corresponding flags were explicitly passed. Tokens and cost are
// independent: a host may report tokens (--tokens-in/--tokens-out) but not cost
// (--cost-usd). An unreported kind stays "unknown" rather than a misleading zero.
func usageFlags(fs *flag.FlagSet) (tokensReported, costReported bool) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "tokens-in", "tokens-out":
			tokensReported = true
		case "cost-usd":
			costReported = true
		}
	})
	return tokensReported, costReported
}

// artifactFlag collects repeated `--artifact name=file` flags. The file content
// is read at load() time (validated to be outside the runtime); the kernel
// validates the logical name against its allowlist.
type artifactFlag []string

func (a *artifactFlag) String() string { return strings.Join(*a, ",") }
func (a *artifactFlag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("--artifact must be name=file, got %q", v)
	}
	*a = append(*a, v)
	return nil
}

// load reads each name=file into a name→content map, rejecting files inside the
// runtime (single-writer) and duplicate names.
func (a artifactFlag) load(runtimeRoot string) (map[string]string, error) {
	if len(a) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(a))
	for _, spec := range a {
		name, path, _ := strings.Cut(spec, "=")
		name = strings.TrimSpace(name)
		if name == "" || path == "" {
			return nil, fmt.Errorf("--artifact must be name=file, got %q", spec)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--artifact %q given twice", name)
		}
		content, err := readHostFile(path, false, runtimeRoot)
		if err != nil {
			return nil, err
		}
		out[name] = content
	}
	return out, nil
}

// readHostFile loads host-provided content (a worker result or a review verdict)
// from stdin or a file. A file path must be OUTSIDE the runtime dir: the kernel
// owns .vichu/runs (single-writer), so the host can never hand it a path it wrote
// inside the runtime.
func readHostFile(path string, stdin bool, runtimeRoot string) (string, error) {
	if stdin {
		data, err := io.ReadAll(os.Stdin)
		return string(data), err
	}
	if path == "" {
		return "", nil
	}
	abs := resolvePath(path)
	if rt := resolvePath(runtimeRoot); rt != "" {
		if abs == rt || strings.HasPrefix(abs, rt+string(filepath.Separator)) {
			return "", fmt.Errorf("the input file must be OUTSIDE the runtime (%s) — the kernel owns it (single-writer); use the --*-stdin flag instead", runtimeRoot)
		}
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

// resolvePath returns the absolute, symlink-resolved form of p (falling back to
// the absolute path if it can't be resolved), so a symlink pointing into the
// runtime can't slip a host input past the OUTSIDE-the-runtime check.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return abs
}

// openWorkerProject loads the project for a host-first command, mapping a missing
// config to the standard hint.
func openWorkerProject() (*project, error) {
	proj, err := openProject()
	if err != nil && config.IsNotFound(err) {
		return nil, errors.New(i18n.T("run.no_config"))
	}
	return proj, err
}

// printJSON writes v as indented JSON to stdout (for host packs).
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
