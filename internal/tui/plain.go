package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/policy"
)

// byUser marks decisions typed by the person at the terminal.
const byUser = "user"

// plainRunner is the --plain loop: events become lines, prompts become
// numbered lists and answers come from stdin one line at a time. No colour,
// no cursor movement, so it works through pipes and screen readers.
type plainRunner struct {
	ctx      context.Context
	ctl      Controller
	opts     Options
	out      io.Writer
	lines    <-chan string
	eof      bool
	running  bool
	midLine  bool // streamed text has no trailing newline yet
	approval *engine.Approval
	question *engine.QuestionEvent
	summary  string
}

// runPlain reads prompts from in (stdin when nil) and writes to out (stdout
// when nil). It returns when stdin closes and no run is active, or when ctx
// is cancelled.
func runPlain(ctx context.Context, ctl Controller, o Options, in io.Reader, out io.Writer) (string, error) {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	lines := make(chan string)
	go readLines(in, lines)
	r := &plainRunner{ctx: ctx, ctl: ctl, opts: o, out: out, lines: lines}
	for _, w := range o.Warnings {
		r.line("[warning] " + w)
	}
	if o.InitialPrompt != "" {
		r.submit(o.InitialPrompt)
	} else {
		r.prompt()
	}
	return r.loop()
}

// readLines feeds stdin lines to the loop and closes the channel at EOF.
func readLines(in io.Reader, lines chan<- string) {
	defer close(lines)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines <- sc.Text()
	}
}

func (r *plainRunner) loop() (string, error) {
	for {
		select {
		case <-r.ctx.Done():
			return r.summary, r.ctx.Err()
		case ev, ok := <-r.opts.Events:
			if !ok {
				return r.summary, nil
			}
			r.event(ev)
		case text, ok := <-r.lines:
			if !ok {
				r.lines = nil
				r.eof = true
				r.onEOF()
			} else {
				r.input(text)
			}
		}
		if r.eof && !r.running && r.approval == nil && r.question == nil {
			return r.summary, nil
		}
	}
}

// onEOF denies a pending prompt: nobody is there to approve.
func (r *plainRunner) onEOF() {
	if r.approval != nil {
		r.ctl.Reply(r.approval.ID, engine.Decision{Allow: false, Reason: "denied: no interactive user", By: byUser})
		r.approval = nil
	}
	if r.question != nil {
		r.ctl.Answer(r.question.ID, engine.Answer{Index: -1})
		r.question = nil
	}
}

// input routes a stdin line to the pending prompt or to the engine.
func (r *plainRunner) input(text string) {
	switch {
	case r.approval != nil:
		r.answerApproval(strings.TrimSpace(text))
	case r.question != nil:
		r.answerQuestion(strings.TrimSpace(text))
	default:
		r.submit(text)
	}
}

func (r *plainRunner) submit(text string) {
	text = strings.TrimSpace(text)
	switch text {
	case "":
		r.prompt()
		return
	case "/quit", "/exit":
		r.eof = true
		return
	}
	if err := r.ctl.Submit(text); err != nil {
		r.line("[error] " + err.Error())
		r.prompt()
		return
	}
	// Count the run as started now: the engine's RunStarted arrives
	// asynchronously, and an EOF in between must not end the loop.
	r.running = true
}

// answerApproval maps a typed choice to a Decision; anything unrecognised
// denies, because a typo must never allow.
func (r *plainRunner) answerApproval(text string) {
	a := r.approval
	r.approval = nil
	d := engine.Decision{Allow: false, Reason: "denied by user", By: byUser}
	switch text {
	case "1", "y":
		d = engine.Decision{Allow: true, By: byUser}
	default:
		// Several numbers are several rules from one answer, the plain-mode
		// counterpart of marking rows in the TUI. One bad number denies the
		// whole answer: a typo must never allow, and silently applying the
		// half that parsed would save a rule the user did not mean.
		if offers, ok := pickOffers(a.Offers, text); ok {
			d = engine.Decision{Allow: true, Grants: offers, By: byUser}
		}
	}
	r.ctl.Reply(a.ID, d)
}

func (r *plainRunner) answerQuestion(text string) {
	q := r.question
	r.question = nil
	if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= len(q.Options) {
		r.ctl.Answer(q.ID, engine.Answer{Text: q.Options[n-1], Index: n - 1})
		return
	}
	r.ctl.Answer(q.ID, engine.Answer{Text: text, Index: -1})
}

// event prints one engine event.
func (r *plainRunner) event(ev engine.Event) {
	switch ev.Kind {
	case engine.KindRunStarted:
		r.running = true
	case engine.KindExternalPrompt:
		r.line("[" + ev.Source + "] › " + ev.Text)
	case engine.KindText:
		fmt.Fprint(r.out, ev.Text)
		r.midLine = !strings.HasSuffix(ev.Text, "\n")
	case engine.KindToolCall:
		if ev.Call != nil {
			r.line(indent(ev.Depth) + "[tool] " + ev.Call.Name + ": " + callSummary(ev))
		}
	case engine.KindToolResult:
		r.toolResult(ev)
	case engine.KindApprovalRequest:
		r.askApproval(ev)
	case engine.KindQuestion:
		r.askQuestion(ev)
	case engine.KindRunFinished:
		r.finished(ev)
	default:
		r.other(ev)
	}
}

func (r *plainRunner) toolResult(ev engine.Event) {
	if ev.Call == nil {
		return
	}
	status := "ok"
	switch {
	case ev.Result != nil && ev.Result.IsError && strings.HasPrefix(ev.Result.Text(), "not approved"):
		status = "denied"
	case ev.Err != nil:
		status = "error: " + ev.Err.Error()
	case ev.Result != nil && ev.Result.IsError:
		status = "error: " + firstLine(ev.Result.Text())
	}
	r.line(fmt.Sprintf("%s[tool] %s → %s (%s)", indent(ev.Depth), ev.Call.Name, status, ev.Duration.Round(1e8)))
}

func (r *plainRunner) askApproval(ev engine.Event) {
	a := ev.Approval
	if a == nil {
		return
	}
	r.approval = a
	r.line(fmt.Sprintf("? approval (%s) %s: %s", a.Severity, a.Tool, a.Preview.Title))
	for l := range strings.SplitSeq(strings.TrimRight(previewText(a), "\n"), "\n") {
		if l != "" {
			r.line("    " + l)
		}
	}
	for _, l := range grantLines(a.Grants) {
		r.line("    " + l)
	}
	// Unnumbered, and before the numbered block, so the offer numbers stay
	// contiguous: askApproval renders them i+2 and pickOffers parses n-2, two
	// offsets that must keep agreeing. Anything numbered inserted here would
	// silently shift which rule an answer saves.
	if u := a.Verdict.Uncovered; u != "" {
		r.line("    your allow rules do not cover " + u)
	}
	for _, h := range a.Verdict.Held {
		if h.Rule.String() == h.By.String() {
			r.line("    already allowed, not offered: " + h.Rule.String())
			continue
		}
		r.line("    already allowed, not offered: " + h.Rule.String() + " (covered by " + h.By.String() + ")")
	}
	r.line("  1) allow once")
	for i, o := range a.Offers {
		r.line(fmt.Sprintf("  %d) allow and remember: %s  [%s]", i+2, o.Rule.String(), o.Scope))
	}
	r.line("  n) deny (default)")
	if len(a.Offers) > 1 {
		r.line("  (several numbers, comma-separated, remember several rules)")
	}
	fmt.Fprint(r.out, "> ")
	r.midLine = true
	if r.eof {
		r.onEOF() // stdin already closed: nobody can answer
	}
}

// pickOffers maps a typed answer — one number, or several separated by
// commas — to the offers it names. It reports false unless every field is a
// valid offer number, so a typo denies rather than partly allowing.
func pickOffers(offers []policy.GrantOffer, text string) ([]policy.GrantOffer, bool) {
	fields := strings.Split(text, ",")
	out := make([]policy.GrantOffer, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 2 || n-2 >= len(offers) {
			return nil, false
		}
		out = append(out, offers[n-2])
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// grantLines say what allowing hands over. Allowing a command the classifier
// says needs the network grants it, so the prompt has to name that (and any
// paths it will make writable) before the answer, not after the failure.
func grantLines(g engine.CallGrant) []string {
	var out []string
	if g.Network {
		out = append(out, "allowing runs this call with network access")
	}
	if len(g.Writable) > 0 {
		out = append(out, "allowing also makes these writable for this call only: "+strings.Join(g.Writable, ", "))
	}
	return out
}

func (r *plainRunner) askQuestion(ev engine.Event) {
	q := ev.Question
	if q == nil {
		return
	}
	r.question = q
	r.line("? " + q.Text)
	for i, o := range q.Options {
		r.line(fmt.Sprintf("  %d) %s", i+1, o))
	}
	fmt.Fprint(r.out, "> ")
	r.midLine = true
	if r.eof {
		r.onEOF()
	}
}

func (r *plainRunner) finished(ev engine.Event) {
	r.running = false
	if f := ev.Finish; f != nil {
		if f.Err != nil {
			r.line("[error] " + f.Err.Error())
		}
		r.summary = fmt.Sprintf("steps %d · tool calls %d · %s tok · %s", f.Steps, f.ToolCalls, formatTokens(f.Usage.TotalTokens), f.Duration.Round(1e8))
		r.line("[done] " + r.summary)
	}
	if !r.eof {
		r.prompt()
	}
}

func (r *plainRunner) other(ev engine.Event) {
	switch ev.Kind {
	case engine.KindError:
		r.line("[error] " + errText(ev))
	case engine.KindNotice:
		r.line("[notice] " + ev.Text)
	case engine.KindRetry:
		r.line(fmt.Sprintf("[retry] attempt %d in %s: %v", ev.Attempt, ev.Delay, ev.Err))
	case engine.KindCompact:
		if c := ev.Compact; c != nil {
			r.line(fmt.Sprintf("[compact] %s: %s → %s tokens", c.Reason, formatTokens(c.Before), formatTokens(c.After)))
		}
	case engine.KindRedacted:
		r.line("[redacted] " + ev.Text)
	case engine.KindInjection:
		r.line("[injection?] " + ev.Text)
	case engine.KindApprovalDecided:
		if d := ev.Decision; d != nil && d.By != "user" {
			r.line(fmt.Sprintf("[approval] %s by %s", allowWord(d.Allow), d.By))
		}
	case engine.KindQueued:
		r.line(fmt.Sprintf("[queued] %d message(s) waiting", ev.Queued))
	default:
	}
}

// line writes one full line, finishing any streamed text first.
func (r *plainRunner) line(s string) {
	if r.midLine {
		fmt.Fprintln(r.out)
		r.midLine = false
	}
	fmt.Fprintln(r.out, s)
}

func (r *plainRunner) prompt() {
	if r.eof {
		return
	}
	if r.midLine {
		fmt.Fprintln(r.out)
		r.midLine = false
	}
	fmt.Fprint(r.out, "wright> ")
	r.midLine = true
}

func indent(depth int) string { return strings.Repeat("  ", depth) }

func callSummary(ev engine.Event) string {
	var m map[string]any
	if err := ev.Call.UnmarshalArgs(&m); err == nil {
		for _, k := range []string{"path", "file", "file_path", "command", "cmd", "pattern", "query", "url"} {
			if v, ok := m[k].(string); ok && v != "" {
				return firstLine(v)
			}
		}
	}
	return firstLine(string(ev.Call.Arguments))
}

func previewText(a *engine.Approval) string {
	switch {
	case a.Preview.Diff != "":
		return a.Preview.Diff
	case a.Preview.Body != "":
		return a.Preview.Body
	case a.Request.Shell != nil:
		return a.Request.Shell.Raw
	}
	return ""
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before + " …"
	}
	return s
}

func allowWord(allow bool) string {
	if allow {
		return "allowed"
	}
	return "denied"
}
