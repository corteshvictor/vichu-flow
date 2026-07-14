package workspace

import (
	"bytes"
	"fmt"
)

// Backup is the captured content of every path that differs from the baseline at a point
// in time (dirty, untracked, and audited ignored files — never the runtime directory). It
// lets the engine undo a gate's damage to pre-existing user work: a gate is a verification
// command and must not change the tree, so when one does, its effects on these paths are
// rolled back.
//
// Every read and write goes through an os.Root confined to the workspace and never follows
// a final symlink. That is not a detail — see access.go for the bug it closes.
type Backup struct {
	root  string
	files map[string]entry
}

// Has reports whether a path was captured in the backup (it differed from the baseline, so
// its content is held here rather than recoverable from it).
func (b *Backup) Has(path string) bool {
	_, ok := b.files[path]
	return ok
}

// BackupChanged captures the current content of all changed-vs-baseline paths.
func (r *Repo) BackupChanged() (*Backup, error) { return captureBackup(r) }

// captureBackup snapshots the current content, mode and TYPE of every changed-vs-baseline
// path a provider reports, so a blocking gate's damage can be rolled back faithfully.
func captureBackup(src changeSource) (*Backup, error) {
	changed, err := src.captureChanged()
	if err != nil {
		return nil, err
	}
	root, err := openRoot(src.Root())
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	b := &Backup{root: src.Root(), files: make(map[string]entry, len(changed))}
	for p := range changed {
		e, rerr := readEntry(root, p)
		if rerr != nil {
			// We cannot hold a copy of this file, so we cannot undo what a gate does to it.
			// Skipping it silently is how an unreadable file got overwritten with the run
			// still reaching `completed` — the gate does not run unless the backup is whole.
			return nil, fmt.Errorf("cannot back up the workspace before running a gate: %w", rerr)
		}
		if e.kind != entryMissing {
			b.files[p] = e // a deleted path has nothing to hold; that is not a failure
		}
	}
	return b, nil
}

// Restore rewrites every backed-up path to its captured content, mode and type, recreating
// ones that were deleted and reverting ones that were modified. Paths the gate newly
// created are left in place (they are not user work that can be lost).
func (b *Backup) Restore() (restored int, err error) {
	root, oerr := openRoot(b.root)
	if oerr != nil {
		return 0, oerr
	}
	defer func() { _ = root.Close() }()

	for p, want := range b.files {
		cur, rerr := readEntry(root, p)
		if rerr == nil && sameEntry(cur, want) {
			continue // unchanged — nothing to restore
		}
		// An unreadable path is restored rather than skipped: a gate that chmods a file to
		// 000 and rewrites it has still destroyed it, and we hold the original.
		if wErr := writeEntry(root, p, want); wErr != nil {
			err = wErr
			continue
		}
		restored++
	}
	return restored, err
}

// sameEntry reports two captures as identical in every way the rollback restores: content,
// TYPE and MODE. Mode is in there because os.WriteFile does not change the permissions of a
// file that already exists — so a gate that widened 0600 to 0644 while editing the content
// used to survive the rollback with its widened mode intact, quietly exposing the file.
func sameEntry(a, b entry) bool {
	return a.kind == b.kind && a.mode == b.mode && bytes.Equal(a.data, b.data)
}
