package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/safeio"
)

// writeFileAtomic writes data to path (an absolute path UNDER projectRoot) via a confined,
// randomly-named temp file and a rename. The confinement root is the PROJECT directory, NOT
// `.vichu`, and that distinction is load-bearing: os.OpenRoot FOLLOWS a symlink at the root
// path itself, so rooting at `.vichu` when `.vichu` is a symlink an agent planted would
// confine writes to the symlink's external target. Rooting at the project and writing
// `.vichu/...` makes os.Root reject an escaping `.vichu` (or any inner component) as "path
// escapes from parent". The shared writer in internal/safeio sets the mode at creation and
// replaces a symlink at the destination rather than following it.
func writeFileAtomic(projectRoot, path string, data []byte, perm fs.FileMode) error {
	r, rel, err := confine(projectRoot, path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.WriteFileAtomic(rel, data, perm)
}

// confine opens a safeio.Root at the PROJECT root and returns the slash-relative path of an
// absolute path that must live under it (e.g. `.vichu/runs/<id>/state.json`). It is the
// single choke point that turns the Store's absolute-path API into confined, root-relative
// writes — and, by rooting at the project rather than at `.vichu`, it catches a symlinked
// `.vichu` that os.OpenRoot(".vichu") would silently follow.
func confine(projectRoot, abs string) (*safeio.Root, string, error) {
	rel, err := filepath.Rel(projectRoot, abs)
	if err != nil {
		return nil, "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("refusing to write %s: it is outside the project root %s", abs, projectRoot)
	}
	r, err := safeio.Open(projectRoot)
	if err != nil {
		return nil, "", err
	}
	slash := filepath.ToSlash(rel)
	if cerr := refuseSymlinkChain(r, slash); cerr != nil {
		_ = r.Close()
		return nil, "", cerr
	}
	return r, slash, nil
}

// refuseSymlinkChain rejects a `.vichu` path if ANY existing component — not just the final one —
// is a symlink. os.Root confines to the project but FOLLOWS a symlink component that stays inside
// it, so an agent that plants `.vichu/runs/<id>/workers -> ../../src` (all within the project)
// could redirect a later kernel write under it and overwrite a file OUTSIDE the runtime, unaudited.
// The runtime never creates a symlink under .vichu, so any component that is one was planted.
//
// This is scoped to `confine` — the runtime's OWN choke point — so it never touches the workspace,
// where a user's repo may legitimately contain symlinked directories. There is a residual TOCTOU
// (a component swapped to a link after its check); the complete fix is an openat(O_NOFOLLOW) per
// component (and Windows reparse-point rejection), tracked as v0.5 hardening. os.Root still blocks
// any link that ESCAPES the project, so even the race can only redirect WITHIN it.
func refuseSymlinkChain(r *safeio.Root, slashRel string) error {
	parts := strings.Split(slashRel, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if prefix == "" || prefix == "." {
			continue
		}
		fi, err := r.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // this component and everything below is created fresh — no planted link
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to operate on %s: path component %q is a symlink, and the runtime must not follow one (an agent may have planted it under .vichu)", slashRel, prefix)
		}
	}
	return nil
}

// writeJSON atomically writes v as indented JSON (with a trailing newline), confined to root.
func writeJSON(root, path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(root, path, append(data, '\n'), 0o644)
}

// readJSON reads and decodes JSON from an absolute path UNDER projectRoot, confined and WITHOUT
// following a final symlink. Plain os.ReadFile followed a symlink an agent could plant in
// `.vichu/` (which is outside the audit), so `state.json → /etc/secret` would be read back as
// the run's own state. Confinement (os.Root) also forbids the path escaping the project. Same
// root discipline as the writer (writeFileAtomic).
func readJSON(projectRoot, path string, v any) error {
	data, err := readFileConfined(projectRoot, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// readFileConfined reads path (absolute, under projectRoot) confined to the project and refusing
// a final symlink.
func readFileConfined(projectRoot, path string) ([]byte, error) {
	r, rel, err := confine(projectRoot, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.ReadFileNoFollow(rel)
}

// readDirConfined lists the entries of dir (absolute, under projectRoot) confined to the project.
func readDirConfined(projectRoot, dir string) ([]fs.DirEntry, error) {
	r, rel, err := confine(projectRoot, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.ReadDir(rel)
}

// existsConfined reports whether path exists (Lstat, so a symlink counts as present) confined to
// the project. It never follows the link — the content read that follows refuses to resolve it.
func existsConfined(projectRoot, path string) (bool, error) {
	r, rel, err := confine(projectRoot, path)
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()
	if _, lerr := r.Lstat(rel); lerr != nil {
		if errors.Is(lerr, fs.ErrNotExist) {
			return false, nil
		}
		return false, lerr
	}
	return true, nil
}
