package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// The DRIVER TOKEN is the capability that says "I am the orchestrator of this run".
//
// It exists because a coding host's permission rules are SESSION-WIDE. When the pack
// pre-authorizes `Bash(vichu worker complete:*)` so the orchestrator can drive a run, it
// authorizes it for EVERY subagent in that session that has Bash — including the
// implementer, which needs Bash to run the project's tests while it codes.
//
// That is not a theoretical leak. An implementer can call `vichu worker complete` on its
// OWN worker and then carry on editing files: mutation tracking ended at the close, so the
// later changes are invisible to the audit and can never block the run. The agent has
// stepped around the one thing this kernel is for.
//
// The permission layer cannot tell the two callers apart — they are the same session. The
// KERNEL can, if driving requires something the subagent does not have. So:
//
//   - `run start` mints a random token, persists only its SHA-256 in state.json, and
//     returns the token once, to the orchestrator.
//   - every command that MUTATES the run demands it. `status` and `observe` do not —
//     reading is harmless, and keeping them open keeps observability easy.
//   - the token is NEVER written under `.vichu/`, so a subagent that can read the runtime
//     still cannot drive the run.
//   - `run resume` — a human action the pack deliberately does not pre-authorize — ROTATES
//     it. That is what makes a leaked token recoverable.

// driverTokenBytes is the token's entropy. 32 bytes is far past guessing.
const driverTokenBytes = 32

// mintDriverToken issues a fresh capability for driving a run: it stores the HASH on the
// state (the caller persists it) and returns the token, which is never written to disk.
func mintDriverToken(state *core.State) (string, error) {
	b := make([]byte, driverTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot generate the run's driver token: %w", err)
	}
	tok := hex.EncodeToString(b)
	state.DriverTokenHash = hashDriverToken(tok)
	return tok, nil
}

func hashDriverToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// requireDriver rejects a state-mutating command that does not hold the run's capability.
//
// A run with no hash predates this (v0.4.0 and earlier). It must NOT be driven without a
// token — that would leave the hole open for exactly the runs most likely to be attacked.
// `run resume` mints one, and it is a human action, which is the point.
func requireDriver(state *core.State, token string) error {
	if state.DriverTokenHash == "" {
		return fmt.Errorf("run %s was created before driver tokens existed — run `vichu run resume --run %s` once (a human approves it) to issue one", state.RunID, state.RunID)
	}
	if token == "" {
		return errors.New("this command changes the run, so it needs --driver-token. `run start` gave the token to the orchestrator; a subagent must never be given it — that is what stops a worker from closing itself and then editing files outside the audit")
	}
	// Constant time: the token is a secret, and a timing oracle on a hash comparison is a
	// silly way to lose one.
	if subtle.ConstantTimeCompare([]byte(hashDriverToken(token)), []byte(state.DriverTokenHash)) != 1 {
		return errors.New("--driver-token does not match this run")
	}
	return nil
}
