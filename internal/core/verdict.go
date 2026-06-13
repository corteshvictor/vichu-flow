package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// VerdictStatus is a reviewer's structured outcome. A review is not "pass/fail":
// the reviewer doing its job and rejecting the work is a needs_fixes verdict, not
// a failure. A failure (adapter error, timeout, unparseable output) is handled
// separately and never silently becomes "approved".
type VerdictStatus string

const (
	// VerdictApproved — the implementation is acceptable; advance.
	VerdictApproved VerdictStatus = "approved"
	// VerdictNeedsFixes — defects found; loop to the fix stage and re-review.
	VerdictNeedsFixes VerdictStatus = "needs_fixes"
	// VerdictBlocked — the reviewer cannot proceed (unsafe/underspecified task);
	// block the run for a human. Do NOT auto-fix.
	VerdictBlocked VerdictStatus = "blocked"
)

// Valid reports whether s is a known verdict status.
func (s VerdictStatus) Valid() bool {
	switch s {
	case VerdictApproved, VerdictNeedsFixes, VerdictBlocked:
		return true
	default:
		return false
	}
}

// Severity ranks a single review finding.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
)

// Finding is one issue a reviewer raised.
type Finding struct {
	Severity Severity `json:"severity,omitempty"`
	File     string   `json:"file,omitempty"`
	Message  string   `json:"message"`
}

// Verdict is a reviewer's normalized, validated outcome. It is the runtime's
// public contract — persisted to reviews/<stage>/iteration-N/verdict.json — not
// the raw Result.Data the adapter happened to return.
type Verdict struct {
	Status     VerdictStatus `json:"status"`
	Summary    string        `json:"summary,omitempty"`
	Findings   []Finding     `json:"findings,omitempty"`
	Stage      string        `json:"stage,omitempty"`
	Iteration  int           `json:"iteration,omitempty"`
	CapturedAt time.Time     `json:"captured_at,omitempty"`
}

// ParseVerdict normalizes a reviewer's raw Result.Data into a validated Verdict.
// It returns an error when the payload is missing or its status is not one of the
// known values — the engine blocks the run on that error rather than assuming the
// review approved anything.
func ParseVerdict(data map[string]any) (Verdict, error) {
	if len(data) == 0 {
		return Verdict{}, errors.New("reviewer produced no structured verdict")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Verdict{}, fmt.Errorf("encoding reviewer output: %w", err)
	}
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return Verdict{}, fmt.Errorf("parsing reviewer verdict: %w", err)
	}
	if !v.Status.Valid() {
		return Verdict{}, fmt.Errorf("reviewer verdict has invalid status %q (want approved|needs_fixes|blocked)", v.Status)
	}
	return v, nil
}

// ParseVerdictFromResult extracts a validated Verdict from a worker result.
// Structured output is AUTHORITATIVE: if Data carries a "status", it is honored
// as-is — even an invalid status is an error, never masked by a different JSON
// object in the prose. Only when Data has no status does it fall back to the LAST
// JSON object carrying a "status" field in the reviewer's text output (Markdown
// for claude-code/codex, stdout for shell). It never defaults to approved — an
// unparseable review is an error the engine blocks on.
func ParseVerdictFromResult(r Result) (Verdict, error) {
	if _, ok := r.Data["status"]; ok {
		return ParseVerdict(r.Data)
	}
	if obj, ok := extractVerdictObject(r.Markdown); ok {
		return ParseVerdict(obj)
	}
	return Verdict{}, errors.New("reviewer produced no structured verdict: expected a JSON object with a \"status\" field (approved|needs_fixes|blocked) in its structured output or final message")
}

// extractVerdictObject scans text for JSON objects and returns the last one that
// carries a "status" key. Reviewers commonly write prose and then a final JSON
// verdict, so the last status-bearing object is the verdict.
func extractVerdictObject(text string) (map[string]any, bool) {
	var found map[string]any
	ok := false
	for _, candidate := range jsonObjects(text) {
		var m map[string]any
		if json.Unmarshal([]byte(candidate), &m) != nil {
			continue
		}
		if _, has := m["status"]; has {
			found, ok = m, true // last status-bearing object wins
		}
	}
	return found, ok
}

// jsonObjects returns the top-level {...} substrings in text, balancing braces
// while skipping string literals so braces inside JSON strings don't throw off
// the balance. A nested object is returned as part of its outermost one.
func jsonObjects(text string) []string {
	var objs []string
	depth, start := 0, -1
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '"':
			i = skipString(text, i)
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 {
					objs = append(objs, text[start:i+1])
				}
			}
		}
	}
	return objs
}

// skipString returns the index of the closing quote of the string literal that
// opens at i (text[i] == '"'), or the last index if it is unterminated.
func skipString(text string, i int) int {
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++ // skip the escaped character
		case '"':
			return j
		}
	}
	return len(text) - 1
}
