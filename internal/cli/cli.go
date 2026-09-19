// Package cli declares the kong command tree for the wright binary and the
// Run methods behind it. It is the only package that may import internal/app
// (the composition root); everything else is reached through app.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kong"
)

// Exit codes shared by the interactive and headless paths. They are part of
// the headless contract (scripts branch on them), so they never change meaning.
const (
	ExitOK          = 0
	ExitError       = 1 // provider or tool error
	ExitUsage       = 2
	ExitApproval    = 3 // headless run needed an approval that was not granted
	ExitBudget      = 4
	ExitInterrupted = 130
)

// errNotImplemented marks commands that exist in the CLI surface but are
// delivered in a later phase. Declaring the full tree now keeps help output,
// docs and scripts stable while the implementation lands.
var errNotImplemented = errors.New("not implemented yet")

// BuildInfo carries the ldflags-injected version metadata from package main.
type BuildInfo struct {
	Version, Commit, Date string
}

// String renders the version line printed by --version.
func (b BuildInfo) String() string {
	return fmt.Sprintf("wright %s (%s, %s)", b.Version, b.Commit, b.Date)
}

// CLI is the single kong root struct. Flags here are global; each subcommand
// carries its own arguments. Only --version, doctor, config paths and the
// hidden __sandbox helper are functional in this phase.
type CLI struct {
	Print                  bool             `short:"p" name:"print" help:"Headless mode: run the prompt, print the result and exit."`
	Output                 string           `enum:"text,json,stream-json" default:"text" help:"Headless output format (${enum})."`
	Model                  string           `short:"m" env:"WRIGHT_MODEL" help:"Model to use (provider auto-detected from env keys when omitted)."`
	Mode                   string           `env:"WRIGHT_MODE" help:"Permission mode: default, plan or auto-edit (bypass only via --bypass-permissions)."`
	BypassPermissions      bool             `help:"Skip approval prompts (hard-deny set still applies; refused without a sandbox)."`
	AllowUnsandboxedBypass bool             `help:"Permit --bypass-permissions even when no OS sandbox is available."`
	Resume                 string           `short:"r" help:"Resume the session with this ID."`
	Continue               bool             `short:"c" help:"Resume the most recent session for this workspace."`
	Cwd                    string           `type:"existingdir" help:"Working directory (defaults to the current directory)."`
	AddDir                 []string         `name:"add-dir" help:"Additional directory the agent may access (repeatable)."`
	Allow                  []string         `help:"Permission rule to allow for this run, e.g. 'bash(go test *)' (repeatable)."`
	Deny                   []string         `help:"Permission rule to deny for this run (repeatable)."`
	Sandbox                string           `enum:"auto,bwrap,landlock,seatbelt,none" default:"auto" env:"WRIGHT_SANDBOX" help:"OS sandbox backend (${enum})."`
	AllowNetwork           bool             `help:"Let sandboxed shell commands reach the network without asking."`
	MaxSteps               int              `help:"Maximum agent steps per run (0 = default budget)."`
	Reasoning              string           `help:"Reasoning effort hint passed to the model (low, medium, high)."`
	Plain                  bool             `env:"WRIGHT_PLAIN" help:"Plain output: no alternate screen, numbered prompts (implied by NO_COLOR, TERM=dumb or a non-TTY)."`
	StrictInjection        bool             `help:"Treat prompt-injection signals in web/MCP content as needing approval."`
	Verbose                bool             `short:"v" help:"Verbose diagnostics on stderr."`
	Version                kong.VersionFlag `short:"V" help:"Print version and exit."`

	Run           RunCmd      `cmd:"" default:"withargs" help:"Start an interactive session (default command)."`
	Sessions      SessionsCmd `cmd:"" help:"List, show, export or delete saved sessions."`
	Models        ModelsCmd   `cmd:"" help:"List models whose provider has credentials."`
	Config        ConfigCmd   `cmd:"" help:"Show effective settings or the paths they are read from."`
	MCP           MCPCmd      `cmd:"" name:"mcp" help:"Manage MCP servers."`
	Skills        SkillsCmd   `cmd:"" help:"List discovered skills."`
	Audit         AuditCmd    `cmd:"" help:"Inspect or verify the audit log."`
	Init          InitCmd     `cmd:"" help:"Create AGENTS.md and .wright/settings.json for this project."`
	Doctor        DoctorCmd   `cmd:"" help:"Check sandbox backends, tools and credentials."`
	SandboxHelper SandboxCmd  `cmd:"" name:"__sandbox" hidden:"" help:"Internal landlock helper."`
}

// Globals is bound into every Run method: the parsed root, build info, the
// cancellable context from main and the output streams (swappable in tests).
type Globals struct {
	CLI    *CLI
	Build  BuildInfo
	Ctx    context.Context
	Stdout io.Writer
	Stderr io.Writer
}

// exitPanic is the sentinel kong.Exit raises so --version and --help unwind
// through Main instead of calling os.Exit from inside the parser.
type exitPanic struct{ code int }

// Main parses args, runs the selected command and maps the result to an exit
// code. It is the whole of package main's logic so tests can drive it.
func Main(ctx context.Context, args []string, build BuildInfo) (code int, err error) {
	return Run(ctx, args, build, os.Stdout, os.Stderr)
}

// Run is Main with explicit output streams, for tests.
func Run(ctx context.Context, args []string, build BuildInfo, stdout, stderr io.Writer) (code int, err error) {
	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("wright"),
		kong.Description("A coding-agent terminal harness: approval-first permissions, an OS sandbox around every command, secrets that stay on your machine, and an audit trail you can verify."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"version": build.String()},
		kong.Writers(stdout, stderr),
		kong.Exit(func(c int) { panic(exitPanic{code: c}) }),
	)
	if err != nil {
		return ExitError, err
	}
	defer func() {
		if r := recover(); r != nil {
			ep, ok := r.(exitPanic)
			if !ok {
				panic(r)
			}
			code, err = ep.code, nil
		}
	}()

	kctx, err := parser.Parse(args)
	if err != nil {
		parser.Errorf("%v", err)
		fmt.Fprintln(stderr, "Run 'wright --help' for usage.")
		return ExitUsage, nil
	}
	g := &Globals{CLI: cli, Build: build, Ctx: ctx, Stdout: stdout, Stderr: stderr}
	if err := kctx.Run(g); err != nil {
		return exitCodeFor(err), err
	}
	return ExitOK, nil
}

// exitCodeFor maps command errors onto the documented exit codes.
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	case errors.Is(err, errNotImplemented):
		return ExitError
	default:
		return ExitError
	}
}

// RunCmd is the default command: an interactive (or, with -p, headless) session.
type RunCmd struct {
	Prompt []string `arg:"" optional:"" help:"Initial prompt. With -p it is the whole request."`
}

// Run starts a session. Interactive and headless modes land in Phase 1/2.
func (c *RunCmd) Run(_ *Globals) error {
	return fmt.Errorf("interactive session: %w", errNotImplemented)
}

// SessionsCmd groups the session management subcommands.
type SessionsCmd struct {
	List   SessionsListCmd   `cmd:"" default:"1" help:"List sessions for this workspace."`
	Show   SessionsShowCmd   `cmd:"" help:"Print a session transcript."`
	Export SessionsExportCmd `cmd:"" help:"Export a session as Markdown."`
	Delete SessionsDeleteCmd `cmd:"" help:"Delete a session, its snapshots and audit log."`
	Purge  SessionsPurgeCmd  `cmd:"" help:"Delete all sessions for this workspace."`
}

// SessionsListCmd lists sessions.
type SessionsListCmd struct{}

// Run lists sessions (Phase 2).
func (c *SessionsListCmd) Run(_ *Globals) error {
	return fmt.Errorf("sessions list: %w", errNotImplemented)
}

// SessionsShowCmd prints one session.
type SessionsShowCmd struct {
	ID string `arg:"" help:"Session ID."`
}

// Run prints a session (Phase 2).
func (c *SessionsShowCmd) Run(_ *Globals) error {
	return fmt.Errorf("sessions show: %w", errNotImplemented)
}

// SessionsExportCmd exports a session as Markdown.
type SessionsExportCmd struct {
	ID string `arg:"" help:"Session ID."`
}

// Run exports a session (Phase 2).
func (c *SessionsExportCmd) Run(_ *Globals) error {
	return fmt.Errorf("sessions export: %w", errNotImplemented)
}

// SessionsDeleteCmd deletes one session.
type SessionsDeleteCmd struct {
	ID string `arg:"" help:"Session ID."`
}

// Run deletes a session (Phase 2).
func (c *SessionsDeleteCmd) Run(_ *Globals) error {
	return fmt.Errorf("sessions delete: %w", errNotImplemented)
}

// SessionsPurgeCmd deletes every session for the workspace.
type SessionsPurgeCmd struct{}

// Run purges sessions (Phase 2).
func (c *SessionsPurgeCmd) Run(_ *Globals) error {
	return fmt.Errorf("sessions purge: %w", errNotImplemented)
}

// ModelsCmd lists usable models.
type ModelsCmd struct{}

// Run lists models (Phase 1).
func (c *ModelsCmd) Run(_ *Globals) error { return fmt.Errorf("models: %w", errNotImplemented) }

// ConfigCmd groups the settings subcommands.
type ConfigCmd struct {
	Show  ConfigShowCmd  `cmd:"" default:"1" help:"Print the effective merged settings as JSON."`
	Paths ConfigPathsCmd `cmd:"" help:"Print where settings, data and cache live."`
}

// ConfigShowCmd prints merged settings.
type ConfigShowCmd struct{}

// Run prints merged settings (Phase 2, needs trust gating for project layers).
func (c *ConfigShowCmd) Run(_ *Globals) error {
	return fmt.Errorf("config show: %w", errNotImplemented)
}

// MCPCmd groups MCP server management.
type MCPCmd struct {
	List   MCPListCmd   `cmd:"" default:"1" help:"List configured MCP servers."`
	Add    MCPAddCmd    `cmd:"" help:"Add an MCP server to project settings."`
	Remove MCPRemoveCmd `cmd:"" help:"Remove an MCP server."`
}

// MCPListCmd lists MCP servers.
type MCPListCmd struct{}

// Run lists servers (Phase 3).
func (c *MCPListCmd) Run(_ *Globals) error { return fmt.Errorf("mcp list: %w", errNotImplemented) }

// MCPAddCmd adds an MCP server.
type MCPAddCmd struct {
	Name    string   `arg:"" help:"Server name."`
	Command []string `arg:"" optional:"" passthrough:"" help:"Command and arguments, or a URL for HTTP transports."`
}

// Run adds a server (Phase 3).
func (c *MCPAddCmd) Run(_ *Globals) error { return fmt.Errorf("mcp add: %w", errNotImplemented) }

// MCPRemoveCmd removes an MCP server.
type MCPRemoveCmd struct {
	Name string `arg:"" help:"Server name."`
}

// Run removes a server (Phase 3).
func (c *MCPRemoveCmd) Run(_ *Globals) error { return fmt.Errorf("mcp remove: %w", errNotImplemented) }

// SkillsCmd lists discovered skills.
type SkillsCmd struct{}

// Run lists skills (Phase 3).
func (c *SkillsCmd) Run(_ *Globals) error { return fmt.Errorf("skills: %w", errNotImplemented) }

// AuditCmd inspects or verifies audit logs.
type AuditCmd struct {
	ID     string `arg:"" optional:"" help:"Session ID (default: latest)."`
	Kind   string `help:"Only events of this kind."`
	JSON   bool   `help:"Print raw JSONL events."`
	Verify bool   `help:"Verify the hash chain instead of printing events."`
}

// Run prints or verifies an audit log (Phase 2).
func (c *AuditCmd) Run(_ *Globals) error { return fmt.Errorf("audit: %w", errNotImplemented) }

// InitCmd scaffolds AGENTS.md and project settings.
type InitCmd struct{}

// Run creates project files (Phase 2).
func (c *InitCmd) Run(_ *Globals) error { return fmt.Errorf("init: %w", errNotImplemented) }
