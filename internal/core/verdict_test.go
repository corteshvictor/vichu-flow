package core

import "testing"

func TestParseVerdictFromResultPrefersData(t *testing.T) {
	r := Result{
		Data:     map[string]any{"status": "approved", "summary": "ok"},
		Markdown: `{"status":"blocked"}`, // must be ignored when Data is valid
	}
	v, err := ParseVerdictFromResult(r)
	if err != nil {
		t.Fatalf("ParseVerdictFromResult: %v", err)
	}
	if v.Status != VerdictApproved {
		t.Fatalf("structured Data must win, got %q", v.Status)
	}
}

func TestParseVerdictFromResultFallsBackToMarkdown(t *testing.T) {
	// A shell/claude/codex reviewer with no structured Data: the verdict lives in
	// its text output, possibly after prose and inside a code fence.
	r := Result{
		Data: map[string]any{"exit_code": 0, "command": "review.sh"},
		Markdown: "Here is my assessment of the change.\n\n" +
			"```json\n{\"status\":\"needs_fixes\",\"summary\":\"missing tests\"," +
			"\"findings\":[{\"severity\":\"major\",\"message\":\"add a test\"}]}\n```\n",
	}
	v, err := ParseVerdictFromResult(r)
	if err != nil {
		t.Fatalf("ParseVerdictFromResult: %v", err)
	}
	if v.Status != VerdictNeedsFixes || len(v.Findings) != 1 {
		t.Fatalf("verdict not extracted from text: %+v", v)
	}
}

func TestParseVerdictFromResultTakesLastStatusObject(t *testing.T) {
	// Prose may contain an illustrative object; the LAST status-bearing object is
	// the actual verdict.
	r := Result{Markdown: `I considered {"status":"approved"} earlier, but my final verdict is:
{"status":"blocked","summary":"task is unsafe"}`}
	v, err := ParseVerdictFromResult(r)
	if err != nil {
		t.Fatalf("ParseVerdictFromResult: %v", err)
	}
	if v.Status != VerdictBlocked {
		t.Fatalf("must take the last status object, got %q", v.Status)
	}
}

func TestParseVerdictFromResultNoVerdictErrors(t *testing.T) {
	// No structured data and no JSON verdict in the text — must error, never
	// silently approve.
	r := Result{Markdown: "Looks good to me, ship it!"}
	if _, err := ParseVerdictFromResult(r); err == nil {
		t.Fatal("a result with no structured verdict must error")
	}
}

func TestParseVerdictFromResultInvalidStatusErrors(t *testing.T) {
	r := Result{Markdown: `{"status":"lgtm"}`}
	if _, err := ParseVerdictFromResult(r); err == nil {
		t.Fatal("an unknown status must error, not fall through to approved")
	}
}

func TestParseVerdictFromResultDataStatusIsAuthoritative(t *testing.T) {
	// Structured Data carries an INVALID status while the prose carries an
	// approval. The structured status is authoritative: the invalid value must
	// error, never be masked by a different JSON object in the text.
	r := Result{
		Data:     map[string]any{"status": "lgtm"},
		Markdown: `my final verdict: {"status":"approved"}`,
	}
	if _, err := ParseVerdictFromResult(r); err == nil {
		t.Fatal("an invalid structured status must error, not fall back to a prose approval")
	}
}

func TestExtractVerdictObjectIgnoresBracesInStrings(t *testing.T) {
	// Braces inside a JSON string value must not break brace balancing.
	text := `{"status":"approved","summary":"handles the {weird} case }{ fine"}`
	obj, ok := extractVerdictObject(text)
	if !ok || obj["status"] != "approved" {
		t.Fatalf("string-embedded braces broke extraction: ok=%v obj=%v", ok, obj)
	}
}

// TestParseVerdictRejectsApprovedWithActionableFinding (ronda 18): "approved" cannot carry an
// actionable (blocker/major) finding — that is a contradiction the kernel used to advance on. A
// "minor" advisory note stays allowed.
func TestParseVerdictRejectsApprovedWithActionableFinding(t *testing.T) {
	for _, sev := range []string{"blocker", "major"} {
		v := map[string]any{"status": "approved", "findings": []any{map[string]any{"severity": sev, "message": "fix X"}}}
		if _, err := ParseVerdict(v); err == nil {
			t.Fatalf("approved + %s finding must be rejected as contradictory", sev)
		}
	}
	// approved + minor (advisory) is allowed.
	if _, err := ParseVerdict(map[string]any{"status": "approved", "findings": []any{map[string]any{"severity": "minor", "message": "optional"}}}); err != nil {
		t.Fatalf("approved + minor advisory must still be accepted: %v", err)
	}
	// needs_fixes + finding is the normal fix-loop path.
	if _, err := ParseVerdict(map[string]any{"status": "needs_fixes", "findings": []any{map[string]any{"severity": "blocker", "message": "fix"}}}); err != nil {
		t.Fatalf("needs_fixes + finding must be accepted: %v", err)
	}
}

// TestParseVerdictRejectsIncoherentEnvelopes (ronda 22): the parser validates internal coherence,
// not just the status name — else the kernel acts (blocks, loops, advances) on a self-contradictory
// verdict.
func TestParseVerdictRejectsIncoherentEnvelopes(t *testing.T) {
	bad := []map[string]any{
		{"status": "blocked"},     // no reason
		{"status": "needs_fixes"}, // no findings
		{"status": "needs_fixes", "findings": []any{map[string]any{"severity": "minor", "message": "x"}}},  // only minor
		{"status": "approved", "findings": []any{map[string]any{"severity": "weird", "message": "x"}}},     // unknown severity
		{"status": "needs_fixes", "findings": []any{map[string]any{"severity": "major", "message": "  "}}}, // empty message
	}
	for i, c := range bad {
		if _, err := ParseVerdict(c); err == nil {
			t.Fatalf("case %d must be rejected as incoherent: %v", i, c)
		}
	}
	for _, ok := range []map[string]any{
		{"status": "blocked", "summary": "unsafe task"},
		{"status": "needs_fixes", "findings": []any{map[string]any{"severity": "major", "message": "fix"}}},
		{"status": "approved", "findings": []any{map[string]any{"severity": "minor", "message": "polish"}}},
	} {
		if _, err := ParseVerdict(ok); err != nil {
			t.Fatalf("coherent verdict must pass: %v (%v)", err, ok)
		}
	}
}
