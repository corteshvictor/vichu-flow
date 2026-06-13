// Package workflows defines the staged DAGs the engine executes: the linear
// `quick` workflow (explore → implement → verify) and `review`, which adds an
// adversarial review → auto-fix loop that branches on a structured verdict.
package workflows

import "fmt"

// Kind distinguishes how the engine runs a stage.
type Kind string

const (
	// KindWorker runs an agent worker via an adapter.
	KindWorker Kind = "worker"
	// KindReview runs an agent worker (like KindWorker) but then REQUIRES a valid
	// structured verdict and branches on it. A review is not pass/fail: the
	// reviewer doing its job and asking for changes is a needs_fixes verdict.
	KindReview Kind = "review"
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
	Next        string   // next stage (worker/gate stages); "" only for terminal
	// Review stages (KindReview) transition on the verdict, not Next. A "blocked"
	// verdict has no target — it blocks the run for a human.
	NextOnApproved   string // verdict "approved"   → advance here
	NextOnNeedsFixes string // verdict "needs_fixes" → loop here (typically a fix stage)
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

// Review adds an adversarial review loop on top of quick:
// explore → implement → review → (approved: verify → done) / (needs_fixes: fix → review).
// The loop is bounded by workflow.maxAutoIterations; a "blocked" verdict stops
// the run for a human instead of looping.
func Review() *Workflow {
	return &Workflow{
		Name:  "review",
		Start: "explore",
		Stages: []Stage{
			{
				Name:        "explore",
				Kind:        KindWorker,
				Role:        "explorer",
				ReadOnly:    true,
				Instruction: "Explore the repository and summarize what is relevant to the task. Do not modify files.",
				Next:        "implement",
			},
			{
				Name:        "implement",
				Kind:        KindWorker,
				Role:        "implementer",
				Instruction: "Implement the task. Make the minimal change needed and keep it consistent with the project's conventions.",
				Next:        "review",
			},
			{
				Name:     "review",
				Kind:     KindReview,
				Role:     "reviewer",
				ReadOnly: true, // a reviewer judges; it must not change the tree
				Instruction: "Review the implementation against the task. Investigate as needed, then END your " +
					"reply with a single JSON object on its own line and NOTHING after it:\n" +
					"{\"status\": \"approved\" | \"needs_fixes\" | \"blocked\", \"summary\": \"<one line>\", " +
					"\"findings\": [{\"severity\": \"blocker\" | \"major\" | \"minor\", \"file\": \"<path>\", \"message\": \"<what to fix>\"}]}\n" +
					"Use \"approved\" if it is correct and complete; \"needs_fixes\" (with findings) if there are " +
					"defects to address; \"blocked\" if the task cannot be done safely or is underspecified.",
				NextOnApproved:   "verify",
				NextOnNeedsFixes: "fix",
			},
			{
				Name:        "fix",
				Kind:        KindWorker,
				Role:        "implementer",
				Instruction: "Address the reviewer's findings from the previous review. Make the minimal changes needed; do not introduce unrelated changes.",
				Next:        "review",
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
	case "review":
		return Review(), nil
	default:
		return nil, fmt.Errorf("unknown workflow %q (built-in: quick, review)", name)
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
