package workspace

import (
	"bytes"
	"os"
	"path/filepath"
)

// backedUpFile is a captured file's content plus its permission bits, so a
// restore reinstates an executable script as executable, not 0o644.
type backedUpFile struct {
	data []byte
	mode os.FileMode
}

// Backup is the captured content of every file that differs from the baseline at
// a point in time (dirty + untracked, excluding the runtime directory). It lets
// the engine undo a gate's damage to pre-existing user work: a gate is a
// verification command and must not change the tree, so when one does, its
// effects on these files are rolled back.
type Backup struct {
	root  string
	files map[string]backedUpFile
}

// Has reports whether a path was captured in the backup (it differed from the
// baseline, so its content is held here rather than recoverable from it).
func (b *Backup) Has(path string) bool {
	_, ok := b.files[path]
	return ok
}

// BackupChanged captures the current content of all changed-vs-baseline files.
func (r *Repo) BackupChanged() (*Backup, error) { return captureBackup(r) }

// captureBackup snapshots the current content and mode of every changed-vs-
// baseline file reported by a provider, so a blocking gate's damage can be
// rolled back faithfully.
func captureBackup(src changeSource) (*Backup, error) {
	changed, err := src.captureChanged()
	if err != nil {
		return nil, err
	}
	root := src.Root()
	b := &Backup{root: root, files: make(map[string]backedUpFile, len(changed))}
	for p := range changed {
		full := filepath.Join(root, p)
		if data, err := os.ReadFile(full); err == nil {
			b.files[p] = backedUpFile{data: data, mode: fileMode(full)} // missing files skipped
		}
	}
	return b, nil
}

// Restore rewrites every backed-up file to its captured content and mode,
// recreating ones that were deleted and reverting ones that were modified. Files
// the gate newly created are left in place (they are not user work that can be
// lost).
func (b *Backup) Restore() (restored int, err error) {
	for p, f := range b.files {
		full := filepath.Join(b.root, p)
		if cur, rerr := os.ReadFile(full); rerr == nil && bytes.Equal(cur, f.data) {
			continue // unchanged — nothing to restore
		}
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			err = mkErr
			continue
		}
		if wErr := os.WriteFile(full, f.data, f.mode); wErr != nil {
			err = wErr
			continue
		}
		restored++
	}
	return restored, err
}
