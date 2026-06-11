package workspace

import (
	"bytes"
	"os"
	"path/filepath"
)

// Backup is the captured content of every file that differs from HEAD at a
// point in time (dirty + untracked, excluding the runtime directory). It lets
// the engine undo a gate's damage to pre-existing user work: a gate is a
// verification command and must not change the tree, so when one does, its
// effects on these files are rolled back.
type Backup struct {
	root  string
	files map[string][]byte
}

// Has reports whether a path was captured in the backup (it was dirty or
// untracked, so its content is held here rather than recoverable from HEAD).
func (b *Backup) Has(path string) bool {
	_, ok := b.files[path]
	return ok
}

// BackupChanged captures the current content of all changed-vs-HEAD files.
func (r *Repo) BackupChanged() (*Backup, error) {
	changed, err := r.captureChanged()
	if err != nil {
		return nil, err
	}
	b := &Backup{root: r.root, files: make(map[string][]byte, len(changed))}
	for p := range changed {
		if data, err := os.ReadFile(filepath.Join(r.root, p)); err == nil {
			b.files[p] = data // missing (already-deleted) files are simply skipped
		}
	}
	return b, nil
}

// Restore rewrites every backed-up file to its captured content, recreating
// ones that were deleted and reverting ones that were modified. Files the gate
// newly created are left in place (they are not user work that can be lost).
func (b *Backup) Restore() (restored int, err error) {
	for p, data := range b.files {
		full := filepath.Join(b.root, p)
		if cur, rerr := os.ReadFile(full); rerr == nil && bytes.Equal(cur, data) {
			continue // unchanged — nothing to restore
		}
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			err = mkErr
			continue
		}
		if wErr := os.WriteFile(full, data, 0o644); wErr != nil {
			err = wErr
			continue
		}
		restored++
	}
	return restored, err
}
