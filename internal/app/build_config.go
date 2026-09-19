package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/snapshot"
	"github.com/richardwooding/wright/internal/trust"
	"github.com/richardwooding/wright/internal/workspace"
)

// builder carries the state between Build's phases.
type builder struct {
	ctx context.Context
	o   RunOptions

	cwd      string
	ws       *workspace.Workspace
	layered  *config.Layered
	user     config.Settings // raw user layer, for rule attribution
	settings config.Settings // effective, trust-gated
	trusted  bool
	warnings []string

	backend sandbox.Backend
	spec    sandbox.Spec

	mode   policy.Mode
	bypass bool
	pol    *policy.Engine

	choice    model.Choice
	store     *session.Store
	sessionID string
	redactor  *redact.Redactor
	auditLog  *audit.Log
	snaps     *snapshot.Store

	eng *engine.Engine
}

func (b *builder) warn(format string, args ...any) {
	b.warnings = append(b.warnings, fmt.Sprintf(format, args...))
}

func (b *builder) env(key string) string {
	if b.o.Env != nil {
		return b.o.Env(key)
	}
	return os.Getenv(key)
}

// headless reports whether no human can answer a prompt before the UI starts.
func (b *builder) headless() bool { return b.o.Print || !b.o.IsTerminal }

// workspaceAndConfig opens the workspace, loads the settings layers and
// applies the trust rule: a project's allow rules, extra directories, env
// passthrough and MCP servers are inert until the user has accepted that
// exact settings file.
func (b *builder) workspaceAndConfig() error {
	cwd, err := resolveCwd(b.o.Cwd)
	if err != nil {
		return err
	}
	b.cwd = cwd
	ws, err := workspace.Open(cwd, b.o.AddDirs)
	if err != nil {
		return err
	}
	l, err := config.Load(cwd)
	if err != nil {
		return err
	}
	user, err := readLayer(l.Paths.UserConfigFile())
	if err != nil {
		return err
	}
	b.ws, b.layered, b.user = ws, l, user
	b.trusted = b.checkTrust()
	b.settings = effectiveSettings(l, user, b.trusted, b.env)
	if extra := b.settings.Permissions.AdditionalDirs; b.trusted && len(extra) > 0 {
		dirs := append(append([]string(nil), b.o.AddDirs...), expandAll(ws, extra)...)
		if b.ws, err = workspace.Open(cwd, dirs); err != nil {
			return fmt.Errorf("additionalDirectories: %w", err)
		}
	}
	return nil
}

// checkTrust decides whether the project layer applies. Without a settings
// file there is nothing to trust. Otherwise the stored hash must match; an
// interactive terminal may accept it now through Confirm, everything else
// runs with the project layer inert and says so.
func (b *builder) checkTrust() bool {
	path := b.layered.Paths.ProjectSettingsFile()
	hash, err := trust.HashFile(path)
	if err != nil {
		b.warn("project settings unreadable (%v); treating as untrusted", err)
		return false
	}
	if hash == "" {
		return true
	}
	root := b.ws.Root()
	store := trust.Open(b.layered.Paths.TrustFile())
	if store.ProjectTrusted(root, hash) {
		return true
	}
	if b.o.Confirm != nil && !b.headless() {
		if b.o.Confirm(TrustPrompt(path, b.layered.Project)) {
			if err := store.AcceptProject(root, hash); err != nil {
				b.warn("could not record project trust: %v", err)
			}
			return true
		}
	}
	b.warn("%s is not trusted yet: its allow rules, extra directories, env passthrough and MCP servers are ignored (run /trust to review and accept it)", b.ws.Rel(path))
	return false
}

// effectiveSettings rebuilds the merged settings with the project layer
// gated by trust. An untrusted project still contributes ask and deny rules:
// tightening is always free.
func effectiveSettings(l *config.Layered, user config.Settings, trusted bool, env func(string) string) config.Settings {
	s := config.Defaults()
	config.Merge(&s, user)
	if trusted {
		config.Merge(&s, l.Project)
	} else {
		config.Merge(&s, tighteningOnly(l.Project))
	}
	config.Merge(&s, l.ProjectLocal)
	if v := env("WRIGHT_MODEL"); v != "" {
		s.Model.Default = v
	}
	if v := env("WRIGHT_MODE"); v != "" {
		s.Permissions.Mode = v
	}
	if v := env("WRIGHT_SANDBOX"); v != "" {
		s.Sandbox.Backend = v
	}
	return s
}

// tighteningOnly keeps the parts of a project layer that can only restrict.
func tighteningOnly(p config.Settings) config.Settings {
	return config.Settings{Permissions: config.Permissions{Ask: p.Permissions.Ask, Deny: p.Permissions.Deny}}
}

// TrustPrompt renders what accepting a project's settings would enable, for
// the trust question (interactive Confirm now, the TUI's /trust later).
func TrustPrompt(path string, p config.Settings) string {
	s := fmt.Sprintf("%s wants to:\n", path)
	if n := len(p.Permissions.Allow); n > 0 {
		s += fmt.Sprintf("  allow %d rule(s): %v\n", n, p.Permissions.Allow)
	}
	if n := len(p.Permissions.AdditionalDirs); n > 0 {
		s += fmt.Sprintf("  add %d directory(ies): %v\n", n, p.Permissions.AdditionalDirs)
	}
	if n := len(p.Sandbox.PassEnv); n > 0 {
		s += fmt.Sprintf("  pass %d env var(s) into the sandbox: %v\n", n, p.Sandbox.PassEnv)
	}
	if n := len(p.MCPServers); n > 0 {
		s += fmt.Sprintf("  configure %d MCP server(s)\n", n)
	}
	return s + "Trust this project's settings?"
}

// readLayer decodes one settings file; a missing file is an empty layer.
func readLayer(path string) (config.Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config.Settings{}, nil
	}
	if err != nil {
		return config.Settings{}, err
	}
	s, err := config.Decode(data)
	if err != nil {
		return config.Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func resolveCwd(flag string) (string, error) {
	if flag == "" {
		return os.Getwd()
	}
	return filepath.Abs(flag)
}

func expandAll(ws *workspace.Workspace, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, ws.Expand(p))
	}
	return out
}

// Effective is the trust-gated view of a workspace's settings, for
// `wright config show` and the other read-only commands.
type Effective struct {
	Layered  *config.Layered
	Settings config.Settings
	Root     string
	Trusted  bool
}

// LoadEffective loads the settings for cwd exactly as a session would see
// them, without opening a session.
func LoadEffective(cwd string, env func(string) string) (Effective, error) {
	if env == nil {
		env = os.Getenv
	}
	b := &builder{ctx: context.Background(), o: RunOptions{Cwd: cwd, Env: env, Print: true}}
	if err := b.workspaceAndConfig(); err != nil {
		return Effective{}, err
	}
	return Effective{Layered: b.layered, Settings: b.settings, Root: b.ws.Root(), Trusted: b.trusted}, nil
}
