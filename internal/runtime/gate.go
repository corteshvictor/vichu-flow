package runtime

import (
	"os"
	"path/filepath"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// GateOutputPath is where a gate's full captured output is streamed. The gates
// package writes this file directly (it is a streamed capture, not a snapshot).
func (s *Store) GateOutputPath(runID, stage string, n int) string {
	return filepath.Join(s.GateDir(runID, stage, n), "output.log")
}

// CreateGateOutput opens the gate's output.log for the subprocess to stream into, confined
// to .vichu/ and refusing to follow a symlink at the final component.
//
// A plain os.Create here was the sharpest hole in the runtime: output.log lives at a path
// the agent can predict (run id, stage, attempt number all knowable during its work), so it
// could pre-plant it as a symlink to any external file and os.Create — which follows and
// TRUNCATES — would let the gate's stdout/stderr, content the agent largely controls, be
// streamed over that file. The caller owns and closes the returned handle.
func (s *Store) CreateGateOutput(runID, stage string, n int) (*os.File, error) {
	r, rel, err := confine(s.projectRoot, s.GateOutputPath(runID, stage, n))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.CreateTruncate(rel, 0o644)
}

// SaveGateCommand persists the exact command a gate ran (command.json).
func (s *Store) SaveGateCommand(runID, stage string, n int, cmd *core.GateCommand) error {
	return writeJSON(s.projectRoot, filepath.Join(s.GateDir(runID, stage, n), "command.json"), cmd)
}

// SaveGateVerdict persists a gate's verified result (verdict.json). This file —
// not any agent-authored markdown — is what authorizes a stage transition.
func (s *Store) SaveGateVerdict(runID, stage string, n int, v *core.GateVerdict) error {
	return writeJSON(s.projectRoot, filepath.Join(s.GateDir(runID, stage, n), "verdict.json"), v)
}

// SaveGateExcerpt persists the bounded excerpt of a failed gate's output
// (excerpt.txt) — what agents and views consume instead of the full log.
func (s *Store) SaveGateExcerpt(runID, stage string, n int, text []byte) error {
	return writeFileAtomic(s.projectRoot, filepath.Join(s.GateDir(runID, stage, n), "excerpt.txt"), text, 0o644)
}

// SaveGateMutationReport persists the files a gate changed (gates verify; they
// should not mutate the tree). mutations.json under the gate directory.
func (s *Store) SaveGateMutationReport(runID, stage string, n int, r *core.MutationReport) error {
	return writeJSON(s.projectRoot, filepath.Join(s.GateDir(runID, stage, n), "mutations.json"), r)
}
