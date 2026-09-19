package policy

import (
	"slices"
	"strings"

	"github.com/richardwooding/wright/internal/policy/shellclass"
)

// impliedCwdReaders are commands that walk a directory tree and default to
// the working directory when given no path operand: `grep -r pattern` reads
// everything under the cwd while naming nothing, so the classifier declares
// no path and the credential scan would have nothing to look at.
//
// The set mirrors the reader entries in shellclass's tables. It is small and
// deliberately conservative: a command listed here only causes the working
// directory to be scanned, never a denial on its own.
var impliedCwdReaders = map[string]func(args []string) bool{
	"grep":   func(a []string) bool { return hasRecursiveFlag(a) },
	"egrep":  func(a []string) bool { return hasRecursiveFlag(a) },
	"fgrep":  func(a []string) bool { return hasRecursiveFlag(a) },
	"rgrep":  func(a []string) bool { return true },
	"rg":     func(a []string) bool { return true },
	"ag":     func(a []string) bool { return true },
	"ack":    func(a []string) bool { return true },
	"find":   func(a []string) bool { return true },
	"fd":     func(a []string) bool { return true },
	"fdfind": func(a []string) bool { return true },
	"tree":   func(a []string) bool { return true },
	"du":     func(a []string) bool { return true },
	"ls":     func(a []string) bool { return hasShortFlag(a, 'R') },
	"tar":    func(a []string) bool { return hasShortFlag(a, 'c') || slices.Contains(a, "--create") },
}

func hasRecursiveFlag(args []string) bool {
	return hasShortFlag(args, 'r') || hasShortFlag(args, 'R') ||
		slices.Contains(args, "--recursive") || slices.Contains(args, "--dereference-recursive")
}

// hasShortFlag reports whether a bundled short flag such as -rn carries c.
func hasShortFlag(args []string, c byte) bool {
	for _, a := range args {
		if len(a) < 2 || !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		if strings.IndexByte(a[1:], c) >= 0 {
			return true
		}
	}
	return false
}

// readsImpliedCwd reports whether any command in the script walks the working
// directory without naming it, so the caller must scan that directory too.
func readsImpliedCwd(sh *shellclass.Analysis) bool {
	if sh == nil {
		return false
	}
	for _, c := range sh.Commands {
		if len(c.Argv) == 0 || len(c.Reads) > 0 {
			continue
		}
		name := c.Argv[0]
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		recursive, ok := impliedCwdReaders[name]
		if ok && recursive(c.Argv[1:]) && !namesAnOperand(c.Argv[1:]) {
			return true
		}
	}
	return false
}

// namesAnOperand reports whether the argument list contains a non-flag word
// that is not the value of a preceding flag. It is approximate on purpose:
// when in doubt it returns false, which only costs an extra directory scan.
func namesAnOperand(args []string) bool {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		// The first bare word of a grep-family command is the pattern.
		if i == 0 && len(args) == 1 {
			return false
		}
		if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.HasPrefix(args[i-1], "--") {
			continue // probably the value of a short flag
		}
		if i > 0 {
			return true
		}
	}
	return false
}
