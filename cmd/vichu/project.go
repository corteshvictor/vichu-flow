package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/corteshvictor/vichu-flow/internal/adapters"
	"github.com/corteshvictor/vichu-flow/internal/config"
	"github.com/corteshvictor/vichu-flow/internal/engine"
	"github.com/corteshvictor/vichu-flow/internal/i18n"
	"github.com/corteshvictor/vichu-flow/internal/runtime"
	"github.com/corteshvictor/vichu-flow/internal/workspace"
)

// project bundles the resolved pieces a command needs to operate on a run.
type project struct {
	root  string
	cfg   *config.Config
	store *runtime.Store
	repo  workspace.Provider
}

// findRoot walks up from the current directory to the nearest vichu.yaml.
func findRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if config.Exists(filepath.Join(dir, config.FileName)) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", config.ErrNotFound
		}
		dir = parent
	}
}

// openProject loads config and resolves the workspace provider for the project
// rooted at the nearest vichu.yaml (git, filesystem, or auto).
func openProject() (*project, error) {
	root, err := findRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(filepath.Join(root, config.FileName))
	if err != nil {
		return nil, err
	}
	i18n.SetLanguage(cfg.UI.Language)
	repo, err := workspace.Open(root, cfg.Workspace.Provider)
	if err != nil {
		return nil, err
	}
	return &project{root: root, cfg: cfg, store: runtime.Open(root), repo: repo}, nil
}

// newEngine builds an engine that prints stage progress to STDOUT (human mode).
func (p *project) newEngine() *engine.Engine {
	return p.newEngineWithLog(func(m string) { fmt.Println("  " + m) })
}

// engineForOutput returns an engine whose progress goes to STDERR when jsonOut is
// set, so stdout stays PURE JSON for `--json` consumers (host packs / automation)
// while a human still sees the progress. The engine logs run completion/block/fail,
// which would otherwise contaminate the JSON object on stdout.
func (p *project) engineForOutput(jsonOut bool) *engine.Engine {
	if jsonOut {
		return p.newEngineWithLog(func(m string) { fmt.Fprintln(os.Stderr, "  "+m) })
	}
	return p.newEngine()
}

func (p *project) newEngineWithLog(log func(string)) *engine.Engine {
	return engine.New(engine.Options{
		Store:    p.store,
		Registry: adapters.DefaultRegistry(),
		Config:   p.cfg,
		Repo:     p.repo,
		Log:      log,
	})
}

// parseArgsAnyOrder parses fs allowing flags to appear BEFORE or AFTER positional
// args. Go's flag package stops at the first positional, so `cmd <id> --json` would
// silently drop `--json`; this re-parses the tail after each positional so both
// `cmd --json <id>` and `cmd <id> --json` work. Returns the positionals in order.
func parseArgsAnyOrder(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	return positionals, nil
}

// firstArg returns the first positional or "" — the optional run id most commands take.
func firstArg(positionals []string) string {
	if len(positionals) == 0 {
		return ""
	}
	return positionals[0]
}

// resolveRunID returns the given id, or the latest run if id is empty.
func (p *project) resolveRunID(id string) (string, error) {
	if id != "" {
		// Reject a traversal/unsafe id with a clear message BEFORE it is turned into a path — the
		// kernel enforces this too, this is just the friendlier CLI-side error.
		if err := runtime.ValidateRunID(id); err != nil {
			return "", err
		}
		if !p.store.RunExists(id) {
			return "", fmt.Errorf(i18n.T("status.not_found"), id)
		}
		return id, nil
	}
	latest, err := p.store.LatestRun()
	if err != nil {
		return "", err
	}
	if latest == "" {
		return "", fmt.Errorf("%s", i18n.T("status.no_runs"))
	}
	return latest, nil
}
