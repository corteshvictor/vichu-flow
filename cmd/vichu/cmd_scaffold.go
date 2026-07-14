package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/templates"
)

// resolveTemplate looks up a template by name, returning an actionable error
// that lists the valid names when it is unknown.
func resolveTemplate(name string) (templates.Template, error) {
	tpl, ok := templates.Get(name)
	if !ok {
		return templates.Template{}, fmt.Errorf(i18n.T("templates.unknown"), name, strings.Join(templates.Names(), ", "))
	}
	return tpl, nil
}

// writeTemplate seeds a template's files into root, returning the relative paths
// written. It refuses to overwrite an existing file unless force is set, and
// preflights ALL targets for conflicts before writing any of them — a failed
// scaffold must never leave a half-seeded project behind.
// preflightTemplate refuses to overwrite an existing destination without --force, EXCEPT a
// regular file already holding exactly this content — a prior interrupted init wrote it, so a
// retry treats it as already applied. Lstat, not Stat: a dangling symlink (Stat would say
// "not there") still counts as present, or the write would follow it outside the project.
func preflightTemplate(pr *projectRoot, files []templates.File) error {
	for _, f := range files {
		fi, err := pr.lstat(f.Path)
		if err != nil {
			continue // not there — will be written
		}
		if fi.Mode().IsRegular() {
			if cur, rerr := pr.readFile(f.Path); rerr == nil && string(cur) == f.Content {
				continue // identical — already applied by an earlier interrupted init
			}
		}
		return fmt.Errorf(i18n.T("templates.file_exists"), f.Path)
	}
	return nil
}

func writeTemplate(root string, tpl templates.Template, projectName string, force bool) ([]string, error) {
	pr, err := openProjectRoot(root)
	if err != nil {
		return nil, err
	}
	defer pr.Close()

	files := tpl.Files(projectName)
	if !force {
		if err := preflightTemplate(pr, files); err != nil {
			return nil, err
		}
	}
	var written []string
	for _, f := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(f.Path, ".sh") {
			mode = 0o755 // shell gates should be directly runnable too
		}
		// Confined + atomic: replaces a symlink rather than writing through it to an external
		// target, even under --force.
		if err := pr.writeFileAtomic(f.Path, []byte(f.Content), mode); err != nil {
			return written, err
		}
		written = append(written, f.Path)
	}
	return written, nil
}
