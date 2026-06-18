package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

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
	ih := loadInstalledHost(root)
	if ih == nil {
		return // no host pack installed — nothing to check
	}
	altered, total := 0, len(ih.Files)
	for dest, want := range ih.Files {
		if cur, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dest))); err != nil || hashBytes(cur) != want {
			altered++ // missing or edited since install
		}
	}
	label := "host: " + ih.Host
	switch {
	case altered > 0:
		// A user-edited or missing file: re-install to restore it.
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_drift"), altered, total))
	case d.hostPackOutdated(ih):
		// Files are intact but the binary's embedded pack has moved on: refresh.
		d.check(label, false, fmt.Sprintf(i18n.T("doctor.host_outdated"), total))
	default:
		d.check(label, true, fmt.Sprintf(i18n.T("doctor.host_ok"), total, ih.Mode))
	}
	d.hostRequiresCheck(ih.Host)
	d.hostAdapterCheck(ih.Host)
}

// hostAdapterCheck probes the agent the installed host pack actually drives — the
// adapter the manifest declares in `verify.adapter` (no host==adapter convention).
// The binary being on PATH is necessary but NOT sufficient: a wrong CLI version or a
// missing login makes the pack unusable, so a failed probe is a HARD doctor failure,
// not just the informational adapter listing. A pack that declares no adapter
// (bridge/fallback) has nothing to probe.
func (d *doctorReport) hostAdapterCheck(host string) {
	m, err := loadHostManifest(host)
	if err != nil || m.Verify.Adapter == "" {
		return // the pack declares no agent to verify
	}
	a, err := adapters.DefaultRegistry().Get(m.Verify.Adapter)
	if err != nil {
		return // declared adapter not registered in this build — nothing to probe
	}
	av, _ := a.Probe(context.Background())
	label := "host agent: " + m.Verify.Adapter
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

// hostPackOutdated reports whether the installed pack predates the binary's current
// embedded pack — intact files that no longer match what this binary would install.
func (d *doctorReport) hostPackOutdated(ih *installedHost) bool {
	if ih.PackHash == "" {
		return false // pre-pack_hash install: can't tell, don't cry wolf
	}
	m, err := loadHostManifest(ih.Host)
	if err != nil {
		return false
	}
	return embeddedPackHash(ih.Host, m) != ih.PackHash
}

// hostRequiresCheck verifies each binary the host pack requires is on PATH.
func (d *doctorReport) hostRequiresCheck(host string) {
	m, err := loadHostManifest(host)
	if err != nil {
		return
	}
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
