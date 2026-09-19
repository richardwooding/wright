package shellclass

import "strings"

// This file holds the two text tools whose *arguments are programs*: awk and
// sed. Both can run shell commands and write files from inside their script,
// so neither can be treated as a command that merely reads the files named on
// its command line.

// ---- awk ----------------------------------------------------------------------

// awkValueOptions take a value that is data (a field separator, a variable
// assignment); they are harmless as long as the value is literal.
var awkValueOptions = map[string]bool{
	"-F": true, "--field-separator": true,
	"-v": true, "--assign": true,
}

// awkProgramOptions supply program text on the command line (gawk's -e).
var awkProgramOptions = map[string]bool{"-e": true, "--source": true}

// awkOpaqueOptions load a program, an extension or a debugger, or write a
// file: in every case the analyser cannot see what will run.
var awkOpaqueOptions = map[string]bool{
	"-f": true, "--file": true,
	"-i": true, "--include": true,
	"-l": true, "--load": true,
	"-E": true, "--exec": true,
	"-o": true, "--pretty-print": true,
	"-p": true, "--profile": true,
	"-D": true, "--debug": true,
	"-d": true, "--dump-variables": true,
	"-W": true,
}

// awkInertOptions change how the program is interpreted but cannot make it do
// anything it could not do already.
var awkInertOptions = map[string]bool{
	"-b": true, "--characters-as-bytes": true,
	"-c": true, "--traditional": true, "--compat": true,
	"-g": true, "--gen-pot": true,
	"-k": true, "--csv": true,
	"-n": true, "--non-decimal-data": true,
	"-N": true, "--use-lc-numeric": true,
	"-P": true, "--posix": true,
	"-r": true, "--re-interval": true,
	"-s": true, "--no-optimize": true,
	"-S": true, "--sandbox": true,
	"-t": true, "--lint-old": true,
	"-L": true, "--lint": true,
	"-V": true, "--version": true, "--help": true,
}

// awkExecTokens are the identifiers that prove an awk program can do more
// than read: run a shell command, read a command's output, or reach the
// environment. They cannot be spelled any other way — awk has no eval and no
// string concatenation of identifiers — so a literal scan is sound.
var awkExecTokens = []string{"system", "getline", "ENVIRON", "close", "/dev/", "/inet/"}

// awkExecChars are the characters awk needs to redirect, pipe, open a socket
// or call a function indirectly (gawk's @f). A program without any of them
// cannot start a process or open a file.
const awkExecChars = "@|<>`"

// handleAwk classifies awk/gawk/mawk/nawk. The program text is code, not a
// file: `awk 'BEGIN{system("…")}'` executes a shell command while naming no
// file at all. Only a program the scanner can prove inert stays a SafeRead;
// anything else — including a program loaded with -f, which cannot be read at
// classify time — is opaque and so can never match an allow rule.
func handleAwk(a *analyzer, name string, args []word) result {
	// GNU getopt permutes, so `awk '{print}' -f prog.awk` still loads the
	// file: an opaque option counts wherever it appears.
	if opt, ok := awkOpaqueOption(args); ok {
		return opaque(name + " " + opt + " loads a program the analyser cannot read")
	}
	prog, files, r, done := awkArgs(name, args)
	if done {
		return r
	}
	if prog.dynamic || !awkProgramInert(prog.text) {
		return opaque(name + " program may run commands or write files")
	}
	out := safe(name + " program reads its input")
	a.readFiles(&out, files)
	return out
}

// awkArgs splits awk's arguments into the program and the file operands.
// done reports that an option alone decided the classification.
func awkArgs(name string, args []word) (prog word, files []word, r result, done bool) {
	i, seen := 0, false
	for i < len(args) && isFlag(args[i].text) {
		opt := awkOptionName(args[i].text)
		switch {
		case awkOpaqueOptions[opt]:
			return word{}, nil, opaque(name + " " + opt + " loads a program the analyser cannot read"), true
		case awkProgramOptions[opt]:
			val, ok, next := awkOptionValue(args, i)
			if !ok {
				return word{}, nil, opaque(name + " " + opt + " without a program"), true
			}
			prog, seen, i = word{text: joinProgram(prog.text, val.text), dynamic: prog.dynamic || val.dynamic}, true, next
		case awkValueOptions[opt]:
			val, _, next := awkOptionValue(args, i)
			if val.dynamic {
				return word{}, nil, opaque(name + " " + opt + " with a dynamic value"), true
			}
			i = next
		case awkInertOptions[opt]:
			i++
		default:
			return word{}, nil, opaque(name + " option " + opt + " is not recognised"), true
		}
	}
	if i < len(args) && args[i].text == "--" {
		i++
	}
	rest := args[i:]
	if !seen {
		if len(rest) == 0 {
			return word{}, nil, opaque(name + " with no visible program"), true
		}
		prog, rest = rest[0], rest[1:]
	}
	return prog, awkOperands(rest), result{}, false
}

// awkOpaqueOption reports the first program-loading option anywhere in argv,
// up to an explicit "--".
func awkOpaqueOption(args []word) (string, bool) {
	for _, w := range args {
		if w.text == "--" {
			return "", false
		}
		if isFlag(w.text) && awkOpaqueOptions[awkOptionName(w.text)] {
			return awkOptionName(w.text), true
		}
	}
	return "", false
}

// awkOptionName is the option without its glued value ("-F:" → "-F",
// "--assign=x=1" → "--assign").
func awkOptionName(t string) string {
	if strings.HasPrefix(t, "--") {
		name, _, _ := strings.Cut(t, "=")
		return name
	}
	if len(t) > 2 {
		return t[:2]
	}
	return t
}

// awkOptionValue returns the value of the option at i in either the glued
// (-F:, --assign=x=1) or separate (-F :) form, and the index after it.
func awkOptionValue(args []word, i int) (val word, ok bool, next int) {
	t := args[i].text
	if strings.HasPrefix(t, "--") {
		if _, v, eq := strings.Cut(t, "="); eq {
			return word{text: v, dynamic: args[i].dynamic}, true, i + 1
		}
	} else if len(t) > 2 {
		return word{text: t[2:], dynamic: args[i].dynamic}, true, i + 1
	}
	if i+1 < len(args) {
		return args[i+1], true, i + 2
	}
	return word{}, false, i + 1
}

// awkOperands keeps the file operands: `var=value` sets a variable and a
// late option is one getopt permuted back to the front, neither names a file.
func awkOperands(rest []word) []word {
	out := make([]word, 0, len(rest))
	for _, w := range rest {
		if isAssignment(w.text) || isFlag(w.text) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// isAssignment reports whether t has the shape NAME=value.
func isAssignment(t string) bool {
	name, _, ok := strings.Cut(t, "=")
	if !ok || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !alnum {
			return false
		}
	}
	return true
}

// awkProgramInert reports whether a program provably cannot start a process,
// open a socket or write a file. It is deliberately a blunt instrument: a
// program using `>` as a comparison is inert but reads as a redirect here, and
// losing a SafeRead is cheaper than granting one.
func awkProgramInert(prog string) bool {
	if strings.ContainsAny(prog, awkExecChars) {
		return false
	}
	for _, t := range awkExecTokens {
		if strings.Contains(prog, t) {
			return false
		}
	}
	return true
}

// joinProgram concatenates the -e fragments the way awk does.
func joinProgram(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "\n" + add
}
