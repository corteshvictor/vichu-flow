package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// hostLabelPrefix prefixes doctor check labels that name a host pack (e.g. "host: claude-code").
const hostLabelPrefix = "host: "

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Printf(i18n.T("doctor.header")+"\n\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

	d := &doctorReport{ok: true}
	d.gitCheck()
	d.projectChecks()
	printAdapters()

	fmt.Println()
	if d.ok {
		fmt.Println(i18n.T("doctor.all_ok"))
		return nil
	}
	// A failed check must exit non-zero so `vichu doctor` is usable as a gate in
	// CI / host pack setup — not just a printed report.
	fmt.Println(i18n.T("doctor.failures"))
	return errors.New(i18n.T("doctor.failed"))
}

// doctorReport tracks whether all required checks passed and renders each line.
type doctorReport struct{ ok bool }

// check renders a pass/fail line and records a failure on the report.
func (d *doctorReport) check(label string, pass bool, detail string) {
	mark := "✓"
	if !pass {
		mark = "✗"
		d.ok = false
	}
	fmt.Printf("  %s %-22s %s\n", mark, label, detail)
}

// warn renders an advisory (e.g. an unbounded budget) without failing doctor.
func (d *doctorReport) warn(label, detail string) {
	fmt.Printf("  ! %-22s %s\n", label, detail)
}

// gitCheck reports git as a recommendation, not a requirement: the filesystem
// provider gives the same undo guarantees without a VCS, so a missing git is an
// advisory, not a failure.
func (d *doctorReport) gitCheck() {
	if workspace.GitAvailable() {
		d.check("git", true, gitDetail(true))
		return
	}
	d.warn("git", i18n.T("doctor.git_missing"))
}

// projectChecks validates the project config and resolves its workspace provider.
func (d *doctorReport) projectChecks() {
	root, err := findRoot()
	if err != nil {
		d.check(config.FileName, false, i18n.T("doctor.no_config"))
		return
	}
	// A vichu.yaml that exists but does not parse is a real failure — doctor must
	// not report green on a broken config.
	cfg, cfgErr := config.Load(filepath.Join(root, config.FileName))
	if cfgErr != nil {
		d.check(config.FileName, false, cfgErr.Error())
		return
	}
	d.check(config.FileName, true, filepath.Join(root, config.FileName))

	mode := config.WorkspaceAuto
	if cfg.Workspace.Provider != "" {
		mode = cfg.Workspace.Provider
	}
	if prov, err := workspace.Open(root, mode); err != nil {
		d.check("workspace", false, err.Error())
	} else {
		d.check("workspace", true, fmt.Sprintf("%s (%s)", prov.Kind(), prov.Root()))
	}
	// Nudge older configs (pre-v0.2.1) whose token budget is still unlimited.
	if cfg.Budgets.Run.MaxTotalTokens == 0 {
		d.warn("token budget", i18n.T("doctor.tokens_unlimited"))
	}
	d.hostPackCheck(root)
}

// hostPackCheck validates an installed host pack: every file present and intact
// (matches what was installed), the install not OUTDATED relative to the binary's
// embedded pack, the host's required binary available, AND the agent it drives
// actually usable (right version + authenticated) — the pack is only as good as the
// agent behind it.
func (d *doctorReport) hostPackCheck(root string) {
	pr, err := openProjectRoot(root)
	if err != nil {
		d.check("host pack", false, err.Error())
		return
	}
	defer pr.Close()

	ih, err := loadInstalledHost(pr)
	if err != nil {
		// A corrupt or hostile record is a FAILURE, not "no pack installed". Doctor is
		// the setup gate; reporting green on a record it could not read is the one thing
		// it must never do — `uninstall` and `init` both trust that file completely.
		d.check("host pack", false, err.Error())
		return
	}
	if ih == nil {
		// No record — but the record is DIAGNOSTIC, not authority (§9.5). Deleting it must not
		// blind doctor to pack files that are still on disk (a hostile clone could ship an altered
		// SKILL.md and no record). Discover the pack from the embedded manifests and verify it
		// against the binary anyway; the missing record is a recoverable diagnostic.
		d.detectUnrecordedPack(pr)
		return
	}
	// Resolve the manifest ONCE, here, and FAIL if it is unknown. A record can name a host
	// this binary does not ship (a corrupt record, a pack from a newer VichuFlow, a hostile
	// repo). Every downstream check used to load the manifest itself and silently return on
	// failure — so `host: not-real` printed a green tick and doctor said "all checks passed".
	// Doctor is the setup gate; a host it cannot even identify is not a pass.
	m, err := loadHostManifest(ih.Host)
	if err != nil {
		d.check(hostLabelPrefix+ih.Host, false, fmt.Sprintf(i18n.T("doctor.host_unknown"), ih.Host))
		return
	}
	d.hostFilesCheck(pr, ih, m)
	d.hostPermissionsCheck(pr, m)
	d.hostRequiresCheck(m)
	d.hostAdapterCheck(m)
}

// hostPermissionsCheck verifies the host's tool-permission rules actually match what the
// pack requires: every rule the manifest declares is present in the host's settings, and no
// rule VichuFlow has since RETIRED (a wildcard like `Bash(vichu *)` that once let any subagent
// unblock its own run) is still authorized. Without this, `doctor` printed a green tick for a
// pack whose permissions were emptied (the host prompts on every call — unusable) or still
// carried a withdrawn, over-broad grant (a live security regression). Read-only: it reuses the
// same settings loader the installer does (`loadHostSettings` refuses a symlink and parses
// strictly), and never writes.
//
// This is the one integrity check the plan (§9.2) sequences into `hostBackend` for v0.5, and
// it will move there when a second host lands — but it is built ENTIRELY on the existing,
// hardened `loadHostSettings`/`missing`/`vichuAuthoredRules`, so it reuses that logic rather
// than duplicating it (which is the thing §9.2 forbids doing before the abstraction).
func (d *doctorReport) hostPermissionsCheck(pr *projectRoot, m *hostManifest) {
	label := "host perms: " + m.Host
	if len(m.Permissions) == 0 {
		return // a pack that pre-authorizes nothing has nothing to verify
	}
	s, err := loadHostSettings(pr)
	if err != nil {
		// A symlinked or corrupt settings.json is a real failure: the pack's rules cannot be
		// confirmed, and the host will prompt on every kernel call.
		d.check(label, false, err.Error())
		return
	}
	if missing := s.missing(m.Permissions); len(missing) > 0 {
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_perms_missing"), len(missing), len(m.Permissions), m.Host))
		return
	}
	// A rule we AUTHORED but no longer declare is one we retired — and every retired rule was
	// pulled precisely because it was unsafe. If it is still authorized, the pack is a security
	// regression, not merely stale.
	declared := make(map[string]bool, len(m.Permissions))
	for _, r := range m.Permissions {
		declared[r] = true
	}
	for _, a := range s.allow {
		str, ok := a.(string)
		if ok && vichuAuthoredRules[str] && !declared[str] {
			d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_perms_retired"), str, m.Host))
			return
		}
	}
	d.check(label, true, fmt.Sprintf(i18n.T("doctor.host_perms_ok"), len(m.Permissions)))
}

// detectUnrecordedPack runs when there is NO install record. It discovers a host from the EMBEDDED
// manifests: if any host's declared files are present in the project, it verifies that host's
// integrity, permissions, requires and adapter against the binary — exactly as it would with a
// record — and reports the missing record as a diagnostic. Nothing is adopted, deleted or written;
// doctor stays read-only. A synthesized installedHost carries only the host name and mode (for
// labels); ownership is proved against the embedded pack, never a record.
func (d *doctorReport) detectUnrecordedPack(pr *projectRoot) {
	for _, host := range availableHosts() {
		m, err := loadHostManifest(host)
		if err != nil {
			continue
		}
		present, perr := packFootprintPresent(pr, m)
		if perr != nil {
			// A non-ErrNotExist error (permissions, an unreadable dir, a symlinked settings.json)
			// is NOT "not installed" — doctor must fail rather than report green on what it could
			// not inspect.
			d.check(hostLabelPrefix+host, false, fmt.Sprintf("cannot inspect the host pack: %v", perr))
			return
		}
		if !present {
			continue
		}
		d.check("host record", false, fmt.Sprintf("%s: .claude/vichu-host.json is missing, but pack files/permissions are present — verifying against the binary (re-run `vichu init --host %s` to restore the record)", host, host))
		ih := &installedHost{Host: m.Host, Mode: m.Mode}
		d.hostFilesCheck(pr, ih, m)
		d.hostPermissionsCheck(pr, m)
		d.hostRequiresCheck(m)
		d.hostAdapterCheck(m)
		return // a project drives one host
	}
}

// packFootprintPresent reports whether a host has left a FOOTPRINT worth verifying: any declared
// file present, OR any Vichu permission rule — current OR retired — in the host's settings. The
// permission half matters because a project can be left with just a retired, insecure rule (e.g.
// `Bash(vichu *)`) and no files; ignoring it would let doctor pass an orphaned, dangerous grant.
// Only fs.ErrNotExist means absence — a permission fault or an unreadable dir/settings is returned
// as an error so doctor fails, never silently treats it as "not installed".
func packFootprintPresent(pr *projectRoot, m *hostManifest) (bool, error) {
	for _, f := range m.Files {
		if _, err := pr.lstat(filepath.FromSlash(f.Dest)); err == nil {
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	s, err := loadHostSettings(pr)
	if err != nil {
		return false, err
	}
	for _, a := range s.allow {
		if str, ok := a.(string); ok && vichuAuthoredRules[str] {
			return true, nil
		}
	}
	return false, nil
}

// hostFilesCheck reports the integrity of the installed files: intact, altered, or simply
// older than the pack this binary now ships.
//
// Integrity is decided against the EMBEDDED pack and its released-versions catalog — NOT the
// portable record's hashes. The record (`.claude/vichu-host.json`) is committed, so a cloned
// repo carries whatever it wants: a tampered `vichu-implementer.md` PLUS a matching forged
// hash used to pass doctor green while an agent loaded the altered instructions. Ownership is
// only ever proved against the binary (see packOwnership). The record stays diagnostic.
func (d *doctorReport) hostFilesCheck(pr *projectRoot, ih *installedHost, m *hostManifest) {
	label := hostLabelPrefix + ih.Host
	own, err := loadPackOwnership(ih.Host, m)
	if err != nil {
		d.check(label, false, fmt.Sprintf("host: %s   cannot verify the pack: %v", ih.Host, err))
		return
	}
	altered, outdated, total := 0, 0, 0
	for _, f := range m.Files {
		total++
		// ReadFileNoFollow: a symlinked destination is refused, not followed — otherwise a
		// `.claude/agents/x.md -> current-pack-copy` link would let a tampered file masquerade
		// as intact.
		cur, rerr := pr.readFileNoFollow(filepath.FromSlash(f.Dest))
		switch {
		case rerr != nil:
			altered++ // missing, unreadable, or a symlink we refuse to follow
		case bytes.Equal(cur, own.current[f.Dest]):
			// byte-for-byte what this binary ships — intact
		case own.owns(f.Dest, cur):
			outdated++ // byte-for-byte a version VichuFlow released before — just stale
		default:
			altered++ // matches nothing we ever shipped: edited, or forged
		}
	}
	switch {
	case altered > 0:
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_drift"), altered, total, ih.Host))
	case outdated > 0:
		// Every file is genuinely ours but at least one is an OLDER released version: refresh.
		// Upgrading the binary does NOT touch the pack files already copied in — this is the
		// only check that tells the user their skill and subagents are stale. Not --force: the
		// files are unmodified, so a plain re-install overwrites them cleanly.
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_outdated"), total, ih.Host))
	default:
		d.check(label, true, fmt.Sprintf(i18n.T("doctor.host_ok"), total, ih.Mode))
	}
}

// hostAdapterCheck probes the agent the installed host pack actually drives — the
// adapter the manifest declares in `verify.adapter` (no host==adapter convention).
// The binary being on PATH is necessary but NOT sufficient: a wrong CLI version or a
// missing login makes the pack unusable, so a failed probe is a HARD doctor failure,
// not just the informational adapter listing. A pack that declares no adapter
// (bridge/fallback) has nothing to probe.
func (d *doctorReport) hostAdapterCheck(m *hostManifest) {
	if m.Verify.Adapter == "" {
		return // the pack declares no agent to verify (bridge/fallback host)
	}
	label := "host agent: " + m.Verify.Adapter
	a, err := adapters.DefaultRegistry().Get(m.Verify.Adapter)
	if err != nil {
		// The pack drives an adapter this build does not have. It CANNOT work here, and
		// staying quiet about it was how doctor passed a pack that could never run.
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_adapter_unknown"), m.Verify.Adapter))
		return
	}
	av, _ := a.Probe(context.Background())
	if !av.Available {
		d.check(label, false, av.Reason)
		return
	}
	detail := av.Version
	if detail == "" {
		detail = i18n.T("adapters.available")
	}
	d.check(label, true, detail)
}

// hostRequiresCheck verifies each binary the host pack requires is on PATH.
func (d *doctorReport) hostRequiresCheck(m *hostManifest) {
	for _, req := range m.Requires {
		// A required bin missing is a real failure: the installed pack can't run.
		if p, _ := exec.LookPath(req.Bin); p != "" {
			d.check("requires: "+req.Bin, true, p)
		} else {
			d.check("requires: "+req.Bin, false, fmt.Sprintf(i18n.T("doctor.host_missing_bin"), req.Bin))
		}
	}
}

// printAdapters probes each registered adapter and reports its availability.
func printAdapters() {
	fmt.Println("\n  " + i18n.T("doctor.adapters"))
	reg := adapters.DefaultRegistry()
	for _, name := range sortedNames(reg) {
		a, err := reg.Get(name)
		if err != nil {
			fmt.Printf("    ✗ %-12s %s\n", name, err.Error())
			continue
		}
		av, _ := a.Probe(context.Background())
		mark, detail := "✓", av.Version
		if !av.Available {
			mark, detail = "—", av.Reason
		}
		fmt.Printf("    %s %-12s %s\n", mark, name, detail)
	}
}

func gitDetail(ok bool) string {
	if ok {
		return i18n.T("doctor.git_ok")
	}
	return i18n.T("doctor.git_missing")
}

func sortedNames(reg *adapters.Registry) []string {
	names := reg.Names()
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}
