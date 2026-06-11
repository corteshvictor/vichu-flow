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
	ID         string      `json:"id"`
	Role       string      `json:"role"`
	Adapter    string      `json:"adapter"`
	Stage      string      `json:"stage"`
	Status     WorkerState `json:"status"`
	SessionID  string      `json:"session_id,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
}

// Result is what a worker produces: a human-facing markdown report and an
// optional machine-readable payload plus usage/cost if the adapter reports it.
type Result struct {
	Markdown    string         `json:"-"`
	Data        map[string]any `json:"data,omitempty"`
	CostUSD     float64        `json:"cost_usd,omitempty"`
	TokensIn    int            `json:"tokens_in,omitempty"`
	TokensOut   int            `json:"tokens_out,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	ExitMessage string         `json:"exit_message,omitempty"`
}
