package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"
	"mvdan.cc/sh/v3/syntax"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
)

const (
	// defaultBashTimeout and maxBashTimeout bound a command's wall time.
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 600 * time.Second
	// maxBashOutput is the size above which output is clipped and spilled.
	maxBashOutput = 30 * 1024
	// progressInterval coalesces streamed output for the UI.
	progressInterval = 100 * time.Millisecond
	// cwdMarker is printed by the script's epilogue so the tool can follow
	// `cd` across calls without a persistent shell.
	cwdMarker = "__WRIGHT_CWD="
	// waitDelay bounds how long Wait blocks on stragglers holding the pipes
	// after the process group was killed.
	waitDelay = 2 * time.Second
)

type bashArgs struct {
	Command     string `json:"command" jsonschema:"bash script to run"`
	Timeout     int    `json:"timeout,omitempty" jsonschema:"seconds before the command is killed (default 120, max 600)"`
	Description string `json:"description,omitempty" jsonschema:"one line saying what the command does, shown to the user"`
	Network     bool   `json:"network,omitempty" jsonschema:"request network access for this command (asks the user)"`
}

// networkKey carries an engine-granted network override.
type networkKey struct{}

// WithNetwork lets the engine grant (or refuse) sandbox network access for
// the bash call running under ctx, overriding the model's argument.
func WithNetwork(ctx context.Context, allow bool) context.Context {
	return context.WithValue(ctx, networkKey{}, allow)
}

// NetworkFrom reports an override set with WithNetwork.
func NetworkFrom(ctx context.Context) (allow, ok bool) {
	allow, ok = ctx.Value(networkKey{}).(bool)
	return allow, ok
}

func (d *Deps) bash() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameBash,
			"Run a bash script in the sandbox. stdout and stderr are merged; the working directory persists across calls; no network unless network is set and granted.",
			d.runBash),
		describe:   d.describeBash,
		sequential: true,
	}
}

func (d *Deps) describeBash(args json.RawMessage) (policy.Request, Preview, error) {
	var a bashArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return policy.Request{}, Preview{}, errors.New("command is required")
	}
	an := shellclass.Analyze(a.Command, policy.NewShellWorkspace(d.WS, nil, nil))
	req := policy.Request{Tool: NameBash, Args: args, Shell: &an, Network: a.Network}
	for _, c := range an.Commands {
		req.Writes = append(req.Writes, c.Writes...)
		req.Paths = append(req.Paths, c.Reads...)
	}
	title := a.Description
	if title == "" {
		title = NameBash
	}
	body := "$ " + a.Command + "\n" + an.Summary()
	return req, Preview{Title: title, Body: body}, nil
}

func (d *Deps) runBash(ctx context.Context, a bashArgs) (agentkit.Output, error) {
	if strings.TrimSpace(a.Command) == "" {
		return agentkit.Output{}, errors.New("command is required")
	}
	timeout := time.Duration(a.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	timeout = min(timeout, maxBashTimeout)
	script, notes := d.prepareScript(a.Command)
	spec := d.SandboxSpec
	spec.Argv = []string{"bash", "-lc", script + "\nprintf '\\n" + cwdMarker + "%s\\n' \"$PWD\""}
	spec.Dir = d.Cwd.Get()
	spec.Network = a.Network
	if allow, ok := NetworkFrom(ctx); ok {
		spec.Network = allow
	}
	spec.Timeout = timeout

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := d.Sandbox.Command(tctx, spec)
	if err != nil {
		return agentkit.Output{}, err
	}
	raw, dur, runErr := d.execute(ctx, cmd)
	if ctx.Err() != nil {
		return agentkit.Output{}, ctx.Err()
	}
	out, newCwd := splitCwd(raw)
	notes = append(notes, d.updateCwd(newCwd)...)
	timedOut := tctx.Err() != nil
	return d.bashResult(ctx, out, cmd, runErr, dur, timedOut, timeout, notes), nil
}

// execute wires the pipes, streams merged output to the UI and waits.
func (d *Deps) execute(ctx context.Context, cmd *exec.Cmd) (string, time.Duration, error) {
	setProcessGroup(cmd)
	cmd.WaitDelay = waitDelay
	var buf bytes.Buffer
	stream := newCoalescer(d.progressSink(ctx), progressInterval)
	cmd.Stdout = io.MultiWriter(&buf, stream)
	cmd.Stderr = cmd.Stdout
	start := time.Now()
	err := cmd.Run()
	stream.Close()
	return buf.String(), time.Since(start), err
}

// progressSink is the UI stream, redacted line by line.
func (d *Deps) progressSink(ctx context.Context) io.Writer {
	w := agentkit.ProgressWriter(ctx)
	if d.Redactor != nil {
		return d.Redactor.Writer(w)
	}
	return w
}

// prepareScript applies the attribution trailer to git commit commands.
func (d *Deps) prepareScript(script string) (string, []string) {
	if !d.Attribution || d.Trailer == "" {
		return script, nil
	}
	rewritten, ok := appendTrailer(script, d.Trailer)
	if !ok {
		return script, nil
	}
	return rewritten, []string{"appended the attribution trailer to the git commit message"}
}

// updateCwd follows the script's final directory when it stays inside the
// workspace; otherwise the next call starts where this one did.
func (d *Deps) updateCwd(newCwd string) []string {
	if newCwd == "" {
		return nil
	}
	abs, inside, err := d.WS.Resolve(newCwd)
	if err != nil || !inside {
		return []string{fmt.Sprintf("working directory %s is outside the workspace; staying in %s", newCwd, d.rel(d.Cwd.Get()))}
	}
	d.Cwd.Set(abs)
	return nil
}

// bashResult assembles the model-facing text: output, notes, status.
func (d *Deps) bashResult(ctx context.Context, out string, cmd *exec.Cmd, runErr error, dur time.Duration, timedOut bool, timeout time.Duration, notes []string) agentkit.Output {
	out, _ = Clip(out, maxBashOutput, d.SpillDir, spillID(ctx))
	out = d.redact(ctx, NameBash, out)
	var b strings.Builder
	b.WriteString(strings.TrimRight(out, "\n"))
	for _, n := range notes {
		b.WriteString("\n[note: " + n + "]")
	}
	code := exitCode(cmd, runErr)
	isErr := false
	switch {
	case timedOut:
		fmt.Fprintf(&b, "\n[timed out after %s; process group killed]", timeout)
		isErr = true
	case runErr != nil && cmd.ProcessState == nil:
		fmt.Fprintf(&b, "\n[failed to start: %v]", runErr)
		isErr = true
	}
	fmt.Fprintf(&b, "\n[exit code %d, %s]", code, dur.Round(time.Millisecond))
	return agentkit.Output{Content: agentkit.Text(strings.TrimLeft(b.String(), "\n")).Content, IsError: isErr}
}

// exitCode reports the process exit code, -1 when it did not run to completion.
func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// splitCwd strips the cwd marker line from the output and returns the
// directory it carried ("" when the epilogue never ran).
func splitCwd(out string) (string, string) {
	i := strings.LastIndex(out, "\n"+cwdMarker)
	if i < 0 {
		return out, ""
	}
	rest := out[i+1+len(cwdMarker):]
	dir, _, _ := strings.Cut(rest, "\n")
	return out[:i], dir
}

// coalescer batches writes and forwards them every interval so a chatty
// build does not become thousands of progress events.
type coalescer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	dst  io.Writer
	stop chan struct{}
	done chan struct{}
}

func newCoalescer(dst io.Writer, every time.Duration) *coalescer {
	c := &coalescer{dst: dst, stop: make(chan struct{}), done: make(chan struct{})}
	go c.loop(every)
	return c
}

func (c *coalescer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *coalescer) loop(every time.Duration) {
	defer close(c.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.flush()
		case <-c.stop:
			c.flush()
			return
		}
	}
}

func (c *coalescer) flush() {
	c.mu.Lock()
	chunk := c.buf.String()
	c.buf.Reset()
	c.mu.Unlock()
	if chunk != "" {
		_, _ = io.WriteString(c.dst, chunk)
	}
}

// Close flushes what is left and stops the ticker.
func (c *coalescer) Close() {
	close(c.stop)
	<-c.done
	if closer, ok := c.dst.(io.Closer); ok {
		_ = closer.Close()
	}
}

// appendTrailer rewrites every `git commit … -m <msg>` in script so that
// msg ends with trailer. It reports false when nothing changed (no commit,
// no -m, trailer already present, or the script does not parse).
func appendTrailer(script, trailer string) (string, bool) {
	if strings.ContainsRune(trailer, '\'') || strings.Contains(script, trailer) {
		return script, false
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return script, false
	}
	changed := false
	syntax.Walk(f, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok && rewriteCommit(call, trailer) {
			changed = true
		}
		return true
	})
	if !changed {
		return script, false
	}
	var b strings.Builder
	if err := syntax.NewPrinter().Print(&b, f); err != nil {
		return script, false
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// rewriteCommit appends the trailer to the message word of a git commit.
func rewriteCommit(call *syntax.CallExpr, trailer string) bool {
	words := call.Args
	if len(words) < 2 || literal(words[0]) != "git" {
		return false
	}
	i := 1
	for i < len(words) && strings.HasPrefix(literal(words[i]), "-") {
		i += 2 // git global options that take a value (-C dir, -c k=v); flags without a value are rare here
	}
	if i >= len(words) || literal(words[i]) != "commit" {
		return false
	}
	suffix := &syntax.SglQuoted{Value: "\n\n" + trailer}
	for j := i + 1; j < len(words); j++ {
		lit := literal(words[j])
		switch {
		case lit == "-m" || lit == "--message":
			if j+1 < len(words) {
				words[j+1].Parts = append(words[j+1].Parts, suffix)
				return true
			}
		case strings.HasPrefix(lit, "-m") && len(lit) > 2, strings.HasPrefix(lit, "--message="):
			words[j].Parts = append(words[j].Parts, suffix)
			return true
		}
	}
	return false
}

// literal returns the plain text of a word made only of literal parts, or
// "" when any part is dynamic.
func literal(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return ""
				}
				b.WriteString(l.Value)
			}
		default:
			return ""
		}
	}
	return b.String()
}
