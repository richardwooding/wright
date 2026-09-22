package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"
	"mvdan.cc/sh/v3/syntax"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/sandbox"
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

	// bashCwdNote states the consequence, not just the fact. Saying only
	// "the working directory persists" left the model prefixing almost every
	// command with `cd <workspace> &&` — 89% of calls in one real session, of
	// which 76 of 78 targeted the directory the shell was already in. This is
	// the same lesson as bashTimeoutNote: a bound the model is not told how to
	// use is one it re-implements.
	bashCwdNote = "It starts in the workspace root and stays wherever cd leaves it, so write the command plainly and use relative paths; cd only to move somewhere else."
	// waitDelay bounds how long Wait blocks on stragglers holding the pipes
	// after the process group was killed.
	waitDelay = 2 * time.Second
)

// bashTimeoutNote is what the model is told about the bound every call
// already has. It existed from the start — 120 s by default, 600 s at most,
// the whole process group killed — but only as a JSON-schema property
// description, so the agent hand-rolled `timeout 120 …` around its commands
// and the rule offered for such a call named the wrapper. It is built from
// the constants because a number stated in prose drifts from the one enforced
// in code, and it is used in both places the model reads: this tool's
// description and the Docs() line that reaches the system prompt.
var bashTimeoutNote = fmt.Sprintf(
	"Every call is bounded already: it is killed, with its whole process group, after %d seconds, or after the timeout argument (at most %d) — so write the command plainly rather than wrapping it in timeout yourself.",
	int(defaultBashTimeout.Seconds()), int(maxBashTimeout.Seconds()))

type bashArgs struct {
	Command     string `json:"command" jsonschema:"bash script to run"`
	Timeout     int    `json:"timeout,omitempty" jsonschema:"seconds before the command is killed (default 120, max 600)"`
	Description string `json:"description,omitempty" jsonschema:"one line saying what the command does, shown to the user"`
	Network     bool   `json:"network,omitempty" jsonschema:"request network access for this command (asks the user)"`
	Background  bool   `json:"background,omitempty" jsonschema:"start the command and return immediately; read its output with the job tool"`
}

func (d *Deps) bash() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameBash,
			"Run a bash script in the sandbox. stdout and stderr are merged; no network unless network is set and granted. "+
				bashCwdNote+" "+
				bashTimeoutNote+
				" Set background for a long-running command (a dev server, a watcher): it returns a job id at once and the job tool reads its output.",
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
	an := shellclass.Analyze(a.Command, d.shellWorkspace())
	req := policy.Request{Tool: NameBash, Args: args, Shell: &an, Network: a.Network, Cwd: d.Cwd.Get()}
	for _, c := range an.Commands {
		req.Writes = append(req.Writes, c.Writes...)
		req.Paths = append(req.Paths, c.Reads...)
	}
	title := a.Description
	if title == "" {
		title = NameBash
	}
	body := "$ " + a.Command + "\n" + an.Summary()
	if a.Background {
		// The approval covers the job's whole life, not one call, so the
		// prompt has to say that before the answer rather than after.
		body += "\nruns in the background until it exits, you kill it, or the session ends"
	}
	return req, Preview{Title: title, Body: body}, nil
}

// shellWorkspace is the analyzer's view of the workspace. policy's adapter
// resolves a relative word against the workspace *root*, but the command
// runs with spec.Dir set to the tracked working directory, which `cd` moves;
// resolving against the root would name a different file in the verdict and
// in the approval preview than the one the command opens. Wrapping here
// keeps the fix on the tools side, since NewShellWorkspace has no parameter
// for a working directory.
func (d *Deps) shellWorkspace() shellclass.Workspace {
	ws := policy.NewShellWorkspace(d.WS, nil, nil)
	if d.Cwd == nil {
		return ws
	}
	return cwdWorkspace{Workspace: ws, cwd: d.Cwd.Get()}
}

// cwdWorkspace resolves relative paths against cwd instead of the root.
type cwdWorkspace struct {
	shellclass.Workspace
	cwd string
}

// Resolve rebases a relative word onto the working directory before handing
// it to the workspace, which still expands, resolves symlinks and decides
// whether the result is inside.
func (c cwdWorkspace) Resolve(p string) (string, bool, error) {
	return c.Workspace.Resolve(c.rebase(p))
}

// rebase leaves absolute paths and the workspace's own prefixes ("~",
// "$WORKSPACE") alone; everything else is relative to the working directory.
func (c cwdWorkspace) rebase(p string) string {
	if c.cwd == "" || p == "" || filepath.IsAbs(p) ||
		strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$WORKSPACE") {
		return p
	}
	return filepath.Join(c.cwd, p)
}

// ProtectedBranches forwards the wrapped workspace's override, which an
// embedded interface would otherwise hide from shellclass.
func (c cwdWorkspace) ProtectedBranches() []string {
	if bp, ok := c.Workspace.(shellclass.BranchProtector); ok {
		return bp.ProtectedBranches()
	}
	return nil
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
	if a.Background {
		return d.startBackground(ctx, a, script, notes)
	}
	spec := d.bashSpec(ctx, a, script+"\nprintf '\\n"+cwdMarker+"%s\\n' \"$PWD\"")
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
	// Say plainly when the sandbox is what failed the command: without this
	// the model spends a dozen calls rediscovering it.
	backend := ""
	if d.Sandbox != nil {
		backend = d.Sandbox.Name()
	}
	notes = append(notes, sandboxHints(out, spec, backend)...)
	return d.bashResult(ctx, out, cmd, runErr, dur, timedOut, timeout, notes), nil
}

// bashSpec builds the sandbox spec for one script.
func (d *Deps) bashSpec(ctx context.Context, a bashArgs, script string) sandbox.Spec {
	spec := d.SandboxSpec
	// "-c", not "-lc": a login shell sources /etc/profile, /etc/profile.d/*
	// and (on the none backend, where $HOME is real) the user's
	// ~/.bash_profile, any of which can change PATH, define functions or
	// export variables that the sandbox's filtered environment deliberately
	// left out. The environment here is explicit and already carries PATH.
	spec.Argv = []string{"bash", "-c", script}
	spec.Dir = d.Cwd.Get()
	// a.Network is trustworthy only because the engine rewrites it to the
	// policy verdict before the call reaches here; spec.Network carries the
	// --allow-network flag.
	spec.Network = spec.Network || a.Network
	// What the user approved for this one call: the network the classifier
	// says the command needs, and — for an install — the tool prefixes it
	// writes, which the approval prompt named. Only the engine sets a grant;
	// the base spec is never widened.
	if g, ok := sandbox.GrantFrom(ctx); ok {
		spec.Network = spec.Network || g.Network
		spec.ReadWrite = withGranted(spec.ReadWrite, g.Writable)
	}
	// The GitHub credential, for a call that runs with the network and only
	// then. spec.Network is the one place all three sources of network have
	// been folded together, which is why the decision is made here.
	spec.Env = withEnv(spec.Env, d.gitHubEnvFor(spec.Network))
	return spec
}

// startBackground starts a job and returns at once.
//
// The job's context is deliberately *not* the call's: agentkit cancels that
// when the tool returns, which is immediately. It is derived with
// WithoutCancel so the job survives the call, and kept cancellable so
// Job.Kill and JobSet.Close can still stop it — a background job that
// nothing can stop would be worse than no background jobs.
//
// There is no default timeout, because waiting indefinitely is the point of
// a dev server or a watcher; an explicit timeout is still honoured, and the
// session's end is the backstop.
func (d *Deps) startBackground(ctx context.Context, a bashArgs, script string, notes []string) (agentkit.Output, error) {
	if d.Jobs == nil {
		return agentkit.Output{}, errors.New("background jobs are not available in this session")
	}
	// No cwd epilogue: a job that never exits has no final directory, and a
	// background command must not move the foreground shell's anyway.
	spec := d.bashSpec(ctx, a, script)
	jctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if a.Timeout > 0 {
		spec.Timeout = time.Duration(a.Timeout) * time.Second
		jctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), spec.Timeout)
	}
	cmd, err := d.Sandbox.Command(jctx, spec)
	if err != nil {
		cancel()
		return agentkit.Output{}, err
	}
	j := &Job{Command: a.Command, Description: a.Description, Started: time.Now(), cancel: cancel}
	if !d.Jobs.add(j) {
		cancel()
		return agentkit.Output{}, errors.New("this session is shutting down; no new background jobs")
	}
	setProcessGroup(cmd)
	cmd.WaitDelay = waitDelay
	cmd.Stdout = &j.out
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		j.finish(-1, err.Error())
		return agentkit.Output{}, fmt.Errorf("failed to start: %w", err)
	}
	go func() {
		defer cancel()
		runErr := cmd.Wait()
		failure := ""
		if jctx.Err() != nil {
			failure = "stopped: " + jctx.Err().Error()
		}
		j.finish(exitCode(cmd, runErr), failure)
	}()
	var b strings.Builder
	fmt.Fprintf(&b, "Started %s in the background: %s", j.ID, singleLine(a.Command))
	for _, n := range notes {
		b.WriteString("\n[note: " + n + "]")
	}
	fmt.Fprintf(&b, "\n[read its output with the %s tool: {\"action\":\"output\",\"id\":%q}]", NameJob, j.ID)
	return agentkit.Text(b.String()), nil
}

// withGranted returns base plus the granted directories, without touching
// base: Deps.SandboxSpec is shared by every call, so appending to its slice
// in place would leak one call's grant into the next.
func withGranted(base, granted []string) []string {
	if len(granted) == 0 {
		return base
	}
	out := slices.Clone(base)
	for _, p := range granted {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
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
	prev := d.Cwd.Get()
	d.Cwd.Set(abs)
	if abs == prev {
		return nil
	}
	// Confirm a move, because until now only a *refused* cd said anything and
	// a successful one was silent — which is what let the model believe the
	// directory had not stuck and re-establish it on the next call. The
	// environment block states the directory too, but it is rebuilt once per
	// run, so mid-run this note is the only ground truth. Nothing is emitted
	// for the overwhelmingly common no-op `cd <the directory we are in>`.
	//
	// Background jobs run no cwd epilogue (see startBackground), so they never
	// reach here and never move the foreground shell.
	return []string{"working directory is now " + d.cwdLabel(abs) + "; it persists into the next call"}
}

// cwdLabel names a directory as the model should see it: workspace-relative,
// and spelled out for the root, which workspace.Rel returns as ".".
func (d *Deps) cwdLabel(abs string) string {
	if rel := d.rel(abs); rel != "." && rel != "" {
		return rel
	}
	return "the workspace root"
}

// bashResult assembles the model-facing text: output, notes, status.
func (d *Deps) bashResult(ctx context.Context, out string, cmd *exec.Cmd, runErr error, dur time.Duration, timedOut bool, timeout time.Duration, notes []string) agentkit.Output {
	// Redact first: Clip writes the full text to the spill file, and an
	// unredacted secret on disk (and a path to it handed to the model) is
	// exactly what redaction is for. Clipping after also keeps the byte
	// count in the truncation note honest about what was saved.
	out = d.redact(ctx, NameBash, out)
	out, _ = Clip(out, maxBashOutput, d.SpillDir, spillID(ctx))
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
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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
