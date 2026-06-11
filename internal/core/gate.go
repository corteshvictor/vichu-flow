package core

import "time"

// GateCommand is the exact command a gate runs, persisted to command.json so a
// run's verification is fully auditable and reproducible.
type GateCommand struct {
	Name    string   `json:"name"`    // test, lint, typecheck
	Command string   `json:"command"` // resolved executable
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir"`
}

// GateVerdict is the verified result of running a gate, persisted to
// verdict.json. This — not any markdown the agent writes — is what authorizes a
// stage transition. The full output lives at OutputPath.
type GateVerdict struct {
	Name        string    `json:"name"`
	Command     string    `json:"command"`
	ExitCode    int       `json:"exit_code"`
	Passed      bool      `json:"passed"`
	DurationMS  int64     `json:"duration_ms"`
	OutputPath  string    `json:"output_path"`
	OutputBytes int64     `json:"output_bytes"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}
