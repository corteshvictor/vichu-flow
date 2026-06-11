// Package workflows defines the staged DAGs the engine executes. v0.1 ships the
// linear `quick` workflow; stages already carry the fields (gates, scope, next)
// the richer `sdd` workflow will need.
package workflows

import "fmt"

// Kind distinguishes how the engine runs a stage.
type Kind string

const (
	// KindWorker runs an agent worker via an adapter.
	KindWorker Kind = "worker"
	// KindGate runs verification commands the runtime executes itself.
	KindGate Kind = "gate"
	// KindTerminal ends the run successfully.
	KindTerminal Kind = "terminal"
)

// Stage is one node in a workflow.
type Stage struct {
	Name        string
	Kind        Kind
	Role        string   // worker stages: which agent role to invoke
	Gates       []string // gate stages: which command names to run (test, lint, typecheck)
	Scope       []string // expected mutation scope globs (empty = unrestricted)
	ReadOnly    bool     // worker stages: any mutation blocks the run
	Instruction string   // worker stages: what to tell the agent
	Next        string   // next stage name ("" only for terminal)
}

// Workflow is an ordered set of stages with a starting point.
type Workflow struct {
	Name   string
	Start  string
	Stages []Stage
}

// Stage looks up a stage by name.
func (w *Workflow) Stage(name string) (Stage, bool) {
	for _, s := range w.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// Quick is the minimal workflow used to validate the engine end to end:
// explore → implement → verify(gates) → done.
func Quick() *Workflow {
	return &Workflow{
		Name:  "quick",
		Start: "explore",
		Stages: []Stage{
			{
				Name:        "explore",
				Kind:        KindWorker,
				Role:        "explorer",
				ReadOnly:    true, // enforced by the runtime, not just the prompt
				Instruction: "Explore the repository and summarize what is relevant to the task. Do not modify files.",
				Next:        "implement",
			},
			{
				Name:        "implement",
				Kind:        KindWorker,
				Role:        "implementer",
				Instruction: "Implement the task. Make the minimal change needed and keep it consistent with the project's conventions.",
				Next:        "verify",
			},
			{
				Name:  "verify",
				Kind:  KindGate,
				Gates: []string{"test", "lint", "typecheck"},
				Next:  "done",
			},
			{
				Name: "done",
				Kind: KindTerminal,
			},
		},
	}
}

// Get returns a built-in workflow by name.
func Get(name string) (*Workflow, error) {
	switch name {
	case "", "quick":
		return Quick(), nil
	default:
		return nil, fmt.Errorf("unknown workflow %q (v0.1 ships: quick)", name)
	}
}

// AllStagesPending returns a fresh stage-status map with every stage pending.
func (w *Workflow) StageNames() []string {
	names := make([]string, 0, len(w.Stages))
	for _, s := range w.Stages {
		names = append(names, s.Name)
	}
	return names
}
