package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	akskills "github.com/richardwooding/agentkit/skills"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/mcpclient"
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

	agentDefs []agents.Definition
	skills    *akskills.Set
	mcp       *mcpclient.Set

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
// applies the trust rule: everything in a project's settings that could
// *widen* what the agent may do is inert until the user has accepted those
// exact files — both .wright/settings.json and .wright/settings.local.json.
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
	b.warnBrokenLayers()
	b.trusted = b.checkTrust()
	b.settings = effectiveSettings(l, user, b.trusted, b.env)
	if extra := b.settings.Permissions.AdditionalDirs; b.trusted && len(extra) > 0 {
		dirs := append(append([]string(nil), b.o.AddDirs...), expandAll(ws, extra)...)
		if b.ws, err = workspace.Open(cwd, dirs); err != nil {
			return fmt.Errorf("additionalDirectories: %w", err)
		}
	}
	b.loadAgents()
	return nil
}

// loadAgents reads the custom sub-agent definitions. They are loaded here,
// before the permission rules are built, because each agent's name becomes
// an allow rule: calling a sub-agent has no effect of its own, and every
// tool the child then uses is evaluated again one level deeper.
func (b *builder) loadAgents() {
	defs, problems, err := agents.LoadCustom(b.ws, b.layered.Paths.UserConfig)
	if err != nil {
		b.warn("agents: %v", err)
		return
	}
	for _, p := range problems {
		b.warn("agent %s: %v", b.ws.Rel(p.Path), p.Err)
	}
	b.agentDefs = defs
}

// ProjectHash identifies a project's settings *as a unit*: the shared
// .wright/settings.json and the per-developer .wright/settings.local.json
// together. Hashing them separately let a repository ship only the local
// file and be trusted by default, because "no settings.json" used to mean
// "nothing to trust". Empty means the project has no settings file at all.
func ProjectHash(paths config.Paths) (string, error) {
	shared, err := trust.HashFile(paths.ProjectSettingsFile())
	if err != nil {
		return "", err
	}
	local, err := trust.HashFile(paths.ProjectLocalFile())
	if err != nil {
		return "", err
	}
	if shared == "" && local == "" {
		return "", nil
	}
	return trust.HashStrings(shared, local), nil
}

// warnBrokenLayers reports a project settings file that could not be parsed.
// It is loud and not fatal: the file came with the repository, so making it
// fatal would let any checkout stop wright from starting in that directory —
// and an empty settings.local.json is exactly what a payload that got one
// write leaves behind.
func (b *builder) warnBrokenLayers() {
	for _, l := range b.layered.Layers {
		if l.Err != nil {
			b.warn("%s could not be read (%v); it is ignored for this session", b.ws.Rel(l.Path), l.Err)
		}
	}
}

// projectFiles names the project settings files that exist, for messages.
func (b *builder) projectFiles() string {
	var names []string
	for _, p := range []string{b.layered.Paths.ProjectSettingsFile(), b.layered.Paths.ProjectLocalFile()} {
		if _, err := os.Stat(p); err == nil {
			names = append(names, b.ws.Rel(p))
		}
	}
	return strings.Join(names, " and ")
}

// untrustedNote lists everything an untrusted project layer loses, so the
// note on stderr is the truth: naming less than is dropped is worse than
// saying nothing, because the user reads it as an assurance.
const untrustedNote = "its allow rules, permission mode, additional directories, env passthrough, " +
	"MCP servers, sandbox settings (network, extra read-write and read-only mounts), " +
	"model, skills directories, git trailer, instruction files and any \"redaction\": false are ignored; " +
	"its ask and deny rules and \"redaction\": true still apply"

// checkTrust decides whether the project layers apply. With no project
// settings file at all there is nothing to trust; as soon as one exists —
// shared or local — the stored hash must match. An interactive terminal may
// accept it now through Confirm, everything else runs with the project
// layers inert and says so.
func (b *builder) checkTrust() bool {
	hash, err := ProjectHash(b.layered.Paths)
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
		if b.o.Confirm(TrustPrompt(b.projectFiles(), b.layered.Project, b.layered.ProjectLocal)) {
			if err := store.AcceptProject(root, hash); err != nil {
				b.warn("could not record project trust: %v", err)
			}
			return true
		}
	}
	b.warn("%s is not trusted yet: %s (run /trust to review and accept it)", b.projectFiles(), untrustedNote)
	return false
}

// effectiveSettings rebuilds the merged settings with *both* project layers
// gated by trust — settings.local.json is a file in the repository like any
// other, so it cannot be the one layer that applies unread. An untrusted
// project still contributes ask and deny rules and redaction: tightening is
// always free.
func effectiveSettings(l *config.Layered, user config.Settings, trusted bool, env func(string) string) config.Settings {
	s := config.Defaults()
	config.Merge(&s, user)
	if trusted {
		config.Merge(&s, l.Project)
		config.Merge(&s, l.ProjectLocal)
	} else {
		config.Merge(&s, tighteningOnly(l.Project))
		config.Merge(&s, tighteningOnly(l.ProjectLocal))
	}
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

// tighteningOnly keeps the parts of a project layer that can only restrict:
// ask and deny rules, and redaction when it is turned *on*. Everything else
// — the mode, the sandbox mounts, the network switch, redaction: false —
// can only widen what the agent may do, so it waits for trust.
func tighteningOnly(p config.Settings) config.Settings {
	out := config.Settings{Permissions: config.Permissions{Ask: p.Permissions.Ask, Deny: p.Permissions.Deny}}
	if p.Redaction != nil && *p.Redaction {
		out.Redaction = p.Redaction
	}
	return out
}

// TrustPrompt renders what accepting a project's settings would enable, for
// the trust question (interactive Confirm now, the TUI's /trust later). Both
// project layers are shown: they are trusted, and dropped, together.
func TrustPrompt(path string, layers ...config.Settings) string {
	var p config.Settings
	for _, l := range layers {
		config.Merge(&p, l)
	}
	s := fmt.Sprintf("%s wants to:\n", path)
	if n := len(p.Permissions.Allow); n > 0 {
		s += fmt.Sprintf("  allow %d rule(s): %v\n", n, p.Permissions.Allow)
	}
	if p.Permissions.Mode != "" {
		s += fmt.Sprintf("  start in permission mode %q\n", p.Permissions.Mode)
	}
	if n := len(p.Permissions.AdditionalDirs); n > 0 {
		s += fmt.Sprintf("  add %d directory(ies): %v\n", n, p.Permissions.AdditionalDirs)
	}
	if n := len(p.Sandbox.PassEnv); n > 0 {
		s += fmt.Sprintf("  pass %d env var(s) into the sandbox: %v\n", n, p.Sandbox.PassEnv)
	}
	if p.Sandbox.AllowNetwork {
		s += "  give sandboxed commands the network\n"
	}
	if p.Sandbox.Backend != "" {
		s += fmt.Sprintf("  use sandbox backend %q\n", p.Sandbox.Backend)
	}
	if n := len(p.Sandbox.ExtraRW); n > 0 {
		s += fmt.Sprintf("  mount %d directory(ies) read-write in the sandbox: %v\n", n, p.Sandbox.ExtraRW)
	}
	if n := len(p.Sandbox.ExtraRO); n > 0 {
		s += fmt.Sprintf("  mount %d directory(ies) read-only in the sandbox: %v\n", n, p.Sandbox.ExtraRO)
	}
	if p.Redaction != nil && !*p.Redaction {
		s += "  turn secret redaction off\n"
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
