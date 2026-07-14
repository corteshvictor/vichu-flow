// Package safeio is the ONE way this codebase touches a file it does not fully control.
//
// It exists because the same two bugs kept being rewritten. Every place that needed an
// atomic write grew its own temp-file dance, and each one got something wrong:
//
//   - A PREDICTABLE temp name (".vichu-tmp-"+basename, or a hash of the path) truncates and
//     then deletes a user file that happens to carry that name — we destroy a file we never
//     looked at — and lets a hostile gate pre-create the name so O_EXCL fails and the
//     rollback it was supposed to enable never happens.
//   - os.ReadFile/os.WriteFile FOLLOW symlinks, so writing to a path a gate has turned into
//     a link writes THROUGH it, outside the project.
//   - os.WriteFile does not change the mode of a file that already exists, so a restore
//     "succeeds" and leaves a private file world-readable.
//
// Fixing those in one caller and then writing the next caller from scratch is how they came
// back. So there is one implementation, and callers get a Root — not a path.
package safeio

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// Root confines every operation to a directory. os.Root refuses any path that ESCAPES it — via
// "..", an absolute path, or a symlink component whose target lands outside the root — and the
// methods here additionally never resolve a FINAL symlink.
//
// What os.Root does NOT give you: it FOLLOWS a symlink component that stays INSIDE the root (a
// link pointing back within the directory is followed). So a caller that must not follow ANY
// planted link — the runtime under agent-writable `.vichu/` — has to walk the component chain
// itself (see runtime.refuseSymlinkChain); Root alone guards only escape and the final component.
type Root struct{ root *os.Root }

// Open returns a Root confined to dir.
func Open(dir string) (*Root, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{root: r}, nil
}

// Close releases the underlying handle.
func (r *Root) Close() error { return r.root.Close() }

// Name is the directory this Root is confined to.
func (r *Root) Name() string { return r.root.Name() }

// osPath converts a forward-slash relative path to this OS's separator. Callers pass
// slash-relative paths; os.Root wants native ones.
func osPath(rel string) string { return filepath.FromSlash(rel) }

func (r *Root) Lstat(rel string) (fs.FileInfo, error) { return r.root.Lstat(osPath(rel)) }
func (r *Root) ReadFile(rel string) ([]byte, error)   { return r.root.ReadFile(osPath(rel)) }

// ReadDir lists rel's entries, confined to the root. os.Root refuses a directory component that
// escapes the root, so a symlinked `.vichu/runs` pointing outside cannot enumerate another tree.
func (r *Root) ReadDir(rel string) ([]fs.DirEntry, error) {
	f, err := r.root.Open(osPath(rel))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadDir(-1)
}
func (r *Root) Readlink(rel string) (string, error) { return r.root.Readlink(osPath(rel)) }
func (r *Root) Remove(rel string) error             { return r.root.Remove(osPath(rel)) }
func (r *Root) MkdirAll(rel string, m fs.FileMode) error {
	return r.root.MkdirAll(osPath(rel), m)
}
func (r *Root) Rename(from, to string) error { return r.root.Rename(osPath(from), osPath(to)) }

// WriteFileAtomic writes data to rel: a randomly-named temp created with O_EXCL, the mode
// set AT CREATION, fsync, then rename over whatever is at rel. mode 0 means 0644.
//
// Every clause there is load-bearing. Random + O_EXCL means we only ever write to a file we
// just created, so we cannot destroy a user file by name collision and a hostile process
// cannot pre-create the name to make the write fail. Mode at creation means the content
// never exists at the wrong permissions, even briefly, and — unlike os.WriteFile — the mode
// is applied even when rel already exists. fsync before rename means a crash cannot leave
// the rename durable but the content empty. Rename REPLACES a symlink at rel rather than
// following it, and never truncates rel before the new content is complete.
func (r *Root) WriteFileAtomic(rel string, data []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	rel = filepath.ToSlash(rel) // accept OS-separator input; path.Dir/Base below need forward slashes
	dir := path.Dir(rel)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, f, err := r.CreateTemp(dir, mode)
	if err != nil {
		return err
	}
	defer func() { _ = r.Remove(tmp) }() // no-op once the rename succeeds; only ever OUR temp
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return r.Rename(tmp, rel)
}

// WriteSymlinkAtomic points rel at target. The link is built at a random temp name and
// RENAMED into place, so there is no window in which rel does not exist — a remove-then-
// create leaves the path empty in between, and a crash there loses it entirely.
func (r *Root) WriteSymlinkAtomic(rel, target string) error {
	rel = filepath.ToSlash(rel) // accept OS-separator input; path.Dir below needs forward slashes
	dir := path.Dir(rel)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := r.symlinkTemp(dir, target)
	if err != nil {
		return err
	}
	defer func() { _ = r.Remove(tmp) }() // no-op once the rename succeeds
	return r.Rename(tmp, rel)
}

// OpenAppend opens rel for appending, confined to the root, creating it if absent. It
// refuses to write through a symlink: os.Root already rejects one whose target ESCAPES the
// root, and the Lstat here additionally refuses one pointing back INSIDE it, so an append
// (which cannot use the temp-and-rename trick a full write can) never lands on a file the
// caller did not name. The caller owns the returned file.
//
// There is a residual TOCTOU — a symlink swapped in AFTER the Lstat and BEFORE the open —
// but os.Root still blocks any such link that escapes the root, so the worst it can do is
// redirect an append to another path WITHIN the root (the kernel's own runtime), never
// outside it.
func (r *Root) OpenAppend(rel string, mode fs.FileMode) (*os.File, error) {
	if err := r.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return nil, err
	}
	if err := r.refuseSymlink(rel); err != nil {
		return nil, err
	}
	return r.root.OpenFile(osPath(rel), os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
}

// OpenReadAppendExisting opens rel for BOTH reading and appending, ONLY if it already exists — no
// O_CREATE, no MkdirAll, refusing a final symlink. A caller that must VALIDATE a file and then APPEND
// to the same file uses this single descriptor for both: two separate opens can see different files if
// the path is repointed in between (a delete OR a regular-file replacement), so the write could land
// on the wrong file. A held descriptor binds both to one inode. Reads start at offset 0; the O_APPEND
// flag forces every write to the end regardless of the read offset. A missing file surfaces as
// fs.ErrNotExist. Same symlink residual as OpenAppend; pair with an inode-identity check (os.SameFile)
// to detect a path repointed WHILE the descriptor is held.
func (r *Root) OpenReadAppendExisting(rel string, mode fs.FileMode) (*os.File, error) {
	if err := r.refuseSymlink(rel); err != nil {
		return nil, err
	}
	return r.root.OpenFile(osPath(rel), os.O_RDWR|os.O_APPEND, mode)
}

// CreateTruncate opens rel for writing and truncates it, confined and refusing to follow a
// symlink at the final component (same guarantee and same residual as OpenAppend). It is for
// a file a subprocess streams into live — a gate's output log — where temp-and-rename is not
// an option because the destination must exist and grow during the run.
func (r *Root) CreateTruncate(rel string, mode fs.FileMode) (*os.File, error) {
	if err := r.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return nil, err
	}
	if err := r.refuseSymlink(rel); err != nil {
		return nil, err
	}
	return r.root.OpenFile(osPath(rel), os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
}

// ReadFileNoFollow reads rel, refusing a FINAL symlink. Plain ReadFile (os.Root.ReadFile)
// only rejects a symlink that ESCAPES the root — an INTERNAL one (e.g. config.snapshot.yaml →
// vichu.yaml, both inside the project) is followed. For a security-critical read like the
// frozen config, that is a hole: an agent redirects the snapshot at the live, tampered file.
// This refuses the symlink instead. (A direct regular-file overwrite of the target is a
// different, deeper problem — the agent writing .vichu directly — tracked as H11.)
func (r *Root) ReadFileNoFollow(rel string) ([]byte, error) {
	if err := r.refuseSymlink(rel); err != nil {
		return nil, err
	}
	return r.root.ReadFile(osPath(rel))
}

// ReadFileLimitedNoFollow reads at most limit bytes of rel, refusing a FINAL symlink. It reads
// no more than limit+1 bytes from the file, so a caller that caps content (e.g. a review prompt
// that shows 8 KiB of each changed file) never loads a file the size of which it does not
// control into memory first — an agent can turn a "changed file" into a multi-gigabyte one.
// truncated is true when the file was longer than limit; the returned data is then exactly
// limit bytes. Confinement (os.Root) guarantees rel cannot escape the root even if a symlink is
// swapped in after the check, so the worst a race yields is another file WITHIN the root.
func (r *Root) ReadFileLimitedNoFollow(rel string, limit int64) (data []byte, truncated bool, err error) {
	if serr := r.refuseSymlink(rel); serr != nil {
		return nil, false, serr
	}
	f, err := r.root.OpenFile(osPath(rel), os.O_RDONLY, 0)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// OpenExclusive creates rel with O_CREATE|O_EXCL (failing if it already exists), confined and
// creating the parent dir. It is for atomic claim files — operation records, locks — where
// "create it, or tell me it was already there" is the whole semantics.
func (r *Root) OpenExclusive(rel string, mode fs.FileMode) (*os.File, error) {
	if err := r.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return nil, err
	}
	return r.root.OpenFile(osPath(rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
}

// refuseSymlink returns an error if rel currently exists and is a symlink. A missing path is
// fine (the open will create it); anything that is not a symlink is fine.
func (r *Root) refuseSymlink(rel string) error {
	fi, err := r.root.Lstat(osPath(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing to open %s: it is a symlink, and this operation must not follow one", rel)
	}
	return nil
}

// CreateTemp makes a uniquely-named file in dir, inside the confined root, refusing to open
// one that already exists. The caller owns the returned file and its name.
func (r *Root) CreateTemp(dir string, mode fs.FileMode) (string, *os.File, error) {
	for range 100 {
		name, err := tempName(dir)
		if err != nil {
			return "", nil, err
		}
		f, err := r.root.OpenFile(osPath(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue // astronomically unlikely — try another name rather than touch it
		}
		if err != nil {
			return "", nil, err
		}
		return name, f, nil
	}
	return "", nil, fmt.Errorf("cannot create a temporary file in %s", dir)
}

// symlinkTemp creates a symlink to target under a fresh random name in dir.
func (r *Root) symlinkTemp(dir, target string) (string, error) {
	for range 100 {
		name, err := tempName(dir)
		if err != nil {
			return "", err
		}
		err = r.root.Symlink(target, osPath(name))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("cannot create a temporary symlink in %s", dir)
}

// tempName is 128 bits from crypto/rand. It is NOT derived from the destination path: a
// derived name is a name an attacker can compute, and a gate that pre-creates it makes the
// O_EXCL open fail — which is to say, it makes the rollback that would have undone its
// damage fail instead.
func tempName(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cannot generate a temporary file name: %w", err)
	}
	name := ".vichu-tmp-" + hex.EncodeToString(b[:])
	if dir == "." || dir == "" {
		return name, nil
	}
	return path.Join(dir, name), nil
}
