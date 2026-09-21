// Package toolview decides how one tool's card should be shown, from what the
// model *asked for* rather than from what came back.
//
// That is the whole point of the split. The language a card highlights comes
// from the path in the request, so a build log full of Go-shaped words is not
// coloured as Go and a file whose extension says nothing stays plain; the
// gutter comes from the tool, because read_file numbers its own lines and grep
// prefixes its own paths; and whether a diff is shown inline comes from its
// size. Guessing any of that from the output is how a card ends up confidently
// wrong, which is worse than plain.
//
// It owns presentation decisions only. The transcript still owns layout — the
// gutter bar, the depth indent, the width clamp and the render cache — so
// nothing here knows how wide a terminal is beyond the width it is handed.
package toolview

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/tui/highlight"
)

// Gutter is the line prefix a tool writes into its own output, which the card
// styles rather than replaces. Adding a second set of line numbers beside a
// tool's own is the obvious mistake here.
type Gutter int

const (
	// GutterNone is output with no prefix of its own.
	GutterNone Gutter = iota
	// GutterNumbered is read_file's "%6d\t" line numbers.
	GutterNumbered
	// GutterPathLine is grep's "path:line:text" (and "path-line-" context).
	GutterPathLine
)

// Caps bound what a card shows. They are named here so the two places that
// count lines agree.
const (
	// OutputTail is how many lines of output a card shows.
	OutputTail = 40
	// DiffInline is the most changed lines a diff may have and still be shown
	// in the collapsed card. Above it the card shows the count and says that
	// ctrl+o has the rest — 48 edits in one session is an ordinary day, and a
	// transcript has to stay scannable.
	DiffInline = 12
	// SummaryMax is the width of the one-line argument summary.
	SummaryMax = 60
)

// Plan is how one tool's card should be shown.
type Plan struct {
	// Lang is the chroma lexer name for this call's content, or "" to show it
	// plain. It comes from the request, never from the output.
	Lang string
	// Gutter is the prefix the tool writes itself.
	Gutter Gutter
	// Diffable is whether a diff belongs on this card at all.
	Diffable bool
	// Trailers is whether wright's own "[exit code …]" and "[note: …]" lines
	// should be lifted out of the output and shown as themselves.
	Trailers bool
}

// writeTools produce a diff; readFile numbers its lines; bash writes trailers.
// Named once so For and its tests cannot drift apart.
const (
	toolBash      = "bash"
	toolReadFile  = "read_file"
	toolGrep      = "grep"
	toolWriteFile = "write_file"
	toolEditFile  = "edit_file"
	toolMultiEdit = "multi_edit"
)

// For decides how to show a call, from its tool name and its arguments.
func For(tool string, args json.RawMessage) Plan {
	switch tool {
	case toolReadFile:
		return Plan{Lang: languageOf(args, "path"), Gutter: GutterNumbered}
	case toolWriteFile, toolEditFile:
		return Plan{Lang: languageOf(args, "path"), Diffable: true}
	case toolMultiEdit:
		return Plan{Lang: multiEditLanguage(args), Diffable: true}
	case toolBash:
		return Plan{Lang: bashLanguage(args), Trailers: true}
	case toolGrep:
		return Plan{Gutter: GutterPathLine}
	}
	// Everything else — web_fetch, list_dir, MCP tools, sub-agents — is shown
	// plain. Their output is prose or data, not a file in a language.
	return Plan{}
}

// Summary picks the most telling argument (path, command, pattern…) for the
// card's one-liner.
//
// It describes the *request*, which is what /ps and the approval block both
// want, and what a running card has instead of a result.
func Summary(args json.RawMessage) string {
	m, ok := decode(args)
	if !ok {
		return ""
	}
	for _, key := range []string{"path", "file", "file_path", "command", "cmd", "pattern", "query", "url", "name", "prompt"} {
		if v, ok := m[key].(string); ok && v != "" {
			v = strings.ReplaceAll(strings.TrimSpace(v), "\n", " ")
			return ansi.Truncate(v, SummaryMax, "…")
		}
	}
	return ""
}

func decode(args json.RawMessage) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, false
	}
	return m, true
}

func stringArg(args json.RawMessage, key string) string {
	m, ok := decode(args)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func languageOf(args json.RawMessage, key string) string {
	return highlight.Language(stringArg(args, key))
}

// multiEditLanguage answers only when every file in the batch is the same
// language. A mixed batch has no single answer, and colouring one file's diff
// as another's language is exactly the confident-wrong case.
func multiEditLanguage(args json.RawMessage) string {
	m, ok := decode(args)
	if !ok {
		return ""
	}
	edits, ok := m["edits"].([]any)
	if !ok {
		return languageOf(args, "path")
	}
	lang := ""
	for _, e := range edits {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		p, _ := em["path"].(string)
		l := highlight.Language(p)
		if l == "" {
			return ""
		}
		if lang != "" && l != lang {
			return ""
		}
		lang = l
	}
	return lang
}

// Headline is what the card says this call did: the tool's own first line of
// output when it wrote a sentence, and otherwise a summary of the request.
//
// The tools already write good ones — "Edited src/X.pas: replaced 1
// occurrence at line 214", "Wrote 8210 bytes (212 lines) to src/Y.pas" — and
// the card ignored every one of them, showing a summary of the arguments over
// a dump of the raw output. A running call has no result yet, and for the
// tools whose output is data rather than a sentence (read_file, grep, bash)
// the first line is a line of the file, not a description of anything.
func Headline(tool string, args json.RawMessage, output string, running bool) string {
	if running || output == "" || !writesASentence(tool) {
		return Summary(args)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	if first == "" {
		return Summary(args)
	}
	return ansi.Truncate(first, SummaryMax, "…")
}

// writesASentence names the tools whose first output line describes what they
// did. Everything else returns content, and the first line of content is not
// a summary of anything.
func writesASentence(tool string) bool {
	switch tool {
	case toolWriteFile, toolEditFile, toolMultiEdit:
		return true
	}
	return false
}
