package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// Review-context budget: a diff-only review prompt carries the changed files'
// content, but capped so a big change set can't blow the reviewer's context (the
// whole point is to spend FEWER tokens than free exploration).
const (
	reviewMaxPerFileBytes = 8 * 1024
	reviewMaxTotalBytes   = 48 * 1024
)

// reviewChangeset builds the "changes to review" section injected into a
// diff-only review prompt: every file the run has changed (from its mutation
// reports) with its current content. It is workspace-agnostic — it reads the
// recorded mutations, not `git diff` — so it works the same under the git and
// (v0.3) filesystem providers. It also returns the files it truncated or omitted
// for the context budget, so the caller can record the truncation (never
// silent). Returns "" when nothing changed.
func (e *Engine) reviewChangeset(state *core.State) (text string, truncated, omitted []string) {
	paths := e.changedPaths(state)
	if len(paths) == 0 {
		return "", nil, nil
	}
	var b strings.Builder
	b.WriteString("# Changes to review\n\n")
	b.WriteString("Base your verdict on the changes below — you do not need to re-read the whole repository.\n\n")
	total := 0
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(e.repo.Root(), p))
		if err != nil {
			fmt.Fprintf(&b, "## %s _(deleted)_\n\n", p)
			continue
		}
		content := string(data)
		wasTruncated := false
		if len(content) > reviewMaxPerFileBytes {
			content, wasTruncated = content[:reviewMaxPerFileBytes], true
		}
		if total+len(content) > reviewMaxTotalBytes {
			fmt.Fprintf(&b, "## %s _(omitted — change set over the review context budget; read it directly if needed)_\n\n", p)
			omitted = append(omitted, p)
			continue
		}
		total += len(content)
		fmt.Fprintf(&b, "## %s\n\n```%s\n%s\n```\n", p, langFence(p), content)
		if wasTruncated {
			b.WriteString("_(truncated for the review context budget)_\n")
			truncated = append(truncated, p)
		}
		b.WriteString("\n")
	}
	return b.String(), truncated, omitted
}

// changedPaths returns the sorted, de-duplicated set of files the run changed,
// gathered from every worker's mutation report.
func (e *Engine) changedPaths(state *core.State) []string {
	workers, err := e.store.ListWorkers(state.RunID)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, w := range workers {
		r, err := e.store.LoadMutationReport(state.RunID, w)
		if err != nil {
			continue
		}
		for _, m := range r.Mutations {
			seen[m.Path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// langFence maps a file extension to a Markdown code-fence language hint.
func langFence(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	case ".yaml", ".yml":
		return "yaml"
	case ".sh":
		return "bash"
	default:
		return ""
	}
}

// withReviewContext appends the diff-only change set to a review prompt. isReview
// is true only for review stages; "full" review context (config) opts out. Any
// truncation/omission of the change set is recorded in the timeline — a reviewer
// judging on incomplete context must never be a silent fact.
func (e *Engine) withReviewContext(prompt string, state *core.State, stageName string, isReview bool) string {
	if !isReview || e.cfg.Workflow.ReviewContext == "full" {
		return prompt
	}
	cs, truncated, omitted := e.reviewChangeset(state)
	if cs == "" {
		return prompt
	}
	if len(truncated) > 0 || len(omitted) > 0 {
		e.emit(state, stageName, "", core.EventReviewContextTruncated, map[string]any{
			"truncated": truncated, "omitted": omitted,
		})
	}
	return prompt + "\n---\n\n" + cs
}
