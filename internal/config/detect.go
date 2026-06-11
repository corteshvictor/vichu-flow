package config

import (
	"os"
	"path/filepath"
)

// Detected is the result of inspecting a repository for its stack and the
// commands a run should verify against.
type Detected struct {
	Language       string
	PackageManager string
	TestCmd        string
	LintCmd        string
	TypecheckCmd   string
}

// Detect inspects a repository root for well-known stack markers and proposes
// verification commands. It is best-effort; users refine the result in vichu.yaml.
func Detect(root string) Detected {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	switch {
	case has("go.mod"):
		return Detected{
			Language:     "go",
			TestCmd:      "go test ./...",
			LintCmd:      "go vet ./...",
			TypecheckCmd: "go build ./...",
		}
	case has("Cargo.toml"):
		return Detected{
			Language:     "rust",
			TestCmd:      "cargo test",
			LintCmd:      "cargo clippy",
			TypecheckCmd: "cargo check",
		}
	case has("package.json"):
		pm := "npm"
		switch {
		case has("pnpm-lock.yaml"):
			pm = "pnpm"
		case has("yarn.lock"):
			pm = "yarn"
		case has("bun.lockb"):
			pm = "bun"
		}
		return Detected{
			Language:       "javascript",
			PackageManager: pm,
			TestCmd:        pm + " test",
			LintCmd:        pm + " run lint",
			TypecheckCmd:   pm + " run typecheck",
		}
	case has("pyproject.toml"), has("requirements.txt"), has("setup.py"), has("setup.cfg"):
		return Detected{
			Language:     "python",
			TestCmd:      "pytest",
			LintCmd:      "ruff check .",
			TypecheckCmd: "mypy .",
		}
	default:
		return Detected{Language: "unknown"}
	}
}
