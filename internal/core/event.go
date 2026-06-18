package core

import "time"

// Event is one record in the append-only events.ndjson audit trail. Adapters
// normalize their native output into these so every run has a uniform timeline.
type Event struct {
	TS      time.Time      `json:"ts"`
	Run     string         `json:"run"`
	Stage   string         `json:"stage,omitempty"`
	Worker  string         `json:"worker,omitempty"`
	Adapter string         `json:"adapter,omitempty"`
	Event   string         `json:"event"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// Canonical event names. Adapters and the engine emit these; tooling can rely
// on them as a stable vocabulary.
const (
	EventRunCreated             = "run_created"
	EventRunResumed             = "run_resumed"
	EventRunCanceled            = "run_canceled"
	EventRunCompleted           = "run_completed"
	EventRunBlocked             = "run_blocked"
	EventRunFailed              = "run_failed"
	EventStageStarted           = "stage_started"
	EventStageCompleted         = "stage_completed"
	EventStageTransition        = "stage_transition"
	EventWorkerStarted          = "worker_started"
	EventWorkerFinished         = "worker_finished"
	EventWorkerInterrupted      = "worker_interrupted" // a running worker found dead on resume
	EventWorkerResumed          = "worker_resumed"     // an agent session continued on resume
	EventWorkerResumeFailed     = "worker_resume_failed"
	EventToolUse                = "tool_use"
	EventAgentText              = "agent_text"
	EventGateStarted            = "gate_started"
	EventGateCompleted          = "gate_completed"
	EventReviewCompleted        = "review_completed"
	EventReviewFindings         = "review_findings"
	EventReviewContextTruncated = "review_context_truncated"
	EventMutationTracked        = "mutation_tracked"
	EventArtifactSaved          = "artifact_saved"
	EventOutOfScopeMut          = "out_of_scope_mutation"
	EventWorkspaceDrift         = "workspace_drift"
	EventBudgetExceeded         = "budget_exceeded"
	EventOutputTruncated        = "output_truncated"
)
