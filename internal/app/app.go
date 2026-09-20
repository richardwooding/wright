// Package app is the composition root: it wires workspace → config → trust →
// sandbox → policy → model → session → tools → engine, then hands the engine
// to the headless runner or to the interactive UI supplied by the caller.
// It is the only place that knows every internal package; the CLI talks to
// it and nothing else. The architecture tests here (import DAG, no unexpected
// network) guard the package boundaries the rest of the tree relies on.
package app

import (
	"context"
	"errors"
	"io"

	akskills "github.com/richardwooding/agentkit/skills"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/diag"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/ghauth"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/workspace"
)

// ErrNoInteractive is returned by Run when an interactive session was
// requested but no UI was wired in.
var ErrNoInteractive = errors.New("app: interactive UI not wired")

// RunOptions are the resolved command-line flags plus the process facts the
// CLI knows (streams, environment, terminal state). Everything is explicit
// so tests can build a session without touching the real environment.
type RunOptions struct {
	Prompt string
	Print  bool   // headless: run Prompt (and stdin) once, report, exit
	Output string // headless format: text, json or stream-json

	Model string
	Mode  string // default, plan, auto-edit; bypass only through Bypass

	Bypass                 bool // --bypass-permissions
	AllowUnsandboxedBypass bool // --allow-unsandboxed-bypass
	// TrustWorkspace answers the startup trust question in advance
	// (--trust), for a wrapper script that starts a session in a directory
	// it already trusts. It is ignored by a headless run, which never asks
	// the question and never takes the baseline.
	TrustWorkspace bool

	Resume   string // session ID to continue
	Continue bool   // continue the most recent session

	Cwd     string
	AddDirs []string
	Allow   []string // permission rules for this run
	Deny    []string

	Sandbox      string // auto, bwrap, landlock, seatbelt, none
	AllowNetwork bool

	MaxSteps  int
	Reasoning string

	// GitHubAuth lets commands that run with network access authenticate to
	// GitHub as the user (--github-auth). Like DebugAddr it is a flag with
	// no environment variable: an env var can be set by a .envrc, a direnv
	// hook or a wrapper a repository ships, and this one hands over a
	// credential. The settings equivalent is honoured from the user's own
	// config only.
	GitHubAuth bool

	// DebugAddr turns on the diagnostics endpoint (--debug-addr). It is a
	// flag only: there is no environment variable and no settings key, so a
	// repository cannot open a port on the machine of anyone who runs
	// wright in it, and neither can anything that can write the user's
	// environment. It must be a loopback address.
	DebugAddr string

	Plain           bool
	Verbose         bool
	StrictInjection bool

	Version string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Env reads an environment variable; nil means os.Getenv.
	Env func(string) string
	// IsTerminal is true when stdin and stdout are both a terminal. Without
	// it, stdin is read as input and an untrusted project cannot be accepted.
	IsTerminal bool
	// Confirm asks the user a yes/no question before the UI starts (the
	// project-trust prompt). nil means "no".
	Confirm func(prompt string) bool
	// Select asks the user to pick one of options before the UI starts (the
	// MCP consent prompt, which has three answers rather than two). It
	// returns the chosen index; ok is false when the user did not choose,
	// which callers must read as the most restrictive option. nil means the
	// same, so a caller that cannot ask never widens anything.
	Select func(prompt string, options []string) (choice int, ok bool)
}

// Built is everything Build produced. Close releases the engine, which in
// turn flushes session metadata and the audit log.
type Built struct {
	Engine    *engine.Engine
	Store     *session.Store
	WS        *workspace.Workspace
	Layered   *config.Layered
	Settings  config.Settings // effective settings after trust gating
	Warnings  []string
	Sandbox   sandbox.Backend
	Choice    model.Choice
	SessionID string
	Trusted   bool // project settings were accepted (or there are none)
	// WorkspaceTrusted is the other acceptance: the user trusts this
	// directory, so edits inside it do not prompt. Always false headless.
	WorkspaceTrusted bool
	// Skills is the merged skill set the agent was given (possibly empty).
	Skills *akskills.Set
	// MCP holds the connected servers and their tools; Close ends them.
	MCP *mcpclient.Set
	// Agents are the custom sub-agent definitions that were loaded.
	Agents []agents.Definition
	Close  func() error

	// diag is the session's self-inspection: the dump signal, and the
	// endpoint when one was asked for. Unexported because it is reached
	// through /debug and the status bar, never by a caller poking at it.
	diag *diag.Server
	// gitHub is the credential holder the bash tool reads per call, and
	// redactor is kept so /github can say truthfully whether a token it
	// resolved later is masked.
	gitHub    *tools.GitHubAuth
	redactor  *redact.Redactor
	ghResolve func() (ghauth.Result, error)

	opts RunOptions
	// jobs is the session's background commands, for /jobs. It is not
	// exported: a UI shows them through Command, and nothing outside the
	// session should be able to reach into a running process.
	jobs *tools.JobSet
}

// InteractiveDeps is what a UI needs beyond the engine.
type InteractiveDeps struct {
	Store         *session.Store
	Git           func(ctx context.Context) git.Summary
	Version       string
	WorkspaceRoot string
	Plain         bool
	InitialPrompt string
	Warnings      []string
	Models        func(ctx context.Context) []model.Choice
	// DebugAddr is the diagnostics endpoint's address, empty when none is
	// running. The UI shows it so a user who started one can find it
	// without remembering what they typed.
	DebugAddr string
	// Command runs a slash command the UI does not implement itself:
	// /init /mcp /skills /audit /trust /diff /redaction. It returns text to
	// show the user.
	Command func(ctx context.Context, name string, args []string) (string, error)
}

// Interactive runs the UI over a built engine and returns a summary line to
// print after it exits. The TUI package supplies it through cmd/wright so
// app never imports it.
type Interactive func(ctx context.Context, e *engine.Engine, deps InteractiveDeps) (summary string, err error)

// Build wires a session without running anything. It fails early on a bad
// model name, an unusable mode or a refused bypass, before any UI starts.
func Build(ctx context.Context, o RunOptions) (*Built, error) {
	b := &builder{ctx: ctx, o: o}
	for _, phase := range []func() error{
		b.workspaceAndConfig,
		b.githubAuth,
		b.sandboxing,
		b.permissions,
		b.modelAndSession,
		b.toolsAndEngine,
		b.diagnostics,
	} {
		if err := phase(); err != nil {
			b.cleanup()
			return nil, err
		}
	}
	return b.built(), nil
}
