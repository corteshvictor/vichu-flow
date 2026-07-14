// Command packhistory records a released host pack so future upgrades keep working.
//
// `vichu init --host` and `vichu uninstall` decide whether a file is OURS by comparing its
// bytes against the pack this binary ships AND every version we ever released. That history
// is `known-hashes.json` (compiled into the binary) plus the released files themselves
// (checked in under cmd/vichu/testdata/, so the gate tests can verify the catalog is truthful).
//
// Miss a release and the failure is silent and nasty: a user's untouched pack from that
// release stops looking like ours, `vichu doctor` tells them to refresh, and the refresh
// REFUSES. The upgrade path dead-ends.
//
// This exists because the obvious shell one-liner is WRONG. `git show --name-only <tag>` lists
// what the TAGGED COMMIT changed — and a release-please tag commit only touches the version and
// the changelog, so it enumerates the pack zero times. The pack must be read from the tag's
// TREE, and the manifest at that tag is the only thing that knows which files the pack had.
//
//	go run ./tools/packhistory --host claude-code --tag v0.4.0
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"

	"github.com/corteshvictor/vichu-flow/internal/hostpacks"
)

// backupSuffix names the directory an in-progress run parks the previous fixtures in.
const backupSuffix = ".backup"

type manifest struct {
	Files []struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	} `json:"files"`
}

func main() {
	host := flag.String("host", "claude-code", "host pack to record")
	tag := flag.String("tag", "", "released git tag whose pack to record (e.g. v0.4.0)")
	flag.Parse()
	if *tag == "" {
		fail(errors.New("--tag is required (the release whose pack you are recording)"))
	}
	if err := run(*host, *tag); err != nil {
		fail(err)
	}
}

// run is ALL-OR-NOTHING. A half-recorded release is the worst outcome available: it passes
// every gate that only compares what exists, and then the ONE file whose hash never made it
// into the catalog turns a future user's untouched pack into "you edited this" and refuses to
// upgrade them. So: read and validate everything, build the result in memory, stage it, and
// only then commit — with the catalog last, because the catalog is what the binary reads.
func run(host, tag string) error {
	packRoot := "internal/hostpacks/packs/" + host

	// One recorder at a time. Two `packhistory` runs for different tags both read the catalog,
	// both add their hash, and the second write wins — one release silently loses its hash, and
	// BOTH commands report success. That is a read-modify-write race on the one file that makes
	// upgrades work.
	//
	// A stale lock is a FAILURE, not something to reclaim: this runs once per release, by a
	// human, and "wait or delete the lock" is a fine thing to tell them. Guessing that the other
	// process is dead is how you get two writers.
	unlock, err := lockHistory(packRoot)
	if err != nil {
		return err
	}
	defer unlock()

	bodies, err := readReleasedPack(tag, packRoot)
	if err != nil {
		return err
	}
	catalogPath := filepath.Join(packRoot, "known-hashes.json")
	catalog, err := loadCatalog(catalogPath)
	if err != nil {
		return err
	}

	// Build the final catalog in memory. Nothing on disk has changed yet.
	added := 0
	for dest, body := range bodies {
		h := sha256Hex(body)
		if !slices.Contains(catalog[dest], h) {
			catalog[dest] = append(catalog[dest], h)
			added++
		}
		sort.Strings(catalog[dest])
	}

	fixtureRoot := filepath.Join("cmd", "vichu", "testdata", "packs", host, tag)
	if err := commit(fixtureRoot, catalogPath, bodies, catalog); err != nil {
		return err
	}

	fmt.Printf("recorded the %s pack from %s: %d file(s), %d new hash(es)\n", host, tag, len(bodies), added)
	fmt.Printf("  catalog:  %s\n  fixtures: %s/\n\nNow run:\n  go test ./cmd/vichu/ -run 'TestKnownHashesCatalogIsTruthful|TestEveryReleasedPackIsRecorded'\n", catalogPath, fixtureRoot)
	return nil
}

// readReleasedPack reads every file the pack had AT THE TAG — from the tag's tree, driven by
// the manifest AT THE TAG, whose destinations are validated before a single byte is written.
func readReleasedPack(tag, packRoot string) (map[string][]byte, error) {
	raw, err := gitShow(tag + ":" + packRoot + "/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("reading the manifest at %s: %w (is the tag fetched? `git fetch --tags`)", tag, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the manifest at %s is not valid JSON: %w", tag, err)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("the manifest at %s declares no files — nothing to record, which is almost certainly wrong", tag)
	}
	dests := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		dests = append(dests, f.Dest)
	}
	if err := hostpacks.ValidateDests(dests); err != nil {
		return nil, fmt.Errorf("the manifest at %s: %w", tag, err)
	}

	bodies := map[string][]byte{}
	for _, f := range m.Files {
		body, gerr := gitShow(tag + ":" + packRoot + "/" + f.Src)
		if gerr != nil {
			return nil, fmt.Errorf("reading %s at %s: %w", f.Src, tag, gerr)
		}
		bodies[f.Dest] = body
	}
	return bodies, nil
}

// stageFixtures writes the release's files into a fresh temp directory, through os.Root so a
// symlink cannot carry a write outside it — the lexical check is not enough on its own.
func stageFixtures(parent string, bodies map[string][]byte) (string, error) {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	staged, err := os.MkdirTemp(parent, ".staging-*")
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(staged)
	if err != nil {
		_ = os.RemoveAll(staged)
		return "", err
	}
	defer func() { _ = root.Close() }()

	for dest, body := range bodies {
		rel := filepath.FromSlash(dest)
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			_ = os.RemoveAll(staged)
			return "", err
		}
		if err := root.WriteFile(rel, body, 0o644); err != nil {
			_ = os.RemoveAll(staged)
			return "", fmt.Errorf("staging %s: %w", dest, err)
		}
	}
	return staged, nil
}

// loadCatalog reads the released-versions catalog. ABSENT is fine (the first release). Any
// other error aborts: treating "I could not read it" as "it is not there" would rewrite the
// catalog from scratch and silently drop every release recorded before this one.
func loadCatalog(p string) (map[string][]string, error) {
	catalog := map[string][]string{}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return catalog, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", p, err)
	}
	if uerr := json.Unmarshal(data, &catalog); uerr != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", p, uerr)
	}
	return catalog, nil
}

func writeJSONAtomic(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".known-hashes-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func gitShow(ref string) ([]byte, error) { return exec.Command("git", "show", ref).Output() }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "packhistory:", err)
	os.Exit(1)
}

// lockHistory takes an exclusive lock on the pack's release history. O_EXCL, no heartbeat, no
// reclaim: an abandoned lock fails loudly and a human removes it.
func lockHistory(packRoot string) (func(), error) {
	p := filepath.Join(packRoot, ".packhistory.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("another packhistory run holds %s — wait for it, or delete that file if it was abandoned", p)
	}
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "pid %d\n", os.Getpid())
	_ = f.Close()
	return func() { _ = os.Remove(p) }, nil
}

// reconcileInterrupted cleans up after a run that died between "park the old fixtures aside"
// and "put the new ones in place". Without this, the leftover `.backup` directory simply
// stayed — a retry exited 0, and BOTH directories went into the repo with CI green, because
// every gate only looks at the real one.
//
// The decision is made from CONTENT, not from the names:
//
//	backup only          → the crash caught us mid-swap. Put it back; this run replaces it.
//	both, new one is ours → the previous run got as far as installing. Drop the stale backup.
//	both, new one is not  → we cannot tell which is the recorded release. FAIL, and name both:
//	                        deleting the wrong history is not a mistake you can undo.
func reconcileInterrupted(fixtureRoot string, bodies map[string][]byte) error {
	backup := fixtureRoot + backupSuffix
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(fixtureRoot); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "packhistory: recovering an interrupted run — restoring %s\n", backup)
		return os.Rename(backup, fixtureRoot)
	} else if err != nil {
		return err
	}
	same, err := fixturesMatch(fixtureRoot, bodies)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("both %s and %s exist, and the first does not match the release you are recording — a previous run was interrupted and I cannot tell which one is real. Inspect both, keep the right one, delete the other, then re-run", fixtureRoot, backup)
	}
	fmt.Fprintf(os.Stderr, "packhistory: an earlier run already installed these fixtures — dropping the stale %s\n", backup)
	return os.RemoveAll(backup)
}

// fixturesMatch reports whether the directory holds exactly the release's files, byte for byte.
func fixturesMatch(root string, bodies map[string][]byte) (bool, error) {
	seen := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		want, ok := bodies[filepath.ToSlash(rel)]
		if !ok {
			return errUnexpectedFixture
		}
		got, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if !bytes.Equal(got, want) {
			return errUnexpectedFixture
		}
		seen++
		return nil
	})
	if errors.Is(err, errUnexpectedFixture) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return seen == len(bodies), nil
}

var errUnexpectedFixture = errors.New("fixture does not match the release")

// commit installs the fixtures and then the catalog, and puts everything back if either fails.
// The catalog goes LAST because it is what the binary compiles in: it is the commit point.
func commit(fixtureRoot, catalogPath string, bodies map[string][]byte, catalog map[string][]string) error {
	// First, clean up after a run that died mid-swap. Recovery is decided by CONTENT, never by a
	// filename — the same rule the installer follows, and for the same reason: a name proves
	// nothing, and guessing is how you delete the history you meant to keep.
	if err := reconcileInterrupted(fixtureRoot, bodies); err != nil {
		return err
	}
	staged, err := stageFixtures(filepath.Dir(fixtureRoot), bodies)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }() // no-op once the rename succeeds

	// Move the existing fixtures ASIDE — never RemoveAll before the commit. Deleting them first
	// meant a later failure (a full disk, an unwritable catalog) destroyed history that was
	// perfectly good: the command exited 1 and took the previous release's files with it.
	backup := ""
	if _, serr := os.Stat(fixtureRoot); serr == nil {
		backup = fixtureRoot + backupSuffix
		if rerr := os.Rename(fixtureRoot, backup); rerr != nil {
			return rerr
		}
	}
	restore := func() {
		_ = os.RemoveAll(fixtureRoot)
		if backup != "" {
			_ = os.Rename(backup, fixtureRoot)
		}
	}
	if err := os.Rename(staged, fixtureRoot); err != nil {
		restore()
		return err
	}
	if err := writeJSONAtomic(catalogPath, catalog); err != nil {
		restore()
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}
