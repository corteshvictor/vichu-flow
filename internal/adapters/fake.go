package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

// FakeScript drives the fake adapter deterministically: per-role actions and an
// optional result text. It can be provided programmatically (tests) or loaded
// from the JSON file named by VICHU_FAKE_SCRIPT (CLI end-to-end runs).
type FakeScript struct {
	ResultText string                  `json:"result_text,omitempty"`
	CostUSD    float64                 `json:"cost_usd,omitempty"`  // simulated per-invocation cost
	TokensIn   int                     `json:"tokens_in,omitempty"` // simulated per-invocation usage
	TokensOut  int                     `json:"tokens_out,omitempty"`
	Actions    map[string][]FakeAction `json:"actions,omitempty"`
}

// Fake is a deterministic adapter used by the conformance suite and CI. It
// never touches the network and produces reproducible mutations so the engine's
// gates and mutation tracking have something real to observe.
type Fake struct {
	script FakeScript
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

func (f *Fake) Resume(ctx context.Context, _ string, inv Invocation) (Session, error) {
	// The fake adapter supports resume by simply re-running deterministically.
	return f.run(ctx, inv)
}

func (f *Fake) run(_ context.Context, inv Invocation) (Session, error) {
	events := make(chan AgentEvent, 16)
	actions := f.script.Actions[inv.Role]

	go func() {
		defer close(events)
		events <- AgentEvent{Kind: EventText, Text: "fake worker started for role " + inv.Role}
		for _, a := range actions {
			switch a.Type {
			case "write_file":
				full := filepath.Join(inv.WorkDir, a.Path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					events <- AgentEvent{Kind: EventError, Text: err.Error()}
					continue
				}
				if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
					events <- AgentEvent{Kind: EventError, Text: err.Error()}
					continue
				}
				events <- AgentEvent{Kind: EventToolUse, Detail: map[string]any{"tool": "write_file", "path": a.Path}}
			case "tool_use":
				events <- AgentEvent{Kind: EventToolUse, Detail: map[string]any{"tool": a.Text}}
			case "text":
				events <- AgentEvent{Kind: EventText, Text: a.Text}
			}
		}
		events <- AgentEvent{Kind: EventDone}
	}()

	result := core.Result{
		Markdown:    f.resultText(inv.Role),
		Data:        map[string]any{"role": inv.Role},
		CostUSD:     f.script.CostUSD,
		TokensIn:    f.script.TokensIn,
		TokensOut:   f.script.TokensOut,
		SessionID:   "fake-session-" + inv.Role,
		ExitMessage: "ok",
	}
	return &bufferedSession{events: events, result: result}, nil
}

func (f *Fake) resultText(role string) string {
	if f.script.ResultText != "" {
		return f.script.ResultText
	}
	return "fake result for role " + role
}
