package safeio

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func openTemp(t *testing.T) (*Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

// TestWriteFileAtomicAcceptsOSSeparatorPath: callers build paths with filepath.Join / FromSlash,
// which use the OS separator — a BACKSLASH on Windows. WriteFileAtomic parses rel with the `path`
// package (forward-slash only), so a backslash path made path.Dir return "." — the temp landed at the
// root and the nested parents were never created, so the rename failed with "the system cannot find
// the path specified". It must normalize and create the nested file. On Unix the separator already is
// "/", so this simply confirms a nested write; on Windows it is the regression guard.
func TestWriteFileAtomicAcceptsOSSeparatorPath(t *testing.T) {
	r, dir := openTemp(t)
	rel := filepath.Join("a", "b", "c", "file.txt") // OS separator: backslashes on Windows
	if err := r.WriteFileAtomic(rel, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic with an OS-separator path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "b", "c", "file.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("nested file not created from an OS-separator path: got %q err %v", got, err)
	}
}

// TestWriteFileAtomicSetsModeOnAnExistingFile: os.WriteFile leaves an existing file's mode
// alone; this must not. A rollback that "restored" 0600 content into a file a gate had
// widened to 0644 used to leave it world-readable.
func TestWriteFileAtomicSetsModeOnAnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	r, dir := openTemp(t)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFileAtomic("f", []byte("new\n"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode not applied to an existing file: got %o want 0600", fi.Mode().Perm())
	}
}

// TestWriteFileAtomicReplacesASymlinkWithoutFollowingIt: rename replaces the link; it does
// not write through it. The target must be untouched.
func TestWriteFileAtomicReplacesASymlinkWithoutFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	r, dir := openTemp(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("UNTOUCHED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFileAtomic("link", []byte("new\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "UNTOUCHED\n" {
		t.Fatalf("wrote through the symlink: target is now %q", data)
	}
	fi, _ := os.Lstat(filepath.Join(dir, "link"))
	if !fi.Mode().IsRegular() {
		t.Fatalf("link should be a regular file now, got %v", fi.Mode())
	}
}

// TestCreateTempIsUnpredictableAndCannotBeSquatted: the temp name is random, so a
// pre-created file at any guessable name does not make the write fail.
func TestCreateTempIsUnpredictableAndCannotBeSquatted(t *testing.T) {
	r, dir := openTemp(t)
	// Litter the directory with plausible predictable names.
	for _, squat := range []string{".vichu-tmp-f", ".vichu-restore-f", ".tmp-f"} {
		if err := os.WriteFile(filepath.Join(dir, squat), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.WriteFileAtomic("f", []byte("content\n"), 0o644); err != nil {
		t.Fatalf("a squatted predictable name must not block the write: %v", err)
	}
	// Two writes in a row must not collide on a name.
	names := map[string]bool{}
	for range 50 {
		name, f, err := r.CreateTemp(".", 0o644)
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		_ = f.Close()
		if names[name] {
			t.Fatalf("CreateTemp produced a duplicate name %q", name)
		}
		names[name] = true
	}
}

// TestConfinementRejectsEscapes: a path climbing out of the root is refused, not followed.
func TestConfinementRejectsEscapes(t *testing.T) {
	r, _ := openTemp(t)
	if err := r.WriteFileAtomic("../escape", []byte("x"), 0o644); err == nil {
		t.Fatal("a path escaping the root must be refused")
	}
}

// TestOpenAppendAndCreateTruncateRefuseASymlink: the two streaming opens (used for the event
// log and the gate output) must not follow a symlink at the final component — the one thing
// they cannot get from temp-and-rename.
func TestOpenAppendAndCreateTruncateRefuseASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		open func(r *Root, rel string) (*os.File, error)
	}{
		{"append", func(r *Root, rel string) (*os.File, error) { return r.OpenAppend(rel, 0o644) }},
		{"truncate", func(r *Root, rel string) (*os.File, error) { return r.CreateTruncate(rel, 0o644) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, dir := openTemp(t)
			if err := os.Symlink(outside, filepath.Join(dir, "log")); err != nil {
				t.Fatal(err)
			}
			f, err := tc.open(r, "log")
			if err == nil {
				_, _ = f.WriteString("kernel data\n")
				_ = f.Close()
			}
			if data, _ := os.ReadFile(outside); string(data) != "ORIGINAL\n" {
				t.Fatalf("%s followed a symlink onto an external file: %q", tc.name, data)
			}
		})
	}
}

// TestOpenAppendCreatesAndAppends: the happy path — a missing file is created, and a second
// open appends rather than truncates.
func TestOpenAppendCreatesAndAppends(t *testing.T) {
	r, dir := openTemp(t)
	for _, line := range []string{"one\n", "two\n"} {
		f, err := r.OpenAppend("events.ndjson", 0o644)
		if err != nil {
			t.Fatalf("OpenAppend: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("append did not accumulate: %q (%v)", data, err)
	}
}
