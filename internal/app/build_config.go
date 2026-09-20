package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	akskills "github.com/richardwooding/agentkit/skills"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/diag"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/ghauth"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/snapshot"
	"github.com/richardwooding/wright/internal/tools"
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
	trusted  bool            // the project's settings files were accepted
	// workspaceTrusted is the other, independent acceptance: the user
	// trusts this directory. It is never set for a headless run.
	workspaceTrusted bool
	warnings         []string

	backend sandbox.Backend
	spec    sandbox.Spec

	mode   policy.Mode
	bypass bool
	pol    *policy.Engine

	agentDefs []agents.Definition
	skills    *akskills.Set
	mcp       *mcpclient.Set
	jobs      *tools.JobSet
	resumed   string // session id when this run continues one, else ""

	choice    model.Choice
	store     *session.Store
	sessionID string
	redactor  *redact.Redactor
	auditLog  *audit.Log
	snaps     *snapshot.Store

	eng  *engine.Engine
	diag *diag.Server

	// The GitHub credential, when this session asked for one. githubToken
	// is the value and must never reach a warning or the audit log;
	// githubSource is its origin's *name*, which may go anywhere.
	githubToken  string
	githubSource string
	githubEnv    map[string]string
	githubGhDir  string
	// gitHub is the holder the bash tool reads per call. It exists whether
	// or not a credential was resolved, so /github on can fill it later.
	gitHub *tools.GitHubAuth
	// ghResolve replaces the real resolver in tests.
	ghResolve func() (ghauth.Result, error)
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
	if err := b.resolveTrust(); err != nil {
		return err
	}
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
	"model, skills directories, git trailer, GitHub authentication, instruction files, web-search provider and any \"redaction\": false are ignored; " +
	"its ask and deny rules and \"redaction\": true still apply"

// trustState is what the two independent trust questions resolved to for
// this run. They are answered together because a first run in a directory
// that also ships settings would otherwise ask twice.
type trustState struct {
	store *trust.Store
	root  string
	// hash is the project settings hash to accept, set only when the
	// settings are pending (askSettings).
	hash        string
	askSettings bool // the settings exist and have not been accepted
	broken      bool // the settings could not be hashed at all
	workspace   bool // the directory itself is accepted
}

// resolveTrust answers both trust questions with at most one prompt.
//
// They are separate propositions and stay separate in trust.json: the
// settings question is "these exact bytes of .wright/settings*.json may
// widen what the agent does", the workspace question is "this directory is
// mine to work in". Accepting one never answers the other — but a first run
// in a directory that has both pending asks once, and the fused prompt
// states both.
//
// Declining the workspace ends the run (ErrWorkspaceNotTrusted): there is no
// half-trusted interactive session to fall back to, and starting one anyway
// would teach the user that the question is decorative.
func (b *builder) resolveTrust() error {
	st := b.pendingTrust()
	// Headless (-p, or any non-terminal run) behaves exactly as it did
	// before workspace trust existed: it never prompts, never exits over
	// trust, and never takes the baseline — not even in a directory the
	// user accepted interactively. A script's permissions must not depend
	// on what someone once answered in a terminal.
	if !b.headless() {
		if err := b.askTrust(&st); err != nil {
			return err
		}
	}
	b.trusted = !st.askSettings && !st.broken
	b.workspaceTrusted = st.workspace && !b.headless()
	if st.askSettings {
		b.warn("%s is not trusted yet: %s (run /trust to review and accept it)", b.projectFiles(), untrustedNote)
	}
	return nil
}

// pendingTrust reads what has already been accepted for this workspace.
func (b *builder) pendingTrust() trustState {
	st := trustState{store: trust.Open(b.layered.Paths.TrustFile()), root: b.ws.Root()}
	hash, err := ProjectHash(b.layered.Paths)
	switch {
	case err != nil:
		b.warn("project settings unreadable (%v); treating as untrusted", err)
		st.broken = true
	case hash == "":
		// No project settings file at all: nothing to trust.
	case st.store.ProjectTrusted(st.root, hash):
		// The stored hash still matches these bytes.
	default:
		st.hash, st.askSettings = hash, true
	}
	st.workspace = st.store.WorkspaceTrusted(st.root)
	return st
}

// askTrust puts the pending questions to the user. --trust answers the
// workspace one in advance, for a wrapper script that starts a session in a
// directory it already trusts; it never answers the settings question, which
// is about content nobody has read.
func (b *builder) askTrust(st *trustState) error {
	if !st.workspace && b.o.TrustWorkspace {
		b.acceptWorkspace(st)
	}
	if b.o.Confirm == nil {
		return nil // nothing can be asked; the run continues with no baseline
	}
	if !st.workspace {
		if !b.o.Confirm(b.workspaceQuestion(st.askSettings)) {
			return ErrWorkspaceNotTrusted
		}
		b.acceptWorkspace(st)
		if st.askSettings {
			b.acceptSettings(st) // the fused prompt asked for both
		}
		return nil
	}
	if st.askSettings && b.o.Confirm(TrustPrompt(b.projectFiles(), b.layered.Project, b.layered.ProjectLocal)) {
		b.acceptSettings(st)
	}
	return nil
}

// workspaceQuestion renders the startup question, fused with the settings
// question when that one is pending too.
func (b *builder) workspaceQuestion(withSettings bool) string {
	settings := ""
	if withSettings {
		settings = trustPromptBody(b.projectFiles(), b.layered.Project, b.layered.ProjectLocal)
	}
	return WorkspaceTrustPrompt(b.ws.Root(), settings)
}

func (b *builder) acceptWorkspace(st *trustState) {
	if err := st.store.AcceptWorkspace(st.root); err != nil {
		b.warn("could not record workspace trust: %v", err)
	}
	st.workspace = true
}

func (b *builder) acceptSettings(st *trustState) {
	if err := st.store.AcceptProject(st.root, st.hash); err != nil {
		b.warn("could not record project trust: %v", err)
	}
	st.askSettings = false
}

// WorkspaceTrustPrompt is the startup question for a directory the user has
// not accepted yet. It says what trust grants and — just as important — what
// it does not: a user who reads it as "wright may now do as it likes" has
// been misled by the prompt, not by the code. settings is TrustPromptBody
// when the project also ships settings nobody has accepted, so one question
// covers both; it is empty otherwise.
func WorkspaceTrustPrompt(root, settings string) string {
	s := "wright has not been trusted with " + root + " yet.\n" +
		"Trusting this directory lets wright, without asking each time:\n" +
		"  read the files in it\n" +
		"  create and edit files in it\n" +
		"It does not allow:\n" +
		"  running shell commands — every command is still approved one at a time\n" +
		"  network access, or reading or writing anything outside this directory\n" +
		"  touching .git, .wright, ignored files, or files that look like secrets\n"
	if settings != "" {
		s += "\nThis directory also ships settings that have not been accepted.\n" + settings
		return s + "Trust this directory and its settings? (answering no exits without starting a session)"
	}
	return s + "Trust this directory? (answering no exits without starting a session)"
}

// TrustPrompt renders what accepting a project's settings would enable, for
// the trust question (interactive Confirm now, the TUI's /trust later). Both
// project layers are shown: they are trusted, and dropped, together.
func TrustPrompt(path string, layers ...config.Settings) string {
	return trustPromptBody(path, layers...) + "Trust this project's settings?"
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
	// GitHub auth is the user's to grant and nobody else's. Trust is
	// answered once for a whole settings file, and the workspace-trust
	// prompt promises "It does not allow: network access"; a repository that
	// could flip on credential injection by being trusted once would
	// contradict the assurance that prompt gives. So it is taken from the
	// user's own config even when the project is trusted.
	s.GitHub = user.GitHub
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

// trustPromptBody is TrustPrompt without its closing question, so the
// workspace prompt can carry the same facts when the two are fused.
func trustPromptBody(path string, layers ...config.Settings) string {
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
	if p.Search.Provider != "" {
		s += fmt.Sprintf("  send web searches to the %q provider\n", p.Search.Provider)
	}
	return s
}

// TrustEntry is one accepted directory, as `wright trust list` shows it.
// The two acceptances are reported separately because they are separate:
// Workspace is "edits here do not ask", Settings is "these settings files
// were read and accepted".
type TrustEntry struct {
	Root      string
	Workspace bool
	Settings  bool
	// Accepted is when the settings were accepted, zero when they never
	// were; WorkspaceAccepted is when the directory was.
	Accepted          time.Time
	WorkspaceAccepted time.Time
}

// ListTrust reports every directory with a trust record, sorted by path.
func ListTrust(cwd string) ([]TrustEntry, error) {
	store, err := trustStore(cwd)
	if err != nil {
		return nil, err
	}
	records := store.Projects()
	out := make([]TrustEntry, 0, len(records))
	for _, r := range records {
		out = append(out, TrustEntry{
			Root:              r.Root,
			Workspace:         !r.WorkspaceAccepted.IsZero(),
			Settings:          r.SettingsHash != "",
			Accepted:          r.Accepted,
			WorkspaceAccepted: r.WorkspaceAccepted,
		})
	}
	return out, nil
}

// AcceptWorkspaceTrust records a directory as trusted without starting a
// session — the scripted equivalent of answering yes at startup. It accepts
// the directory only: settings files are content someone has to read, so
// they keep their own prompt. It returns the path as it was recorded.
func AcceptWorkspaceTrust(cwd string) (string, error) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return "", err
	}
	store, err := trustStore(dir)
	if err != nil {
		return "", err
	}
	if err := store.AcceptWorkspace(dir); err != nil {
		return "", err
	}
	if r, ok := store.Project(dir); ok {
		return r.Root, nil
	}
	return dir, nil
}

// ForgetTrust removes a directory's record, revoking both acceptances: the
// workspace baseline and any accepted settings hash.
func ForgetTrust(cwd string) (string, error) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return "", err
	}
	store, err := trustStore(dir)
	if err != nil {
		return "", err
	}
	root := dir
	if r, ok := store.Project(dir); ok {
		root = r.Root
	}
	return root, store.ForgetProject(dir)
}

// trustStore opens trust.json for the config directory cwd resolves to.
func trustStore(cwd string) (*trust.Store, error) {
	dir, err := resolveCwd(cwd)
	if err != nil {
		return nil, err
	}
	return trust.Open(config.DefaultPaths(dir).TrustFile()), nil
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
