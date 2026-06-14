package main

import (
	"fmt"
	"os"
	"path/filepath"
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
func writeTemplate(root string, tpl templates.Template, projectName string, force bool) ([]string, error) {
	files := tpl.Files(projectName)
	if !force {
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f.Path))); err == nil {
				return nil, fmt.Errorf(i18n.T("templates.file_exists"), f.Path)
			}
		}
	}
	var written []string
	for _, f := range files {
		full := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return written, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(f.Path, ".sh") {
			mode = 0o755 // shell gates should be directly runnable too
		}
		if err := os.WriteFile(full, []byte(f.Content), mode); err != nil {
			return written, err
		}
		written = append(written, f.Path)
	}
	return written, nil
}
