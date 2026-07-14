package main

import (
	"bytes"
	"context"
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
	runtime "github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/safeio"
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
	return filepath.Join(root, relPortableRecord)
}

// Project-relative paths, resolved through the confined root (never joined by hand).
var (
	relPortableRecord = filepath.Join(hostRecordDir, hostRecordFileName)
	relLegacyRecord   = filepath.Join(vichuDir, legacyHostRecord)
	relHostSettings   = filepath.Join(hostRecordDir, hostSettingsFile)
)

func relOwnership(host string) string {
	return filepath.Join(vichuDir, "hosts", host, "ownership.json")
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
	// Permissions are the host's tool-permission rules the pack needs pre-authorized —
	// the kernel commands the orchestrator calls on every run. Without them the host
	// prompts the user for approval on each `vichu` call, which makes a run unusable.
	// They are MERGED into the host's shared settings, never clobbered (see
	// mergeHostPermissions).
	Permissions []string `json:"permissions"`
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

// dests lists the manifest's destinations, for hostpacks.ValidateDests.
func (m *hostManifest) dests() []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Dest)
	}
	return out
}

// installedHost records what a host pack install wrote, under .claude/vichu-host.json (with the
// pack), so `init` can re-install idempotently. Its hashes are DIAGNOSTIC ONLY — the record is
// committed, so a clone carries any hashes it likes. `doctor` verifies integrity against the
// EMBEDDED pack + known-hashes.json (see packOwnership), never against this record.
type installedHost struct {
	Host string `json:"host"`
	Mode string `json:"mode"`
	// PackHash fingerprints the embedded pack this install came from (hash over the sorted
	// dest+content of every file). It is DIAGNOSTIC ONLY — recorded for human inspection and
	// compatibility. It grants no authority: the record is committed, so a clone carries whatever
	// pack_hash it likes. doctor decides integrity AND outdated from the BYTES on disk vs the
	// embedded pack and known-hashes.json (see packOwnership / hostFilesCheck), never from this.
	PackHash string            `json:"pack_hash,omitempty"`
	Files    map[string]string `json:"files"` // dest (repo-relative) → sha256, also diagnostic only
}

// hostOwnership is the local ledger's CLAIM about which permission rules an install of ours
// added to `.claude/settings.json`. It is a claim, not proof — and the difference is the whole
// contract (plan §9.5).
//
// It lives under `.vichu/`, which is excluded from the mutation audit so the KERNEL can write
// there — which means an agent with a write tool can too. Nothing inside a workspace an agent
// can write is evidence. (The portable record is worse: it is committed, so a cloned repo
// writes it.)
//
// So nothing destructive happens on this file's word alone:
//
//   - `uninstall` will NOT withdraw the rules it names without `--withdraw-permissions`.
//     Leaving a permission behind is one edit to undo; deleting one the user wrote is not.
//   - `init` DOES withdraw a stale rule, but only one from the published catalog
//     (vichuAuthoredRules) — a bounded security exception, because removing a grant can only
//     ever REDUCE what an agent may do, and the rules we retire are ones we deemed unsafe.
//
// Files need no ledger at all: ownership there is proved against the pack embedded in this
// binary (see packOwnership), which is the one reference a repo cannot forge.
type hostOwnership struct {
	Host string `json:"host"`
	// AddedPermissions is what this ledger CLAIMS an install of ours added. Not proof — see the
	// type comment. `uninstall` will not act on it without `--withdraw-permissions`.
	AddedPermissions []string `json:"added_permissions,omitempty"`
}

// vichuAuthoredRules is every permission rule any VichuFlow release has ever asked a host to
// authorize. The ledger records WHICH of these we added to a given project; this list bounds
// which rules could possibly be ours AT ALL.
//
// It exists because the ledger is a plain JSON file inside the workspace, under `.vichu/` —
// which is deliberately excluded from the mutation audit (it is the kernel's own runtime, so
// a worker writing there must never count as a mutation). An agent with a write tool can
// therefore forge it: add `Bash(user-owned *)` to `added_permissions`, and the next
// `vichu uninstall` strips a rule the user wrote themselves. That is the kernel presenting
// manipulable data as proof of ownership.
//
// A forged claim about a rule we never authored is now simply refused. An agent can still
// forge a claim about one of OUR rules — and gains nothing: the rules only ever GRANT, so
// withdrawing one hurts the agent, not the user.
//
// v0.5's `--allow-gates` will add rules derived from the user's vichu.yaml, which cannot be
// listed statically. That is why the plan blocks it on `hostBackend` (§9.2): the ownership
// store has to move somewhere an agent inside the workspace cannot reach first.
var vichuAuthoredRules = map[string]bool{
	// v0.4.1-dev, WITHDRAWN — a wildcard over every subcommand, including `run resume`.
	"Bash(vichu *)": true,
	// v0.4.1-dev, WITHDRAWN — `run start` opens a NEW run, which re-baselines the tree.
	"Bash(vichu run start:*)": true,
	// The per-step commands the orchestrator actually needs.
	"Bash(vichu worker start:*)":    true,
	"Bash(vichu worker complete:*)": true,
	"Bash(vichu review complete:*)": true,
	"Bash(vichu stage close:*)":     true,
	"Bash(vichu status:*)":          true,
	"Bash(vichu observe:*)":         true,
}

// ourRules filters a ledger's ownership claims down to rules VichuFlow could actually have
// authored. Anything else is a forged claim; we refuse it and say so, because a ledger
// naming a rule we never shipped is evidence that something wrote to `.vichu/` that should
// not have.
func ourRules(claimed []string) []string {
	var out []string
	for _, r := range claimed {
		if vichuAuthoredRules[r] {
			out = append(out, r)
			continue
		}
		fmt.Fprintf(os.Stderr, i18n.T("host.forged_claim")+"\n", r)
	}
	return out
}

// loadHostOwnership reads the local ledger. Absent (nil, nil) is normal — a fresh clone,
// or an install from a build that predates the ledger.
func loadHostOwnership(pr *projectRoot, host string) (*hostOwnership, error) {
	rel := relOwnership(host)
	data, err := pr.readFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the local ownership ledger for %q: %w", host, err)
	}
	var ow hostOwnership
	if uerr := json.Unmarshal(data, &ow); uerr != nil {
		return nil, fmt.Errorf("the local ownership ledger for %q is corrupt: %w — delete %s and re-run `vichu init --host %s`", host, uerr, rel, host)
	}
	return &ow, nil
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

// installPlan is everything an install WILL do, computed before a single byte is
// written. Validating first and mutating second is the same rule the kernel follows
// for its transactional commands: if a call is going to fail, it must fail without
// having touched disk. Without it, a malformed settings.json left the pack files and
// the install record on disk and then errored out — a half-installed project.
type installPlan struct {
	manifest   *hostManifest
	files      map[string][]byte // dest → content, already read out of the embedded FS
	packHash   string
	prev       *installedHost // the previous install record, if any (nil = first install)
	owned      *hostOwnership // the LOCAL ledger of rules a previous install on this machine added
	settings   *hostSettings  // the parsed, VALIDATED settings.json (empty if absent)
	addRules   []string       // rules missing from settings.allow — what we would add
	staleRules []string       // rules a PREVIOUS install added that this pack no longer declares
	// How the LEDGER CLAIMS each rule already in settings.json got there — ours, or the user's.
	// A claim, not proof (see hostOwnership): it decides what we SAY, and what
	// `--withdraw-permissions` would act on. Telling someone "these were already yours" about
	// rules we put there is a lie, and it is precisely the lie that decides whether they expect
	// `uninstall` to clean up after itself.
	ledgerClaimedOurs  []string
	ledgerClaimedUsers []string
	// How each destination that ALREADY exists was recognized. The record plays no part in
	// AUTHORIZING anything (see packOwnership) — it is consulted here only to choose the
	// right words, because "we are claiming a file nothing vouched for" and "we are upgrading
	// a file we installed" are different things to say to a person.
	alreadyCurrent []string // byte-for-byte what this binary ships: nothing changes
	fromRelease    []string // an earlier VichuFlow release: this refresh upgrades it
	adopted        []string // ours by content, but NO install record vouched for it
	replaced       []string // NOT ours — overwritten only because --force said so
	// identical is the set of destinations whose ON-DISK bytes already equal what we would write,
	// whether vouched (alreadyCurrent) or not (an adopted file that happens to match current). We
	// never rewrite these: doing so only churns the inode/mtime and drops xattrs/ACLs for no change.
	identical map[string]bool
}

// planHostInstall does everything that can fail WITHOUT side effects: load the manifest,
// read every embedded file, check we are not clobbering the user's work, and parse and
// type-check the host's settings.json. Only if all of that holds does the caller start
// writing. It touches nothing on disk.
func planHostInstall(pr *projectRoot, host string, force bool) (*installPlan, error) {
	m, err := loadHostManifest(host)
	if err != nil {
		return nil, err
	}
	// The manifest is embedded (ours), but validate its destinations anyway — and validate them
	// with the SHARED checker, because `files` drives every write and there is now more than
	// one consumer of a manifest (see hostpacks.ValidateDests).
	if err := hostpacks.ValidateDests(m.dests()); err != nil {
		return nil, err
	}
	prev, err := loadInstalledHost(pr)
	if err != nil {
		return nil, err
	}
	// What this binary knows about which bytes are ours — the pack it ships, plus every
	// version we ever released. The clobber check needs it.
	own, err := loadPackOwnership(host, m)
	if err != nil {
		return nil, err
	}
	files := own.current
	owned, err := loadHostOwnership(pr, host)
	if err != nil {
		return nil, err
	}
	plan, err := preflightHostPack(pr, m, own, prev, force)
	if err != nil {
		return nil, err
	}
	// The ledger is read even with no install record (a crashed install, a deleted marker).
	// It is NOT authority — it lives inside the workspace, under `.vichu/`, which is excluded
	// from the mutation audit so the kernel can write there, which means an agent with a write
	// tool can too. Its claims are a PROPOSAL (§9.5): `uninstall` will not act on them without
	// `--withdraw-permissions`, and `init` only ever withdraws rules from the published
	// catalog — a bounded security exception, because removing a grant can only reduce what an
	// agent may do.
	settings, err := loadHostSettings(pr)
	if err != nil {
		return nil, err
	}
	plan.manifest, plan.files, plan.packHash = m, files, embeddedPackHash(host, m)
	plan.prev, plan.owned, plan.settings = prev, owned, settings
	plan.addRules = settings.missing(m.Permissions)
	plan.staleRules = staleOwnedRules(owned, m.Permissions)
	plan.ledgerClaimedOurs = alreadyPresent(settings, m.Permissions, owned, true)
	plan.ledgerClaimedUsers = alreadyPresent(settings, m.Permissions, owned, false)
	return plan, nil
}

// alreadyPresent splits the pack's rules ALREADY in settings.json by what the LEDGER CLAIMS:
// that an earlier install of ours added it, or that it was the user's own.
//
// "Claims" is the operative word — the ledger is forgeable (§9.5). This split decides what we
// SAY, and what `--withdraw-permissions` would act on if the user asks. It is not proof.
func alreadyPresent(s *hostSettings, declared []string, owned *hostOwnership, ledgerClaimed bool) []string {
	inLedger := map[string]bool{}
	for _, r := range prevOwned(owned) {
		inLedger[r] = true
	}
	missing := map[string]bool{}
	for _, r := range s.missing(declared) {
		missing[r] = true // not in settings at all — neither ours nor theirs yet
	}
	var out []string
	for _, r := range declared {
		if !missing[r] && inLedger[r] == ledgerClaimed {
			out = append(out, r)
		}
	}
	return out
}

// staleOwnedRules are rules the LEDGER CLAIMS a previous install of ours added, which this pack
// no longer declares. Refreshing withdraws them, in the same write that adds the new ones.
//
// This is the one place `init` acts on the ledger without asking, and it is a BOUNDED security
// exception, not a claim that the ledger is trustworthy: only rules from the published catalog
// (vichuAuthoredRules) can be withdrawn, and withdrawing a grant can only ever REDUCE what an
// agent may do. Leaving a rule we have declared unsafe in place because the ledger *might* have
// been forged is the worse failure of the two.
//
// This exists because of a real hole we shipped: the pack once authorized `Bash(vichu *)`,
// a wildcard covering EVERY vichu subcommand — including `vichu run resume`, which clears a
// block. An agent whose read-only violation blocked the run could therefore unblock itself,
// which makes the entire product theater. The pack now enumerates only the transactional
// commands. But narrowing the manifest is not enough: the old wildcard is already sitting in
// the user's settings.json, and an upgrade that leaves it there fixes nothing.
//
// Only rules the LEDGER CLAIMS we added are withdrawn — and "claims" is the honest word: the
// ledger is forgeable (see hostOwnership). What makes acting on it safe here is the bound, not
// the claim: `ourRules` restricts it to the published catalog, and withdrawing a grant can only
// ever REDUCE what an agent may do. Nothing the user wrote can be taken from them by this path
// unless they wrote one of OUR retired rules — and losing that is one line to re-add, while
// leaving a rule we have declared unsafe in place is a hole we chose not to close.
func staleOwnedRules(owned *hostOwnership, declared []string) []string {
	if owned == nil {
		return nil
	}
	want := map[string]bool{}
	for _, r := range declared {
		want[r] = true
	}
	var stale []string
	for _, r := range ourRules(owned.AddedPermissions) {
		if !want[r] {
			stale = append(stale, r)
		}
	}
	return stale
}

// projectRoot confines every path the installer touches to the project directory.
//
// Plain filepath.Join + os.WriteFile FOLLOW SYMLINKS. Make `.claude` a symlink to a
// directory outside the repo and `vichu init --host` cheerfully writes the whole pack —
// the skill, the subagents, the settings, the record — out there, and reports success. An
// installer that can be redirected by a symlink in the tree it is installing into is an
// installer that writes wherever the repo tells it to.
//
// os.Root resolves every path relative to the opened directory and refuses to escape it,
// symlinks included. (The escape it is closing exists in os.Root itself before Go 1.26.5
// — GO-2026-4970 — which is why go.mod's floor is a security floor.)
type projectRoot struct{ root *safeio.Root }

func openProjectRoot(root string) (*projectRoot, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	r, err := safeio.Open(root)
	if err != nil {
		return nil, err
	}
	return &projectRoot{root: r}, nil
}

func (p *projectRoot) Close() { _ = p.root.Close() }

// writeFileAtomic writes rel through the confined root. The atomic-write machinery — random
// O_EXCL temp, mode at creation, fsync, rename — lives in internal/safeio, and it lives
// there because writing a second copy of it is exactly how the predictable-temp-name bug
// came back after being fixed here.
func (p *projectRoot) writeFileAtomic(rel string, data []byte, mode fs.FileMode) error {
	return p.root.WriteFileAtomic(rel, data, mode)
}

func (p *projectRoot) readFile(rel string) ([]byte, error) { return p.root.ReadFile(rel) }
func (p *projectRoot) readFileNoFollow(rel string) ([]byte, error) {
	return p.root.ReadFileNoFollow(rel)
}
func (p *projectRoot) remove(rel string) error { return p.root.Remove(rel) }
func (p *projectRoot) lstat(rel string) (fs.FileInfo, error) {
	return p.root.Lstat(rel)
}

// installHostPack copies a host pack's files into the project, refusing to
// overwrite files VichuFlow did not install (unless force). dryRun reports what
// would be written without touching disk. It returns the plan and the files written.
//
// The install is ALL-OR-NOTHING **for errors it can see**. A failed install that leaves half
// a skill, an authorized permission and no install record is worse than no install: the user
// is told it failed, but their project changed. So: everything that can fail without side
// effects fails first (manifest, embedded files, clobber check, settings shape); then we
// write, keeping an undo log; and any error after that rolls the project back to exactly how
// we found it.
//
// The honest limit: an abrupt KILL (SIGKILL, power loss) never returns an error, so the
// rollback never runs. What covers that is not this function but the CONTENT check in
// preflight — the pack files a crashed install left behind are byte-for-byte ours, so the
// retry recognizes and overwrites them instead of demanding --force. A durable install
// journal (plan §9.3) would close the remaining gap: a crash between the settings write and
// the install record leaves rules authorized that the record does not yet account for.
func installHostPack(root, host string, force, dryRun bool) (*installPlan, []string, error) {
	// One host-pack operation at a time per project. Two `vichu init --host` processes
	// otherwise read the same `.claude/settings.json` allow-list, each append their rules,
	// and the last write wins — silently dropping the other's. The lock is held across
	// plan, writes, rollback and commit, so the whole operation is single-writer.
	//
	// NOT for --dry-run: taking the lock creates .vichu/, and a dry run that creates a
	// directory is a dry run that lied. It writes nothing, so it needs nothing.
	if !dryRun {
		unlock, lerr := lockHostPack(root)
		if lerr != nil {
			return nil, nil, lerr
		}
		defer unlock()
	}

	pr, err := openProjectRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer pr.Close()

	plan, err := planHostInstall(pr, host, force)
	if err != nil {
		return nil, nil, err
	}
	if dryRun {
		return plan, manifestDests(plan.manifest), nil
	}
	undo := &undoLog{pr: pr}
	written, err := applyHostInstall(pr, plan, undo)
	if err != nil {
		if rerr := undo.rollback(); rerr != nil {
			// The install failed AND we could not fully undo it. Say so: a rollback that
			// silently half-works leaves the user believing nothing happened.
			return nil, nil, fmt.Errorf("%w — and the rollback did not fully succeed: %v; check your project's .claude/ before re-running", err, rerr)
		}
		return nil, nil, err
	}
	return plan, written, nil
}

// applyHostInstall performs the writes, in commit order: pack files, then the shared
// settings, then the local ownership ledger, then the portable record LAST — the record is
// the commit point, because it is what says "this project has a pack".
func applyHostInstall(pr *projectRoot, plan *installPlan, undo *undoLog) ([]string, error) {
	written, err := copyHostPack(pr, plan, undo)
	if err != nil {
		return nil, err
	}
	if err := plan.settings.addAndSave(pr, plan.addRules, plan.staleRules, undo); err != nil {
		return nil, err
	}
	// The ledger goes BEFORE the portable record — not because it authorizes anything (it does
	// not; see hostOwnership), but because it is the only note we keep of a change we JUST made
	// to the user's settings. If a crash lands between the two writes, the note must already
	// exist, or we forget what we did to a file that is theirs.
	if err := saveHostOwnership(pr, plan.ownership(), undo); err != nil {
		return nil, err
	}
	if err := saveInstalledHost(pr, plan.record(), undo); err != nil {
		return nil, err
	}
	return written, nil
}

// undoLog remembers how to put every touched path back. Restoring is best-effort by
// necessity — if the disk is failing, the rollback can fail too — but it turns the
// common failures (a bad destination, a read-only file, a full disk mid-copy) from
// "your project is now half-modified" into "nothing happened".
//
// It restores through the same confined root it wrote through: a rollback that followed a
// symlink out of the project would be its own escape.
type undoLog struct {
	pr      *projectRoot
	restore []func() error
}

// before records a path's current state BEFORE we touch it: its content and mode if it
// exists, or "delete it" if it does not. If we cannot snapshot an existing file, we say
// so — pretending we can restore something we never read is how a "rollback" quietly
// destroys the thing it was meant to protect.
func (u *undoLog) before(rel string) error {
	pr := u.pr
	info, err := pr.lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		u.restore = append(u.restore, func() error { return ignoreNotExist(pr.remove(rel)) })
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect %s before overwriting it: %w", rel, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		// The destination is a SYMLINK. Capture its target text and restore it as a symlink —
		// reading through it (readFile) and rewriting a regular file would turn a link into a
		// file on a rollback that reports FAILURE, permanently changing a project that shares
		// config via symlinks. Lstat above already did not follow it.
		target, lerr := pr.root.Readlink(rel)
		if lerr != nil {
			return fmt.Errorf("cannot read symlink %s before overwriting it (so a rollback could not restore it): %w", rel, lerr)
		}
		u.restore = append(u.restore, func() error { return pr.root.WriteSymlinkAtomic(rel, target) })
		return nil
	}
	prev, rerr := pr.readFile(rel)
	if rerr != nil {
		return fmt.Errorf("cannot snapshot %s before overwriting it (so a rollback could not restore it): %w", rel, rerr)
	}
	mode := info.Mode().Perm()
	u.restore = append(u.restore, func() error { return pr.writeFileAtomic(rel, prev, mode) })
	return nil
}

// rollback undoes the recorded writes, most recent first, and reports whether it fully
// succeeded. It keeps going after a failure — a partial restore beats abandoning the
// remaining files — but it does NOT swallow the errors.
func (u *undoLog) rollback() error {
	var errs []error
	for i := len(u.restore) - 1; i >= 0; i-- {
		if err := u.restore[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ignoreNotExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// destKind classifies a pack destination WITHOUT following a symlink. The pack only ever
// writes regular files (via the atomic writer), so anything that is not a regular file — a
// symlink, a directory, a device — is the user's own construct, never ours. Classifying a
// symlink by the bytes it POINTS AT (what a plain read does) is how the installer used to
// mistake a user's `worker.md -> shared/worker.md` link for its own file and then replace or
// delete the link. Lstat, not Stat: a dangling symlink still counts as present.
//
// Only a NOT-EXIST result means "safe to write". Any other stat error — a permission problem,
// an I/O fault — is returned: treating "I could not inspect it" as "it is not there" is how an
// installer overwrites a file it was never allowed to look at.
func destKind(pr *projectRoot, dest string) (exists, regular bool, err error) {
	info, lerr := pr.lstat(dest)
	if errors.Is(lerr, fs.ErrNotExist) {
		return false, false, nil
	}
	if lerr != nil {
		return false, false, lerr
	}
	return true, info.Mode().IsRegular(), nil
}

// preflightHostPack refuses to clobber a destination that exists and was NOT
// installed by VichuFlow (the user's file), unless force.
func preflightHostPack(pr *projectRoot, m *hostManifest, own packOwnership, prev *installedHost, force bool) (*installPlan, error) {
	plan := &installPlan{}
	for _, f := range m.Files {
		if err := plan.preflightDest(pr, f.Dest, own, prev, force); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// preflightDest decides what installing over ONE destination would do, recording the outcome
// on the plan. A missing path is free to write. A symlink or special file is the user's (the
// pack only ever writes regular files), so it may only be replaced under --force. A regular
// file is classified by content, read WITHOUT following a symlink that slipped in after the
// stat. Anything not ours, and not force-authorized, aborts the whole install.
func (p *installPlan) preflightDest(pr *projectRoot, destSlash string, own packOwnership, prev *installedHost, force bool) error {
	dest := filepath.FromSlash(destSlash)
	exists, regular, kerr := destKind(pr, dest)
	switch {
	case kerr != nil:
		return fmt.Errorf("cannot inspect %s to check whether it is yours: %w", destSlash, kerr)
	case !exists:
		return nil // doesn't exist — safe to write
	case !regular:
		return p.clobberOrReplace(destSlash, force) // a symlink/special the user made
	}
	cur, rerr := pr.readFileNoFollow(dest)
	if rerr != nil {
		return fmt.Errorf("cannot read %s to check whether it is yours: %w", destSlash, rerr)
	}
	// Ours? Then writing destroys nothing — it is either a no-op or an upgrade of a file we
	// shipped. Record HOW we recognized it, so the report can say what actually happens to it
	// instead of one blanket sentence for three different situations.
	if own.owns(destSlash, cur) {
		p.classify(destSlash, cur, own, prev)
		return nil
	}
	return p.clobberOrReplace(destSlash, force) // content we cannot reproduce
}

// clobberOrReplace handles a destination that is NOT ours: only the human (--force) can
// authorize destroying it; without that, the install aborts, changing nothing.
func (p *installPlan) clobberOrReplace(destSlash string, force bool) error {
	if force {
		p.replaced = append(p.replaced, destSlash)
		return nil
	}
	return fmt.Errorf(i18n.T("host.would_clobber"), destSlash)
}

// classify records HOW an existing destination was recognized, so the report can say what
// actually happens to it rather than one blanket sentence for all three cases.
func (p *installPlan) classify(dest string, cur []byte, own packOwnership, prev *installedHost) {
	// "Did anything on disk VOUCH for this file?" is the first question, not the last. A clone
	// carries the current pack and no record: content-wise nothing changes, but we are claiming
	// a file nothing vouched for, and from now on `uninstall` removes it. That is the thing the
	// user needs told — and checking content first hid it behind "already current".
	//
	// The record is consulted HERE ONLY, to choose words. It authorizes nothing: ownership is
	// proved against the binary (see packOwnership).
	// Byte-for-byte what we would write? Then a rewrite changes nothing on disk — record it so
	// copyHostPack skips it, whether or not the record vouches for it. An adopted (unvouched) file
	// that already matches must NOT be rewritten just to "adopt" it: adoption is a RECORD change,
	// not a reason to churn the inode/mtime and strip the user's xattrs/ACLs.
	isCurrent := bytes.Equal(cur, own.current[dest])
	if isCurrent {
		if p.identical == nil {
			p.identical = map[string]bool{}
		}
		p.identical[dest] = true
	}

	vouched := prev != nil && prev.Files[dest] == hashBytes(cur)
	switch {
	case !vouched:
		// A clone, or an install killed before its commit marker.
		p.adopted = append(p.adopted, dest)
	case isCurrent:
		p.alreadyCurrent = append(p.alreadyCurrent, dest) // nothing changes
	default:
		p.fromRelease = append(p.fromRelease, dest) // we installed it; this refresh upgrades it
	}
}

// copyHostPack writes the pack's files, recording each destination's prior state in the
// undo log first. The contents come from plan.files, which planHostInstall already read
// out of the embedded FS — so a pack missing a file fails during planning, before a
// single byte is written.
func copyHostPack(pr *projectRoot, plan *installPlan, undo *undoLog) ([]string, error) {
	// A file that is byte-for-byte what we would write changes NOTHING: rewriting it only churns
	// its inode/mtime, resets its mode to 0644 and drops its xattrs/ACLs. Skipping every such file
	// (plan.identical — vouched OR adopted) is what makes "unchanged"/"adopted" true instead of a
	// claim the CLI prints while quietly overwriting the file.
	var written []string
	for _, f := range plan.manifest.Files {
		if plan.identical[f.Dest] {
			continue
		}
		rel := filepath.FromSlash(f.Dest)
		if err := undo.before(rel); err != nil {
			return written, err
		}
		// Preserve the mode of an existing regular file (an upgrade or adopt must not reset a 0600
		// the user set); a brand-new file gets 0644 (writeFileAtomic treats mode 0 as 0644).
		if err := pr.writeFileAtomic(rel, plan.files[f.Dest], existingRegularMode(pr, rel)); err != nil {
			return written, fmt.Errorf("installing %s: %w", f.Dest, err)
		}
		written = append(written, f.Dest)
	}
	return written, nil
}

// existingRegularMode returns rel's permission bits if it is an existing regular file, else 0
// (which writeFileAtomic treats as 0644). It keeps an upgrade/adopt from resetting a mode the
// user deliberately set, the same way settings.json and .gitignore already preserve theirs.
func existingRegularMode(pr *projectRoot, rel string) fs.FileMode {
	if info, err := pr.lstat(rel); err == nil && info.Mode().IsRegular() {
		return info.Mode().Perm()
	}
	return 0
}

// record builds the install record: what we wrote, its hashes, and — crucially — only
// the permission rules we actually ADDED (see installedHost.AddedPermissions).
func (p *installPlan) record() *installedHost {
	ih := &installedHost{
		Host: p.manifest.Host, Mode: p.manifest.Mode,
		PackHash: p.packHash,
		Files:    map[string]string{},
	}
	for dest, content := range p.files {
		ih.Files[dest] = hashBytes(content)
	}
	return ih
}

// ownership builds the LOCAL ledger: what this install added to the user's settings, plus
// what an earlier install on THIS machine already added. Re-installing must not forget the
// first install's rules — they are already in settings (so this run adds nothing), and
// dropping them from the ledger would strand them there, un-withdrawable by uninstall.
func (p *installPlan) ownership() *hostOwnership {
	stale := map[string]bool{}
	for _, r := range p.staleRules {
		stale[r] = true // withdrawn by this install — we no longer own it
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range append(ourRules(prevOwned(p.owned)), p.addRules...) {
		if !seen[r] && !stale[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return &hostOwnership{Host: p.manifest.Host, AddedPermissions: out}
}

func prevOwned(prev *hostOwnership) []string {
	if prev == nil {
		return nil
	}
	return prev.AddedPermissions
}

// saveHostOwnership writes the local ledger atomically under the gitignored runtime dir.
func saveHostOwnership(pr *projectRoot, ow *hostOwnership, undo *undoLog) error {
	rel := relOwnership(ow.Host)
	if undo != nil {
		if err := undo.before(rel); err != nil {
			return err
		}
	}
	return pr.writeJSONAtomic(rel, ow, 0o644)
}

func manifestDests(m *hostManifest) []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.Dest)
	}
	return out
}

// packOwnership is what this binary knows about which bytes belong to a pack: the file it
// would write NOW, plus every version of that file any released VichuFlow has shipped.
//
// Both come from the BINARY, and that is the entire point. No record proves ownership. The
// portable one (`.claude/vichu-host.json`) is committed, so a cloned repo writes it. The local
// ledger sits under `.vichu/`, which is excluded from the mutation audit precisely so the
// kernel can write there — which means an agent with a write tool can too. Moving the
// authority from one to the other only changes WHO forges the proof: ownership metadata stored
// inside a workspace an agent can write to is not evidence. See plan §9.5.
type packOwnership struct {
	current map[string][]byte   // dest → the bytes this binary would install
	shipped map[string][]string // dest → sha256 of every version we have ever released
}

// owns reports whether the bytes at dest are OURS: byte-for-byte what we would write, or
// byte-for-byte a version we shipped before. Both are provable against the binary.
//
// The historical set is what makes an UPGRADE work. Without it, a v0.4.0 pack file — untouched,
// unmistakably ours — differs from what this binary ships, so it looked like a stranger's file:
// `doctor` told you to run `vichu init --host`, and that command then refused. Anything that
// matches NO version we ever shipped is content we cannot reproduce (you edited it, or it was
// never ours), and destroying it takes a human: --force.
func (p packOwnership) owns(dest string, cur []byte) bool {
	if bytes.Equal(cur, p.current[dest]) {
		return true
	}
	h := hashBytes(cur)
	for _, shipped := range p.shipped[dest] {
		if h == shipped {
			return true
		}
	}
	return false
}

// loadPackOwnership reads the pack this binary ships plus its released-versions catalog.
func loadPackOwnership(host string, m *hostManifest) (packOwnership, error) {
	current, err := packFiles(host, m)
	if err != nil {
		return packOwnership{}, err
	}
	shipped := map[string][]string{}
	// The catalog is optional: a pack with no releases yet simply has no history.
	if data, rerr := hostpacks.FS.ReadFile(path("packs", host, "known-hashes.json")); rerr == nil {
		if uerr := json.Unmarshal(data, &shipped); uerr != nil {
			return packOwnership{}, fmt.Errorf("host pack %q has an invalid known-hashes.json: %w", host, uerr)
		}
	}
	return packOwnership{current: current, shipped: shipped}, nil
}

// validateRecordPaths rejects an install record whose file paths could escape the
// project. The record lives at .claude/vichu-host.json — WITH the pack, so a team can
// commit it — which means on any repo you cloned it is untrusted input. And `uninstall`
// is a delete loop over exactly these paths.
//
// filepath.IsLocal is the check that matters: it rejects absolute paths, `..` anywhere,
// Windows volume names (`C:\`) and UNC paths, on every OS. Symlink escapes are closed
// separately, by doing the actual I/O through os.Root.
func validateRecordPaths(ih *installedHost) error {
	for _, dest := range sortedKeys(ih.Files) {
		if !filepath.IsLocal(filepath.FromSlash(dest)) {
			return fmt.Errorf("install record lists %q, which is not a path inside this project — refusing to act on it (the record is committed with the pack, so a hostile one could make VichuFlow delete files outside your repo)", dest)
		}
	}
	return nil
}

// loadInstalledHost reads the install record, preferring the portable location and
// falling back to the legacy one — so existing installs keep working and a committed
// pack carries its record into a clone.
//
// It distinguishes ABSENT (nil, nil — no pack here, which is fine) from CORRUPT (an
// error). Collapsing the two would let `doctor` report green on an unreadable record and
// let `init` treat an existing install as a fresh one.
func loadInstalledHost(pr *projectRoot) (*installedHost, error) {
	for _, p := range []string{relPortableRecord, relLegacyRecord} {
		// No-follow: the record is managed regular-file metadata, not a declared pack file. A
		// symlinked `.claude/vichu-host.json` is refused (not followed) rather than read through —
		// otherwise a link the user planted would be read here and then REPLACED at save time.
		data, err := pr.readFileNoFollow(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read the install record %s: %w", p, err)
		}
		var ih installedHost
		if uerr := json.Unmarshal(data, &ih); uerr != nil {
			return nil, fmt.Errorf("the install record %s is corrupt: %w — fix or delete it, then re-run `vichu init --host <host>`", p, uerr)
		}
		if verr := validateRecordPaths(&ih); verr != nil {
			return nil, verr
		}
		return &ih, nil
	}
	return nil, nil
}

// saveInstalledHost writes the record to the portable location (with the pack) and
// removes any legacy .vichu/host.json — a one-shot migration on the next install.
// It is written ATOMICALLY (temp + fsync + rename), because it is the install's commit
// point and its inventory: `uninstall` reads it to know WHICH files an install wrote (though it
// proves each is ours against the embedded pack, never the record's hashes — see packOwnership),
// and a truncated record would name the wrong set. `doctor` does NOT trust it: it verifies
// integrity against the embedded pack + known-hashes.json. The record is a diagnostic commit
// marker, not authority (§5.1/§9.5).
func saveInstalledHost(pr *projectRoot, ih *installedHost, undo *undoLog) error {
	// Refuse to replace a symlinked record with a regular file (writeJSONAtomic renames over it,
	// breaking a shared record). Re-checked here — after preflight — to also catch a link planted
	// in the meantime. The record is not a declared pack file, so `--force` does not authorize this.
	switch info, lerr := pr.lstat(relPortableRecord); {
	case lerr == nil && info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink — refusing to replace it with a regular file; edit its target or remove the link, then re-run", relPortableRecord)
	case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
		return lerr
	}
	if undo != nil {
		if err := undo.before(relPortableRecord); err != nil {
			return err
		}
	}
	if err := pr.writeJSONAtomic(relPortableRecord, ih, 0o644); err != nil {
		return err
	}
	// Migrate away from the gitignored spot. A legacy record we CANNOT remove (immutable, ACL,
	// permissions) must not be reported as a completed migration: propagate the error so the
	// install rolls back (undo reverts the portable write) and the legacy stays for a retry.
	if err := pr.remove(relLegacyRecord); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove the legacy install record %s: %w", relLegacyRecord, err)
	}
	return nil
}

// hostSettingsFile is the host's SHARED project settings (distinct from the
// machine-local settings.local.json, which is the host's own bookkeeping).
const hostSettingsFile = "settings.json"

// hostSettings is the user's .claude/settings.json, parsed and TYPE-CHECKED. It is
// the user's file: we add rules to permissions.allow and touch nothing else.
//
// The type check is the point. Reading `permissions` with a bare type assertion
// silently yields nil when the value is not an object — and writing our object back
// then DESTROYS whatever the user had there. A settings file we cannot safely edit is
// an error the user must see, never a file we quietly overwrite.
type hostSettings struct {
	root  map[string]any // every key, preserved verbatim
	allow []any          // permissions.allow (entries kept as-is, strings or not)
	mode  fs.FileMode    // the file's existing permissions (0 when it does not exist)
}

// loadHostSettings reads and validates the host's settings.json. A missing file is
// fine (empty settings). Invalid JSON, or a `permissions` / `permissions.allow` of an
// unexpected shape, is a hard error: we refuse to guess, and we refuse to clobber.
//
// Only a NOT-EXIST error means "no settings". Any other read error (permission denied,
// an I/O fault) is propagated: treating it as an empty file would make us write a fresh
// settings.json over one we simply failed to read.
func loadHostSettings(pr *projectRoot) (*hostSettings, error) {
	p := relHostSettings
	s := &hostSettings{root: map[string]any{}}
	// Lstat FIRST, and refuse a symlink. Saving the merged settings goes through the atomic
	// writer, which replaces the destination with a regular file — so a `.claude/settings.json`
	// that is a symlink to a shared config would be silently turned into a plain file, breaking
	// the sharing (and the merge would land on the new local file, not the shared target). This
	// aborts before any pack file is written, and is NOT lifted by --force (which authorizes
	// replacing pack files, not converting the user's symlink).
	info, lerr := pr.lstat(p)
	if errors.Is(lerr, fs.ErrNotExist) {
		return s, nil
	}
	if lerr != nil {
		return nil, fmt.Errorf("cannot inspect %s: %w", p, lerr)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink — refusing to merge permissions through it, because saving would replace the link with a regular file and break your shared config. Edit its target directly, or replace the link yourself, then re-run", p)
	}
	s.mode = info.Mode().Perm() // keep the user's mode; do not widen it
	data, err := pr.readFile(p)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", p, err)
	}
	if uerr := json.Unmarshal(data, &s.root); uerr != nil {
		return nil, fmt.Errorf("%s is not valid JSON — fix or remove it, then re-run: %w", p, uerr)
	}
	raw, ok := s.root["permissions"]
	if !ok || raw == nil {
		return s, nil
	}
	perms, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: \"permissions\" must be an object, found %T — refusing to overwrite it", p, raw)
	}
	rawAllow, ok := perms["allow"]
	if !ok || rawAllow == nil {
		return s, nil
	}
	allow, ok := rawAllow.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: \"permissions.allow\" must be a list, found %T — refusing to overwrite it", p, rawAllow)
	}
	s.allow = allow
	return s, nil
}

// missing returns the rules not already present in permissions.allow — exactly what
// an install would add, and therefore exactly what an uninstall may take back.
func (s *hostSettings) missing(rules []string) []string {
	have := map[string]bool{}
	for _, a := range s.allow {
		if str, ok := a.(string); ok {
			have[str] = true
		}
	}
	var out []string
	for _, r := range rules {
		if !have[r] {
			have[r] = true // a manifest listing the same rule twice adds it once
			out = append(out, r)
		}
	}
	return out
}

// addAndSave appends the given rules to permissions.allow and writes the file back,
// preserving every other key. A no-op when there is nothing to add (so re-installing
// is idempotent and does not rewrite the user's file).
func (s *hostSettings) addAndSave(pr *projectRoot, add, remove []string, undo *undoLog) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	// Remove and add in ONE write. Doing them as two writes would leave a window where the
	// old over-broad rule and the new narrow ones are both authorized.
	s.allow = withoutRules(s.allow, remove)
	for _, r := range add {
		s.allow = append(s.allow, r)
	}
	perms, _ := s.root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	perms["allow"] = s.allow
	s.root["permissions"] = perms
	if undo != nil {
		if err := undo.before(relHostSettings); err != nil {
			return err
		}
	}
	return pr.writeJSONAtomic(relHostSettings, s.root, s.mode)
}

// dropHostPermissions removes ONLY the rules this install added (installedHost.
// AddedPermissions), leaving the user's own rules — and the file itself, which is
// theirs — intact. It never removes a rule the user already had before we installed.
// It returns an error if the rules could NOT be withdrawn, so the caller can keep the
// install record: a record is what makes an uninstall possible at all, and deleting it
// after a partial uninstall would strand our rules in the user's settings with nothing
// left that knows they are ours.
func dropHostPermissions(pr *projectRoot, added []string) error {
	if len(added) == 0 {
		return nil
	}
	s, err := loadHostSettings(pr)
	if err != nil {
		return err
	}
	if len(s.allow) == 0 {
		return nil // nothing to drop
	}
	ours := map[string]bool{}
	for _, r := range added {
		ours[r] = true
	}
	kept := make([]any, 0, len(s.allow))
	for _, a := range s.allow {
		if str, ok := a.(string); ok && ours[str] {
			continue
		}
		kept = append(kept, a)
	}
	perms, ok := s.root["permissions"].(map[string]any)
	if !ok {
		return nil
	}
	perms["allow"] = kept
	s.root["permissions"] = perms
	return pr.writeJSONAtomic(relHostSettings, s.root, s.mode)
}

// writeJSONAtomic writes JSON via a temp file + rename, so a crash or a full disk can
// never leave the user's settings truncated — they either see the old file or the new
// one. It is their config; a half-written one could lock them out of their own host.
//
// mode is the file's EXISTING permissions (0 when it does not exist yet). We keep them:
// a settings.json the user deliberately chmod'ed to 0600 may hold host configuration
// they do not want other local accounts reading, and an installer that silently widens
// it to 0644 is a privacy regression nobody asked for. Only a brand-new file gets 0644.
func (p *projectRoot) writeJSONAtomic(rel string, v any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return p.writeFileAtomic(rel, append(data, '\n'), mode)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// embeddedPackHash fingerprints a host pack as it ships in THIS binary: a hash over each file's
// dest and content, in a stable (dest-sorted) order, PLUS the permission rules it declares. It
// is stored in the install record (installedHost.PackHash) as a DIAGNOSTIC fingerprint only.
//
// doctor does NOT compare it to decide anything — a committed record's hash is forgeable, so
// integrity/outdated are decided per-file against the embedded pack + known-hashes.json, and
// permissions against settings.json (see hostFilesCheck / hostPermissionsCheck). The permissions
// are folded into the fingerprint so a human reading two records can see that a release changed
// what the pack authorizes, but that judgement is doctor's job against the binary, not this hash.
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
	rules := append([]string(nil), m.Permissions...)
	sort.Strings(rules)
	for _, r := range rules {
		fmt.Fprintf(h, "permission\n%s\n", r)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// uninstallHostPack removes the named host pack: only the files VichuFlow installed
// (whose content still matches the recorded install hash) are deleted; user-modified
// files are kept. It refuses to act on an unknown host or one that does not match
// what is actually installed, so a typo or a future multi-host setup can never
// delete the wrong pack. Returns the removed paths and the kept (user-modified) ones.
func uninstallHostPack(root, host string, withdrawPermissions, force bool) (removed, kept []string, err error) {
	// Reject an unknown host before touching anything on disk. The manifest is also what
	// BOUNDS the deletion: see removeOwnedFiles.
	m, err := loadHostManifest(host)
	if err != nil {
		return nil, nil, err
	}
	// Same project-wide lock as install: uninstall edits the same settings.json, and an
	// install racing it would see a half-removed pack.
	unlock, lerr := lockHostPack(root)
	if lerr != nil {
		return nil, nil, lerr
	}
	defer unlock()

	pr, err := openProjectRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer pr.Close()

	ih, err := loadInstalledHost(pr)
	if err != nil {
		return nil, nil, err
	}
	if ih == nil {
		return nil, nil, errors.New(i18n.T("host.not_installed"))
	}
	// Never delete a different host's pack because of a typo or wrong --host.
	if ih.Host != host {
		return nil, nil, fmt.Errorf(i18n.T("host.mismatch"), ih.Host, host)
	}

	// Every path we are about to DELETE comes from the record — and the record is
	// designed to be COMMITTED (it travels with .claude/). So on a repo you cloned, it
	// is attacker-controlled input, and `vichu uninstall` is a delete loop driven by it.
	// Validate the whole set BEFORE removing anything: one bad path aborts the command
	// rather than deleting some files and then noticing.
	if err := validateRecordPaths(ih); err != nil {
		return nil, nil, err
	}
	// Ownership of the user's permission rules comes from the LOCAL ledger, never from the
	// committed record — see hostOwnership. In a fresh clone there is no ledger, so we
	// withdraw nothing and say so.
	owned, err := loadHostOwnership(pr, host)
	if err != nil {
		return nil, nil, err
	}

	removed, kept, err = removeOwnedFiles(pr, host, m, force)
	if err != nil {
		// A file we own could not be removed. KEEP everything: the records are the only
		// thing that still knows which files and rules are ours, and a half-uninstalled
		// project with no record is one doctor can neither see nor repair.
		return removed, kept, err
	}
	// Permissions are NOT withdrawn by default, and that is a deliberate asymmetry.
	//
	// The ledger that says which rules are ours lives inside the workspace, under `.vichu/` —
	// which is excluded from the mutation audit (it is the kernel's runtime, so a worker
	// writing there must never count as a mutation). An agent with a write tool can forge it.
	// The authored-rules catalog stops it claiming a rule we never shipped, but the user may
	// have written one of OURS themselves, and a forged ledger could still get it stripped.
	//
	// So the ledger's claims are a PROPOSAL, not an authorization. Leaving a permission behind
	// is recoverable in one edit; deleting one the user wrote is not. `--withdraw-permissions`
	// is the user saying yes. (The proper fix — an ownership store outside the workspace —
	// needs `hostBackend`; see plan §9.3.)
	claims := ourRules(prevOwned(owned))
	if !withdrawPermissions {
		reportUnwithdrawnRules(claims)
	} else if derr := dropHostPermissions(pr, claims); derr != nil {
		return removed, kept, fmt.Errorf("removed the pack files, but could not withdraw its permission rules: %w — the install records are kept, so re-run `vichu uninstall` once that is fixed", derr)
	}
	if derr := removeUninstallRecords(pr, host); derr != nil {
		// The files and rules are gone but a record survives: doctor would keep reporting
		// an install that no longer exists. Say so and exit non-zero — a retry is safe.
		return removed, kept, derr
	}
	if owned == nil && len(ih.Files) > 0 {
		fmt.Fprintf(os.Stderr, i18n.T("uninstall.no_ledger")+"\n", relHostSettings)
	}
	return removed, kept, nil
}

// removeUninstallRecords drops the ledger and both install records — the last step, and
// the one that says "this project no longer has a pack". A failure here must NOT be
// swallowed: leaving a record behind makes doctor report a pack whose files are gone.
func removeUninstallRecords(pr *projectRoot, host string) error {
	// STOP at the first failure. Accumulating errors and pressing on to the final delete was
	// the bug: the ownership ledger could survive while the portable record — the thing that
	// says "an uninstall is still owed here" — got deleted anyway. The retry then reported
	// "no host pack is installed", and the orphaned ledger went on claiming ownership of
	// permission rules the user had since re-added, so the NEXT uninstall deleted theirs.
	//
	// The portable record is the COMMIT MARKER: while it exists, uninstall work is still
	// owed. It goes last, and only if everything before it succeeded.
	for _, rel := range []string{
		relOwnership(host),
		relLegacyRecord,
		relPortableRecord, // the commit marker: last, and only on full success
	} {
		if err := ignoreNotExist(pr.remove(rel)); err != nil {
			return fmt.Errorf("cannot remove %s: %w — the pack files and permission rules were removed, but the install record is KEPT so `vichu uninstall` can finish the job; fix the permission problem and re-run it", rel, err)
		}
	}
	return nil
}

// removeOwnedFiles deletes the pack files whose content still matches what we recorded
// installing. A file the user edited is KEPT (it is theirs now). A file that is already
// gone is fine. Any OTHER error — a read-only directory, a permission problem — is
// returned: reporting a successful uninstall while a file is still installed leaves a
// residue that doctor can no longer recognize, because the record is about to be deleted.
func removeOwnedFiles(pr *projectRoot, host string, m *hostManifest, force bool) (removed, kept []string, err error) {
	own, oerr := loadPackOwnership(host, m)
	if oerr != nil {
		return nil, nil, oerr
	}
	// PLAN FIRST. Deleting some files, keeping others, then dropping the install record and
	// printing "Uninstalled" is exactly what this used to do — and a retry could no longer
	// even recognize the install. Classify every destination before touching one.
	var doomed, strangers []string
	for _, f := range m.Files {
		dest := filepath.FromSlash(f.Dest)
		exists, regular, kerr := destKind(pr, dest)
		switch {
		case kerr != nil:
			return nil, nil, fmt.Errorf("cannot inspect %s to check whether it is ours: %w", f.Dest, kerr)
		case !exists:
			continue // already gone
		case !regular:
			// A symlink or special file: the user's construct, which the pack never writes.
			// Deleting it would remove structure we did not create — treat it as a stranger, so
			// without --force uninstall aborts rather than silently unlinking it.
			strangers = append(strangers, f.Dest)
			continue
		}
		cur, rerr := pr.readFileNoFollow(dest)
		if rerr != nil {
			return nil, nil, fmt.Errorf("cannot read %s to check whether it is ours: %w", f.Dest, rerr)
		}
		if own.owns(f.Dest, cur) {
			doomed = append(doomed, f.Dest) // ours: this binary's pack, or one we shipped before
			continue
		}
		strangers = append(strangers, f.Dest) // yours: edited, or never ours
	}
	// A file we cannot prove is ours is not ours to destroy, and half-uninstalling is worse
	// than not starting: abort having changed NOTHING, and name the files.
	if len(strangers) > 0 && !force {
		sort.Strings(strangers)
		return nil, nil, fmt.Errorf(i18n.T("uninstall.strangers"), strings.Join(strangers, "\n  "))
	}
	if force {
		doomed = append(doomed, strangers...) // the human said yes
	}
	sort.Strings(doomed)
	for _, dest := range doomed {
		if derr := pr.remove(filepath.FromSlash(dest)); derr != nil && !errors.Is(derr, fs.ErrNotExist) {
			return removed, nil, fmt.Errorf("cannot remove %s: %w", dest, derr)
		}
		removed = append(removed, dest)
	}
	return removed, nil, nil
}

// packFiles reads the pack this binary ships: dest → content.
func packFiles(host string, m *hostManifest) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, f := range m.Files {
		content, err := hostpacks.FS.ReadFile(path("packs", host, f.Src))
		if err != nil {
			return nil, fmt.Errorf("host pack %q is missing %q: %w", host, f.Src, err)
		}
		files[f.Dest] = content
	}
	return files, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// installHostAndReport installs a host pack and prints what it did (or, with
// --dry-run, what it WOULD do), then warns about any missing required binary.
func installHostAndReport(root, host string, force, dryRun bool) error {
	plan, written, err := installHostPack(root, host, force, dryRun)
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
	reportPermissions(plan, dryRun)
	warnMissingRequires(host)
	if !dryRun {
		fmt.Println("\n" + i18n.T("host.next"))
	}
	return nil
}

// reportPermissions tells the user exactly which kernel commands the pack authorized
// its host to run without prompting — and, on --dry-run, says "would" and means it.
// A dry run that claims "Pre-authorized in .claude/settings.json" while writing
// nothing is a tool lying about what it did; the whole product is a bet that this
// one does not.
func reportPermissions(plan *installPlan, dryRun bool) {
	if plan == nil || len(plan.manifest.Permissions) == 0 {
		return
	}
	settingsPath := path(hostRecordDir, hostSettingsFile)
	// Four categories, and they are NOT interchangeable — which one a rule falls into is
	// what tells the user whether `vichu uninstall` will clean it up or leave it.
	added := i18n.T("host.preauthorized")
	if dryRun {
		added = i18n.T("host.would_preauthorize")
	}
	printRules(added, settingsPath, plan.addRules)
	printRules(i18n.T("host.managed_by_vichu"), settingsPath, plan.ledgerClaimedOurs)
	printRules(i18n.T("host.already_yours"), settingsPath, plan.ledgerClaimedUsers)

	// An upgrade that narrows the pack's rules must SAY what it took away. Silently
	// withdrawing a permission the user has been relying on is its own kind of lie.
	withdrawn := i18n.T("host.withdrew")
	if dryRun {
		withdrawn = i18n.T("host.would_withdraw")
	}
	printRules(withdrawn, settingsPath, plan.staleRules)

	reportFiles(plan, dryRun)
}

// reportFiles says what actually happens to each destination that already existed. One
// blanket sentence for all of them told three separate lies: it claimed there was no install
// record when there was, it said "now manages" during a --dry-run that wrote nothing, and it
// lumped an untouched refresh in with a genuine new claim.
func reportFiles(plan *installPlan, dryRun bool) {
	printFiles(i18n.T("host.files_current"), plan.alreadyCurrent)
	if dryRun {
		printFiles(i18n.T("host.files_would_upgrade"), plan.fromRelease)
		printFiles(i18n.T("host.files_would_adopt"), plan.adopted)
		// The destructive one goes LAST, so it is the thing still on screen. A --dry-run exists
		// so someone can weigh the risk of --force before running it; hiding the one file it
		// would destroy is the only part of the preview that could actually hurt them.
		printFiles(i18n.T("host.files_would_replace"), plan.replaced)
		return
	}
	printFiles(i18n.T("host.files_upgraded"), plan.fromRelease)
	printFiles(i18n.T("host.files_adopted"), plan.adopted)
	printFiles(i18n.T("host.files_replaced"), plan.replaced)
}

func printFiles(heading string, files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Println("\n" + heading)
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
}

func printRules(heading, settingsPath string, rules []string) {
	if len(rules) == 0 {
		return
	}
	fmt.Printf("\n"+heading+"\n", settingsPath)
	for _, r := range rules {
		fmt.Printf("  %s\n", r)
	}
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

// lockHostPack takes the project-wide host-pack lock, reusing the runtime's lock (atomic
// hard-link acquisition, heartbeat, orphan reclaim from a dead process) rather than
// inventing a second, weaker one.
func lockHostPack(root string) (func(), error) {
	store := runtime.Open(root)
	h, err := store.AcquireLock(runtime.HostPackScope)
	if err != nil {
		if errors.Is(err, runtime.ErrLocked) {
			return nil, errors.New(i18n.T("host.locked"))
		}
		return nil, err
	}
	// Keep the heartbeat alive for as long as we hold it. This lock is reclaimed when its
	// heartbeat goes stale — so a long install (a slow filesystem, antivirus scanning every
	// written file, a big pack) that does not renew it would be declared orphaned and handed
	// to a second process while the first is still writing.
	ctx, stop := context.WithCancel(context.Background())
	go h.StartHeartbeat(ctx, nil)
	return func() {
		stop()
		_ = h.Release()
	}, nil
}

// withoutRules drops the named rules from an allow-list, preserving every other entry
// (including non-string ones the user may have put there) exactly as it found them.
func withoutRules(allow []any, remove []string) []any {
	if len(remove) == 0 {
		return allow
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	kept := make([]any, 0, len(allow))
	for _, a := range allow {
		if str, ok := a.(string); ok && drop[str] {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// reportUnwithdrawnRules tells the user which permission rules VichuFlow believes it added,
// and how to remove them. It does not remove them: see uninstallHostPack.
func reportUnwithdrawnRules(claims []string) {
	if len(claims) == 0 {
		return
	}
	fmt.Printf("\n"+i18n.T("uninstall.rules_kept")+"\n", relHostSettings)
	for _, r := range claims {
		fmt.Printf("  %s\n", r)
	}
}
