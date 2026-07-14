package core

import (
	"math"
	"time"
)

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

// TokensTotalSpent is the sum of input and output tokens consumed by the run. It saturates:
// the per-dimension counters already clamp at MaxInt, so a plain int add of two near-MaxInt
// values would WRAP to a large negative — resetting the run's total below its cap, the one
// number a runaway must never reset. saturatingAdd keeps `total >= max` true.
func (b BudgetState) TokensTotalSpent() int {
	return saturatingAdd(b.TokensInSpent, b.TokensOutSpent)
}

// saturatingAdd returns a+b, clamping at MaxInt/MinInt instead of wrapping on overflow.
func saturatingAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	if b < 0 && a < math.MinInt-b {
		return math.MinInt
	}
	return a + b
}

// StageTokensTotal is the total tokens (in + out) spent within a single stage.
func (b BudgetState) StageTokensTotal(stage string) int {
	return saturatingAdd(b.StageTokensIn[stage], b.StageTokensOut[stage])
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
	// DriverTokenHash is the sha256 of the run's DRIVER TOKEN — the capability that says
	// "I am the orchestrator of this run". Every command that MUTATES the run requires the
	// token; `status` and `observe` do not, because reading is harmless.
	//
	// It exists because a host's permission rules are SESSION-WIDE. `Bash(vichu worker
	// complete:*)` is authorized for the orchestrator, and therefore also for every subagent
	// that has Bash — including the implementer, which needs it to run the project's tests.
	// Without a capability, that implementer can close its OWN worker and then keep editing
	// files: the audit stopped at the close, so the later changes are invisible and can never
	// block the run. The permission layer cannot distinguish the two callers; the kernel can.
	//
	// Only the HASH is persisted. The token itself is returned once, to the orchestrator, and
	// never written to `.vichu/` — so a subagent that can read the runtime still cannot drive
	// the run. `run resume` (a human action) ROTATES it, which is what makes a leaked token
	// recoverable.
	DriverTokenHash string    `json:"driver_token_hash,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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
