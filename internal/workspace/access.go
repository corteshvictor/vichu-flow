package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/corteshvictor/vichu-flow/internal/safeio"
)

// entryKind is what a path IS, decided without resolving its final element.
type entryKind int

const (
	entryMissing entryKind = iota
	entryRegular
	entrySymlink
	entryOther // directory, socket, device — never content we back up or restore
)

// symlinkHashPrefix namespaces a symlink's fingerprint. A symlink's identity is the
// TEXT of its target, not the bytes it happens to point at: retargeting a link is a
// mutation even when both targets hold identical content, and hashing through it
// fingerprints content the workspace does not own. The prefix is not hex, so it can
// never collide with a regular file's hash — which stays a bare sha256, byte-identical
// to what earlier versions wrote.
//
// This DID change the on-disk format for symlinks, which older versions fingerprinted by
// following them. core.Workspace.FingerprintVersion records which format a run used, and a
// legacy run that tracks a symlink is failed closed rather than guessed at.
const symlinkHashPrefix = "symlink:"

// rollbackDir is where a rollback quarantines whatever it has to move out of the way. It
// lives under the runtime directory, which is already excluded from the audit wholesale —
// unlike a name-based exclusion, which would be a blind spot an agent could write into.
const rollbackDir = runtimeDirName + "/rollback"

// entry is a captured path: a regular file's content, or a symlink's target text, with
// the permission bits needed to put it back exactly as it was. A kind of entryMissing
// means the path is genuinely absent — never "we could not read it", which is a
// different thing and is always reported as an error.
type entry struct {
	kind entryKind
	mode os.FileMode
	data []byte
}

// openRoot returns a handle that confines every read and write to dir, refuses any path
// that escapes it, and never resolves a final symlink. Everything this package touches in
// the workspace goes through it — see internal/safeio for why there is exactly one of these.
func openRoot(dir string) (*safeio.Root, error) { return safeio.Open(dir) }

// lstatEntry reports what rel is without following it when it is a symlink.
//
// Only ErrNotExist means "absent". EVERY other error is returned, because the two are not
// interchangeable: a file that exists but cannot be READ (mode 000, a denied ACL, a
// directory we may not traverse) used to collapse into "missing" — so it hashed to "",
// looked like a path the worker had just created, was never backed up, and a gate could
// chmod it and overwrite it with the run still reaching `completed`. Failing to read is
// not evidence of absence; it is a reason to stop.
func lstatEntry(r *safeio.Root, rel string) (entryKind, os.FileMode, error) {
	fi, err := r.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return entryMissing, 0, nil
		}
		return entryMissing, 0, fmt.Errorf("cannot stat %s: %w", rel, err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return entrySymlink, fi.Mode().Perm(), nil
	case fi.Mode().IsRegular():
		return entryRegular, fi.Mode().Perm(), nil
	}
	return entryOther, fi.Mode().Perm(), nil
}

// readEntry captures rel's content (regular file) or target text (symlink). A directory,
// socket or device is not content we can restore, so it comes back as entryMissing with
// no error — we simply have nothing to hold. A path we cannot READ, though, is an error.
func readEntry(r *safeio.Root, rel string) (entry, error) {
	kind, mode, err := lstatEntry(r, rel)
	if err != nil {
		return entry{}, err
	}
	switch kind {
	case entryRegular:
		data, rerr := r.ReadFile(rel)
		if rerr != nil {
			return entry{}, fmt.Errorf("cannot read %s: %w", rel, rerr)
		}
		return entry{kind: entryRegular, mode: mode, data: data}, nil
	case entrySymlink:
		target, rerr := r.Readlink(rel)
		if rerr != nil {
			return entry{}, fmt.Errorf("cannot read symlink %s: %w", rel, rerr)
		}
		return entry{kind: entrySymlink, mode: mode, data: []byte(target)}, nil
	}
	return entry{kind: entryMissing}, nil
}

// writeEntry puts e back at rel: content, permission bits and TYPE, all through the shared
// atomic writer (random O_EXCL temp → fsync → rename), so the destination is replaced
// rather than opened — a symlink a gate planted there is never followed.
//
// The one case rename cannot handle is a DIRECTORY sitting where the file belongs: a gate
// that replaced `draft.txt` with `draft.txt/generated`. Remove() will not delete a non-empty
// directory, so the rollback simply failed and the user's file stayed gone. It is moved out
// of the way instead — see quarantine.
func writeEntry(r *safeio.Root, rel string, e entry) error {
	kind, _, err := lstatEntry(r, rel)
	if err != nil {
		return err
	}
	if kind == entryOther {
		if qerr := quarantine(r, rel); qerr != nil {
			return qerr
		}
	}
	if e.kind == entrySymlink {
		return r.WriteSymlinkAtomic(rel, string(e.data))
	}
	return r.WriteFileAtomic(rel, e.data, e.mode)
}

// quarantineRecord is written and fsynced BEFORE anything moves, so an interrupted rollback
// is explainable rather than mysterious: it names what was moved, and where it went.
type quarantineRecord struct {
	Path        string    `json:"path"`
	DisplacedTo string    `json:"displaced_to"`
	MovedAt     time.Time `json:"moved_at"`
}

// quarantine moves whatever non-file thing occupies rel into .vichu/rollback/, so the
// original can be restored over the space it vacates.
//
// It does NOT RemoveAll. We did not create that directory and we have not looked inside it;
// deleting it to make room would be destroying data to undo the destruction of data. Moving
// it preserves everything and leaves the evidence where a human can find it.
func quarantine(r *safeio.Root, rel string) error {
	sum := sha256.Sum256([]byte(rel))
	dest := path.Join(rollbackDir, hex.EncodeToString(sum[:8])+"-"+strings.ReplaceAll(path.Base(rel), "/", "_"))
	if err := r.MkdirAll(rollbackDir, 0o755); err != nil {
		return fmt.Errorf("cannot prepare the rollback quarantine: %w", err)
	}
	// A previous rollback already quarantined something here. Do not clobber it — that is
	// the one thing quarantine exists to avoid — so give this one its own slot.
	if kind, _, err := lstatEntry(r, dest); err != nil {
		return err
	} else if kind != entryMissing {
		suffix, terr := uniqueSuffix()
		if terr != nil {
			return terr
		}
		dest += "-" + suffix
	}

	rec, err := json.Marshal(quarantineRecord{Path: rel, DisplacedTo: dest, MovedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	// The record lands FIRST so a CRASH between here and the rename still leaves a breadcrumb.
	// But a rename that returns an ERROR is not a crash: the move did not happen, and a record
	// claiming it did — plus a message saying "it is quarantined at" — is the kernel lying about
	// data it never moved. On that path we remove the record and report the failure honestly.
	if werr := r.WriteFileAtomic(dest+".json", append(rec, '\n'), 0o644); werr != nil {
		return fmt.Errorf("cannot record the rollback quarantine: %w", werr)
	}
	if rerr := r.Rename(rel, dest); rerr != nil {
		_ = r.Remove(dest + ".json") // no move happened — do not leave a record asserting one
		return fmt.Errorf("a gate replaced %s with a directory and it could NOT be moved aside to restore the original — nothing was quarantined (the move to %s failed, likely because a parent directory is not writable): %w", rel, dest, rerr)
	}
	return nil
}

// uniqueSuffix is a short random tag for a quarantine slot that is already taken.
func uniqueSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cannot name a rollback quarantine slot: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// hashEntry fingerprints rel without following a final symlink. Regular files keep the
// bare sha256 hex earlier versions wrote; symlinks hash their target text under a distinct
// prefix. A genuinely absent path hashes to "" — but a path we cannot read is an ERROR,
// never an empty hash, because "" is how the tracker records "this file is not there".
func hashEntry(r *safeio.Root, rel string) (string, entry, error) {
	e, err := readEntry(r, rel)
	if err != nil {
		return "", entry{}, err
	}
	if e.kind == entryMissing {
		return "", e, nil
	}
	sum := sha256.Sum256(e.data)
	if e.kind == entrySymlink {
		return symlinkHashPrefix + hex.EncodeToString(sum[:]), e, nil
	}
	return hex.EncodeToString(sum[:]), e, nil
}

// writeConfinedAtomic writes data to abs — which must live under root — atomically and
// confined, so a symlink an agent planted under .vichu cannot redirect the write through it
// or truncate an external file. It is for the baseline metadata (baseline.id, the manifest)
// that sits directly under .vichu, outside the baseline tree that rebaseline wipes first.
func writeConfinedAtomic(root, abs string, data []byte, mode os.FileMode) error {
	r, err := openRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write %s outside %s", abs, root)
	}
	return r.WriteFileAtomic(filepath.ToSlash(rel), data, mode)
}

// ensureRealRuntimeDir refuses to proceed if .vichu under projectRoot exists but is not a
// real directory (a symlink, or a file). The kernel's confined writers root at the project
// and reject an escaping .vichu on their own, but os.RemoveAll and any other raw path
// operation follow a symlinked parent — so this guards the paths that do not go through the
// confined writer. A missing .vichu is fine; it will be created as a real directory.
func ensureRealRuntimeDir(projectRoot string) error {
	fi, err := os.Lstat(filepath.Join(projectRoot, runtimeDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory (it is a symlink or a file) — move it aside so VichuFlow can create a real one; following it would let a run read and write outside the project", runtimeDirName)
	}
	return nil
}

// IsSymlinkFingerprint reports a fingerprint produced by the current format for a symlink.
// Runs snapshotted by an older binary followed the link and stored the target's CONTENT
// hash instead, which is indistinguishable from a regular file's — so a bare hex hash where
// we now compute a symlink one means the two are not comparable, not that the link changed.
// See core.Workspace.FingerprintVersion.
func IsSymlinkFingerprint(h string) bool { return strings.HasPrefix(h, symlinkHashPrefix) }
