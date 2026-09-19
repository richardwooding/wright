package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/headless"
	"github.com/richardwooding/wright/internal/model"
)

// maxStdin bounds what a piped stdin contributes to the prompt.
const maxStdin = 1 << 20

// Run builds a session and runs it: headless when Print is set (or stdin is
// piped with no prompt), otherwise through interactive. It returns the
// process exit code and, for failures worth a message, the error.
func Run(ctx context.Context, o RunOptions, interactive Interactive) (int, error) {
	o = normalize(o)
	stdin, err := readStdin(o)
	if err != nil {
		return headless.ExitError, err
	}
	// A pipe with no prompt is a headless request even without -p.
	if !o.Print && o.Prompt == "" && stdin != "" {
		o.Print = true
	}
	if o.Print {
		return runHeadless(ctx, o, stdin)
	}
	if interactive == nil {
		return headless.ExitError, ErrNoInteractive
	}
	return runInteractive(ctx, o, interactive)
}

func normalize(o RunOptions) RunOptions {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Env == nil {
		o.Env = os.Getenv
	}
	return o
}

// readStdin drains a piped stdin (never a terminal) up to maxStdin.
func readStdin(o RunOptions) (string, error) {
	if o.Stdin == nil || o.IsTerminal {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(o.Stdin, maxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > maxStdin {
		data = append(data[:maxStdin], []byte("\n[stdin truncated at 1 MiB]")...)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// composePrompt is the headless prompt: the argument, with piped input
// fenced as <stdin> after it, or the input alone.
func composePrompt(promptText, stdin string) string {
	switch {
	case stdin == "":
		return promptText
	case promptText == "":
		return stdin
	default:
		return promptText + "\n\n<stdin>\n" + stdin + "\n</stdin>"
	}
}

func runHeadless(ctx context.Context, o RunOptions, stdin string) (int, error) {
	format, err := headless.ParseFormat(o.Output)
	if err != nil {
		return headless.ExitUsage, err
	}
	promptText := composePrompt(o.Prompt, stdin)
	if strings.TrimSpace(promptText) == "" {
		return headless.ExitUsage, errors.New("app: -p needs a prompt argument or piped stdin")
	}
	b, err := Build(ctx, o)
	if err != nil {
		return headless.ExitError, err
	}
	for _, w := range b.Warnings {
		fmt.Fprintln(o.Stderr, "wright: "+w)
	}
	code := headless.Run(ctx, b.Engine, promptText, nil, format, o.Stdout, o.Stderr, o.Verbose)
	closeErr := b.Close()
	if o.Verbose {
		fmt.Fprintln(o.Stderr, b.Summary())
	}
	if closeErr != nil {
		fmt.Fprintln(o.Stderr, "wright: close: "+closeErr.Error())
	}
	return code, nil
}

func runInteractive(ctx context.Context, o RunOptions, interactive Interactive) (int, error) {
	b, err := Build(ctx, o)
	if err != nil {
		return headless.ExitError, err
	}
	summary, uiErr := interactive(ctx, b.Engine, b.interactiveDeps())
	closeErr := b.Close()
	if summary != "" {
		fmt.Fprintln(o.Stdout, summary)
	}
	fmt.Fprintln(o.Stdout, b.Summary())
	switch {
	case uiErr != nil && errors.Is(uiErr, context.Canceled):
		return headless.ExitInterrupted, nil
	case uiErr != nil:
		return headless.ExitError, uiErr
	case closeErr != nil:
		return headless.ExitError, closeErr
	}
	return headless.ExitOK, nil
}

func (b *Built) interactiveDeps() InteractiveDeps {
	cwd := b.WS.Root()
	env := b.opts.Env
	return InteractiveDeps{
		Store:         b.Store,
		Git:           func(ctx context.Context) git.Summary { return git.Status(ctx, cwd) },
		Version:       b.opts.Version,
		WorkspaceRoot: cwd,
		Plain:         b.opts.Plain,
		InitialPrompt: b.opts.Prompt,
		Warnings:      b.Warnings,
		Models:        func(context.Context) []model.Choice { return model.List(env) },
		Command:       b.Command,
	}
}

// Summary renders the end-of-session digest from the audit log.
func (b *Built) Summary() string {
	s, err := audit.Summarize(audit.Read(b.Store.AuditPath(b.Engine.SessionID())))
	if err != nil {
		return "audit summary unavailable: " + err.Error()
	}
	return s.String()
}
