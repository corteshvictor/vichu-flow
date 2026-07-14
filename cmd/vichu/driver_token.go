package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// driverTokenResolver registers the driver-token flags on fs and returns a function that
// yields the resolved token after fs.Parse.
//
// The token is a per-run SECRET. Passed as `--driver-token <value>` it lands in argv, where
// any process on the machine can read it with `ps` while the command runs — and a subagent
// (which has a shell) can sit in a loop doing exactly that, harvest the token from the
// orchestrator's next mutating command, and then drive the run itself, defeating the very
// boundary the token exists to hold. `--driver-token-stdin` reads it from stdin instead, so
// it never appears in argv. The old flag still works, and warns, so nothing breaks at once.
//
// This closes the passive `ps` leak. It does NOT create a boundary against a hostile process
// running as the SAME user — that needs host-level isolation and is a documented limit.
type driverTokenResolver struct {
	value *string
	stdin *bool
}

func driverTokenFlags(fs *flag.FlagSet) *driverTokenResolver {
	return &driverTokenResolver{
		value: fs.String("driver-token", "", i18n.T("op.flag_driver_token")),
		stdin: fs.Bool("driver-token-stdin", false, i18n.T("op.flag_driver_token_stdin")),
	}
}

// resolve returns the token, reading stdin when --driver-token-stdin was given. stdinTaken
// reports whether another flag (e.g. --result-stdin) is already consuming stdin, in which
// case the two cannot share it and the caller must use a file for that other input.
func (r *driverTokenResolver) resolve(stdinTaken bool) (string, error) {
	if *r.stdin {
		if *r.value != "" {
			return "", errors.New("pass the driver token via --driver-token-stdin OR --driver-token, not both")
		}
		if stdinTaken {
			return "", errors.New("--driver-token-stdin and --result-stdin both read stdin; keep the token on stdin and pass the result with --result <file>")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading driver token from stdin: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if *r.value != "" {
		fmt.Fprintln(os.Stderr, i18n.T("op.driver_token_argv_warning"))
	}
	return *r.value, nil
}
