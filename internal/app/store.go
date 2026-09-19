package app

import (
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/workspace"
)

// openWorkspace opens the workspace containing cwd for the read-only
// commands, which never build a session.
func openWorkspace(cwd string) (*workspace.Workspace, error) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return nil, err
	}
	return workspace.Open(dir, nil)
}

// OpenStore opens the session store for the workspace containing cwd, for
// the read-only CLI commands (sessions, audit) that never start an engine.
func OpenStore(cwd string) (*session.Store, *config.Layered, error) {
	cwd, err := resolveCwd(cwd)
	if err != nil {
		return nil, nil, err
	}
	ws, err := workspace.Open(cwd, nil)
	if err != nil {
		return nil, nil, err
	}
	l, err := config.Load(cwd)
	if err != nil {
		return nil, nil, err
	}
	store, err := session.Open(l.Paths.UserData, ws)
	if err != nil {
		return nil, nil, err
	}
	return store, l, nil
}
