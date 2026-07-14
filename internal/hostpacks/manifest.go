package hostpacks

import (
	"fmt"
	"path/filepath"
)

// ValidateDests checks the destinations a host pack's manifest declares.
//
// Every consumer of a manifest turns these strings into paths it WRITES to, and there is now
// more than one: the installer, and the release-history tool. A check that lives inside one of
// them is a check the next consumer forgets — which is exactly what happened: `packhistory`
// took `dest` straight to filepath.Join, so a manifest declaring `../../../escaped` wrote
// outside the fixtures directory and exited 0. The manifest is our own file, so this is a
// typo-class bug rather than an attack — but a typo that silently overwrites a repo file is
// not one we get to shrug at.
//
// This is the LEXICAL half. filepath.IsLocal rejects absolute paths, `..` anywhere, Windows
// volume names and UNC paths, on every OS. It does NOT stop a symlink escape: callers must
// also do their I/O through os.Root.
func ValidateDests(dests []string) error {
	seen := map[string]bool{}
	for _, d := range dests {
		if d == "" {
			return fmt.Errorf("host pack manifest declares an empty destination")
		}
		if !filepath.IsLocal(filepath.FromSlash(d)) {
			return fmt.Errorf("host pack manifest declares destination %q, which is not a path inside the project", d)
		}
		if seen[d] {
			return fmt.Errorf("host pack manifest declares destination %q twice — which file would win?", d)
		}
		seen[d] = true
	}
	return nil
}
