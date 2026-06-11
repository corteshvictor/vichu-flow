// Package contextpack builds the project context injected into every worker.
// A generic orchestrator over an unknown repo produces mediocre work; the
// context pack is what carries a project's conventions to the agents. It is
// also copied into each run (contextpack.md) for auditability.
package contextpack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
)

// conventionFiles are well-known places projects record their conventions.
var conventionFiles = []string{"CLAUDE.md", "AGENTS.md", ".cursorrules", "CONTRIBUTING.md"}

// Pack is the rendered context plus the list of sources it drew from.
type Pack struct {
	Markdown string
	Sources  []string
}

// Build assembles the context pack for a repo, honoring the context-pack size
// budget: files that fit are inlined; larger ones are referenced by path so the
// worker can read them on demand rather than flooding the prompt.
func Build(root string, cfg *config.Config) (*Pack, error) {
	budget := cfg.Budgets.Context.MaxContextPackKB * 1024
	if budget <= 0 {
		budget = 64 * 1024
	}

	var b strings.Builder
	var sources []string

	b.WriteString("# Project Context\n\n")
	det := config.Detect(root)
	fmt.Fprintf(&b, "- Language: %s\n", orAuto(det.Language))
	if c := cfg.CommandFor("test"); c != "" {
		fmt.Fprintf(&b, "- Test command: %s\n", c)
	}
	if c := cfg.CommandFor("lint"); c != "" {
		fmt.Fprintf(&b, "- Lint command: %s\n", c)
	}
	if c := cfg.CommandFor("typecheck"); c != "" {
		fmt.Fprintf(&b, "- Typecheck command: %s\n", c)
	}
	b.WriteString("\n")

	remaining := budget - b.Len()

	// Collect convention sources: well-known files plus any declared in config.
	var candidates []string
	candidates = append(candidates, conventionFiles...)
	candidates = append(candidates, cfg.Conventions...)

	wroteHeading := false
	for _, rel := range dedupe(candidates) {
		full := filepath.Join(root, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			continue // not present — skip silently
		}
		sources = append(sources, rel)
		if !wroteHeading {
			b.WriteString("## Conventions\n\n")
			wroteHeading = true
		}
		fmt.Fprintf(&b, "### %s\n\n", rel)
		if len(data) <= remaining {
			b.Write(data)
			b.WriteString("\n\n")
			remaining -= len(data)
		} else {
			fmt.Fprintf(&b, "_(%d KB — over context budget; read %s directly when needed.)_\n\n", len(data)/1024, rel)
		}
	}

	return &Pack{Markdown: b.String(), Sources: sources}, nil
}

func orAuto(s string) string {
	if s == "" || s == "unknown" {
		return "unknown"
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
