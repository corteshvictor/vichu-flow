package adapters

import "encoding/json"

// codexEventLine is the union shape of one JSONL line from
// `codex exec --json`. Codex emits "thread events": a thread.started carrying the
// thread id (our session id for resume), item.* events carrying the agent's
// messages and tool calls, and a turn.completed carrying token usage. Only the
// fields VichuFlow consumes are modeled; unknown types and fields are ignored so
// CLI additions don't break parsing — the agent CLI is the fragile side here.
type codexEventLine struct {
	Type     string `json:"type"`      // thread.started | item.completed | turn.completed | turn.failed | error
	ThreadID string `json:"thread_id"` // thread.started

	// item.* events
	Item struct {
		Type       string `json:"type"` // agent_message | reasoning | command_execution | file_change | mcp_tool_call | error
		Text       string `json:"text"`
		Command    string `json:"command"`
		ExitCode   *int   `json:"exit_code"`
		Status     string `json:"status"`
		Tool       string `json:"tool"`
		ServerName string `json:"server"`
		Message    string `json:"message"` // error items
	} `json:"item"`

	// turn.completed
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	// turn.failed / top-level error
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// codexOutcome accumulates the parts of a codex run scattered across its event
// stream: the thread id (from thread.started), the last assistant message (the
// final result text), token usage (from turn.completed), and whether the turn
// completed or carried an error.
type codexOutcome struct {
	threadID    string
	lastMessage string
	tokensIn    int
	tokensOut   int
	completed   bool
	errMsg      string
}

// decodeCodexLine normalizes one JSONL line into agent events and folds its
// outcome-bearing fields into oc. Malformed lines are skipped (never panic):
// the CLI's output is the fragile boundary and must not crash a run.
func decodeCodexLine(line []byte, oc *codexOutcome) []AgentEvent {
	var l codexEventLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			oc.threadID = l.ThreadID
		}
	case "item.completed", "item.updated":
		return decodeCodexItem(l, oc)
	case "turn.completed":
		oc.completed = true
		oc.tokensIn = l.Usage.InputTokens
		oc.tokensOut = l.Usage.OutputTokens
	case "turn.failed":
		oc.errMsg = l.Error.Message
	case "error":
		oc.errMsg = firstNonEmpty(l.Message, l.Error.Message)
	}
	return nil
}

// decodeCodexItem turns a completed item into an event and records the agent's
// final message as the run's result text. Codex names that item "agent_message"
// (verified against codex-cli 0.136.0); "assistant_message" is accepted too in
// case a future/older build uses it.
func decodeCodexItem(l codexEventLine, oc *codexOutcome) []AgentEvent {
	switch l.Item.Type {
	case "agent_message", "assistant_message":
		if l.Item.Text == "" {
			return nil
		}
		oc.lastMessage = l.Item.Text
		return []AgentEvent{{Kind: EventText, Text: l.Item.Text}}
	case "reasoning":
		if l.Item.Text == "" {
			return nil
		}
		return []AgentEvent{{Kind: EventText, Text: l.Item.Text}}
	case "command_execution":
		return []AgentEvent{{Kind: EventToolUse, Detail: codexCommandDetail(l)}}
	case "file_change":
		return []AgentEvent{{Kind: EventToolUse, Detail: map[string]any{"tool": "file_change"}}}
	case "mcp_tool_call":
		return []AgentEvent{{Kind: EventToolUse, Detail: map[string]any{"tool": firstNonEmpty(l.Item.Tool, "mcp"), "server": l.Item.ServerName}}}
	case "error":
		oc.errMsg = l.Item.Message
		return []AgentEvent{{Kind: EventError, Text: l.Item.Message}}
	}
	return nil
}

// codexCommandDetail surfaces the shell command and its exit code (when known).
func codexCommandDetail(l codexEventLine) map[string]any {
	d := map[string]any{"tool": "shell", "command": l.Item.Command}
	if l.Item.ExitCode != nil {
		d["exit_code"] = *l.Item.ExitCode
	}
	return d
}
