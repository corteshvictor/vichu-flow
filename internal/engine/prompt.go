package engine

import (
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/shellwords"
)

// buildPrompt composes a worker prompt: project context pack, the stage
// instruction, the task, the prior stage summary, and an output-language hint.
func buildPrompt(pack, instruction, task, priorSummary string, cfg *config.Config) string {
	var b strings.Builder
	if pack != "" {
		b.WriteString(pack)
		b.WriteString("\n---\n\n")
	}
	b.WriteString("# Task\n\n")
	b.WriteString(task)
	b.WriteString("\n\n# Your job for this stage\n\n")
	b.WriteString(instruction)
	if priorSummary != "" {
		b.WriteString("\n\n# Summary of the previous stage\n\n")
		b.WriteString(priorSummary)
	}
	if lang := outputLanguage(cfg); lang != "" {
		b.WriteString("\n\n# Output language\n\n")
		b.WriteString("Write your results in " + lang + ".")
	}
	b.WriteString("\n")
	return b.String()
}

// outputLanguage resolves the agentOutputLanguage setting to a concrete hint.
func outputLanguage(cfg *config.Config) string {
	switch cfg.UI.AgentOutputLanguage {
	case "en":
		return "English"
	case "es":
		return "Spanish"
	default:
		return "" // "project" — let the agent follow the repo's conventions
	}
}

// splitCommand tokenizes a configured command string into argv. See
// shellwords.Split for the quoting rules.
func splitCommand(s string) []string {
	return shellwords.Split(s)
}

// truncate returns s capped to maxRunes with an ellipsis marker, for summaries
// passed between stages (context budget).
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "\n…(truncated)"
}
