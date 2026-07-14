package main

import (
	"strings"
	"testing"
)

// TestReadOnlyAndResumeCommandsRejectOpID guards the documented CLI contract (README, concepts.md):
// only the MUTATING operations (worker start/complete, review complete, stage close) and run-start
// creation take --op-id. The read-only views (status, observe) and the human resume action must NOT —
// a command silently accepting --op-id would let the next host build on a retry-safety guarantee that
// is not there. The flag is rejected at parse time, before any project is opened, so this needs no
// fixture. It intentionally fails if someone adds --op-id to one of these commands without updating
// the docs that promise otherwise.
func TestReadOnlyAndResumeCommandsRejectOpID(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]string) error
	}{
		{"status", cmdStatus},
		{"observe", cmdObserve},
		{"run resume", cmdRunResume},
	} {
		err := tc.fn([]string{"--op-id", "x"})
		if err == nil || !strings.Contains(err.Error(), "op-id") {
			t.Errorf("%s must reject --op-id (documented as not taking one), got err=%v", tc.name, err)
		}
	}
}
