package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/hostpacks"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// Host-pack install record locations. The record travels WITH the pack: it lives
// under the pack's own directory (.claude/) so it shares that directory's git fate
// — if a team commits .claude/, a teammate's clone gets both the pack files and the
// record, and `doctor`/`init --host` work. The old location was .vichu/host.json,
// but .vichu/ is gitignored (runtime), so the record never reached a clone while the
// pack files did — leaving doctor blind and re-install refusing to "clobber". We
// still READ the legacy path for migration and rewrite to the portable one on save.
const (
	vichuDir           = ".vichu"
	legacyHostRecord   = "host.json"       // legacy: .vichu/host.json (migrated from)
	hostRecordDir      = ".claude"         // portable: lives with the pack
	hostRecordFileName = "vichu-host.json" // portable: .claude/vichu-host.json
)

// portableHostRecordPath is where the install record lives now (with the pack).
func portableHostRecordPath(root string) string {
	return filepath.Join(root, hostRecordDir, hostRecordFileName)
}

// legacyHostRecordPath is the pre-portable location, kept only for migration.
func legacyHostRecordPath(root string) string {
	return filepath.Join(root, vichuDir, legacyHostRecord)
}

// hostManifest is a host pack's install contract (packs/<host>/manifest.json).
type hostManifest struct {
	Host         string             `json:"host"`
	Mode         string             `json:"mode"`
	Description  string             `json:"description"`
	Capabilities map[string]bool    `json:"capabilities"`
	Requires     []hostRequirement  `json:"requires"`
	Files        []hostManifestFile `json:"files"`
	Verify       hostVerify         `json:"verify"`
}

// hostVerify declares how `vichu doctor` checks the pack's agent is usable. Adapter
// names the registered adapter to probe (version + auth); empty means the pack drives
// no native adapter (a bridge/fallback host), so there is nothing to probe.
type hostVerify struct {
	Adapter string `json:"adapter"`
}

type hostRequirement struct {
	Bin    string `json:"bin"`
	Reason string `json:"reason"`
}

type hostManifestFile struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

// installedHost records what a host pack install wrote, under
// .claude/vichu-host.json (with the pack), so `init` can re-install idempotently and
// `doctor` can verify integrity even in a fresh clone. Hashes let us tell "VichuFlow
// installed this" from "the user edited it".
type installedHost struct {
	Host string `json:"host"`
	Mode string `json:"mode"`
	// PackHash fingerprints the embedded pack this install came from (hash over the
	// sorted dest+content of every file). doctor compares it against the binary's
	// current embedded pack to detect an OUTDATED install after an upgrade, distinct
	// from a user-ALTERED file (which the per-file Files hashes catch).
	PackHash string            `json:"pack_hash,omitempty"`
	Files    map[string]string `json:"files"` // dest (repo-relative) → sha256
}

// loadHostManifest reads and parses an embedded host pack's manifest.
func loadHostManifest(host string) (*hostManifest, error) {
	data, err := hostpacks.FS.ReadFile(path("packs", host, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf(i18n.T("host.unknown"), host, strings.Join(availableHosts(), ", "))
	}
	var m hostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("host pack %q has an invalid manifest: %w", host, err)
	}
	return &m, nil
}

// availableHosts lists the embedded host packs.
func availableHosts() []string {
	entries, _ := fs.ReadDir(hostpacks.FS, "packs")
	var hosts []string
	for _, e := range entries {
		if e.IsDir() {
			hosts = append(hosts, e.Name())
		}
	}
	sort.Strings(hosts)
	return hosts
}

// installHostPack copies a host pack's files into the project, refusing to
// overwrite files VichuFlow did not install (unless force). dryRun reports what
// would be written without touching disk. It returns the files written.
func installHostPack(root, host string, force, dryRun bool) ([]string, error) {
	m, err := loadHostManifest(host)
	if err != nil {
		return nil, err
	}
	if err := preflightHostPack(root, m, force); err != nil {
		return nil, err
	}
	if dryRun {
		return manifestDests(m), nil
	}
	return copyHostPack(root, host, m)
}

// preflightHostPack refuses to clobber a destination that exists and was NOT
// installed by VichuFlow (the user's file), unless force.
func preflightHostPack(root string, m *hostManifest, force bool) error {
	prev := loadInstalledHost(root) // may be nil
	for _, f := range m.Files {
		cur, statErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Dest)))
		if statErr != nil {
			continue // doesn't exist — safe to write
		}
		if force || weInstalled(prev, f.Dest, cur) {
			continue
		}
		return fmt.Errorf(i18n.T("host.would_clobber"), f.Dest)
	}
	return nil
}

// copyHostPack writes the pack's files and records the install in the portable
// record (.claude/vichu-host.json), so it travels with the committed pack.
func copyHostPack(root, host string, m *hostManifest) ([]string, error) {
	installed := &installedHost{
		Host: m.Host, Mode: m.Mode, PackHash: embeddedPackHash(host, m), Files: map[string]string{},
	}
	var written []string
	for _, f := range m.Files {
		content, rerr := hostpacks.FS.ReadFile(path("packs", host, f.Src))
		if rerr != nil {
			return written, fmt.Errorf("host pack %q is missing %q: %w", host, f.Src, rerr)
		}
		dst := filepath.Join(root, filepath.FromSlash(f.Dest))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return written, err
		}
		installed.Files[f.Dest] = hashBytes(content)
		written = append(written, f.Dest)
	}
	if err := saveInstalledHost(root, installed); err != nil {
		return written, err
	}
	return written, nil
}

func manifestDests(m *hostManifest) []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Dest)
	}
	return out
}

// weInstalled reports whether the on-disk content matches what VichuFlow recorded
// installing at dest (so re-install / overwrite is safe).
func weInstalled(prev *installedHost, dest string, cur []byte) bool {
	if prev == nil {
		return false
	}
	want, ok := prev.Files[dest]
	return ok && want == hashBytes(cur)
}

// loadInstalledHost reads the install record, preferring the portable location and
// falling back to the legacy one — so existing installs keep working and a committed
// pack carries its record into a clone.
func loadInstalledHost(root string) *installedHost {
	for _, p := range []string{portableHostRecordPath(root), legacyHostRecordPath(root)} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var ih installedHost
		if json.Unmarshal(data, &ih) == nil {
			return &ih
		}
	}
	return nil
}

// saveInstalledHost writes the record to the portable location (with the pack) and
// removes any legacy .vichu/host.json — a one-shot migration on the next install.
func saveInstalledHost(root string, ih *installedHost) error {
	data, err := json.MarshalIndent(ih, "", "  ")
	if err != nil {
		return err
	}
	dst := portableHostRecordPath(root)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, append(data, '\n'), 0o644); err != nil {
		return err
	}
	_ = os.Remove(legacyHostRecordPath(root)) // migrate away from the gitignored spot
	return nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// embeddedPackHash fingerprints a host pack as it ships in THIS binary: a hash over
// each file's dest and content, in a stable (dest-sorted) order. Comparing it to
// the hash recorded at install time tells doctor whether an upgrade changed the
// pack (the install is outdated) without re-reading every file on disk.
func embeddedPackHash(host string, m *hostManifest) string {
	files := append([]hostManifestFile(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Dest < files[j].Dest })
	h := sha256.New()
	for _, f := range files {
		content, err := hostpacks.FS.ReadFile(path("packs", host, f.Src))
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s\n%s\n", f.Dest, hashBytes(content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// uninstallHostPack removes the named host pack: only the files VichuFlow installed
// (whose content still matches the recorded install hash) are deleted; user-modified
// files are kept. It refuses to act on an unknown host or one that does not match
// what is actually installed, so a typo or a future multi-host setup can never
// delete the wrong pack. Returns the removed paths and the kept (user-modified) ones.
func uninstallHostPack(root, host string) (removed, kept []string, err error) {
	// Reject an unknown host before touching anything on disk.
	if _, err := loadHostManifest(host); err != nil {
		return nil, nil, err
	}
	ih := loadInstalledHost(root)
	if ih == nil {
		return nil, nil, errors.New(i18n.T("host.not_installed"))
	}
	// Never delete a different host's pack because of a typo or wrong --host.
	if ih.Host != host {
		return nil, nil, fmt.Errorf(i18n.T("host.mismatch"), ih.Host, host)
	}
	for dest, want := range ih.Files {
		full := filepath.Join(root, filepath.FromSlash(dest))
		cur, rerr := os.ReadFile(full)
		if rerr != nil {
			continue // already gone
		}
		if hashBytes(cur) != want {
			kept = append(kept, dest) // user edited it — never delete
			continue
		}
		if derr := os.Remove(full); derr == nil {
			removed = append(removed, dest)
		}
	}
	sort.Strings(removed)
	sort.Strings(kept)
	// The pack is uninstalled: always drop the install record (both the portable and
	// any legacy copy). Any kept file is now the user's own, so doctor must stop
	// treating the host pack as installed.
	_ = os.Remove(portableHostRecordPath(root))
	_ = os.Remove(legacyHostRecordPath(root))
	return removed, kept, nil
}

// installHostAndReport installs a host pack and prints what it did (or, with
// --dry-run, what it would do), then warns about any missing required binary.
func installHostAndReport(root, host string, force, dryRun bool) error {
	written, err := installHostPack(root, host, force, dryRun)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf(i18n.T("host.dry_run")+"\n", host)
	} else {
		fmt.Printf(i18n.T("host.installed")+"\n", host)
	}
	for _, f := range written {
		fmt.Printf("  %s\n", f)
	}
	warnMissingRequires(host)
	if !dryRun {
		fmt.Println("\n" + i18n.T("host.next"))
	}
	return nil
}

// warnMissingRequires checks the pack's required binaries and warns (non-fatal)
// if one is missing — you can install the pack now and the tool later. `vichu
// doctor` is the hard gate that fails on a missing requirement.
func warnMissingRequires(host string) {
	m, err := loadHostManifest(host)
	if err != nil {
		return
	}
	for _, req := range m.Requires {
		if _, lerr := exec.LookPath(req.Bin); lerr != nil {
			fmt.Fprintf(os.Stderr, i18n.T("host.req_warn")+"\n", req.Bin)
		}
	}
}

// path joins embed.FS path segments (always forward-slash, regardless of OS).
func path(parts ...string) string { return strings.Join(parts, "/") }
