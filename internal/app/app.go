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
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/session"
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
	// Skills is the merged skill set the agent was given (possibly empty).
	Skills *akskills.Set
	// MCP holds the connected servers and their tools; Close ends them.
	MCP *mcpclient.Set
	// Agents are the custom sub-agent definitions that were loaded.
	Agents []agents.Definition
	Close  func() error

	opts RunOptions
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
		b.sandboxing,
		b.permissions,
		b.modelAndSession,
		b.toolsAndEngine,
	} {
		if err := phase(); err != nil {
			b.cleanup()
			return nil, err
		}
	}
	return b.built(), nil
}
