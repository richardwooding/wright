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
	// OutputIsSource is whether the *result text* is the file's content
	// rather than a report about it. Lang alone is not enough to decide:
	// write_file knows its language (for the diff) but its output is
	// wright's own sentence — "Wrote 271 bytes (18 lines) to src/X.php" —
	// and lexing that as PHP paints the byte count as a numeric literal.
	// Colouring the harness's words as if the program had said them is the
	// one thing highlighting must never do.
	OutputIsSource bool
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
		return Plan{Lang: languageOf(args, "path"), Gutter: GutterNumbered, OutputIsSource: true}
	case toolWriteFile, toolEditFile:
		return Plan{Lang: languageOf(args, "path"), Diffable: true}
	case toolMultiEdit:
		return Plan{Lang: multiEditLanguage(args), Diffable: true}
	case toolBash:
		// A pager's output is the file; every other command reports.
		lang := bashLanguage(args)
		return Plan{Lang: lang, Trailers: true, OutputIsSource: lang != ""}
	case toolGrep:
		return Plan{Gutter: GutterPathLine}
	}
	// Everything else — web_fetch, list_dir, MCP tools, sub-agents — is shown
	// plain. Their output is prose or data, not a file in a language.
	return Plan{}
}

// SummaryFor is the card's one-liner for a call, given the workspace root.
//
// It dispatches on the tool: bash goes through a path that drops the leading
// "cd <workspace> &&" the model writes on almost every command, and every
// other tool falls straight through to Summary. Summary itself stays generic
// and untouched — teaching it about `cd` would apply shell lexing to any tool
// with a "command" key, MCP tools included.
//
// Nothing is hidden by the strip: the expanded card prints the arguments
// verbatim, the approval overlay shows the exact bytes that will run, and the
// audit log records the whole call. No consent surface changes.
func SummaryFor(tool string, args json.RawMessage, root string) string {
	if tool != toolBash {
		return Summary(args)
	}
	cmd := strings.ReplaceAll(strings.TrimSpace(stringArg(args, "command")), "\n", " ")
	if cmd == "" {
		return Summary(args)
	}
	dir, rest, ok := stripLeadingCd(cmd)
	if !ok {
		return ansi.Truncate(cmd, SummaryMax, "…")
	}
	// `cd a && cd b && go test` collapses to the last directory. The string
	// strictly shrinks each time, so this terminates.
	for {
		d, r, more := stripLeadingCd(rest)
		if !more {
			break
		}
		dir, rest = d, r
	}
	if label := dirLabel(dir, root); label != "" {
		rest = label + ": " + rest
	}
	// Truncating *after* the strip is the entire point.
	return ansi.Truncate(rest, SummaryMax, "…")
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
func Headline(tool string, args json.RawMessage, output string, running bool, root string) string {
	if running || output == "" || !writesASentence(tool) {
		return SummaryFor(tool, args, root)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	if first == "" {
		return SummaryFor(tool, args, root)
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

// Written is the text a write_file call is putting on disk, taken from the
// arguments it was already carrying.
//
// It exists because a new file has no diff to show — Describe reports "new
// file, N lines" and nothing else, correctly, since a diff against nothing is
// all "+" — and the card was therefore left with the arguments, where the
// whole file appears as one escaped JSON string clipped at the terminal's
// width. In a project being written from scratch that is most of the
// transcript: 154 of one session's 315 calls were write_file.
func Written(tool string, args json.RawMessage) string {
	if tool != toolWriteFile {
		return ""
	}
	return stringArg(args, "content")
}
