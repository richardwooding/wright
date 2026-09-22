package toolview

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// stripLeadingCd splits a leading "cd <dir> &&" (or ";") off a command.
//
// The model writes it on almost every call — 89% of bash calls in one real
// session, and 76 of those 78 targeted the directory bash was already in —
// and the prefix for an ordinary project path is about sixty characters,
// which is the entire summary budget. The card therefore showed the `cd` and
// nothing of the command that ran.
//
// It is purely lexical and gated hard, because its failure mode has to be
// "unchanged" and never "mangled". Deliberately not a call into
// policy/shellclass: that is a security component the import DAG keeps out of
// the TUI, and re-parsing every command on the render path to answer a
// cosmetic question would be the wrong trade — the same reasoning as
// bashLanguage in lang.go.
func stripLeadingCd(s string) (dir, rest string, ok bool) {
	after, found := strings.CutPrefix(s, "cd")
	if !found || after == "" || !isSpace(after[0]) {
		// "cdinstall x && y" is not a cd.
		return "", "", false
	}
	// The separator is && or ; — never ||, whose semantics differ: in
	// `cd /x || ls` the second command runs *because* the first failed.
	i, sep := strings.Index(after, "&&"), 2
	if j := strings.Index(after, ";"); j >= 0 && (i < 0 || j < i) {
		i, sep = j, 1
	}
	if i < 0 {
		// `cd /x` alone is the command; an empty summary would be a lie.
		return "", "", false
	}
	if k := strings.Index(after, "||"); k >= 0 && k < i {
		return "", "", false
	}
	// Exactly one field between, so `cd "my dir" && ls`, `cd -- /x && ls` and
	// `cd $(ls | head -1) && ls` all fall through untouched.
	mid := strings.Fields(after[:i])
	if len(mid) != 1 || strings.ContainsAny(mid[0], "\"'`$") {
		return "", "", false
	}
	// The remainder keeps its original bytes: rebuilding it from Fields would
	// rewrite spacing inside quoted arguments.
	rest = strings.TrimSpace(after[i+sep:])
	if rest == "" {
		return "", "", false
	}
	return mid[0], rest, true
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// homeDir is looked up once; a renderer should not stat per card.
var homeDir = sync.OnceValue(func() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Clean(h)
})

// dirLabel is how a cd target is shown beside the command, or "" when the
// target is the directory bash is already in and saying so adds nothing.
//
// The comparison fails safe. One directory has several spellings — this is
// Fedora Silverblue, where /home is a symlink to var/home, and macOS has the
// same hazard with /tmp → /private/tmp — and a renderer must not call
// filepath.EvalSymlinks on the draw path to resolve them. So the label is
// dropped only on an exact match of the cleaned paths, and shown whenever the
// answer is uncertain: the worst case is a card that still shows a redundant
// cd, never one that hides a real change of directory.
func dirLabel(dir, root string) string {
	if dir == "" {
		return ""
	}
	home := homeDir()
	abs := dir
	switch {
	case strings.HasPrefix(dir, "~"):
		if home == "" {
			return dir
		}
		abs = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	case !filepath.IsAbs(dir):
		// A relative target is already short, and it is real information.
		return dir
	}
	abs = filepath.Clean(abs)
	if root != "" {
		root = filepath.Clean(root)
		if abs == root {
			return ""
		}
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if home != "" && strings.HasPrefix(abs, home+string(filepath.Separator)) {
		return "~" + abs[len(home):]
	}
	return abs
}
