package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/diffview"
	"github.com/richardwooding/wright/internal/tui/markdown"
	"github.com/richardwooding/wright/internal/tui/toolview"
)

// Status is the state of a tool card.
type Status uint8

// Card states. Each renders as glyph + word.
const (
	StatusRunning Status = iota
	StatusOK
	StatusError
	StatusDenied
)

// Level is a notice's prominence.
type Level uint8

// Notice levels.
const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Limits that keep a single card from swallowing the screen.
const (
	outputTail = 40 // lines of tool output shown when expanded
	argsLines  = 20 // lines of pretty-printed arguments
	summaryMax = 60 // width of the one-line argument summary
)

// User is a message the user sent.
type User struct {
	Text string
	// Source names where the turn came from when it was not typed here —
	// the diagnostics endpoint. Empty for an ordinary message.
	Source string
}

// Assistant is one model turn: markdown text plus optional reasoning. Live is
// true while it is still streaming.
type Assistant struct {
	Text      string
	Reasoning string
	Live      bool
}

// ToolCard is one tool invocation, live or finished.
type ToolCard struct {
	ID       string
	Name     string
	Args     json.RawMessage
	Output   string   // result text (tail shown when expanded)
	Diff     string   // unified diff when the call is an edit
	Progress []string // streamed progress lines
	Expanded bool
	Status   Status
	Depth    int // subagent nesting; indented two spaces per level
	Started  time.Time
	Duration time.Duration
	Badges   []string // "injection", "redacted ×2" …
}

// Approval records a permission decision so the transcript shows what was
// decided, by whom and what rule (if any) was granted.
type Approval struct {
	Tool    string
	Summary string
	Allowed bool
	By      string
	// Rules are the rules the user chose to remember, each already spelled
	// "<rule> (<scope>)"; one prompt can accept several.
	Rules  []string
	Reason string
}

// Notice is an informational line: compaction, redaction, mode changes.
type Notice struct {
	Text  string
	Level Level
}

// Error is a run or engine error.
type Error struct{ Text string }

func (u *User) render(ctx renderContext) []string {
	prefix := ctx.th.UserPrompt.Render("› ")
	out := prefixed(prefix, "  ", wrap(u.Text, ctx.width-2))
	if u.Source == "" {
		return out
	}
	// Above the turn, not beside it: a reader scanning the conversation has
	// to see that this one was not typed here before they read it.
	return append([]string{ctx.th.Subtle.Render("  via " + u.Source)}, out...)
}

func (a *Assistant) render(ctx renderContext) []string {
	var out []string
	if a.Reasoning != "" {
		out = append(out, renderReasoning(ctx, a.Reasoning)...)
	}
	text := strings.TrimSpace(a.Text)
	switch {
	case text == "" && a.Live:
		out = append(out, ctx.th.Subtle.Render("…"))
	case text == "":
	case ctx.md == nil:
		out = append(out, strings.Split(markdown.Plain(text, ctx.width), "\n")...)
	default:
		out = append(out, strings.Split(ctx.md.Render(text, ctx.width, ctx.th.IsDark), "\n")...)
	}
	return out
}

// renderReasoning shows reasoning collapsed to one dim line unless the user
// asked for it with /reasoning.
func renderReasoning(ctx renderContext, reasoning string) []string {
	lines := wrap(strings.TrimSpace(reasoning), ctx.width-2)
	if !ctx.showReasoning {
		return []string{ctx.th.Subtle.Render(fmt.Sprintf("%s reasoning (%d lines, /reasoning to show)", theme.GlyphCollapsed, len(lines)))}
	}
	out := []string{ctx.th.Subtle.Render(theme.GlyphExpanded + " reasoning")}
	for _, l := range lines {
		out = append(out, ctx.th.Subtle.Render("  "+l))
	}
	return out
}

func (c *ToolCard) render(ctx renderContext) []string {
	indent := strings.Repeat("  ", c.Depth)
	width := ctx.width - len(indent)
	out := []string{indent + c.headline(ctx.th, width)}
	bar := ctx.th.Subtle.Render("│ ")
	body := c.body(ctx, width-2)
	if !c.Expanded {
		// A collapsed card is one line for almost everything. The exception
		// is a small edit, where a few rows of diff answer the question the
		// card exists to raise; toolview.Digest decides, and returns nothing
		// for every other tool.
		body = toolview.Digest(c.input(ctx, width-2), c.deps(ctx))
	}
	for _, l := range body {
		out = append(out, indent+bar+l)
	}
	return out
}

// input is everything toolview needs to decide how this card looks.
func (c *ToolCard) input(ctx renderContext, width int) toolview.Input {
	return toolview.Input{
		Plan:     toolview.For(c.Name, c.Args),
		Output:   c.Output,
		Diff:     c.Diff,
		Progress: c.Progress,
		Running:  c.Status == StatusRunning,
		Errored:  c.Status == StatusError || c.Status == StatusDenied,
		Width:    width,
	}
}

func (c *ToolCard) deps(ctx renderContext) toolview.Deps {
	return toolview.Deps{Theme: ctx.th, Highlighter: ctx.hl}
}

// headline is the collapsed one-liner: glyph, name, argument summary, diff
// stats, duration and the status word.
func (c *ToolCard) headline(th theme.Theme, width int) string {
	arrow := theme.GlyphCollapsed
	if c.Expanded {
		arrow = theme.GlyphExpanded
	}
	parts := []string{arrow + " " + th.Bold.Render(c.Name)}
	if s := toolview.Headline(c.Name, c.Args, c.Output, c.Status == StatusRunning); s != "" {
		parts = append(parts, s)
	}
	if c.Diff != "" {
		add, del := diffview.Stats(c.Diff)
		parts = append(parts, th.DiffAdd.Render(fmt.Sprintf("+%d", add))+" "+th.DiffDel.Render(fmt.Sprintf("−%d", del)))
	}
	if c.Duration > 0 {
		parts = append(parts, th.Subtle.Render(formatDuration(c.Duration)))
	}
	parts = append(parts, c.statusWord(th))
	for _, b := range c.Badges {
		parts = append(parts, th.Badge.Render(theme.GlyphWarn+" "+b))
	}
	return ansi.Truncate(strings.Join(parts, "  "), width, "…")
}

// statusWord is glyph + word, coloured but never colour-only.
func (c *ToolCard) statusWord(th theme.Theme) string {
	switch c.Status {
	case StatusOK:
		return th.Good.Render(theme.GlyphOK + " ok")
	case StatusError:
		return th.Hot.Render(theme.GlyphFail + " error")
	case StatusDenied:
		return th.Hot.Render(theme.GlyphFail + " denied")
	default:
		return th.Warm.Render(theme.GlyphRunning + " running")
	}
}

// body is the expanded content: arguments, then the diff or the output tail,
// then progress lines while running.
func (c *ToolCard) body(ctx renderContext, width int) []string {
	var out []string
	if args := prettyArgs(c.Args); len(args) > 0 {
		out = append(out, ctx.th.Subtle.Render("args:"))
		out = append(out, tail(args, argsLines)...)
	}
	return append(out, toolview.Full(c.input(ctx, width), c.deps(ctx))...)
}

// Summary is the most telling argument (path, command, pattern…) for the
// one-liner. /ps and the approval block call it too, and both want the
// request rather than the result, which is why it stays as it was.
func (c *ToolCard) Summary() string { return toolview.Summary(c.Args) }

func (a *Approval) render(ctx renderContext) []string {
	th := ctx.th
	var verdict string
	if a.Allowed {
		verdict = th.Good.Render(theme.GlyphOK + " allowed")
	} else {
		verdict = th.Hot.Render(theme.GlyphFail + " denied")
	}
	line := fmt.Sprintf("%s %s %s → %s", th.Warm.Render(theme.GlyphAsk+" approval"), th.Bold.Render(a.Tool), a.Summary, verdict)
	if a.By != "" {
		line += th.Subtle.Render(" by " + a.By)
	}
	if len(a.Rules) > 0 {
		word := " · rule "
		if len(a.Rules) > 1 {
			word = " · rules "
		}
		line += th.Subtle.Render(word + strings.Join(a.Rules, ", "))
	}
	if a.Reason != "" {
		line += th.Subtle.Render(": " + a.Reason)
	}
	return wrap(line, ctx.width)
}

func (n *Notice) render(ctx renderContext) []string {
	th := ctx.th
	var prefix string
	style := th.Subtle
	switch n.Level {
	case LevelWarn:
		prefix, style = theme.GlyphWarn+" ", th.Warm
	case LevelError:
		prefix, style = theme.GlyphStop+" ", th.Hot
	default:
		prefix = "ℹ "
	}
	lines := wrap(n.Text, ctx.width-2)
	for i, l := range lines {
		if i == 0 {
			lines[i] = style.Render(prefix + l)
		} else {
			lines[i] = style.Render("  " + l)
		}
	}
	return lines
}

func (e *Error) render(ctx renderContext) []string {
	lines := wrap(e.Text, ctx.width-2)
	for i, l := range lines {
		if i == 0 {
			lines[i] = ctx.th.Hot.Render(theme.GlyphFail + " " + l)
		} else {
			lines[i] = ctx.th.Hot.Render("  " + l)
		}
	}
	return lines
}

// prettyArgs indents JSON arguments; invalid JSON is shown raw.
func prettyArgs(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return strings.Split(string(raw), "\n")
	}
	return strings.Split(buf.String(), "\n")
}

// tail keeps at most n lines, noting how many were dropped.
func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return append(lines[:n:n], fmt.Sprintf("… %d more lines", len(lines)-n))
}

// wrap hard-wraps plain text at width, never returning an empty slice.
func wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	out := strings.Split(strings.TrimRight(ansi.Wrap(text, width, ""), "\n"), "\n")
	return out
}

// prefixed puts first before the first line and rest before the others.
func prefixed(first, rest string, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if i == 0 {
			out[i] = first + l
		} else {
			out[i] = rest + l
		}
	}
	return out
}

// clampWidth is the last guard: no rendered line may exceed width, whatever
// a renderer did with wide glyphs.
func clampWidth(lines []string, width int) []string {
	for i, l := range lines {
		if ansi.StringWidth(l) > width {
			lines[i] = ansi.Truncate(l, width, "…")
		}
	}
	return lines
}

// formatDuration renders 340ms / 1.2s / 2m03s for card headlines.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}
