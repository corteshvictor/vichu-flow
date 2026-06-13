package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// FakeName is the registry name of the deterministic test adapter.
const FakeName = "fake"

// FakeAction is a single scripted step the fake adapter performs when a worker
// runs. Type is "write_file", "text", or "tool_use".
type FakeAction struct {
	Type    string `json:"type"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Text    string `json:"text,omitempty"`
}

// FakeVerdict is a scripted reviewer outcome the fake returns in Result.Data so
// the engine can exercise the review loop without a real agent.
type FakeVerdict struct {
	Status   string         `json:"status"` // approved | needs_fixes | blocked
	Summary  string         `json:"summary,omitempty"`
	Findings []core.Finding `json:"findings,omitempty"`
}

// FakeScript drives the fake adapter deterministically: per-role actions and an
// optional result text. It can be provided programmatically (tests) or loaded
// from the JSON file named by VICHU_FAKE_SCRIPT (CLI end-to-end runs).
type FakeScript struct {
	ResultText string  `json:"result_text,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`  // simulated per-invocation cost
	TokensIn   int     `json:"tokens_in,omitempty"` // simulated per-invocation usage
	TokensOut  int     `json:"tokens_out,omitempty"`
	// ResultErr, when set, makes Result return this error while still reporting
	// the scripted cost/tokens — for testing the failed-worker accounting path.
	ResultErr string                  `json:"result_err,omitempty"`
	Actions   map[string][]FakeAction `json:"actions,omitempty"`
	// Verdicts maps a role to a sequence of verdicts indexed by the engine's
	// 1-based stage iteration (verdict[0] for the first review, [1] for the next,
	// …); the last entry repeats once exhausted. Selection uses Invocation.Iteration,
	// so it works even when the registry builds a fresh Fake per stage (e.g. via
	// VICHU_FAKE_SCRIPT) — no shared instance required.
	Verdicts map[string][]FakeVerdict `json:"verdicts,omitempty"`
}

// Fake is a deterministic adapter used by the conformance suite and CI. It
// never touches the network and produces reproducible mutations so the engine's
// gates and mutation tracking have something real to observe.
type Fake struct {
	script      FakeScript
	mu          sync.Mutex
	resumedWith []string // session ids passed to Resume, for resume tests
}

// NewFake builds a fake adapter from a script.
func NewFake(script FakeScript) *Fake { return &Fake{script: script} }

// NewFakeFromEnv builds a fake adapter, loading its script from the file named
// by VICHU_FAKE_SCRIPT if set. Used when the fake adapter is selected via config.
func NewFakeFromEnv() (*Fake, error) {
	path := os.Getenv("VICHU_FAKE_SCRIPT")
	if path == "" {
		return &Fake{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading VICHU_FAKE_SCRIPT: %w", err)
	}
	var sc FakeScript
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("parsing VICHU_FAKE_SCRIPT: %w", err)
	}
	return &Fake{script: sc}, nil
}

func (f *Fake) Name() string { return FakeName }

func (f *Fake) Probe(context.Context) (Availability, error) {
	return Availability{Name: FakeName, Available: true, Version: "fake"}, nil
}

func (f *Fake) Capabilities() Caps {
	return Caps{Streaming: true, Resume: true, CostReporting: true, StructuredOutput: true}
}

func (f *Fake) Start(ctx context.Context, inv Invocation) (Session, error) {
	return f.run(ctx, inv)
}

func (f *Fake) Resume(ctx context.Context, sessionID string, inv Invocation) (Session, error) {
	// The fake adapter supports resume by simply re-running deterministically; it
	// records the session id so tests can assert the engine continued the right
	// agent session instead of starting cold.
	f.mu.Lock()
	f.resumedWith = append(f.resumedWith, sessionID)
	f.mu.Unlock()
	return f.run(ctx, inv)
}

// ResumedWith returns the session ids passed to Resume, in call order. Used by
// engine resume tests to verify agent-session continuity.
func (f *Fake) ResumedWith() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resumedWith...)
}

func (f *Fake) run(_ context.Context, inv Invocation) (Session, error) {
	events := make(chan AgentEvent, 16)
	go f.streamActions(events, inv)
	result := core.Result{
		Markdown:    f.resultText(inv.Role),
		Data:        f.resultData(inv.Role, inv.Iteration),
		CostUSD:     f.script.CostUSD,
		TokensIn:    f.script.TokensIn,
		TokensOut:   f.script.TokensOut,
		SessionID:   "fake-session-" + inv.Role,
		ExitMessage: "ok",
	}
	sess := &bufferedSession{events: events, result: result}
	if f.script.ResultErr != "" {
		sess.err = errors.New(f.script.ResultErr)
	}
	return sess, nil
}

// streamActions plays the role's scripted events onto the channel (performing
// any file writes), then closes it. Run as a goroutine by run/Resume.
func (f *Fake) streamActions(events chan AgentEvent, inv Invocation) {
	defer close(events)
	events <- AgentEvent{Kind: EventText, Text: "fake worker started for role " + inv.Role}
	for _, a := range f.script.Actions[inv.Role] {
		switch a.Type {
		case "write_file":
			f.writeFileAction(events, inv.WorkDir, a)
		case "tool_use":
			events <- AgentEvent{Kind: EventToolUse, Detail: map[string]any{"tool": a.Text}}
		case "text":
			events <- AgentEvent{Kind: EventText, Text: a.Text}
		}
	}
	events <- AgentEvent{Kind: EventDone}
}

// writeFileAction performs a scripted file write and emits the matching event,
// surfacing any filesystem error as an EventError instead of a panic.
func (f *Fake) writeFileAction(events chan AgentEvent, workDir string, a FakeAction) {
	full := filepath.Join(workDir, a.Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		events <- AgentEvent{Kind: EventError, Text: err.Error()}
		return
	}
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
		events <- AgentEvent{Kind: EventError, Text: err.Error()}
		return
	}
	events <- AgentEvent{Kind: EventToolUse, Detail: map[string]any{"tool": "write_file", "path": a.Path}}
}

// resultData builds the worker's machine-readable payload: the role plus, when
// a verdict is scripted for that role, the verdict for this engine iteration.
// Selection is driven by the engine's 1-based iteration (not adapter-internal
// state), so a fresh Fake per stage still advances needs_fixes → approved; the
// last scripted verdict repeats once the script is exhausted.
func (f *Fake) resultData(role string, iteration int) map[string]any {
	data := map[string]any{"role": role}
	vs := f.script.Verdicts[role]
	if len(vs) == 0 {
		return data
	}
	idx := iteration - 1 // iteration is 1-based; verdicts are 0-indexed
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vs) {
		idx = len(vs) - 1 // repeat the last scripted verdict
	}
	v := vs[idx]
	data["status"] = v.Status
	data["summary"] = v.Summary
	data["findings"] = v.Findings
	return data
}

func (f *Fake) resultText(role string) string {
	if f.script.ResultText != "" {
		return f.script.ResultText
	}
	return "fake result for role " + role
}
