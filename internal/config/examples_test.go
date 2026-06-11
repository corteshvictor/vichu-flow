package config

import (
	"path/filepath"
	"testing"
)

// TestExamplesAreValid keeps the examples/ starter templates from rotting: each
// must auto-detect as the stack its vichu.yaml declares, and its vichu.yaml must
// parse. This also proves the product is stack-agnostic — Go is just the build
// language.
func TestExamplesAreValid(t *testing.T) {
	cases := []struct {
		dir      string
		language string
		testCmd  string
	}{
		{"python", "python", "python3 -B -m unittest"},
		{"node", "javascript", "node --test"},
		{"go", "go", "go test ./..."},
		{"rust", "rust", "cargo test"},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			root := filepath.Join("..", "..", "examples", c.dir)

			if got := Detect(root).Language; got != c.language {
				t.Errorf("Detect(%s) = %q, want %q", c.dir, got, c.language)
			}

			cfg, err := Load(filepath.Join(root, FileName))
			if err != nil {
				t.Fatalf("loading %s/vichu.yaml: %v", c.dir, err)
			}
			if cfg.Project.Language != c.language {
				t.Errorf("%s vichu.yaml language = %q, want %q", c.dir, cfg.Project.Language, c.language)
			}
			if got := cfg.CommandFor("test"); got != c.testCmd {
				t.Errorf("%s test gate = %q, want %q", c.dir, got, c.testCmd)
			}
		})
	}
}
