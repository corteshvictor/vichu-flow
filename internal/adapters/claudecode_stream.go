package adapters

import "encoding/json"

// claudeStreamLine is the union shape of one stream-json line from
// `claude -p --output-format stream-json`. Only the fields VichuFlow consumes
// are modeled; unknown fields are ignored so CLI additions don't break parsing.
type claudeStreamLine struct {
	Type      string `json:"type"`    // system | assistant | user | result
	Subtype   string `json:"subtype"` // init | success | error_* ...
	SessionID string `json:"session_id"`

	// type == "assistant"
	Message struct {
		Content []struct {
			Type  string         `json:"type"` // text | tool_use
			Text  string         `json:"text"`
			Name  string         `json:"name"` // tool name
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`

	// type == "result"
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// claudeResult is the final outcome extracted from a "result" line.
type claudeResult struct {
	Subtype      string
	IsError      bool
	Result       string
	TotalCostUSD float64
	SessionID    string
	Usage        struct {
		InputTokens  int
		OutputTokens int
	}
}

// decodeClaudeLine normalizes one stream-json line into agent events, a session
// id (if the line carries one), and the final result (for "result" lines).
// Malformed lines are skipped — the CLI's output is the fragile side of this
// boundary and must never crash a run.
func decodeClaudeLine(line []byte) (evs []AgentEvent, sessionID string, final *claudeResult) {
	var l claudeStreamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, "", nil
	}
	switch l.Type {
	case "assistant":
		evs = decodeAssistant(l)
	case "result":
		final = decodeResult(l)
	}
	return evs, l.SessionID, final
}

// decodeAssistant turns an assistant message's content blocks into events.
func decodeAssistant(l claudeStreamLine) []AgentEvent {
	var evs []AgentEvent
	for _, c := range l.Message.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				evs = append(evs, AgentEvent{Kind: EventText, Text: c.Text})
			}
		case "tool_use":
			evs = append(evs, AgentEvent{Kind: EventToolUse, Detail: toolDetail(c.Name, c.Input)})
		}
	}
	return evs
}

// toolDetail surfaces the most useful input field without dumping whole inputs.
func toolDetail(name string, input map[string]any) map[string]any {
	detail := map[string]any{"tool": name}
	for _, k := range []string{"file_path", "path", "command", "pattern"} {
		if v, ok := input[k]; ok {
			detail[k] = v
			break
		}
	}
	return detail
}

// decodeResult extracts the final outcome from a "result" line.
func decodeResult(l claudeStreamLine) *claudeResult {
	r := &claudeResult{
		Subtype:      l.Subtype,
		IsError:      l.IsError,
		Result:       l.Result,
		TotalCostUSD: l.TotalCostUSD,
		SessionID:    l.SessionID,
	}
	r.Usage.InputTokens = l.Usage.InputTokens
	r.Usage.OutputTokens = l.Usage.OutputTokens
	return r
}
