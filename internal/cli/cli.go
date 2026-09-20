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
	"strconv"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/charmbracelet/x/term"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/headless"
)

// Exit codes shared by the interactive and headless paths. They are part of
// the headless contract (scripts branch on them), so they never change meaning.
const (
	ExitOK          = headless.ExitOK
	ExitFailure     = headless.ExitError // provider or tool error
	ExitUsage       = headless.ExitUsage
	ExitApproval    = headless.ExitApprovalRequired // headless run needed an approval that was not granted
	ExitBudget      = headless.ExitBudget
	ExitInterrupted = headless.ExitInterrupted
	// ExitTrustDeclined is returned when the user answered "no" to the
	// startup workspace-trust question: nothing ran, and that is not a
	// failure, so a wrapper script can tell it apart from one.
	ExitTrustDeclined = app.ExitTrustDeclined
)

// ExitError carries a specific process exit code out of a command. Err is
// optional: a headless run that ends with exit 3 has already reported itself
// and needs no extra message from main.
type ExitError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Unwrap exposes the wrapped error to errors.Is/As.
func (e *ExitError) Unwrap() error { return e.Err }

// BuildInfo carries the ldflags-injected version metadata from package main.
type BuildInfo struct {
	Version, Commit, Date string
}

// String renders the version line printed by --version.
func (b BuildInfo) String() string {
	return fmt.Sprintf("wright %s (%s, %s)", b.Version, b.Commit, b.Date)
}

// IO bundles the process streams so tests can substitute them.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CLI is the single kong root struct. Flags here are global; each subcommand
// carries its own arguments.
type CLI struct {
	Print                  bool             `short:"p" name:"print" help:"Headless mode: run the prompt, print the result and exit."`
	Output                 string           `enum:"text,json,stream-json" default:"text" help:"Headless output format (${enum})."`
	Model                  string           `short:"m" env:"WRIGHT_MODEL" help:"Model to use (provider auto-detected from env keys when omitted)."`
	Mode                   string           `env:"WRIGHT_MODE" help:"Permission mode: default, plan or auto-edit (bypass only via --bypass-permissions)."`
	BypassPermissions      bool             `help:"Skip approval prompts (hard-deny set still applies; refused without a sandbox)."`
	Trust                  bool             `help:"Trust this directory without being asked at startup: edits to files in it do not prompt (shell commands still do). Ignored with -p."`
	AllowUnsandboxedBypass bool             `help:"Permit --bypass-permissions even when no OS sandbox is available."`
	Resume                 string           `short:"r" help:"Resume the session with this ID."`
	Continue               bool             `short:"c" help:"Resume the most recent session for this workspace."`
	Cwd                    string           `type:"existingdir" help:"Working directory (defaults to the current directory)."`
	AddDir                 []string         `name:"add-dir" sep:"none" help:"Additional directory the agent may access (repeatable)."`
	Allow                  []string         `sep:"none" help:"Permission rule to allow for this run, e.g. 'bash(go test *)' (repeatable)."`
	Deny                   []string         `sep:"none" help:"Permission rule to deny for this run (repeatable)."`
	Sandbox                string           `enum:"auto,bwrap,landlock,seatbelt,none" default:"auto" env:"WRIGHT_SANDBOX" help:"OS sandbox backend (${enum})."`
	AllowNetwork           bool             `help:"Let sandboxed shell commands reach the network without asking."`
	MaxSteps               int              `help:"Maximum agent steps per run (0 = default budget)."`
	Reasoning              string           `help:"Reasoning effort hint passed to the model (low, medium, high)."`
	Plain                  bool             `env:"WRIGHT_PLAIN" help:"Plain output: no alternate screen, numbered prompts (implied by NO_COLOR, TERM=dumb or a non-TTY)."`
	StrictInjection        bool             `help:"Treat prompt-injection signals in web/MCP content as needing approval."`
	GitHubAuth             bool             `name:"github-auth" help:"Let shell commands that run with network access authenticate to GitHub as you: a token from gh, and git's credential helper pointed at it. Off by default; flag only, with no environment variable."`
	DebugAddr              string           `name:"debug-addr" placeholder:"127.0.0.1:6060" help:"Serve diagnostics and pprof on this loopback address (off by default). Flag only: no environment variable and no settings key."`
	Verbose                bool             `short:"v" help:"Verbose diagnostics on stderr."`
	Version                kong.VersionFlag `short:"V" help:"Print version and exit."`

	Run           RunCmd      `cmd:"" default:"withargs" help:"Start an interactive session (default command)."`
	Sessions      SessionsCmd `cmd:"" help:"List, show, export or delete saved sessions."`
	Models        ModelsCmd   `cmd:"" help:"List models whose provider has credentials."`
	Config        ConfigCmd   `cmd:"" help:"Show effective settings or the paths they are read from."`
	MCP           MCPCmd      `cmd:"" name:"mcp" help:"Manage MCP servers."`
	Skills        SkillsCmd   `cmd:"" help:"List discovered skills."`
	Audit         AuditCmd    `cmd:"" help:"Inspect or verify the audit log."`
	Trusted       TrustCmd    `cmd:"" name:"trust" help:"List, accept or forget trusted workspaces."`
	Init          InitCmd     `cmd:"" help:"Create AGENTS.md and .wright/settings.json for this project."`
	Doctor        DoctorCmd   `cmd:"" help:"Check sandbox backends, tools and credentials."`
	SandboxHelper SandboxCmd  `cmd:"" name:"__sandbox" hidden:"" help:"Internal landlock helper."`
}

// Globals is bound into every Run method: the parsed root, build info, the
// cancellable context from main, the streams (swappable in tests) and the
// interactive UI hook (nil when no UI is linked in).
type Globals struct {
	CLI         *CLI
	Build       BuildInfo
	Ctx         context.Context
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Interactive app.Interactive
}

// exitPanic is the sentinel kong.Exit raises so --version and --help unwind
// through Main instead of calling os.Exit from inside the parser.
type exitPanic struct{ code int }

// Main parses args, runs the selected command and maps the result to an exit
// code. It is the whole of package main's logic so tests can drive it.
// interactive is the TUI entry point; nil means only headless runs work.
func Main(ctx context.Context, args []string, build BuildInfo, interactive app.Interactive) (code int, err error) {
	return Run(ctx, args, build, IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, interactive)
}

// Run is Main with explicit streams, for tests.
func Run(ctx context.Context, args []string, build BuildInfo, streams IO, interactive app.Interactive) (code int, err error) {
	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("wright"),
		kong.Description("A coding-agent terminal harness: approval-first permissions, an OS sandbox around every command, secrets that stay on your machine, and an audit trail you can verify."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		kong.Vars{"version": build.String()},
		kong.Writers(streams.Stdout, streams.Stderr),
		kong.Exit(func(c int) { panic(exitPanic{code: c}) }),
	)
	if err != nil {
		return ExitFailure, err
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
		fmt.Fprintln(streams.Stderr, "Run 'wright --help' for usage.")
		return ExitUsage, nil
	}
	g := &Globals{CLI: cli, Build: build, Ctx: ctx, Stdin: streams.Stdin, Stdout: streams.Stdout, Stderr: streams.Stderr, Interactive: interactive}
	if err := kctx.Run(g); err != nil {
		return exitCodeFor(err), reportable(err)
	}
	return ExitOK, nil
}

// exitCodeFor maps command errors onto the documented exit codes.
func exitCodeFor(err error) int {
	var ee *ExitError
	switch {
	case errors.As(err, &ee):
		return ee.Code
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	default:
		return ExitFailure
	}
}

// reportable strips an ExitError that carries no message, so main prints
// nothing for a run that already reported itself.
func reportable(err error) error {
	var ee *ExitError
	if errors.As(err, &ee) && ee.Err == nil {
		return nil
	}
	return err
}

// RunCmd is the default command: an interactive (or, with -p, headless) session.
type RunCmd struct {
	Prompt []string `arg:"" optional:"" help:"Initial prompt. With -p it is the whole request."`
}

// Run starts a session through the composition root.
func (c *RunCmd) Run(g *Globals) error {
	f := g.CLI
	stdoutTTY := isTerminal(g.Stdout)
	o := app.RunOptions{
		Prompt:                 strings.Join(c.Prompt, " "),
		Print:                  f.Print,
		Output:                 f.Output,
		Model:                  f.Model,
		Mode:                   f.Mode,
		Bypass:                 f.BypassPermissions,
		TrustWorkspace:         f.Trust,
		AllowUnsandboxedBypass: f.AllowUnsandboxedBypass,
		Resume:                 f.Resume,
		Continue:               f.Continue,
		Cwd:                    f.Cwd,
		AddDirs:                f.AddDir,
		Allow:                  f.Allow,
		Deny:                   f.Deny,
		Sandbox:                f.Sandbox,
		AllowNetwork:           f.AllowNetwork,
		MaxSteps:               f.MaxSteps,
		Reasoning:              f.Reasoning,
		Plain:                  f.Plain || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || !stdoutTTY,
		Verbose:                f.Verbose,
		StrictInjection:        f.StrictInjection,
		DebugAddr:              f.DebugAddr,
		GitHubAuth:             f.GitHubAuth,
		Version:                g.Build.Version,
		Stdin:                  g.Stdin,
		Stdout:                 g.Stdout,
		Stderr:                 g.Stderr,
		Env:                    os.Getenv,
		IsTerminal:             stdoutTTY && isTerminal(g.Stdin),
		Confirm:                confirm(g),
		Select:                 selectOne(g),
	}
	code, err := app.Run(g.Ctx, o, g.Interactive)
	if code != ExitOK || err != nil {
		return &ExitError{Code: code, Err: err}
	}
	return nil
}

// confirm asks a yes/no question on the terminal before the UI starts; it
// is only offered when both streams are a terminal.
func confirm(g *Globals) func(string) bool {
	if !isTerminal(g.Stdin) || !isTerminal(g.Stdout) {
		return nil
	}
	return func(question string) bool {
		fmt.Fprintf(g.Stdout, "%s [y/N] ", question)
		var answer string
		if _, err := fmt.Fscanln(g.Stdin, &answer); err != nil {
			return false
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes"
	}
}

// selectOne asks the user to pick one of several answers on the terminal
// before the UI starts. Like confirm it is offered only on a terminal, and
// anything that is not one of the options — an empty line, EOF, a typo — is
// not a choice, so the caller applies its own default rather than this
// function guessing one.
func selectOne(g *Globals) func(string, []string) (int, bool) {
	if !isTerminal(g.Stdin) || !isTerminal(g.Stdout) {
		return nil
	}
	return func(question string, options []string) (int, bool) {
		fmt.Fprintln(g.Stdout, question)
		for i, o := range options {
			fmt.Fprintf(g.Stdout, "  %d) %s\n", i+1, o)
		}
		fmt.Fprintf(g.Stdout, "Choose 1-%d: ", len(options))
		var answer string
		if _, err := fmt.Fscanln(g.Stdin, &answer); err != nil {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || n < 1 || n > len(options) {
			return 0, false
		}
		return n - 1, true
	}
}

// isTerminal reports whether a stream is a terminal; anything that is not an
// *os.File (a test buffer, a pipe wrapper) is not.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// ModelsCmd lists usable models.
type ModelsCmd struct{}

// ConfigCmd groups the settings subcommands.
type ConfigCmd struct {
	Show  ConfigShowCmd  `cmd:"" default:"1" help:"Print the effective merged settings as JSON."`
	Paths ConfigPathsCmd `cmd:"" help:"Print where settings, data and cache live."`
}

// MCPCmd groups MCP server management.
type MCPCmd struct {
	List   MCPListCmd   `cmd:"" default:"1" help:"List configured MCP servers."`
	Add    MCPAddCmd    `cmd:"" help:"Add an MCP server to project settings."`
	Remove MCPRemoveCmd `cmd:"" help:"Remove an MCP server."`
}

// MCPListCmd lists MCP servers.
type MCPListCmd struct{}

// MCPAddCmd adds an MCP server to .wright/settings.json. Env names a
// variable to pass through; its value is read from wright's environment when
// the server starts, so nothing secret is written to the file.
type MCPAddCmd struct {
	Name    string   `arg:"" help:"Server name (tools appear as mcp_<name>_<tool>)."`
	Type    string   `enum:"stdio,http," default:"" help:"Transport: stdio or http (inferred from --command/--url when omitted)."`
	Command string   `help:"Program to run for a stdio server."`
	Arg     []string `help:"Argument for the stdio command (repeatable; use --arg=--flag for arguments that start with a dash)."`
	URL     string   `name:"url" help:"Endpoint for an http server."`
	Env     []string `help:"Environment variable NAME to pass to a stdio server (repeatable; names only, never values)."`
	Network bool     `help:"Let the stdio server reach the network from inside the sandbox."`
}

// MCPRemoveCmd removes an MCP server.
type MCPRemoveCmd struct {
	Name string `arg:"" help:"Server name."`
}

// SkillsCmd lists discovered skills.
type SkillsCmd struct{}

// InitCmd scaffolds AGENTS.md and project settings.
type InitCmd struct{}

// Run creates the project files that are missing and reports what it wrote.
func (c *InitCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	written, err := app.InitProject(cwd)
	if err != nil {
		return err
	}
	if len(written) == 0 {
		fmt.Fprintln(g.Stdout, "nothing to do: AGENTS.md and .wright/settings.json already exist")
		return nil
	}
	for _, w := range written {
		fmt.Fprintln(g.Stdout, "created "+w)
	}
	return nil
}
