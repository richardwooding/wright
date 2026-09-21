package highlight

import (
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2/lexers"
)

// Language returns the chroma lexer name for a filename, or "" when there is
// no confident answer.
//
// It matches on the *name* only — lexers.Match is a registry lookup on the
// filename and its extension. chroma can also guess a language from content
// (lexers.Analyse), and this deliberately never calls it: the language comes
// from what the model asked for, so a build log full of Go-shaped words is not
// coloured as Go, and a file whose extension says nothing stays plain. A wrong
// lexer looks worse than none.
func Language(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	lex := lexers.Match(base)
	if lex == nil {
		// chroma's globs are case-sensitive, and a DOS-descended Pascal or
		// COBOL tree is full of X.PAS and Y.CBL. An extension is not case
		// sensitive on any filesystem wright supports, so try the lower
		// spelling before giving up.
		lex = lexers.Match(strings.ToLower(base))
	}
	if lex == nil {
		return ""
	}
	name := lex.Config().Name
	switch strings.ToLower(name) {
	case "", "plaintext", "fallback", "plain text":
		return ""
	}
	return name
}
