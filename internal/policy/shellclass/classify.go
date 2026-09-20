package shellclass

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// result is what a command handler reports back.
type result struct {
	class    Class
	reason   string
	network  bool
	installs bool
	unknown  bool
	// unrecognised marks a command whose argv is fully known but whose
	// program is not in the table. That is a weaker fact than unknown and
	// must not be confused with it: the script is perfectly legible, so a
	// rule naming the program can cover it.
	unrecognised bool
	hardDeny     string
	writes       []string
	reads        []string
}

// raise lifts the class to at least c.
func (r *result) raise(c Class) { r.class = max(r.class, c) }

// Name groups used by several rules.
var (
	privilegeWrappers = []string{"sudo", "su", "doas", "pkexec", "run0"}
	opaqueBuiltins    = []string{"eval", "source", ".", "alias", "trap", "enable", "hash"}
	shells            = []string{"sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "fish"}
	interpreters      = []string{"python", "python2", "python3", "pypy", "pypy3", "node", "nodejs", "deno", "ruby", "perl", "php", "lua", "luajit", "Rscript", "julia", "tsx", "ts-node", "groovy", "kotlin", "scala", "osascript", "pwsh", "powershell", "elixir", "erl", "swift"}
	fetchers          = []string{"curl", "wget", "fetch", "http", "https", "xh", "aria2c"}
	binDirs           = []string{"/usr/bin", "/bin", "/usr/local/bin", "/usr/sbin", "/sbin", "/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin", "/usr/local/go/bin", "/snap/bin"}
)

// classifyWords turns an expanded argv into a Command via the handler chain.
func (a *analyzer) classifyWords(words []word) Command {
	argv := make([]string, len(words))
	dynamic := false
	for i, w := range words {
		argv[i] = w.text
		dynamic = dynamic || w.dynamic
	}
	c := Command{Argv: argv, Dynamic: dynamic}
	r := a.classify(words)
	c.Class, c.Reason, c.Network, c.Installs = r.class, r.reason, r.network, r.installs
	c.Writes, c.Reads = r.writes, r.reads
	if r.unknown {
		c.Dynamic = true
	}
	c.Unrecognised = r.unrecognised
	if r.hardDeny != "" {
		a.hardDeny(r.hardDeny, r.class)
	}
	return c
}

// classify resolves argv[0] and dispatches: privilege wrappers, opaque
// builtins, transparent wrappers, shells, then the command table.
func (a *analyzer) classify(words []word) result {
	if len(words) == 0 {
		return result{}
	}
	name, r, done := a.commandName(words[0])
	if done {
		return r
	}
	switch {
	case slices.Contains(privilegeWrappers, name):
		return result{class: Privilege, hardDeny: "privilege escalation via " + name, reason: "privilege escalation via " + name}
	case slices.Contains(opaqueBuiltins, name):
		return result{class: MutatingWorkspace, unknown: true, reason: name + " is opaque to static analysis"}
	case slices.Contains(shells, name):
		return a.shell(name, words[1:])
	}
	if h, ok := wrappers[name]; ok {
		return h(a, words[1:])
	}
	if h, ok := table[name]; ok {
		return h(a, name, words[1:])
	}
	// Not opaque: the argv is fully expanded and readable, we simply have no
	// spec for this program. Everything is still assumed about its effects
	// (MutatingWorkspace, so it is never auto-allowed), but a rule naming it
	// can cover it — otherwise no program outside this table could ever be
	// allowed, by an offer or by hand.
	return result{class: MutatingWorkspace, unrecognised: true, reason: "unknown command " + name}
}

// commandName resolves argv[0]. Paths inside the workspace are workspace
// executables (mutating); paths in system bin dirs are looked up by base
// name; a dynamic or otherwise unresolvable name is opaque.
func (a *analyzer) commandName(w word) (name string, r result, done bool) {
	if w.dynamic {
		return "", result{class: MutatingWorkspace, unknown: true, reason: "dynamic command name"}, true
	}
	name = w.text
	if !strings.Contains(name, "/") {
		return name, result{}, false
	}
	abs, inside, err := a.ws.Resolve(name)
	if err != nil {
		return "", result{class: MutatingWorkspace, unknown: true, reason: "unresolvable command path"}, true
	}
	if inside {
		return "", result{class: MutatingWorkspace, reason: "runs workspace executable " + a.display(abs)}, true
	}
	if slices.Contains(binDirs, filepath.Dir(abs)) {
		return filepath.Base(abs), result{}, false
	}
	return "", result{class: MutatingWorkspace, unknown: true, reason: "runs executable outside the workspace: " + abs}, true
}

// wrapperFunc peels one transparent wrapper and classifies the inner command.
type wrapperFunc func(a *analyzer, args []word) result

var wrappers map[string]wrapperFunc

func init() {
	// Populated in init to avoid an initialisation cycle with classify.
	wrappers = map[string]wrapperFunc{
		"env":      wrapEnv,
		"command":  wrapCommand,
		"exec":     wrapSkipFlags(map[string]int{"-a": 1, "-c": 0, "-l": 0}),
		"nice":     wrapSkipFlags(map[string]int{"-n": 1, "--adjustment": 1}),
		"nohup":    wrapSkipFlags(nil),
		"time":     wrapSkipFlags(map[string]int{"-p": 0, "-v": 0, "-f": 1, "-o": 1}),
		"timeout":  wrapTimeout,
		"stdbuf":   wrapSkipFlags(map[string]int{"-i": 1, "-o": 1, "-e": 1}),
		"xargs":    wrapXargs,
		"busybox":  wrapSkipFlags(nil),
		"ionice":   wrapSkipFlags(map[string]int{"-c": 1, "-n": 1, "-p": 1}),
		"chrt":     wrapSkipFlags(map[string]int{"-p": 1}),
		"setsid":   wrapSkipFlags(map[string]int{"-w": 0, "-f": 0, "-c": 0}),
		"unbuffer": wrapSkipFlags(map[string]int{"-p": 0}),
		"caffeinate": wrapSkipFlags(map[string]int{
			"-d": 0, "-i": 0, "-m": 0, "-s": 0, "-u": 0, "-t": 1, "-w": 1,
		}),
	}
}

// inner classifies the wrapped command starting at index i; an index past
// the end (a trailing option that expected a value) is an empty command.
func (a *analyzer) inner(args []word, i int) result {
	if i >= len(args) {
		return result{class: SafeRead}
	}
	return a.classify(args[i:])
}

// wrapSkipFlags builds a peeler that drops known options (and their values)
// before classifying what follows.
func wrapSkipFlags(flags map[string]int) wrapperFunc {
	return func(a *analyzer, args []word) result {
		i := 0
		for i < len(args) && strings.HasPrefix(args[i].text, "-") && args[i].text != "-" {
			opt, _, hasEq := strings.Cut(args[i].text, "=")
			n, known := flags[opt]
			switch {
			case args[i].text == "--":
				i++
				return a.inner(args, i)
			case known && !hasEq:
				i += 1 + n
			default:
				i++
			}
		}
		return a.inner(args, i)
	}
}

// wrapEnv peels `env [-i] [NAME=value…] cmd`. `env -i PATH=… cmd` replaces the
// lookup path wholesale and is hard-denied; other PATH/IFS assignments make
// the command opaque.
func wrapEnv(a *analyzer, args []word) result {
	ignoreEnv := false
	i := 0
	for i < len(args) {
		t := args[i].text
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "-") {
			if r, done := a.envAssignment(args, i, ignoreEnv); done {
				return r
			}
			i++
			continue
		}
		skip, isIgnore, done := envOption(t)
		switch {
		case done:
			return a.inner(args, i+skip)
		case skip == 0:
			return opaque("env -S splits arguments at runtime")
		}
		ignoreEnv = ignoreEnv || isIgnore
		i += skip
	}
	return result{class: SafeRead, reason: "prints environment"}
}

// envOption interprets one env option. skip is how many words it consumes;
// isIgnore marks -i; done means the command starts after the skipped words.
// skip == 0 (and !done) is the -S form, which cannot be analysed.
func envOption(t string) (skip int, isIgnore, done bool) {
	switch {
	case t == "-i" || t == "--ignore-environment" || t == "-":
		return 1, true, false
	case t == "-u" || t == "--unset" || t == "-C" || t == "--chdir":
		return 2, false, false
	case strings.HasPrefix(t, "--unset=") || strings.HasPrefix(t, "--chdir="):
		return 1, false, false
	case t == "-S" || t == "--split-string" || strings.HasPrefix(t, "-S"):
		return 0, false, false
	case t == "--":
		return 1, false, true
	default:
		return 0, false, true
	}
}

// envAssignment handles one NAME=value argument of env. `env -i PATH=…` is
// hard-denied; other dangerous or dynamic assignments make the inner command
// opaque. done is false when the assignment is harmless.
func (a *analyzer) envAssignment(args []word, i int, ignoreEnv bool) (result, bool) {
	name, _, _ := strings.Cut(args[i].text, "=")
	if name == "PATH" && ignoreEnv {
		return privilegeDeny("PATH override via env -i"), true
	}
	if dangerousVar(name) || args[i].dynamic {
		r := a.inner(args, i+1)
		r.unknown = true
		r.reason = joinReason(r.reason, "overrides "+name)
		return r, true
	}
	return result{}, false
}

// wrapCommand peels `command [-pvV] cmd`; -v/-V only look the command up.
func wrapCommand(a *analyzer, args []word) result {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].text, "-") {
		if strings.ContainsAny(args[i].text, "vV") {
			return result{class: SafeRead, reason: "command lookup"}
		}
		i++
	}
	return a.inner(args, i)
}

// wrapTimeout peels `timeout [opts] DURATION cmd`.
func wrapTimeout(a *analyzer, args []word) result {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].text, "-") {
		switch args[i].text {
		case "-s", "--signal", "-k", "--kill-after":
			i += 2
		default:
			i++
		}
	}
	if i < len(args) {
		i++ // the duration
	}
	return a.inner(args, i)
}

// wrapXargs peels xargs options; the inner command's arguments come from
// stdin, so its class is raised to at least MutatingWorkspace.
func wrapXargs(a *analyzer, args []word) result {
	withValue := map[string]bool{"-I": true, "-n": true, "-P": true, "-d": true, "-L": true, "-s": true, "-E": true, "-a": true, "--arg-file": true, "--delimiter": true, "--max-args": true, "--max-procs": true, "--max-lines": true, "--replace": true}
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].text, "-") {
		opt, _, hasEq := strings.Cut(args[i].text, "=")
		if withValue[opt] && !hasEq {
			i += 2
		} else {
			i++
		}
	}
	if i >= len(args) {
		return result{class: SafeRead, reason: "xargs echo"}
	}
	r := a.classify(args[i:])
	r.raise(MutatingWorkspace)
	r.reason = joinReason(r.reason, "arguments supplied by xargs")
	return r
}

// shell handles `sh|bash|… [-c script | file]`. A literal -c script is
// analysed recursively (depth ≤ 3) and its commands replace the wrapper; a
// dynamic script, stdin or interactive shell is opaque.
func (a *analyzer) shell(name string, args []word) result {
	var script *word
	var file *word
	for i := 0; i < len(args); i++ {
		t := args[i].text
		switch {
		case t == "-c" || (strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.Contains(t, "c") && len(t) <= 4):
			if i+1 < len(args) {
				script = &args[i+1]
			}
			i = len(args)
		case t == "-s" || t == "-i" || t == "--stdin" || t == "--interactive":
			return result{class: MutatingWorkspace, unknown: true, reason: name + " reading commands from stdin"}
		case t == "-o":
			i++
		case strings.HasPrefix(t, "-"):
			// -e -u -x -l … : harmless mode flags
		default:
			w := args[i]
			file = &w
			i = len(args)
		}
	}
	switch {
	case script != nil:
		return a.subScript(name, *script)
	case file != nil:
		return a.scriptFile(name, *file)
	default:
		return result{class: MutatingWorkspace, unknown: true, reason: name + " with no script (stdin)"}
	}
}

// subScript analyses a literal `-c` script in place.
func (a *analyzer) subScript(name string, script word) result {
	if script.dynamic {
		return result{class: MutatingWorkspace, unknown: true, reason: name + " -c with a dynamic script"}
	}
	if a.depth+1 >= maxDepth {
		return result{class: MutatingWorkspace, unknown: true, reason: "shell nesting deeper than " + string(rune('0'+maxDepth))}
	}
	sub := &analyzer{ws: a.ws, out: a.out, depth: a.depth + 1}
	sub.run(script.text)
	a.floor = max(a.floor, sub.floor)
	// The inner commands were appended to out; the wrapper itself contributes
	// nothing further and is reported as a harmless shell invocation.
	return result{class: SafeRead, reason: "inline " + name + " script"}
}

// scriptFile classifies `sh file`: a workspace script is mutating; anything
// else is opaque.
func (a *analyzer) scriptFile(name string, file word) result {
	if file.dynamic {
		return result{class: MutatingWorkspace, unknown: true, reason: name + " with a dynamic script path"}
	}
	abs, inside, err := a.ws.Resolve(file.text)
	if err != nil || !inside {
		return result{class: MutatingWorkspace, unknown: true, reason: name + " runs a script outside the workspace"}
	}
	return result{class: MutatingWorkspace, reason: "runs workspace script " + a.display(abs), reads: []string{abs}}
}

// checkPipeline looks across pipeline stages for two catastrophic shapes:
// a fetcher piped into a shell or interpreter (remote code execution) and a
// decoder piped into a shell (obfuscated payload). Both are hard-denied.
func (a *analyzer) checkPipeline(stages []*syntax.Stmt) {
	argvs := make([][]string, 0, len(stages))
	for _, s := range stages {
		argvs = append(argvs, a.stageArgv(s))
	}
	fetch, decode := -1, -1
	for i, argv := range argvs {
		if len(argv) == 0 {
			continue
		}
		name := peelForPattern(argv)
		switch {
		case slices.Contains(fetchers, name) && fetch < 0:
			fetch = i
		case isDecoder(argv) && decode < 0:
			decode = i
		case runsCode(name):
			if fetch >= 0 && fetch < i {
				a.hardDeny("remote code execution pattern ("+argvs[fetch][0]+" | "+name+")", Privilege)
			}
			if decode >= 0 && decode < i {
				a.hardDeny("decode-to-shell pattern ("+argvs[decode][0]+" | "+name+")", Privilege)
				a.out.Unknown = true
			}
		}
	}
}

// stageArgv renders a pipeline stage's argv without classifying it.
func (a *analyzer) stageArgv(s *syntax.Stmt) []string {
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		out = append(out, w.Lit())
	}
	return out
}

// peelForPattern strips leading wrappers so `sudo bash` and `env sh` are seen.
func peelForPattern(argv []string) string {
	for len(argv) > 0 {
		name := filepath.Base(argv[0])
		if _, isWrapper := wrappers[name]; !isWrapper && !slices.Contains(privilegeWrappers, name) {
			return name
		}
		argv = argv[1:]
		for len(argv) > 0 && (strings.HasPrefix(argv[0], "-") || strings.Contains(argv[0], "=")) {
			argv = argv[1:]
		}
	}
	return ""
}

// runsCode reports names that execute what they read from stdin.
func runsCode(name string) bool {
	return slices.Contains(shells, name) || slices.Contains(interpreters, name) || name == "eval" || name == "source" || slices.Contains(privilegeWrappers, name)
}

// isDecoder recognises base64 -d, xxd -r, openssl enc -d and friends.
func isDecoder(argv []string) bool {
	name := filepath.Base(argv[0])
	has := func(flags ...string) bool {
		for _, f := range argv[1:] {
			if slices.Contains(flags, f) {
				return true
			}
		}
		return false
	}
	switch name {
	case "base64", "base32", "basenc":
		return has("-d", "-D", "--decode")
	case "xxd":
		return has("-r", "-revert")
	case "openssl":
		return len(argv) > 1 && (argv[1] == "enc" || argv[1] == "base64") && has("-d", "-a")
	case "uudecode", "unxz", "gunzip", "zcat", "bunzip2", "unzstd":
		return true
	}
	return false
}
