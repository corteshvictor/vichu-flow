package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/config"
)

func TestNamesAndGet(t *testing.T) {
	want := map[string]bool{"empty": true, "go": true, "node": true, "python": true, "rust": true}
	for _, n := range Names() {
		if !want[n] {
			t.Errorf("unexpected template %q", n)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing templates: %v", want)
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("Get should fail for an unknown template")
	}
}

// TestEveryTemplateYieldsRunnableConfig: each template must produce a vichu.yaml
// that parses and configures a REAL test gate (not "auto") — that is what lets a
// scaffolded project reach `completed` from scratch.
func TestEveryTemplateYieldsRunnableConfig(t *testing.T) {
	for _, name := range Names() {
		tpl, _ := Get(name)
		if tpl.Detected.TestCmd == "" {
			t.Errorf("%s: template must configure a real test gate", name)
		}
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, config.FileName)
		if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML(tpl.Detected, "demo")), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("%s: generated vichu.yaml does not parse: %v", name, err)
		}
		if cfg.CommandFor("test") == "" {
			t.Errorf("%s: test gate resolves to empty/auto", name)
		}
		if !cfg.Workflow.GatesRequired() {
			t.Errorf("%s: requireGates should default true", name)
		}
	}
}

// TestTemplateFilesAreSafe: seeded paths must be clean relative paths (no
// absolute, no parent traversal) so a scaffold can never write outside the root.
func TestTemplateFilesAreSafe(t *testing.T) {
	for _, name := range Names() {
		tpl, _ := Get(name)
		files := tpl.Files("Demo Project")
		if len(files) == 0 {
			t.Errorf("%s: template seeds no files", name)
		}
		for _, f := range files {
			if filepath.IsAbs(f.Path) || strings.Contains(f.Path, "..") {
				t.Errorf("%s: unsafe seeded path %q", name, f.Path)
			}
			if f.Content == "" {
				t.Errorf("%s: empty content for %q", name, f.Path)
			}
		}
	}
}

// TestEmptyTemplateGateIsCrossPlatform: the empty template must complete on
// Windows too — its gate is per-OS and it seeds both shell scripts.
func TestEmptyTemplateGateIsCrossPlatform(t *testing.T) {
	tpl, _ := Get("empty")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(cfgPath, []byte(config.DefaultYAML(tpl.Detected, "demo")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	gate := cfg.Commands["test"]
	if gate.Unix == "" || gate.Windows == "" {
		t.Fatalf("empty gate must be set for both OSes, got %+v", gate)
	}
	if gate.Unix == gate.Windows {
		t.Fatalf("empty gate should differ per OS, got %q", gate.Unix)
	}
	paths := map[string]bool{}
	for _, f := range tpl.Files("demo") {
		paths[f.Path] = true
	}
	if !paths["tests.sh"] || !paths["tests.cmd"] {
		t.Fatalf("empty must seed tests.sh and tests.cmd, got %v", paths)
	}
}

func TestSlugSanitizesIdentifiers(t *testing.T) {
	cases := map[string]string{
		"My App":     "my-app",
		"  ":         "app",
		"svc_v2":     "svc_v2",
		"Foo/Bar":    "foo-bar",
		"trailing--": "trailing",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	// The slug must appear in identifier files (e.g. go.mod module line).
	goTpl, _ := Get("go")
	for _, f := range goTpl.Files("My App") {
		if f.Path == "go.mod" && !strings.Contains(f.Content, "module my-app") {
			t.Fatalf("go.mod should use the slugified name, got %q", f.Content)
		}
	}
}
