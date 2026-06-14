package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// FilesystemWorkspace is a workspace backend for folders without a VCS. It keeps
// a content copy of the tree under .vichu/baseline as the baseline, so it can
// report exactly which files a worker changed and roll those changes back —
// giving Git-optional runs the same undo guarantees as Git, just without Git.
// The copy is refreshed each time a run starts (Snapshot), so every run measures
// change against the tree as it was when that run began.
type FilesystemWorkspace struct {
	root string
}

// OpenFilesystem returns a filesystem-backed workspace rooted at dir. Unlike the
// Git provider it never fails on a missing VCS — any readable directory works.
func OpenFilesystem(dir string) (*FilesystemWorkspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("workspace root is not a directory: " + abs)
	}
	return &FilesystemWorkspace{root: abs}, nil
}

// Root returns the workspace top-level directory.
func (w *FilesystemWorkspace) Root() string { return w.root }

// Kind names this backend; it satisfies Provider.Kind.
func (w *FilesystemWorkspace) Kind() string { return KindFilesystem }

func (w *FilesystemWorkspace) baselinePath() string {
	return filepath.Join(w.root, runtimeDirName, "baseline")
}
func (w *FilesystemWorkspace) manifestPath() string {
	return filepath.Join(w.root, runtimeDirName, "baseline.manifest")
}
func (w *FilesystemWorkspace) baseIDPath() string {
	return filepath.Join(w.root, runtimeDirName, "baseline.id")
}

// Snapshot refreshes the baseline copy to the current tree and returns a
// Workspace describing it. Right after a snapshot the tree equals the baseline,
// so the dirty set is empty — every subsequent change is attributable to the run.
func (w *FilesystemWorkspace) Snapshot(isolation string) (*core.Workspace, error) {
	if isolation == "" {
		isolation = core.IsolationCurrentWorktree
	}
	id, err := w.rebaseline()
	if err != nil {
		return nil, err
	}
	return &core.Workspace{
		Provider:     KindFilesystem,
		Isolation:    isolation,
		Branch:       "",
		BaseSHA:      id,
		DirtyFiles:   nil,
		Fingerprints: map[string]string{},
		CapturedAt:   time.Now().UTC(),
	}, nil
}

// rebaseline clears the baseline copy, copies the current tree into it
// (excluding .git and the .vichu runtime), and persists the manifest and id.
func (w *FilesystemWorkspace) rebaseline() (string, error) {
	base := w.baselinePath()
	if err := os.RemoveAll(base); err != nil {
		return "", err
	}
	manifest := map[string]string{}
	walkErr := w.walkFiles(func(rel, full string) error {
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		h, err := copyFileHash(full, dst)
		if err != nil {
			return err
		}
		manifest[rel] = h
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if err := os.MkdirAll(filepath.Join(w.root, runtimeDirName), 0o755); err != nil {
		return "", err
	}
	if err := writeManifest(w.manifestPath(), manifest); err != nil {
		return "", err
	}
	id := manifestID(manifest)
	if err := os.WriteFile(w.baseIDPath(), []byte(id), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// FingerprintChanged returns the current changed-vs-baseline fileset as a
// path→content-hash map.
func (w *FilesystemWorkspace) FingerprintChanged() (map[string]string, error) {
	changed, err := w.captureChanged()
	if err != nil {
		return nil, err
	}
	prints := make(map[string]string, len(changed))
	for p, f := range changed {
		prints[p] = f.hash
	}
	return prints, nil
}

// BeginTracking snapshots the changed set before a worker runs.
func (w *FilesystemWorkspace) BeginTracking() (*Tracker, error) { return newTracker(w) }

// BackupChanged captures the current content of all changed-vs-baseline files.
func (w *FilesystemWorkspace) BackupChanged() (*Backup, error) { return captureBackup(w) }

// RestoreBaseline restores paths to their baseline content, recreating ones that
// were deleted and reverting ones that were modified. Paths absent from the
// baseline (files the run newly created) have nothing to restore to and are
// skipped. Returns how many paths were restored.
func (w *FilesystemWorkspace) RestoreBaseline(paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	base := w.baselinePath()
	restored := 0
	for _, p := range paths {
		src := filepath.Join(base, filepath.FromSlash(p))
		data, err := os.ReadFile(src)
		if err != nil {
			continue // not in baseline — a file the run added; nothing to restore
		}
		dst := filepath.Join(w.root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return restored, err
		}
		if err := os.WriteFile(dst, data, fileMode(src)); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}

// BaseID returns the persisted baseline id, or "" if no baseline exists yet.
func (w *FilesystemWorkspace) BaseID() string {
	data, err := os.ReadFile(w.baseIDPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// captureChanged compares the current tree against the persisted baseline
// manifest and returns the changed fileset with porcelain-style codes that
// kindFromCode understands: "??" (new), " M" (modified), " D" (deleted).
func (w *FilesystemWorkspace) captureChanged() (map[string]changedFile, error) {
	baseManifest, err := readManifest(w.manifestPath())
	if err != nil {
		return nil, err
	}
	changed := map[string]changedFile{}
	seen := map[string]bool{}
	walkErr := w.walkFiles(func(rel, full string) error {
		seen[rel] = true
		h := hashFile(full)
		switch base, ok := baseManifest[rel]; {
		case !ok:
			changed[rel] = changedFile{code: "??", hash: h}
		case base != h:
			changed[rel] = changedFile{code: " M", hash: h}
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	for rel := range baseManifest {
		if !seen[rel] {
			changed[rel] = changedFile{code: " D", hash: ""}
		}
	}
	return changed, nil
}

// lineStats reports approximate added/deleted line counts for a changed path by
// comparing the current file against its baseline copy.
func (w *FilesystemWorkspace) lineStats(path string, untracked bool) (added, deleted int) {
	cur := filepath.Join(w.root, filepath.FromSlash(path))
	if untracked {
		return countLines(cur), 0
	}
	oldData, _ := os.ReadFile(filepath.Join(w.baselinePath(), filepath.FromSlash(path)))
	newData, _ := os.ReadFile(cur)
	return lineDelta(oldData, newData)
}

// walkFiles visits every regular file under the workspace root, skipping the
// .git and .vichu directories. Paths passed to fn are forward-slash relative.
func (w *FilesystemWorkspace) walkFiles(fn func(rel, full string) error) error {
	return filepath.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == w.root {
			return nil
		}
		rel, rerr := filepath.Rel(w.root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == runtimeDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, sockets, devices
		}
		return fn(rel, p)
	})
}

// copyFileHash copies src to dst (preserving permission bits) and returns the
// sha256 hex of its content.
func copyFileHash(src, dst string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, fileMode(src)); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// fileMode returns path's permission bits, defaulting to 0o644 if it can't stat.
func fileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// manifestID is a stable digest of a path→hash manifest, used as the baseline id.
func manifestID(m map[string]string) string {
	h := sha256.New()
	for _, k := range sortedKeys(m) {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(m[k]))
		h.Write([]byte{'\n'})
	}
	return "fs:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// writeManifest persists a path→hash manifest as "hash\tpath" lines.
func writeManifest(path string, m map[string]string) error {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(m[k])
		b.WriteByte('\t')
		b.WriteString(k)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// readManifest loads a manifest written by writeManifest. A missing manifest is
// an empty baseline, not an error.
func readManifest(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		i := strings.IndexByte(line, '\t')
		if i < 0 {
			continue
		}
		m[line[i+1:]] = line[:i]
	}
	return m, nil
}

// lineDelta approximates added/deleted line counts as the multiset difference
// between old and new content — order-insensitive, but a reasonable stat without
// pulling in a full diff algorithm.
func lineDelta(oldData, newData []byte) (added, deleted int) {
	counts := map[string]int{}
	for _, l := range splitLines(oldData) {
		counts[l]--
	}
	for _, l := range splitLines(newData) {
		counts[l]++
	}
	for _, c := range counts {
		switch {
		case c > 0:
			added += c
		case c < 0:
			deleted += -c
		}
	}
	return added, deleted
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Both providers satisfy the engine's Provider seam and the internal change
// source Tracker/Backup share.
var (
	_ Provider     = (*FilesystemWorkspace)(nil)
	_ changeSource = (*FilesystemWorkspace)(nil)
	_ changeSource = (*Repo)(nil)
)
