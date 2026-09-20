// Package tuiwire adapts the app's Interactive hook to the Bubble Tea UI. It
// is the only place that knows both app and tui, so neither imports the
// other and cmd/wright stays a one-liner.
package tuiwire

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/tui"
)

// maxCompletionFiles bounds the "@" completion walk on large repositories.
const maxCompletionFiles = 5000

// Interactive runs the TUI over a built engine. Pass it to cli.Main.
func Interactive(ctx context.Context, e *engine.Engine, deps app.InteractiveDeps) (string, error) {
	ctl := controller{Engine: e, models: deps.Models}
	o := tui.Options{
		Events:        e.Events(),
		Git:           deps.Git,
		Files:         filesUnder(deps.WorkspaceRoot),
		Command:       deps.Command,
		Version:       deps.Version,
		WorkspaceRoot: deps.WorkspaceRoot,
		Plain:         deps.Plain,
		InitialPrompt: deps.InitialPrompt,
		Warnings:      deps.Warnings,
		DebugAddr:     deps.DebugAddr,
	}
	if deps.Store != nil {
		o.Sessions = sessions{store: deps.Store}
	}
	return tui.Run(ctx, ctl, o)
}

// controller bridges the two method shapes that differ between the engine
// and the UI contract.
type controller struct {
	*engine.Engine
	models func(context.Context) []model.Choice
}

// Compact runs a manual compaction; the UI has no context to pass.
func (c controller) Compact() error { return c.Engine.Compact(context.Background()) }

// Models lists the models the picker can switch to.
func (c controller) Models(ctx context.Context) []tui.ModelChoice {
	if c.models == nil {
		return nil
	}
	choices := c.models(ctx)
	out := make([]tui.ModelChoice, 0, len(choices))
	for _, ch := range choices {
		out = append(out, tui.ModelChoice{Name: ch.Model, Provider: ch.Provider, Known: ch.Info.Known})
	}
	return out
}

type sessions struct{ store *session.Store }

func (s sessions) List(ctx context.Context) ([]tui.SessionMeta, error) {
	metas, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SessionMeta, 0, len(metas))
	for _, m := range metas {
		out = append(out, tui.SessionMeta{ID: m.ID, Title: m.Title, Updated: m.Updated, Turns: m.Turns})
	}
	return out, nil
}

func (s sessions) Export(ctx context.Context, id, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := s.store.ExportMarkdown(ctx, id, f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// filesUnder lists workspace files for "@" completion, skipping VCS and
// dependency directories; the walk is bounded so a monorepo stays snappy.
func filesUnder(root string) func() []string {
	return func() []string {
		var out []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // unreadable entries are simply not offered
			}
			name := d.Name()
			if d.IsDir() {
				if p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" || name == "dist") {
					return fs.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			out = append(out, filepath.ToSlash(rel))
			if len(out) >= maxCompletionFiles {
				return fs.SkipAll
			}
			return nil
		})
		return out
	}
}
