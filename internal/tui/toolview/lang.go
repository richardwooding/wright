package toolview

import (
	"encoding/json"
	"strings"

	"github.com/richardwooding/wright/internal/tui/highlight"
)

// pagers are the commands whose output *is* the file they were given. Only
// these can make a bash card highlight, because only for these is the output
// the content of a named file rather than a report about one.
//
// grep is deliberately absent: its output is "path:line:text", which is a set
// of fragments from possibly several languages, and colouring it as one of
// them would be wrong more often than right.
var pagers = map[string]bool{
	"cat": true, "bat": true, "head": true, "tail": true,
	"nl": true, "more": true, "less": true, "sed": true,
}

// segmentBreaks are the characters that mean the output is no longer simply
// the file. `cat x.go | wc -l` prints a number; `cat x.go > y.go` prints
// nothing at all.
const segmentBreaks = ";|&><\n"

// bashLanguage returns the language of a bash call's output, or "" when there
// is no confident answer — which is the usual case and the right default.
//
// This is a word scan, not a shell parse, and deliberately not a call into
// internal/policy/shellclass. The classifier is a security component the
// import DAG keeps out of the TUI on purpose, and reaching into it for a
// cosmetic decision would make the renderer depend on the permission engine
// and re-parse every command on the render path. The scan is gated hard
// enough that its failure mode is "plain", never "wrong".
func bashLanguage(args json.RawMessage) string {
	cmd := stringArg(args, "command")
	if cmd == "" || strings.ContainsAny(cmd, segmentBreaks) {
		return ""
	}
	fields := strings.Fields(cmd)
	// A leading NAME=value prefix is still the same command.
	for len(fields) > 0 && isAssignment(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) < 2 || !pagers[fields[0]] {
		return ""
	}
	// Exactly one operand may look like a source file. Two means the output
	// is a concatenation of possibly different languages; none means the
	// command reads standard input or names nothing we recognise.
	lang := ""
	for _, f := range fields[1:] {
		l := highlight.Language(strings.Trim(f, `"'`))
		if l == "" {
			continue
		}
		if lang != "" {
			return ""
		}
		lang = l
	}
	return lang
}

// isAssignment reports whether a word is a NAME=value environment prefix.
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
