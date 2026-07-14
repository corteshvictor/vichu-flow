package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/core"
	"github.com/corteshvictor/vichu-flow/internal/safeio"
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
//
// NOTE (token efficiency, planned): this sends each changed file's FULL (capped)
// content, not unified-diff hunks. Sending hunks would spend fewer tokens on large
// files with small edits. Doing that portably needs a provider-level diff API (git:
// `git diff --unified=N`; filesystem: a unified diff of `.vichu/baseline/<path>` vs
// the live file), which pulls in a diff algorithm we don't yet carry — so it's a
// deliberate follow-up, not a silent gap. The per-file/total byte caps below bound
// the cost in the meantime, and any truncation is recorded in the timeline.
func (e *Engine) reviewChangeset(state *core.State) (text string, truncated, omitted []string, err error) {
	paths, err := e.changedPaths(state)
	if err != nil {
		return "", nil, nil, err
	}
	if len(paths) == 0 {
		return "", nil, nil, nil
	}
	root, err := safeio.Open(e.repo.Root())
	if err != nil {
		return "", nil, nil, err
	}
	defer func() { _ = root.Close() }()

	var b strings.Builder
	b.WriteString("# Changes to review\n\n")
	b.WriteString("Base your verdict on the changes below — you do not need to re-read the whole repository.\n\n")
	total := 0
	for _, p := range paths {
		ent, rerr := readReviewEntry(root, p)
		if rerr != nil {
			// A changed file we cannot classify or read is not "deleted" and not safe to guess at:
			// abort so the caller blocks BEFORE persisting the prompt or invoking the reviewer,
			// rather than sending the reviewer a half-truth.
			return "", nil, nil, fmt.Errorf("cannot read changed file %q for review: %w", p, rerr)
		}
		if ent.kind == entryRegular && total+len(ent.content) > reviewMaxTotalBytes {
			fmt.Fprintf(&b, "## %s _(omitted — change set over the review context budget; read it directly if needed)_\n\n", p)
			omitted = append(omitted, p)
			continue
		}
		total += len(ent.content)
		ent.render(&b, p)
		if ent.truncated {
			truncated = append(truncated, p)
		}
	}
	return b.String(), truncated, omitted, nil
}

// reviewEntry is one changed file classified for the review prompt: WITHOUT following a symlink
// (a changed file the agent turned into a link must be shown as the link it is, never as the
// external bytes it points at) and WITHOUT loading more than the per-file cap into memory.
type reviewEntry struct {
	kind      entryKind
	content   string // regular files only
	target    string // symlink files only
	truncated bool
}

type entryKind int

const (
	entryRegular entryKind = iota
	entryDeleted
	entrySymlink
	entrySpecial
)

// readReviewEntry classifies and (for a regular file) reads one changed path, confined to the
// workspace root. A non-local path, or any read error other than "gone", is returned as an
// error so the run blocks rather than reviewing on a guess.
func readReviewEntry(root *safeio.Root, p string) (reviewEntry, error) {
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return reviewEntry{}, fmt.Errorf("path is not local to the workspace")
	}
	info, err := root.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return reviewEntry{kind: entryDeleted}, nil
	case err != nil:
		return reviewEntry{}, err
	case info.Mode()&fs.ModeSymlink != 0:
		target, lerr := root.Readlink(p)
		if lerr != nil {
			return reviewEntry{}, lerr
		}
		return reviewEntry{kind: entrySymlink, target: target}, nil
	case !info.Mode().IsRegular():
		return reviewEntry{kind: entrySpecial}, nil
	}
	data, tr, rerr := root.ReadFileLimitedNoFollow(p, reviewMaxPerFileBytes)
	if rerr != nil {
		return reviewEntry{}, rerr
	}
	return reviewEntry{kind: entryRegular, content: string(data), truncated: tr}, nil
}

// render writes the entry into the review prompt. A symlink shows its target QUOTED (%q escapes
// newlines/backticks) so the target text cannot break out of the section or forge a file body.
func (ent reviewEntry) render(b *strings.Builder, p string) {
	switch ent.kind {
	case entryDeleted:
		fmt.Fprintf(b, "## %s _(deleted)_\n\n", p)
	case entrySymlink:
		fmt.Fprintf(b, "## %s _(now a symlink → %q)_\n\n", p, ent.target)
	case entrySpecial:
		fmt.Fprintf(b, "## %s _(now a non-regular file — not shown)_\n\n", p)
	default:
		fmt.Fprintf(b, "## %s\n\n```%s\n%s\n```\n", p, langFence(p), ent.content)
		if ent.truncated {
			b.WriteString("_(truncated for the review context budget)_\n")
		}
		b.WriteString("\n")
	}
}

// changedPaths returns the sorted, de-duplicated set of files the run changed,
// gathered from every worker's mutation report.
func (e *Engine) changedPaths(state *core.State) ([]string, error) {
	workers, err := e.store.ListWorkers(state.RunID)
	if err != nil {
		// If we cannot even enumerate the run's workers, we cannot know what changed — reviewing
		// on a guessed-empty change set would silently ask the reviewer to approve blind.
		return nil, fmt.Errorf("cannot list workers to assemble the review change set: %w", err)
	}
	seen := map[string]struct{}{}
	for _, w := range workers {
		r, err := e.store.LoadMutationReport(state.RunID, w)
		if err != nil {
			// EVERY worker that ran produced a mutation report — even a read-only one writes an
			// empty report — so by review time each prior worker has one (a worker that failed to
			// start would have blocked the run before it got here). A MISSING report is therefore
			// not "no changes": it was deleted or lost. Absent, corrupt, or unreadable, dropping it
			// would hide that worker's changes from the reviewer, who would approve a diff it never
			// saw. Block instead. (fs.ErrNotExist is NOT skipped — that was the incomplete fix.)
			return nil, fmt.Errorf("cannot read the mutation report for worker %q: %w", w, err)
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
	return paths, nil
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
func (e *Engine) withReviewContext(prompt string, state *core.State, stageName string, isReview bool) (string, error) {
	if !isReview || e.cfg.Workflow.ReviewContext == "full" {
		return prompt, nil
	}
	cs, truncated, omitted, err := e.reviewChangeset(state)
	if err != nil {
		return "", err
	}
	if cs == "" {
		return prompt, nil
	}
	if len(truncated) > 0 || len(omitted) > 0 {
		e.emit(state, stageName, "", core.EventReviewContextTruncated, map[string]any{
			"truncated": truncated, "omitted": omitted,
		})
	}
	return prompt + "\n---\n\n" + cs, nil
}
