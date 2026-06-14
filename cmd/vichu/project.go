package main

import (
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

// newEngine builds an engine for the project, printing stage progress.
func (p *project) newEngine() *engine.Engine {
	return engine.New(engine.Options{
		Store:    p.store,
		Registry: adapters.DefaultRegistry(),
		Config:   p.cfg,
		Repo:     p.repo,
		Log:      func(m string) { fmt.Println("  " + m) },
	})
}

// resolveRunID returns the given id, or the latest run if id is empty.
func (p *project) resolveRunID(id string) (string, error) {
	if id != "" {
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
