package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestReconcileRestoresAnInterruptedSwap: a run that dies between "park the old fixtures aside"
// and "put the new ones in place" leaves only the backup. The next run must put it back, not
// carry on as though the release had never been recorded.
func TestReconcileRestoresAnInterruptedSwap(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "v1.0.0")
	backup := root + backupSuffix
	writeTree(t, backup, map[string][]byte{".claude/a.md": []byte("old")})

	if err := reconcileInterrupted(root, map[string][]byte{".claude/a.md": []byte("new")}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, ".claude", "a.md")); got != "old" {
		t.Fatalf("the parked fixtures must be restored, got %q", got)
	}
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the backup must be gone once restored")
	}
}

// TestReconcileDropsAStaleBackup: the crash the reviewer reproduced. A retry installed the new
// fixtures but the leftover `.backup` simply stayed — exit 0, and BOTH directories went into the
// repo with CI green, because every gate only looks at the real one.
func TestReconcileDropsAStaleBackup(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "v1.0.0")
	bodies := map[string][]byte{".claude/a.md": []byte("new")}
	writeTree(t, root, bodies)                                                        // already installed
	writeTree(t, root+backupSuffix, map[string][]byte{".claude/a.md": []byte("old")}) // residue

	if err := reconcileInterrupted(root, bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root + backupSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a stale backup must be dropped — otherwise it gets committed and nothing notices")
	}
	if got := readFile(t, filepath.Join(root, ".claude", "a.md")); got != "new" {
		t.Fatalf("the installed fixtures must survive, got %q", got)
	}
}

// TestReconcileFailsClosedWhenItCannotTell: both directories exist and NEITHER matches the
// release being recorded. A name proves nothing. Deleting the wrong history is not a mistake
// you can undo, so refuse and say which two paths to look at.
func TestReconcileFailsClosedWhenItCannotTell(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "v1.0.0")
	writeTree(t, root, map[string][]byte{".claude/a.md": []byte("something else")})
	writeTree(t, root+backupSuffix, map[string][]byte{".claude/a.md": []byte("old")})

	err := reconcileInterrupted(root, map[string][]byte{".claude/a.md": []byte("new")})
	if err == nil {
		t.Fatal("an ambiguous interrupted state must fail closed, not guess")
	}
	if _, serr := os.Stat(root); serr != nil {
		t.Fatal("a refused reconcile must delete nothing")
	}
	if _, serr := os.Stat(root + backupSuffix); serr != nil {
		t.Fatal("a refused reconcile must delete nothing")
	}
}

// TestLockIsExclusive: two recorders both read the catalog, both add their hash, and the second
// write wins — one release silently loses its hash while BOTH commands report success. A stale
// lock FAILS rather than being reclaimed: guessing the other process is dead is how you get two
// writers (see plan H2).
func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockHistory(dir); err == nil {
		t.Fatal("a second recorder must be refused while the first holds the lock")
	}
	unlock()
	unlock2, err := lockHistory(dir)
	if err != nil {
		t.Fatalf("the lock must be released: %v", err)
	}
	unlock2()
}

// TestLoadCatalogFailsClosedOnAnUnreadableFile: treating "I could not read it" as "it is not
// there" would rewrite the catalog from scratch and silently drop every release recorded before
// this one.
func TestLoadCatalogFailsClosedOnAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "known-hashes.json")
	if err := os.Mkdir(p, 0o755); err != nil { // a directory where the file should be
		t.Fatal(err)
	}
	if _, err := loadCatalog(p); err == nil {
		t.Fatal("an unreadable catalog must abort, not be treated as absent")
	}
	// But a genuinely absent one is the first-release case, and is fine.
	if _, err := loadCatalog(filepath.Join(dir, "nope.json")); err != nil {
		t.Fatalf("an absent catalog is the first release, not an error: %v", err)
	}
}

func writeTree(t *testing.T, root string, bodies map[string][]byte) {
	t.Helper()
	for rel, body := range bodies {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
