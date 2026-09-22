package toolview

import (
	"fmt"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/diffview"
	"github.com/richardwooding/wright/internal/tui/highlight"
)

// Input is everything a card body is rendered from.
type Input struct {
	Plan     Plan
	Output   string // the result text, already unwrapped
	Diff     string
	Progress []string
	// Written is the content a write_file call is putting on disk, shown
	// when there is no diff to show instead.
	Written string
	Running bool
	Errored bool
	Width   int
}

// Deps are the renderers the transcript owns and lends.
type Deps struct {
	Theme       theme.Theme
	Highlighter *highlight.Renderer
}

// Digest is what a *collapsed* card shows beneath its headline: nothing at
// all for most tools, and a short diff for a small edit — the one case where
// a few lines answer the question "what did it just change?" that otherwise
// takes a keystroke and a scroll.
func Digest(in Input, d Deps) []string {
	switch {
	case in.Errored:
		// A refusal or a failure the user has to expand to read is the worst
		// the old card did. Say it where they are already looking.
		return errorLines(in, d, 6)
	case in.Diff != "" && in.Plan.Diffable:
		return shortDiff(in, d)
	}
	return nil
}

// Full is the expanded body: the diff or the output tail, then any progress.
func Full(in Input, d Deps) []string {
	var out []string
	switch {
	case in.Diff != "":
		out = append(out, diffview.Lines(in.Diff, d.Theme, in.Width)...)
	case in.Written != "":
		// A new file: show what was written, which the result line only
		// counts. The output line itself is redundant beside it — the
		// headline already carries "Wrote N bytes (M lines) to path".
		out = append(out, writtenLines(in, d)...)
	case in.Output != "":
		out = append(out, outputLines(in, d)...)
	}
	if in.Running {
		for _, p := range in.Progress {
			out = append(out, d.Theme.Subtle.Render(p))
		}
	}
	return out
}

// shortDiff shows a small diff inline, and a count for a large one. The
// threshold is the whole of the "only if appropriate" rule for edits: a
// three-line change is worth four rows, a two-hundred-line rewrite is not.
func shortDiff(in Input, d Deps) []string {
	add, del := diffview.Stats(in.Diff)
	if add+del > DiffInline {
		return []string{d.Theme.Subtle.Render(fmt.Sprintf("%d changed lines · ctrl+o for the diff", add+del))}
	}
	var out []string
	for _, l := range diffview.Lines(in.Diff, d.Theme, in.Width) {
		// The headline already names the file and carries +N −M, so the
		// file headers are four rows that say nothing new here.
		if isDiffHeader(l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// diffHeaderPrefixes are the lines a collapsed diff drops. Matched on the
// text with its styling stripped, since diffview has already coloured it.
var diffHeaderPrefixes = []string{"diff ", "index ", "--- ", "+++ ", "new file", "deleted file", "similarity index", "rename ", "old mode", "new mode"}

func isDiffHeader(styled string) bool {
	plain := ansi.Strip(styled)
	for _, p := range diffHeaderPrefixes {
		if strings.HasPrefix(plain, p) {
			return true
		}
	}
	return false
}

// outputLines is the tail of a tool's output, with wright's own trailers
// separated out and the content highlighted when the request named a
// language.
func outputLines(in Input, d Deps) []string {
	body, trailers := splitTrailers(in.Output, in.Plan.Trailers)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	var out []string
	if len(lines) > OutputTail {
		out = append(out, d.Theme.Subtle.Render(fmt.Sprintf("… %d earlier lines", len(lines)-OutputTail)))
		lines = lines[len(lines)-OutputTail:]
	}
	out = append(out, content(lines, in, d)...)
	for _, t := range trailers {
		out = append(out, trailerStyle(t, d.Theme).Render(t))
	}
	return out
}

// writtenLines renders the content of a newly written file: the same tail
// window and the same highlighting an ordinary read would get, so a file the
// agent just created reads exactly like one it just showed you.
func writtenLines(in Input, d Deps) []string {
	lines := strings.Split(strings.TrimRight(in.Written, "\n"), "\n")
	var out []string
	if len(lines) > OutputTail {
		out = append(out, d.Theme.Subtle.Render(fmt.Sprintf("… %d earlier lines", len(lines)-OutputTail)))
		lines = lines[len(lines)-OutputTail:]
	}
	if in.Running || d.Highlighter == nil {
		return append(out, lines...)
	}
	styled, ok := d.Highlighter.Lines(strings.Join(lines, "\n"), in.Plan.Lang, d.Theme)
	if !ok {
		return append(out, lines...)
	}
	return append(out, styled...)
}

// content applies the gutter and the highlighting to the lines that will be
// shown — and only those: tokenising scales with the input, and a card never
// shows more than OutputTail lines of it.
func content(lines []string, in Input, d Deps) []string {
	// A running card is never highlighted. Its body re-renders on every
	// progress event, and half an output is the least likely to lex cleanly.
	if in.Running || d.Highlighter == nil {
		return applyGutter(lines, nil, in, d)
	}
	// Only when the result text is the file's content. A write_file result
	// is wright's own sentence and must never be painted as source.
	if !in.Plan.OutputIsSource {
		return applyGutter(lines, nil, in, d)
	}
	text, prefixes := splitGutter(lines, in.Plan.Gutter)
	styled, ok := d.Highlighter.Lines(strings.Join(text, "\n"), in.Plan.Lang, d.Theme)
	if !ok {
		styled = nil
	}
	return joinGutter(prefixes, text, styled, d)
}

func applyGutter(lines []string, _ []string, in Input, d Deps) []string {
	text, prefixes := splitGutter(lines, in.Plan.Gutter)
	return joinGutter(prefixes, text, nil, d)
}

// splitGutter separates the prefix a tool wrote itself from the text after
// it, so the prefix can be made subtle and only the text is lexed. read_file
// numbers its own lines and grep prefixes its own paths; adding a second
// gutter beside either is the mistake this exists to avoid.
func splitGutter(lines []string, g Gutter) (text, prefixes []string) {
	text = make([]string, len(lines))
	prefixes = make([]string, len(lines))
	for i, l := range lines {
		switch g {
		case GutterNumbered:
			if n, rest, ok := strings.Cut(l, "\t"); ok && isNumeric(n) {
				prefixes[i], text[i] = n+"\t", rest
				continue
			}
		case GutterPathLine:
			if p, rest, ok := cutGrepPrefix(l); ok {
				prefixes[i], text[i] = p, rest
				continue
			}
		case GutterNone:
		}
		text[i] = l
	}
	return text, prefixes
}

func joinGutter(prefixes, text, styled []string, d Deps) []string {
	out := make([]string, len(text))
	for i := range text {
		body := text[i]
		if styled != nil && i < len(styled) {
			body = styled[i]
		}
		if prefixes[i] != "" {
			// NoTabConversion: read_file's gutter is "%6d\t" and lipgloss
			// would turn that tab into spaces, so a highlighted card would
			// disagree with the plain one about where the text starts.
			body = d.Theme.Subtle.TabWidth(lipgloss.NoTabConversion).Render(prefixes[i]) + body
		}
		out[i] = body
	}
	return out
}

// grepPrefix matches grep's "path:line:" and its "path-line-" context form.
var grepPrefix = regexp.MustCompile(`^([^\s:]+[:-]\d+[:-])`)

func cutGrepPrefix(l string) (prefix, rest string, ok bool) {
	m := grepPrefix.FindString(l)
	if m == "" {
		return "", "", false
	}
	return m, l[len(m):], true
}

func isNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// trailerLine matches the lines wright appends to a bash result: its own
// words about the command, not the command's output.
var trailerLine = regexp.MustCompile(`^\[(exit code |note: )`)

// splitTrailers lifts wright's own trailers off the end of an output, so a
// nine-hundred-line result cannot scroll the exit code out of view and so the
// harness's words are never coloured as if the program had said them.
func splitTrailers(out string, want bool) (body string, trailers []string) {
	if !want {
		return out, nil
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(lines) > 0 {
		last := lines[len(lines)-1]
		if !trailerLine.MatchString(last) {
			break
		}
		trailers = append([]string{last}, trailers...)
		lines = lines[:len(lines)-1]
	}
	if len(trailers) == 0 {
		return out, nil
	}
	return strings.Join(lines, "\n"), trailers
}

// trailerStyle makes a non-zero exit stand out. The text says the code either
// way: colour is never the only signal.
func trailerStyle(t string, th theme.Theme) lipgloss.Style {
	if strings.HasPrefix(t, "[exit code 0") || strings.HasPrefix(t, "[note: ") {
		return th.Subtle
	}
	if strings.HasPrefix(t, "[exit code ") {
		return th.Hot
	}
	return th.Subtle
}

// errorLines are the first few lines of a failure or a denial, shown without
// the user having to expand anything. A refusal or a build error that has to
// be expanded to be read is the worst thing the card did.
func errorLines(in Input, d Deps, n int) []string {
	body, trailers := splitTrailers(in.Output, in.Plan.Trailers)
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")
	truncated := len(lines) > n
	if truncated {
		lines = lines[:n]
	}
	out := make([]string, 0, len(lines)+1)
	for _, l := range lines {
		out = append(out, d.Theme.Hot.Render(l))
	}
	if truncated {
		out = append(out, d.Theme.Subtle.Render("ctrl+o for the rest"))
	}
	// The exit code is the first thing anyone wants from a failed command,
	// and it is the one line that is never part of the error text.
	for _, t := range trailers {
		out = append(out, trailerStyle(t, d.Theme).Render(t))
	}
	return out
}
