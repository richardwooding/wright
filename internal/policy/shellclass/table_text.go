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

// ---- sed ----------------------------------------------------------------------

// handleSed classifies sed. GNU sed's `e` command and `s///e` flag run the
// pattern space as a shell command — `sed 's/.*/&/e' f` executes every line of
// f — so a script containing either is opaque, as is a script read with -f,
// which does not exist for the analyser to read. `w`/`W` targets and `s///w`
// files are declared writes, `r`/`R` targets declared reads, and -i still
// writes the file operands.
func handleSed(a *analyzer, name string, args []word) result {
	scripts, files, inPlace, r, done := sedArgs(name, args)
	if done {
		return r
	}
	var found sedFindings
	for _, s := range scripts {
		if s.dynamic {
			return opaque(name + " with a dynamic script")
		}
		found.merge(scanSed(s.text))
	}
	switch {
	case found.execs:
		return opaque(name + " script runs a shell command (e)")
	case found.opaque:
		return opaque(name + " script cannot be parsed")
	}
	out := safe(name + " script reads its input")
	if inPlace {
		out = mutating("edits files in place")
		a.writeFiles(&out, files, false)
	} else {
		a.readFiles(&out, files)
	}
	a.writeFiles(&out, wordsOf(found.writes), true)
	a.readFiles(&out, wordsOf(found.reads))
	return out
}

// wordsOf lifts literal file names out of a sed script into words.
func wordsOf(names []string) []word {
	out := make([]word, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, word{text: n})
		}
	}
	return out
}

// sedValueOptions take a separate value that is not a script.
var sedValueOptions = map[string]bool{"-l": true, "--line-length": true}

// sedInertOptions change how the script is applied but add no capability.
var sedInertOptions = map[string]bool{
	"-n": true, "--quiet": true, "--silent": true,
	"-s": true, "--separate": true,
	"-z": true, "--null-data": true, "--zero-terminated": true,
	"-u": true, "--unbuffered": true,
	"-E": true, "-r": true, "--regexp-extended": true,
	"--posix": true, "--debug": true, "--sandbox": true, "--follow-symlinks": true,
	"--help": true, "--version": true,
}

// sedParse is the state of one walk over sed's command line.
type sedParse struct {
	name    string
	args    []word
	i       int
	scripts []word
	inPlace bool
	// fromOption is true once -e supplied a script, so the first operand is
	// a file rather than the script.
	fromOption bool
}

// sedArgs splits sed's arguments into its scripts and file operands. done
// reports that an option alone decided the classification.
func sedArgs(name string, args []word) (scripts, files []word, inPlace bool, r result, done bool) {
	p := &sedParse{name: name, args: args}
	for p.i < len(args) && isFlag(args[p.i].text) {
		if r, stop := p.option(); stop {
			return nil, nil, false, r, true
		}
	}
	if p.i < len(args) && args[p.i].text == "--" {
		p.i++
	}
	rest := args[p.i:]
	if !p.fromOption {
		if len(rest) == 0 {
			return nil, nil, false, opaque(name + " with no visible script"), true
		}
		p.scripts, rest = append(p.scripts, rest[0]), rest[1:]
	}
	return p.scripts, rest, p.inPlace, result{}, false
}

// option consumes one option word; stop means it decided the classification.
func (p *sedParse) option() (result, bool) {
	t := p.args[p.i].text
	if !strings.HasPrefix(t, "--") {
		return p.shortCluster()
	}
	opt, _, _ := strings.Cut(t, "=")
	switch {
	case opt == "--expression":
		return p.takeScript(opt)
	case opt == "--file":
		return opaque(p.name + " -f runs a script file the analyser cannot read"), true
	case opt == "--in-place":
		p.inPlace, p.i = true, p.i+1
	case sedValueOptions[opt]:
		_, _, next := awkOptionValue(p.args, p.i)
		p.i = next
	case sedInertOptions[opt]:
		p.i++
	default:
		return opaque(p.name + " option " + opt + " is not recognised"), true
	}
	return result{}, false
}

// takeScript records the value of -e/--expression as one more script.
func (p *sedParse) takeScript(opt string) (result, bool) {
	val, ok, next := awkOptionValue(p.args, p.i)
	if !ok {
		return opaque(p.name + " " + opt + " without a script"), true
	}
	p.scripts = append(p.scripts, val)
	p.fromOption, p.i = true, next
	return result{}, false
}

// shortCluster interprets a short-option word such as -n, -ne or -i.bak. e, f
// and l swallow the rest of the cluster as their value; i takes the rest as a
// backup suffix.
func (p *sedParse) shortCluster() (result, bool) {
	t := p.args[p.i].text
	for j := 1; j < len(t); j++ {
		switch c := t[j]; c {
		case 'f':
			return opaque(p.name + " -f runs a script file the analyser cannot read"), true
		case 'e', 'l':
			if glued := t[j+1:]; glued != "" {
				if c == 'e' {
					p.scripts = append(p.scripts, word{text: glued, dynamic: p.args[p.i].dynamic})
					p.fromOption = true
				}
				p.i++
				return result{}, false
			}
			if p.i+1 >= len(p.args) {
				return opaque(p.name + " -" + string(c) + " without a value"), true
			}
			if c == 'e' {
				p.scripts = append(p.scripts, p.args[p.i+1])
				p.fromOption = true
			}
			p.i += 2
			return result{}, false
		case 'i':
			p.inPlace, p.i = true, p.i+1
			return result{}, false
		case 'n', 's', 'z', 'u', 'E', 'r':
			continue
		default:
			return opaque(p.name + " option -" + string(c) + " is not recognised"), true
		}
	}
	p.i++
	return result{}, false
}

// sedFindings is what scanning a sed script revealed.
type sedFindings struct {
	// execs is an `e` command or an s///e flag: sed runs the pattern space.
	execs bool
	// writes and reads are the w/W and r/R file operands.
	writes, reads []string
	// opaque means the scanner met something it does not understand, which
	// may be an `e` in disguise.
	opaque bool
}

func (f *sedFindings) merge(o sedFindings) {
	f.execs = f.execs || o.execs
	f.opaque = f.opaque || o.opaque
	f.writes = append(f.writes, o.writes...)
	f.reads = append(f.reads, o.reads...)
}

// scanSed walks a sed script command by command. It is deliberately
// conservative: a construct it cannot parse sets opaque rather than being
// assumed harmless.
func scanSed(script string) sedFindings {
	s := &sedScanner{src: script}
	for s.i < len(s.src) && !s.f.opaque {
		if !s.separators() {
			break
		}
		s.addresses()
		if s.i >= len(s.src) {
			break
		}
		s.command()
	}
	return s.f
}

type sedScanner struct {
	src string
	i   int
	f   sedFindings
}

// separators consumes whitespace, command separators and block braces. It
// returns false at end of script.
func (s *sedScanner) separators() bool {
	for s.i < len(s.src) {
		switch s.src[s.i] {
		case ' ', '\t', '\n', '\r', ';', '{', '}':
			s.i++
		default:
			return true
		}
	}
	return false
}

// addresses consumes an optional address or address range and any negation.
func (s *sedScanner) addresses() {
	s.address()
	for s.i < len(s.src) && s.src[s.i] == ',' {
		s.i++
		s.address()
	}
	for s.i < len(s.src) && (s.src[s.i] == '!' || s.src[s.i] == ' ' || s.src[s.i] == '\t') {
		s.i++
	}
}

// address consumes one address: a line number, $, /re/ or \%re%.
func (s *sedScanner) address() {
	for s.i < len(s.src) && (s.src[s.i] == ' ' || s.src[s.i] == '\t') {
		s.i++
	}
	if s.i >= len(s.src) {
		return
	}
	switch c := s.src[s.i]; {
	case c >= '0' && c <= '9', c == '~', c == '+':
		for s.i < len(s.src) && (isDigitByte(s.src[s.i]) || s.src[s.i] == '~' || s.src[s.i] == '+') {
			s.i++
		}
	case c == '$':
		s.i++
	case c == '/':
		s.i++
		s.delimited('/')
		s.addressFlags()
	case c == '\\':
		s.i++
		if s.i < len(s.src) {
			d := s.src[s.i]
			s.i++
			s.delimited(d)
			s.addressFlags()
		}
	}
}

// addressFlags consumes the I/M modifiers of a regex address.
func (s *sedScanner) addressFlags() {
	for s.i < len(s.src) && (s.src[s.i] == 'I' || s.src[s.i] == 'M') {
		s.i++
	}
}

// command consumes one sed command and records what it can do.
func (s *sedScanner) command() {
	c := s.src[s.i]
	s.i++
	switch c {
	case 's':
		s.substitute()
	case 'y':
		s.transliterate()
	case 'e':
		s.f.execs = true
		s.toEOL()
	case 'w', 'W':
		s.f.writes = append(s.f.writes, s.filename())
	case 'r', 'R':
		s.f.reads = append(s.f.reads, s.filename())
	case 'a', 'i', 'c':
		s.text()
	case 'b', 't', 'T', ':':
		s.label()
	case '#', 'v':
		s.toEOL()
	case 'q', 'Q', 'l', 'L':
		s.number()
	case '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		// No argument.
	default:
		s.f.opaque = true
	}
}

// substitute consumes s/PATTERN/REPLACEMENT/FLAGS.
func (s *sedScanner) substitute() {
	if s.i >= len(s.src) {
		s.f.opaque = true
		return
	}
	d := s.src[s.i]
	if d == '\\' || d == '\n' {
		s.f.opaque = true
		return
	}
	s.i++
	if _, ok := s.delimited(d); !ok {
		s.f.opaque = true
		return
	}
	if _, ok := s.delimited(d); !ok {
		s.f.opaque = true
		return
	}
	s.substituteFlags()
}

// substituteFlags consumes the s/// flags. `e` executes the pattern space and
// `w` names a file the substitution writes.
func (s *sedScanner) substituteFlags() {
	for s.i < len(s.src) {
		switch c := s.src[s.i]; {
		case c == 'e':
			s.f.execs = true
			s.i++
		case c == 'w':
			s.i++
			s.f.writes = append(s.f.writes, s.filename())
			return
		case c == 'g' || c == 'p' || c == 'i' || c == 'I' || c == 'm' || c == 'M' || isDigitByte(c):
			s.i++
		default:
			return
		}
	}
}

// transliterate consumes y/SOURCE/DEST/.
func (s *sedScanner) transliterate() {
	if s.i >= len(s.src) {
		s.f.opaque = true
		return
	}
	d := s.src[s.i]
	s.i++
	if _, ok := s.delimited(d); !ok {
		s.f.opaque = true
		return
	}
	if _, ok := s.delimited(d); !ok {
		s.f.opaque = true
	}
}

// delimited consumes up to and including the next unescaped delimiter and
// returns the text between. ok is false when the script ends first.
func (s *sedScanner) delimited(d byte) (string, bool) {
	start := s.i
	for s.i < len(s.src) {
		switch s.src[s.i] {
		case '\\':
			s.i += 2
			continue
		case d:
			text := s.src[start:s.i]
			s.i++
			return text, true
		}
		s.i++
	}
	s.i = len(s.src)
	return s.src[min(start, len(s.src)):], false
}

// filename takes the rest of the line as a file name (sed allows no
// separator after w/r, so the name runs to the newline).
func (s *sedScanner) filename() string {
	for s.i < len(s.src) && (s.src[s.i] == ' ' || s.src[s.i] == '\t') {
		s.i++
	}
	start := s.i
	s.toEOL()
	return strings.TrimSpace(s.src[start:s.i])
}

// text consumes the argument of a/i/c, honouring backslash continuations.
func (s *sedScanner) text() {
	for s.i < len(s.src) {
		if s.src[s.i] == '\\' {
			s.i += 2
			continue
		}
		if s.src[s.i] == '\n' {
			return
		}
		s.i++
	}
}

// label consumes a branch label, which ends at ; } or newline.
func (s *sedScanner) label() {
	for s.i < len(s.src) && strings.IndexByte(";}\n", s.src[s.i]) < 0 {
		s.i++
	}
}

// number consumes the optional numeric argument of q/Q/l/L.
func (s *sedScanner) number() {
	for s.i < len(s.src) && (s.src[s.i] == ' ' || isDigitByte(s.src[s.i])) {
		s.i++
	}
}

// toEOL consumes the rest of the line.
func (s *sedScanner) toEOL() {
	for s.i < len(s.src) && s.src[s.i] != '\n' {
		s.i++
	}
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }
