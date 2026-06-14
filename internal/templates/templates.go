// Package templates seeds a ready-to-run VichuFlow project: minimal source plus
// a REAL verification gate, so `vichu init --template` and `vichu new` let a run
// reach `completed` from scratch with no manual config. Each built-in template
// uses its stack's BUILT-IN test runner (go test, node --test, python3 -m
// unittest, cargo test), so the gate passes with no package install — matching
// the philosophy of examples/.
package templates

import (
	"sort"
	"strings"

	"github.com/corteshvictor/vichu-flow/internal/config"
)

// File is one seeded file: a forward-slash relative path and its content.
type File struct {
	Path    string
	Content string
}

// Template seeds a project's source and the stack config its gate verifies.
type Template struct {
	Name     string
	Detected config.Detected
	files    func(slug string) []File
}

// Files returns the template's files with the project name (slugified for use in
// module/package identifiers) substituted.
func (t Template) Files(projectName string) []File { return t.files(slug(projectName)) }

// Get returns the template by name.
func Get(name string) (Template, bool) {
	t, ok := registry[name]
	return t, ok
}

// Names lists the available template names, sorted.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// slug turns a project name into an identifier safe for go.mod / package.json /
// Cargo.toml names: lowercase, only [a-z0-9-_], never empty.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "app"
	}
	return s
}

var registry = map[string]Template{
	"empty": {
		Name: "empty",
		// Per-OS gate so the empty template completes on Windows too, not just on
		// POSIX shells: `sh tests.sh` on unix, a `.cmd` on Windows.
		Detected: config.Detected{Language: "shell", TestCmd: "sh tests.sh", TestCmdWindows: "cmd /c tests.cmd"},
		files: func(slug string) []File {
			return []File{
				{Path: "README.md", Content: "# " + slug + "\n\nA VichuFlow project. The gate runs `tests.sh` (unix) / `tests.cmd`\n(Windows) — replace them with your project's real tests/lint/typecheck.\n"},
				{Path: "tests.sh", Content: emptyTestSh},
				{Path: "tests.cmd", Content: emptyTestCmd},
			}
		},
	},
	"go": {
		Name: "go",
		Detected: config.Detected{
			Language:     "go",
			TestCmd:      "go test ./...",
			LintCmd:      "go vet ./...",
			TypecheckCmd: "go build ./...",
		},
		files: func(slug string) []File {
			return []File{
				{Path: "go.mod", Content: "module " + slug + "\n\ngo 1.22\n"},
				{Path: "calc.go", Content: goCalc},
				{Path: "calc_test.go", Content: goCalcTest},
			}
		},
	},
	"node": {
		Name:     "node",
		Detected: config.Detected{Language: "javascript", PackageManager: "node", TestCmd: "node --test"},
		files: func(slug string) []File {
			return []File{
				{Path: "package.json", Content: "{\n  \"name\": \"" + slug + "\",\n  \"version\": \"0.0.0\",\n  \"private\": true,\n  \"type\": \"module\"\n}\n"},
				{Path: "calc.js", Content: nodeCalc},
				{Path: "calc.test.js", Content: nodeCalcTest},
			}
		},
	},
	"python": {
		Name:     "python",
		Detected: config.Detected{Language: "python", TestCmd: "python3 -B -m unittest"},
		files: func(slug string) []File {
			return []File{
				{Path: "pyproject.toml", Content: "[project]\nname = \"" + slug + "\"\nversion = \"0.0.0\"\n"},
				{Path: "calc.py", Content: pyCalc},
				{Path: "test_calc.py", Content: pyCalcTest},
			}
		},
	},
	"rust": {
		Name: "rust",
		Detected: config.Detected{
			Language:     "rust",
			TestCmd:      "cargo test",
			TypecheckCmd: "cargo check",
		},
		files: func(slug string) []File {
			return []File{
				{Path: "Cargo.toml", Content: "[package]\nname = \"" + slug + "\"\nversion = \"0.0.0\"\nedition = \"2021\"\n\n[lib]\npath = \"src/lib.rs\"\n"},
				{Path: "src/lib.rs", Content: rustLib},
			}
		},
	},
}

const emptyTestSh = `#!/bin/sh
# Minimal verification gate. VichuFlow runs this and reads its exit code — a gate
# must only VERIFY, never change files. Replace it with your project's real
# tests/lint/typecheck.
set -e
test -f README.md
echo "ok"
`

const emptyTestCmd = `@echo off
REM Minimal verification gate (Windows). VichuFlow runs this and reads its exit
REM code — replace it with your project's real tests/lint/typecheck.
if exist README.md ( exit /b 0 ) else ( exit /b 1 )
`

const goCalc = `package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}
`

const goCalcTest = `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("Add(2,3) should be 5")
	}
}
`

const nodeCalc = `export function add(a, b) {
  return a + b;
}
`

const nodeCalcTest = `import test from "node:test";
import assert from "node:assert";
import { add } from "./calc.js";

test("add", () => {
  assert.strictEqual(add(2, 3), 5);
});
`

const pyCalc = `def add(a, b):
    return a + b
`

const pyCalcTest = `import unittest

from calc import add


class TestCalc(unittest.TestCase):
    def test_add(self):
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
`

const rustLib = `pub fn add(a: i64, b: i64) -> i64 {
    a + b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_add() {
        assert_eq!(add(2, 3), 5);
    }
}
`
