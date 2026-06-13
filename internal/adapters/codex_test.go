package adapters

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// runCodexStub is invoked from TestMain when VICHU_CODEX_STUB=1: the test binary
// impersonates the codex CLI, answering `--version` / `login status`, draining
// stdin, and emitting a canned JSONL thread-event conversation. Tests point the
// adapter's Bin at os.Args[0] so the same binary plays both roles on every OS.
func runCodexStub() {
	args := os.Args[1:]
	if handleCodexStubVersion(args) || handleCodexStubLogin(args) {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	emitCodexConversation(codexStubResumeID(args))
	os.Exit(0)
}

func handleCodexStubVersion(args []string) bool {
	for _, a := range args {
		if a == "--version" {
			v := os.Getenv("VICHU_CODEX_STUB_VERSION")
			if v == "" {
				v = "codex-cli 0.30.0"
			}
			fmt.Println(v)
			os.Exit(0)
		}
	}
	return false
}

func handleCodexStubLogin(args []string) bool {
	if len(args) < 2 || args[0] != "login" || args[1] != "status" {
		return false
	}
	if os.Getenv("VICHU_CODEX_STUB_LOGGED_OUT") == "1" {
		fmt.Println("Not logged in")
		os.Exit(1)
	}
	fmt.Println("Logged in using ChatGPT")
	os.Exit(0)
	return true
}

// codexStubResumeID returns the id after `exec resume <id>`, or "".
func codexStubResumeID(args []string) string {
	for i, a := range args {
		if a == "resume" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func emitCodexConversation(resumed string) {
	fmt.Println(`{"type":"thread.started","thread_id":"th-abc"}`)
	fmt.Println(`{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`)
	fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","exit_code":0}}`)
	if resumed != "" {
		fmt.Printf(`{"type":"item.completed","item":{"type":"agent_message","text":"resumed %s"}}`+"\n", resumed)
		fmt.Printf(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}` + "\n")
		return
	}
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"all done"}}`)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":45}}`)
}

func codexStubAdapter(t *testing.T) *Codex {
	t.Helper()
	t.Setenv("VICHU_CODEX_STUB", "1")
	return NewCodex(CodexOptions{Bin: os.Args[0]})
}

func TestCodexConformance(t *testing.T) {
	a := codexStubAdapter(t)
	runConformance(t, a, Invocation{Role: "implementer", Prompt: "do it", WorkDir: t.TempDir()})
}

func TestCodexResultFields(t *testing.T) {
	a := codexStubAdapter(t)
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
			if ev.Detail["tool"] == "shell" && ev.Detail["command"] != "go test ./..." {
				t.Errorf("command detail not normalized: %v", ev.Detail)
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
		t.Errorf("result markdown (last assistant message): %q", res.Markdown)
	}
	if res.SessionID != "th-abc" {
		t.Errorf("thread id not captured as session id: %q", res.SessionID)
	}
	if res.TokensIn != 120 || res.TokensOut != 45 {
		t.Errorf("usage not captured: in=%d out=%d", res.TokensIn, res.TokensOut)
	}
}

func TestCodexResumePassesSessionID(t *testing.T) {
	a := codexStubAdapter(t)
	sess, err := a.Resume(context.Background(), "th-prev", Invocation{Prompt: "continue", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	drainEvents(sess.Events())
	res, err := sess.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if !strings.Contains(res.Markdown, "resumed th-prev") {
		t.Fatalf("resume <id> not forwarded: %q", res.Markdown)
	}
}

func TestCodexBuildArgsResume(t *testing.T) {
	c := NewCodex(CodexOptions{Bin: "codex"})

	fresh := c.buildArgs(Invocation{}, "")
	if fresh[0] != "exec" || fresh[len(fresh)-1] != "-" {
		t.Fatalf("fresh args should be `exec … -`, got %v", fresh)
	}
	if indexOf(fresh, "resume") >= 0 {
		t.Fatalf("fresh args must not contain resume: %v", fresh)
	}

	resumed := c.buildArgs(Invocation{}, "th-1")
	// exec-level flags MUST precede the resume subcommand, else the real CLI
	// rejects them. Expect `exec --json --sandbox <mode> … resume th-1 -`.
	ri := indexOf(resumed, "resume")
	if ri < 0 || resumed[ri+1] != "th-1" {
		t.Fatalf("resume args should contain `resume th-1`, got %v", resumed)
	}
	if resumed[len(resumed)-1] != "-" {
		t.Fatalf("resume args should end with the stdin marker `-`, got %v", resumed)
	}
	if si := indexOf(resumed, "--sandbox"); si < 0 || si > ri {
		t.Fatalf("--sandbox must come BEFORE resume, got %v", resumed)
	}
	if ji := indexOf(resumed, "--json"); ji < 0 || ji > ri {
		t.Fatalf("--json must come BEFORE resume, got %v", resumed)
	}
}

func TestCodexBuildArgsReadOnly(t *testing.T) {
	c := NewCodex(CodexOptions{Bin: "codex"}) // default sandbox workspace-write

	ro := c.buildArgs(Invocation{ReadOnly: true}, "")
	if i := indexOf(ro, "--sandbox"); i < 0 || ro[i+1] != "read-only" {
		t.Fatalf("a read-only invocation must run with --sandbox read-only, got %v", ro)
	}
	rw := c.buildArgs(Invocation{}, "")
	if i := indexOf(rw, "--sandbox"); i < 0 || rw[i+1] != "workspace-write" {
		t.Fatalf("a normal invocation keeps the configured sandbox, got %v", rw)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func TestCodexBuildArgsEffort(t *testing.T) {
	c := NewCodex(CodexOptions{Bin: "codex"})

	args := c.buildArgs(Invocation{Effort: "high"}, "")
	if i := indexOf(args, "-c"); i < 0 || args[i+1] != "model_reasoning_effort=high" {
		t.Fatalf("effort should map to `-c model_reasoning_effort=high`, got %v", args)
	}
	// No effort configured → no -c flag.
	if i := indexOf(c.buildArgs(Invocation{}, ""), "-c"); i >= 0 {
		t.Fatal("no effort must add no -c flag")
	}
}

func TestCodexProbeVersionAndAuth(t *testing.T) {
	cases := []struct {
		name        string
		stubVersion string // VICHU_CODEX_STUB_VERSION ("" = default supported)
		loggedOut   bool   // VICHU_CODEX_STUB_LOGGED_OUT
		apiKey      string // OPENAI_API_KEY
		wantAvail   bool
		wantText    string // substring expected in Version (available) or Reason (not)
	}{
		{name: "supported and logged in", wantAvail: true, wantText: "0.30.0"},
		{name: "unsupported version degrades", stubVersion: "codex-cli 9.0.0", wantText: "unsupported"},
		{name: "logged out degrades", loggedOut: true, wantText: "not authenticated"},
		{name: "api key authenticates without login check", loggedOut: true, apiKey: "sk-test", wantAvail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := codexStubAdapter(t)
			t.Setenv("VICHU_CODEX_STUB_VERSION", tc.stubVersion)
			t.Setenv("VICHU_CODEX_STUB_LOGGED_OUT", boolEnv(tc.loggedOut))
			t.Setenv("OPENAI_API_KEY", tc.apiKey)
			t.Setenv("CODEX_API_KEY", "")

			av, err := a.Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertProbe(t, av, tc.wantAvail, tc.wantText)
		})
	}
}

// assertProbe checks a probe's availability and that wantText appears in the
// field that carries it (Version when available, Reason when not).
func assertProbe(t *testing.T, av Availability, wantAvail bool, wantText string) {
	t.Helper()
	if av.Available != wantAvail {
		t.Fatalf("available = %v, want %v (%+v)", av.Available, wantAvail, av)
	}
	field := av.Reason
	if wantAvail {
		field = av.Version
	}
	if wantText != "" && !strings.Contains(field, wantText) {
		t.Fatalf("want %q in probe result, got %+v", wantText, av)
	}
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return ""
}

func TestCodexProbeMissingBinary(t *testing.T) {
	a := NewCodex(CodexOptions{Bin: "definitely-not-codex-xyz"})
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

func TestDecodeCodexLine(t *testing.T) {
	var oc codexOutcome

	if evs := decodeCodexLine([]byte(`{"type":"thread.started","thread_id":"t1"}`), &oc); len(evs) != 0 {
		t.Fatalf("thread.started should emit no events, got %v", evs)
	}
	if oc.threadID != "t1" {
		t.Fatalf("thread id not captured: %q", oc.threadID)
	}

	evs := decodeCodexLine([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`), &oc)
	if len(evs) != 1 || evs[0].Kind != EventText || oc.lastMessage != "hi" {
		t.Fatalf("agent_message not normalized: evs=%v last=%q", evs, oc.lastMessage)
	}

	evs = decodeCodexLine([]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"ls","exit_code":0}}`), &oc)
	if len(evs) != 1 || evs[0].Kind != EventToolUse || evs[0].Detail["command"] != "ls" {
		t.Fatalf("command_execution not normalized: %v", evs)
	}

	decodeCodexLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":7}}`), &oc)
	if !oc.completed || oc.tokensIn != 3 || oc.tokensOut != 7 {
		t.Fatalf("turn.completed not folded: %+v", oc)
	}

	// Garbage must be skipped, never panic.
	if evs := decodeCodexLine([]byte(`not json at all`), &oc); evs != nil {
		t.Fatal("garbage line should be ignored")
	}
}

func TestCodexErrorTurnIsError(t *testing.T) {
	var oc codexOutcome
	decodeCodexLine([]byte(`{"type":"turn.failed","error":{"message":"model overloaded"}}`), &oc)
	_, err := buildCodexResult(oc, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("turn.failed must produce an error result, got %v", err)
	}
}
