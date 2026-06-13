package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectProposesOnlyAvailableGates: Detect must never invent a gate the
// project can't run (a `npm run lint` with no `lint` script, pytest/ruff/mypy
// that aren't installed, cargo clippy that isn't a default component) — under
// requireGates an invented gate would block a perfectly valid project. An empty
// expectation asserts the gate is left unset (auto).
func TestDetectProposesOnlyAvailableGates(t *testing.T) {
	cases := []struct {
		name                            string
		file, content                   string
		language, test, lint, typecheck string
	}{
		{
			name: "node only configures scripts that exist", file: "package.json",
			content:  `{"name":"x","scripts":{"test":"jest"}}`,
			language: "javascript", test: "npm test",
		},
		{
			name: "node configures lint/typecheck when their scripts exist", file: "package.json",
			content:  `{"scripts":{"test":"x","lint":"eslint .","typecheck":"tsc --noEmit"}}`,
			language: "javascript", test: "npm test", lint: "npm run lint", typecheck: "npm run typecheck",
		},
		{
			name: "python uses unittest and assumes no lint/typecheck", file: "pyproject.toml",
			content:  "[project]\nname = \"x\"\n",
			language: "python", test: "python3 -B -m unittest",
		},
		{
			name: "rust leaves clippy to the user", file: "Cargo.toml",
			content:  "[package]\nname = \"x\"\n",
			language: "rust", test: "cargo test", typecheck: "cargo check",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.file), []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			d := Detect(dir)
			assertField(t, "language", d.Language, c.language)
			assertField(t, "test", d.TestCmd, c.test)
			assertField(t, "lint", d.LintCmd, c.lint)
			assertField(t, "typecheck", d.TypecheckCmd, c.typecheck)
		})
	}
}

func assertField(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
