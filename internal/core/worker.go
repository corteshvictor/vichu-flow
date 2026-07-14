package core

import "time"

// WorkerState is the lifecycle status of a single worker invocation.
type WorkerState string

const (
	WorkerRunning  WorkerState = "running"
	WorkerDone     WorkerState = "done"
	WorkerFailed   WorkerState = "failed"
	WorkerCanceled WorkerState = "canceled"
)

// WorkerStatus is persisted to workers/<id>/status.json and tracks one agent
// invocation: which role it played, which adapter ran it, and how it ended.
type WorkerStatus struct {
	ID      string      `json:"id"`
	Role    string      `json:"role"`
	Adapter string      `json:"adapter"`
	Stage   string      `json:"stage"`
	Status  WorkerState `json:"status"`
	// CloseOpID and CloseBlockReason are the operation JOURNAL. Closing a worker is
	// only PART of `worker complete` / `review complete` — the run state still has to
	// be blocked, branched or advanced afterwards. Writing both here, in the same
	// write that marks the worker done, makes that write the operation's COMMIT
	// POINT: a retry of the SAME op-id re-applies the recorded outcome from durable
	// evidence instead of assuming the operation finished. Without this, a crash
	// between "worker done" and "state applied" makes the retry report success for an
	// operation that never landed — the kernel lying about the run's state.
	//
	// A DIFFERENT op-id against a closed worker is not a retry: it is a new operation
	// on a terminal worker, and it must fail.
	CloseOpID string `json:"close_op_id,omitempty"`
	// CloseFingerprint binds that op-id to ITS payload (result, usage, artifacts,
	// verdict). The same op-id resent with DIFFERENT evidence is not a retry — it is a
	// new operation wearing an old id, and answering "already done" to it would silently
	// discard what the host just sent.
	CloseFingerprint string     `json:"close_fingerprint,omitempty"`
	CloseBlockReason string     `json:"close_block_reason,omitempty"`
	SessionID        string     `json:"session_id,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

// Result is what a worker produces: a human-facing markdown report and an
// optional machine-readable payload plus usage/cost if the adapter reports it.
// TokensReported and CostReported are independent: an adapter may surface tokens
// but not USD cost (codex), so a zero cost must not be mistaken for a real $0.00.
type Result struct {
	Markdown       string         `json:"-"`
	Data           map[string]any `json:"data,omitempty"`
	CostUSD        float64        `json:"cost_usd,omitempty"`
	CostReported   bool           `json:"-"`
	TokensIn       int            `json:"tokens_in,omitempty"`
	TokensOut      int            `json:"tokens_out,omitempty"`
	TokensReported bool           `json:"-"`
	SessionID      string         `json:"session_id,omitempty"`
	ExitMessage    string         `json:"exit_message,omitempty"`
}
