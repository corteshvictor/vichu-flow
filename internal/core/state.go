package core

import "time"

// RunStatus is the lifecycle status of a run.
type RunStatus string

const (
	// StatusActive means the run is currently executing a stage.
	StatusActive RunStatus = "active"
	// StatusBlocked means the run stopped awaiting a human decision or because
	// a budget or safety guard tripped. blocked_reason explains why.
	StatusBlocked RunStatus = "blocked"
	// StatusPaused means the run was intentionally suspended and can resume.
	StatusPaused RunStatus = "paused"
	// StatusCompleted means the workflow reached its terminal stage successfully.
	StatusCompleted RunStatus = "completed"
	// StatusCanceled means a human canceled the run.
	StatusCanceled RunStatus = "canceled"
	// StatusFailed means the run stopped on an unrecoverable error.
	StatusFailed RunStatus = "failed"
)

// StageStatus is the status of a single stage within a run.
type StageStatus string

const (
	// StagePending means the stage has not started.
	StagePending StageStatus = "pending"
	// StageActive means the stage is executing.
	StageActive StageStatus = "active"
	// StageDone means the stage completed and its exit evidence passed.
	StageDone StageStatus = "done"
	// StageSkipped means the stage was skipped (e.g. optional, not applicable).
	StageSkipped StageStatus = "skipped"
	// StageFailed means the stage stopped on an error.
	StageFailed StageStatus = "failed"
)

// BudgetState records how much of each run-level budget has been consumed,
// aggregated across every worker. Durations are expressed in seconds for
// machine readability (the plan's "PT41M" notation is illustrative; flat-file
// consumers get plain numbers).
type BudgetState struct {
	CostUSDSpent          float64 `json:"cost_usd_spent"`
	WallClockSpentSeconds float64 `json:"wall_clock_spent_seconds"`
	AgentInvocations      int     `json:"agent_invocations"`
	TokensInSpent         int     `json:"tokens_in_spent"`
	TokensOutSpent        int     `json:"tokens_out_spent"`
	// TokensReported / CostReported are true once any worker reported that kind of
	// usage. Invocations and wall-clock are always kernel-measured; tokens and cost
	// are only known when the runner (headless) or host (native) exposes them — and
	// they are independent (codex reports tokens but not USD cost). When a flag is
	// false, status renders that field "unknown" rather than a misleading zero.
	TokensReported bool `json:"tokens_reported,omitempty"`
	CostReported   bool `json:"cost_reported,omitempty"`
	// Per-stage token spend, aggregated across a stage's iterations, for
	// budgets.stage.<stage>.max*Tokens.
	StageTokensIn  map[string]int `json:"stage_tokens_in,omitempty"`
	StageTokensOut map[string]int `json:"stage_tokens_out,omitempty"`
}

// TokensTotalSpent is the sum of input and output tokens consumed by the run.
func (b BudgetState) TokensTotalSpent() int {
	return b.TokensInSpent + b.TokensOutSpent
}

// StageTokensTotal is the total tokens (in + out) spent within a single stage.
func (b BudgetState) StageTokensTotal(stage string) int {
	return b.StageTokensIn[stage] + b.StageTokensOut[stage]
}

// State is the source of truth for a run, persisted atomically to state.json.
type State struct {
	SchemaVersion int                    `json:"schema_version"`
	RunID         string                 `json:"run_id"`
	Status        RunStatus              `json:"status"`
	Workflow      string                 `json:"workflow"`
	Provider      string                 `json:"provider,omitempty"`
	Task          string                 `json:"task"`
	CurrentStage  string                 `json:"current_stage"`
	Stages        map[string]StageStatus `json:"stages"`
	Iterations    map[string]int         `json:"iterations,omitempty"`
	Budgets       BudgetState            `json:"budgets"`
	ActiveWorker  string                 `json:"active_worker,omitempty"`
	BlockedReason string                 `json:"blocked_reason,omitempty"`
	NextAction    string                 `json:"next_action,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// Terminal reports whether the run has reached a state from which it will not
// continue without a new command.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusCanceled, StatusFailed:
		return true
	default:
		return false
	}
}

// Settled reports whether the run has stopped progressing on its own: terminal,
// or stable-but-awaiting-a-human (blocked, paused). Only Active runs keep
// moving. `status --watch` stops following once a run is settled.
func (s RunStatus) Settled() bool {
	return s != StatusActive
}
