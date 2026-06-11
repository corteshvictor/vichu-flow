package adapters

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// TestMain doubles as a cross-platform stub for the claude CLI: when
// VICHU_CC_STUB=1 the test binary ignores its arguments, drains stdin, and
// emits a canned stream-json conversation. Tests point the adapter's Bin at
// os.Args[0] so the same binary plays both roles on every OS.
func TestMain(m *testing.M) {
	if os.Getenv("VICHU_CC_STUB") == "1" {
		runClaudeStub()
		return
	}
	os.Exit(m.Run())
}

func runClaudeStub() {
	args := os.Args[1:]
	if handleStubVersion(args) || handleStubAuth(args) {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	emitStubConversation(stubResumeID(args))
	os.Exit(0)
}

// handleStubVersion answers `--version` and reports whether it did.
func handleStubVersion(args []string) bool {
	for _, a := range args {
		if a == "--version" {
			v := os.Getenv("VICHU_CC_STUB_VERSION")
			if v == "" {
				v = "2.1.170 (stub)"
			}
			fmt.Println(v)
			os.Exit(0)
		}
	}
	return false
}

// handleStubAuth answers `auth status` and reports whether it did.
func handleStubAuth(args []string) bool {
	if len(args) < 2 || args[0] != "auth" || args[1] != "status" {
		return false
	}
	logged := "true"
	if os.Getenv("VICHU_CC_STUB_LOGGED_OUT") == "1" {
		logged = "false"
	}
	fmt.Printf(`{"loggedIn": %s, "authMethod": "stub"}`+"\n", logged)
	os.Exit(0)
	return true
}

func stubResumeID(args []string) string {
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func emitStubConversation(resumed string) {
	fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-abc"}`)
	fmt.Println(`{"type":"assistant","session_id":"sess-abc","message":{"content":[{"type":"text","text":"working on it"}]}}`)
	fmt.Println(`{"type":"assistant","session_id":"sess-abc","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"src/x.go"}}]}}`)
	if resumed != "" {
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"resumed %s","session_id":"%s","total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":5}}`+"\n", resumed, resumed)
		return
	}
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"sess-abc","total_cost_usd":0.0421,"usage":{"input_tokens":120,"output_tokens":45}}`)
}

func stubAdapter(t *testing.T) *ClaudeCode {
	t.Helper()
	t.Setenv("VICHU_CC_STUB", "1")
	return NewClaudeCode(ClaudeCodeOptions{Bin: os.Args[0]})
}

// drainEvents consumes a session's event stream until it closes, returning how
// many events it saw. A session finishes once its stream is drained.
func drainEvents(ch <-chan AgentEvent) int {
	n := 0
	for range ch {
		n++
	}
	return n
}

func TestClaudeCodeConformance(t *testing.T) {
	a := stubAdapter(t)
	runConformance(t, a, Invocation{Role: "implementer", Prompt: "do it", WorkDir: t.TempDir()})
}

func TestClaudeCodeResultFields(t *testing.T) {
	a := stubAdapter(t)
	sess, err := a.Start(context.Background(), Invocation{Prompt: "p", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var texts, tools int
	for ev := range sess.Events() {
		switch ev.Kind {
		case EventText:
			texts++
		case EventToolUse:
			tools++
			if ev.Detail["tool"] != "Edit" || ev.Detail["file_path"] != "src/x.go" {
				t.Errorf("tool_use detail not normalized: %v", ev.Detail)
			}
		}
	}
	if texts == 0 || tools == 0 {
		t.Fatalf("expected text and tool_use events, got texts=%d tools=%d", texts, tools)
	}

	res, err := sess.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Markdown != "all done" {
		t.Errorf("result markdown: %q", res.Markdown)
	}
	if res.SessionID != "sess-abc" {
		t.Errorf("session id not captured: %q", res.SessionID)
	}
	if res.CostUSD != 0.0421 || res.TokensIn != 120 || res.TokensOut != 45 {
		t.Errorf("usage not captured: cost=%v in=%d out=%d", res.CostUSD, res.TokensIn, res.TokensOut)
	}
}

func TestClaudeCodeResumePassesSessionID(t *testing.T) {
	a := stubAdapter(t)
	sess, err := a.Resume(context.Background(), "sess-prev", Invocation{Prompt: "continue", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	drainEvents(sess.Events())
	res, err := sess.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if !strings.Contains(res.Markdown, "resumed sess-prev") {
		t.Fatalf("--resume not forwarded: %q", res.Markdown)
	}
}

func TestClaudeCodeProbeVersionAndAuth(t *testing.T) {
	t.Run("supported and logged in", func(t *testing.T) {
		a := stubAdapter(t)
		av, err := a.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !av.Available || !strings.HasPrefix(av.Version, "2.1.170") {
			t.Fatalf("expected available 2.1.170, got %+v", av)
		}
	})
	t.Run("unsupported version degrades", func(t *testing.T) {
		a := stubAdapter(t)
		t.Setenv("VICHU_CC_STUB_VERSION", "9.0.0 (stub)")
		av, err := a.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if av.Available || !strings.Contains(av.Reason, "unsupported") {
			t.Fatalf("9.x must degrade with reason, got %+v", av)
		}
	})
	t.Run("logged out degrades", func(t *testing.T) {
		a := stubAdapter(t)
		t.Setenv("VICHU_CC_STUB_LOGGED_OUT", "1")
		av, err := a.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if av.Available || !strings.Contains(av.Reason, "not authenticated") {
			t.Fatalf("logged-out CLI must degrade with reason, got %+v", av)
		}
	})
}

func TestClaudeCodeDisallowedToolsFlag(t *testing.T) {
	a := stubAdapter(t)
	sess, err := a.Start(context.Background(), Invocation{
		Prompt:          "p",
		WorkDir:         t.TempDir(),
		DisallowedTools: []string{"Bash(git push:*)", "Bash(sudo:*)"},
	})
	if err != nil {
		t.Fatalf("Start with disallowed tools must work: %v", err)
	}
	drainEvents(sess.Events())
	if _, err := sess.Result(context.Background()); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

func TestParseMajor(t *testing.T) {
	cases := []struct {
		in    string
		major int
		ok    bool
	}{
		{"2.1.170 (Claude Code)", 2, true},
		{"1.0.3", 1, true},
		{"10.2", 10, true},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		major, ok := parseMajor(c.in)
		if major != c.major || ok != c.ok {
			t.Errorf("parseMajor(%q) = %d,%v want %d,%v", c.in, major, ok, c.major, c.ok)
		}
	}
}

func TestClaudeCodeProbeMissingBinary(t *testing.T) {
	a := NewClaudeCode(ClaudeCodeOptions{Bin: "definitely-not-claude-xyz"})
	av, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe should degrade, not error: %v", err)
	}
	if av.Available {
		t.Fatal("missing binary must report unavailable")
	}
	if av.Reason == "" {
		t.Fatal("unavailable must carry an actionable reason")
	}
}

func TestDecodeClaudeLine(t *testing.T) {
	evs, sid, final := decodeClaudeLine([]byte(`{"type":"system","subtype":"init","session_id":"s1"}`))
	if len(evs) != 0 || sid != "s1" || final != nil {
		t.Fatalf("init line: evs=%v sid=%q final=%v", evs, sid, final)
	}

	evs, _, _ = decodeClaudeLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash","input":{"command":"go test"}}]}}`))
	if len(evs) != 2 || evs[0].Kind != EventText || evs[1].Kind != EventToolUse {
		t.Fatalf("assistant line not normalized: %v", evs)
	}

	_, _, final = decodeClaudeLine([]byte(`{"type":"result","subtype":"success","result":"ok","total_cost_usd":1.5,"session_id":"s1","usage":{"input_tokens":3,"output_tokens":7}}`))
	if final == nil || final.Result != "ok" || final.TotalCostUSD != 1.5 || final.Usage.OutputTokens != 7 {
		t.Fatalf("result line not parsed: %+v", final)
	}

	// Garbage must be skipped, never panic.
	evs, sid, final = decodeClaudeLine([]byte(`not json at all`))
	if evs != nil || sid != "" || final != nil {
		t.Fatal("garbage line should be ignored")
	}
}

func TestClaudeCodeErrorResult(t *testing.T) {
	// Reuse the stub but force an error result via a dedicated env knob is
	// overkill; instead verify the decode path flags is_error.
	_, _, final := decodeClaudeLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out","session_id":"s"}`))
	if final == nil || !final.IsError {
		t.Fatal("is_error result must be flagged")
	}
}
