package config

import (
	"encoding/json"
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
// verification commands. It only proposes commands that are actually available
// for the project — a gate VichuFlow invents (e.g. `npm run lint` when there is
// no `lint` script) would make a real project block under requireGates. It is
// best-effort; users refine the result in vichu.yaml.
func Detect(root string) Detected {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	switch {
	case has("go.mod"):
		// go test/vet/build all ship with the Go toolchain — always safe.
		return Detected{
			Language:     "go",
			TestCmd:      "go test ./...",
			LintCmd:      "go vet ./...",
			TypecheckCmd: "go build ./...",
		}
	case has("Cargo.toml"):
		// cargo test/check ship with the toolchain; clippy needs an extra
		// component that may not be installed, so leave lint to the user.
		return Detected{
			Language:     "rust",
			TestCmd:      "cargo test",
			TypecheckCmd: "cargo check",
		}
	case has("package.json"):
		return detectNode(root, has)
	case has("pyproject.toml"), has("requirements.txt"), has("setup.py"), has("setup.cfg"):
		// unittest ships with Python; pytest/ruff/mypy are NOT assumed — they may
		// not be installed or configured, and a missing tool would block the run.
		return Detected{
			Language: "python",
			TestCmd:  "python3 -B -m unittest",
		}
	default:
		return Detected{Language: "unknown"}
	}
}

// detectNode proposes Node gates from the package.json scripts that actually
// exist — never a `run <script>` for a script the project does not declare.
func detectNode(root string, has func(string) bool) Detected {
	pm := "npm"
	switch {
	case has("pnpm-lock.yaml"):
		pm = "pnpm"
	case has("yarn.lock"):
		pm = "yarn"
	case has("bun.lockb"):
		pm = "bun"
	}
	d := Detected{Language: "javascript", PackageManager: pm}
	scripts := packageScripts(filepath.Join(root, "package.json"))
	if _, ok := scripts["test"]; ok {
		d.TestCmd = pm + " test"
	}
	if _, ok := scripts["lint"]; ok {
		d.LintCmd = pm + " run lint"
	}
	if _, ok := scripts["typecheck"]; ok {
		d.TypecheckCmd = pm + " run typecheck"
	}
	return d
}

// packageScripts returns the "scripts" map from a package.json, or nil.
func packageScripts(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}
