package core

import "time"

// Event is one record in the append-only events.ndjson audit trail. Adapters
// normalize their native output into these so every run has a uniform timeline.
type Event struct {
	TS      time.Time `json:"ts"`
	Run     string    `json:"run"`
	Stage   string    `json:"stage,omitempty"`
	Worker  string    `json:"worker,omitempty"`
	Adapter string    `json:"adapter,omitempty"`
	Event   string    `json:"event"`
	// OpID and Seq identify an event emitted inside a host-first transactional command.
	// A retry REPLAYS the whole operation (that is how it recovers), which would append
	// its events a second time — and events.ndjson is the public audit trail, so a
	// duplicate makes it ambiguous how many operations actually happened. The pair
	// (op_id, seq) is stable across replays, so the append can skip what it already has:
	// a retried operation leaves exactly one copy of each event.
	OpID string `json:"op_id,omitempty"`
	// OpFP is the operation's fingerprint (kind + identifying args + payload). Dedup keys on
	// (op_id, op_fp), not op_id alone: an op-id whose record failed to write can be reused for
	// a DIFFERENT operation, and keying on the id alone made that operation's events get
	// suppressed as "already written" — silently deleting evidence from the audit trail.
	OpFP   string         `json:"op_fp,omitempty"`
	Seq    int            `json:"seq,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
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
