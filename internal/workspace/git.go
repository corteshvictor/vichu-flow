// Package workspace gives runs an undo guarantee: it snapshots the workspace
// when a run starts, detects drift on resume, and tracks exactly which files
// each worker mutates. It is provider-based (see Provider): the Git provider
// uses the repository as the baseline, and the filesystem provider keeps a
// content copy under .vichu/ — so Git is recommended but not required, and a run
// in any folder still has a verifiable record of and a rollback for every change.
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// gitRevParse is the git subcommand used to read commit/toplevel info.
const gitRevParse = "rev-parse"

// ErrNoGit means the git binary is not installed or not on PATH.
var ErrNoGit = errors.New("git is not installed or not on PATH")

// ErrNotRepo means the target directory is not inside a git repository.
var ErrNotRepo = errors.New("not a git repository — run `git init` first (agents writing code without version control have no undo)")

// GitAvailable reports whether the git binary can be invoked.
func GitAvailable() bool {
	return exec.Command("git", "--version").Run() == nil
}

// git runs a git command in the repo root and returns trimmed stdout.
func (r *Repo) git(args ...string) (string, error) {
	full := append([]string{"-C", r.root}, args...)
	cmd := exec.Command("git", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}
