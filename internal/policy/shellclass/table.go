package shellclass

import (
	"slices"
	"strings"
)

// handler classifies one named command from its arguments (argv[1:]).
type handler func(a *analyzer, name string, args []word) result

// table maps command names to handlers. It is filled by the register*
// functions in init so the per-domain files stay independent.
var table = map[string]handler{}

func register(h handler, names ...string) {
	for _, n := range names {
		table[n] = h
	}
}

func init() {
	registerReaders()
	registerBuildTools()
	registerGit()
	registerFiles()
	registerSystem()
	registerInterpreters()
	registerInfra()
	registerNetwork()
}

// ---- argument helpers -------------------------------------------------------

// isFlag reports whether t is an option ("-x", "--long") rather than "-" or "--".
func isFlag(t string) bool { return strings.HasPrefix(t, "-") && t != "-" && t != "--" }

// nonFlags returns the positional words, honouring "--".
func nonFlags(args []word) []word {
	var out []word
	for i, w := range args {
		if w.text == "--" {
			return append(out, args[i+1:]...)
		}
		if !isFlag(w.text) {
			out = append(out, w)
		}
	}
	return out
}

// texts renders words as strings.
func texts(args []word) []string {
	out := make([]string, len(args))
	for i, w := range args {
		out[i] = w.text
	}
	return out
}

// hasFlag reports whether any argument equals one of flags (or, for a long
// option, starts with "flag=").
func hasFlag(args []word, flags ...string) bool {
	for _, w := range args {
		for _, f := range flags {
			if w.text == f || (strings.HasPrefix(f, "--") && strings.HasPrefix(w.text, f+"=")) {
				return true
			}
		}
	}
	return false
}

// hasShort reports whether a short option cluster ("-rf") contains letter.
func hasShort(args []word, letter byte) bool {
	for _, w := range args {
		t := w.text
		if len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.IndexByte(t[1:], letter) >= 0 {
			return true
		}
	}
	return false
}

// flagValue returns the value of `-o VALUE`, `--opt VALUE` or `--opt=VALUE`.
func flagValue(args []word, names ...string) (word, bool) {
	for i, w := range args {
		for _, n := range names {
			if w.text == n && i+1 < len(args) {
				return args[i+1], true
			}
			if strings.HasPrefix(n, "--") && strings.HasPrefix(w.text, n+"=") {
				return word{text: strings.TrimPrefix(w.text, n+"="), dynamic: w.dynamic}, true
			}
		}
	}
	return word{}, false
}

// first returns the first positional word's text ("" when none).
func first(args []word) string {
	nf := nonFlags(args)
	if len(nf) == 0 {
		return ""
	}
	return nf[0].text
}

// ---- result helpers -----------------------------------------------------------

func safe(reason string) result     { return result{class: SafeRead, reason: reason} }
func mutating(reason string) result { return result{class: MutatingWorkspace, reason: reason} }
func network(reason string) result  { return result{class: Network, reason: reason, network: true} }
func destructive(reason string) result {
	return result{class: Destructive, reason: reason}
}

// installer is a command that downloads software and writes it outside the
// workspace (brew install, go install, npm install -g…). Approving one is
// what makes the tool prefixes writable for that single call, so the verb
// has to be marked here or the install fails on a read-only mount.
func installer(reason string) result {
	r := network(reason)
	r.installs = true
	return r
}

// uninstaller removes or prunes installed software: destructive, and it
// writes the same prefixes, but it needs no network.
func uninstaller(reason string) result {
	r := destructive(reason)
	r.installs = true
	return r
}

func privilegeDeny(reason string) result {
	return result{class: Privilege, reason: reason, hardDeny: reason}
}

// privilegeDenyNet is privilegeDeny for commands that would also have needed
// the network, so NeedsNetwork stays truthful in the audit record.
func privilegeDenyNet(reason string) result {
	r := privilegeDeny(reason)
	r.network = true
	return r
}

func opaque(reason string) result {
	return result{class: MutatingWorkspace, reason: reason, unknown: true}
}

// readFiles declares reads of the given words (dynamic words are skipped;
// the command is already marked dynamic).
func (a *analyzer) readFiles(r *result, files []word) {
	c := Command{}
	for _, f := range files {
		if f.dynamic {
			continue
		}
		a.declareRead(&c, f.text)
	}
	r.reads = append(r.reads, c.Reads...)
	r.raise(c.Class)
}

// writeFiles declares writes of the given words.
func (a *analyzer) writeFiles(r *result, files []word, truncate bool) {
	c := Command{}
	for _, f := range files {
		if f.dynamic {
			continue
		}
		a.declareWrite(&c, f.text, truncate)
	}
	r.writes = append(r.writes, c.Writes...)
	r.raise(c.Class)
	r.reason = joinReason(r.reason, c.Reason)
}

// verbs maps a subcommand to its classification.
type verbs map[string]result

// lookup classifies by first positional argument, falling back to def.
func (v verbs) lookup(args []word, def result) result {
	if r, ok := v[first(args)]; ok {
		return r
	}
	return def
}

// ---- readers ------------------------------------------------------------------

// readerSpec describes a command that reads its file arguments, in enough
// detail to notice the options that make it do something else. Without it an
// option *value* looks like a positional file: `sort main.go -o main.go` read
// as two file reads and classified SafeRead while truncating a tracked file.
type readerSpec struct {
	// skip is how many leading positionals are not files (grep's pattern).
	skip int
	// pattern lists the options that supply that positional themselves, so
	// nothing is skipped when one of them is present (grep -e PAT FILE).
	pattern []string
	// value maps an option to the number of following words it consumes.
	value map[string]int
	// output lists the options whose value is a file the command writes.
	output []string
	// exec lists the options whose value is a program the command runs.
	exec []string
	// outputPositional, when non-zero, is the index (after skip) of the first
	// positional that is an output file rather than an input (xxd in out).
	outputPositional int
}

// reader builds a handler for commands that only read their file arguments.
func reader(skip int) handler { return readerFrom(readerSpec{skip: skip}) }

// readerFrom builds a handler from a spec: option values are not mistaken for
// files, an output option's value becomes a declared write (raising the
// class), and an option naming a program makes the command opaque.
func readerFrom(spec readerSpec) handler {
	return func(a *analyzer, name string, args []word) result {
		files, writes, exec := spec.split(args)
		if exec != "" {
			return opaque(name + " " + exec + " runs a program of the caller's choosing")
		}
		r := safe("")
		a.readFiles(&r, files)
		if len(writes) > 0 {
			r.reason = joinReason(r.reason, name+" writes its output file")
			a.writeFiles(&r, writes, true)
		}
		return r
	}
}

// split separates the input files from the output files. exec names the first
// program-running option found, if any.
func (s readerSpec) split(args []word) (files, writes []word, exec string) {
	var positional []word
	skip, i := s.skip, 0
	for i < len(args) {
		t := args[i].text
		switch {
		case t == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case !isFlag(t):
			positional = append(positional, args[i])
			i++
		default:
			opt, _, _ := strings.Cut(t, "=")
			if slices.Contains(s.exec, opt) {
				return nil, nil, opt
			}
			if slices.Contains(s.pattern, opt) {
				skip = 0
			}
			val, ok, next := optionArg(args, i, s.value[opt])
			if ok && slices.Contains(s.output, opt) {
				writes = append(writes, val)
			}
			i = next
		}
	}
	return s.operands(positional, skip, writes)
}

// operands applies skip and outputPositional to the positional arguments.
func (s readerSpec) operands(positional []word, skip int, writes []word) (files, out []word, exec string) {
	if len(positional) <= skip {
		return nil, writes, ""
	}
	rest := positional[skip:]
	if n := s.outputPositional; n > 0 && len(rest) > n {
		writes = append(writes, rest[n:]...)
		rest = rest[:n]
	}
	return rest, writes, ""
}

// optionArg returns the value of the option at i — `--opt=value` or, when the
// option consumes n following words, `-o value` — and the index after it.
func optionArg(args []word, i, n int) (val word, ok bool, next int) {
	if _, v, eq := strings.Cut(args[i].text, "="); eq {
		return word{text: v, dynamic: args[i].dynamic}, true, i + 1
	}
	if n > 0 && i+1 < len(args) {
		return args[i+1], true, i + 1 + n
	}
	return word{}, false, i + 1
}

// vals builds a value map where each option consumes one following word.
func vals(flags ...string) map[string]int {
	m := make(map[string]int, len(flags))
	for _, f := range flags {
		m[f] = 1
	}
	return m
}

// noFiles is for commands whose positional arguments are not files.
// noFiles is the handler for commands that take no file operands at all, so
// nothing they are handed can become a path. That is what makes a dynamic
// argument to one of them harmless: `echo "exit=$?"` cannot act on the value.
// Contrast a reader such as `ls $(…)`, which does take file operands — it
// simply cannot *declare* the read when the word is dynamic, which is exactly
// when the script must stay opaque.
func noFiles(a *analyzer, name string, args []word) result {
	return result{class: SafeRead, inert: true}
}

// readerSpecs are the readers whose options need inspecting: a value that
// looks like a file, a file they write, or a program they run.
var readerSpecs = map[string]readerSpec{
	"head":     {value: vals("-n", "-c", "--lines", "--bytes")},
	"tail":     {value: vals("-n", "-c", "-s", "--lines", "--bytes", "--sleep-interval", "--pid", "--max-unchanged-stats")},
	"nl":       {value: vals("-b", "-d", "-f", "-h", "-i", "-l", "-n", "-s", "-v", "-w")},
	"od":       {value: vals("-A", "-j", "-N", "-S", "-w", "-t", "--address-radix", "--skip-bytes", "--read-bytes", "--strings", "--width", "--format")},
	"hexdump":  {value: vals("-e", "-f", "-n", "-s")},
	"xxd":      {value: vals("-c", "-g", "-l", "-o", "-s"), outputPositional: 1},
	"strings":  {value: vals("-n", "-t", "-e", "-T", "--bytes", "--radix", "--encoding", "--target")},
	"file":     {value: vals("-f", "-m", "-F", "--files-from", "--magic-file", "--separator")},
	"stat":     {value: vals("-c", "--format", "--printf")},
	"less":     {value: vals("-o", "-O", "-b", "-h", "-j", "-k", "-P", "-x", "-y", "-z", "--log-file"), output: []string{"-o", "-O", "--log-file"}},
	"sort":     {value: vals("-o", "-k", "-t", "-S", "-T", "--output", "--key", "--field-separator", "--buffer-size", "--temporary-directory", "--files0-from", "--compress-program"), output: []string{"-o", "--output"}, exec: []string{"--compress-program"}},
	"uniq":     {value: vals("-f", "-s", "-w", "--skip-fields", "--skip-chars", "--check-chars", "--group", "--all-repeated"), outputPositional: 1},
	"cut":      {value: vals("-b", "-c", "-d", "-f", "--bytes", "--characters", "--delimiter", "--fields", "--output-delimiter")},
	"paste":    {value: vals("-d", "--delimiters")},
	"join":     {value: vals("-1", "-2", "-a", "-e", "-o", "-t", "-v", "--output-delimiter")},
	"comm":     {value: vals("--output-delimiter")},
	"column":   {value: vals("-c", "-s", "-o", "-N", "-l", "-W", "-R", "-H", "-T", "--columns", "--separator", "--output-separator", "--table-columns")},
	"fold":     {value: vals("-w", "--width")},
	"fmt":      {value: vals("-w", "-p", "--width", "--prefix")},
	"expand":   {value: vals("-t", "--tabs")},
	"unexpand": {value: vals("-t", "--tabs")},
	"diff":     {value: vals("-U", "-C", "-W", "-S", "-X", "-D", "--unified", "--context", "--width", "--label", "--starting-file", "--exclude", "--exclude-from", "--ifdef", "--from-file", "--to-file", "--horizon-lines")},
	"cmp":      {value: vals("-i", "-n", "--ignore-initial", "--bytes")},
	"ls":       {value: vals("-I", "-w", "-T", "--ignore", "--hide", "--width", "--tabsize", "--block-size", "--format", "--time-style", "--sort", "--indicator-style", "--quoting-style")},
	"tree":     {value: vals("-L", "-P", "-I", "-o", "-H", "-T", "--filelimit"), output: []string{"-o"}},
	"du":       {value: vals("-d", "-B", "-t", "--max-depth", "--block-size", "--threshold", "--exclude", "--files0-from")},
	"df":       {value: vals("-B", "-t", "-x", "--block-size", "--type", "--exclude-type", "--output")},
	"base64":   {value: vals("-w", "--wrap")},
	"base32":   {value: vals("-w", "--wrap")},
	"jq":       {value: map[string]int{"-f": 1, "--indent": 1, "--arg": 2, "--argjson": 2, "--slurpfile": 2, "--rawfile": 2}, skip: 1},
	"grep":     {skip: 1, pattern: []string{"-e", "-f", "--regexp", "--file"}, value: vals("-e", "-f", "-m", "-A", "-B", "-C", "-d", "-D", "--regexp", "--file", "--max-count", "--after-context", "--before-context", "--context", "--include", "--exclude", "--exclude-dir", "--exclude-from", "--label", "--binary-files", "--devices", "--directories")},
	"rg": {
		skip: 1, pattern: []string{"-e", "-f", "--regexp", "--file"},
		value: vals("-e", "-f", "-m", "-A", "-B", "-C", "-g", "-t", "-T", "-r", "-j", "-E", "--regexp", "--file", "--max-count", "--after-context", "--before-context", "--context", "--glob", "--iglob", "--type", "--type-not", "--type-add", "--replace", "--threads", "--max-depth", "--max-filesize", "--encoding", "--ignore-file", "--path-separator", "--colors", "--sort", "--sortr", "--engine", "--pre", "--pre-glob", "--hostname-bin"),
		exec:  []string{"--pre", "--hostname-bin"},
	},
	"ag":  {skip: 1, pattern: []string{"-G", "--file-search-regex"}, value: vals("-A", "-B", "-C", "-m", "-p", "--pager", "--ignore", "--path-to-ignore", "--depth", "--workers"), exec: []string{"--pager"}},
	"ack": {skip: 1, pattern: []string{"--match"}, value: vals("-A", "-B", "-C", "-m", "--match", "--pager", "--ignore-dir", "--ignore-file", "--type-add", "--type-set"), exec: []string{"--pager"}},
}

func registerReaders() {
	register(reader(0),
		"cat", "more", "wc", "tac", "rev",
		"md5sum", "sha1sum", "sha256sum", "sha512sum", "b2sum", "cksum", "sum",
		"readelf", "objdump", "nm", "ldd",
	)
	for name, spec := range readerSpecs {
		register(readerFrom(spec), name)
	}
	register(readerFrom(readerSpecs["grep"]), "egrep", "fgrep")
	register(handleYq, "yq")
	// awk is not a reader: its first argument is a program that can run
	// shell commands and write files.
	register(handleAwk, "awk", "gawk", "mawk", "nawk", "busybox-awk")
	register(noFiles,
		"echo", "printf", "true", "false", ":", "pwd", "cd", "pushd", "popd", "dirs", "read", "return", "exit",
		"break", "continue", "shift", "wait", "sleep", "date", "uname", "hostname", "id", "whoami", "groups",
		"nproc", "uptime", "free", "ps", "top", "htop", "pgrep", "lsof", "tty", "stty", "printenv", "locale",
		"getconf", "man", "tldr", "help", "info", "apropos", "seq", "expr", "bc", "dc", "yes", "history",
		"which", "whereis", "whatis", "type", "test", "[", "[[", "basename", "dirname", "realpath", "readlink",
		"tr", "set", "shopt", "ulimit", "umask", "getopts", "let", "local", "declare", "typeset", "readonly",
		"export", "true", "arch", "lsblk", "blkid", "dmesg", "journalctl", "lscpu", "lsmem", "lsusb", "lspci",
		"w", "who", "last", "uptime", "cal", "factor", "numfmt", "sha256", "shasum",
	)
	register(handleUnset, "unset")
	register(handleSed, "sed")
	register(handleTee, "tee")
	register(handleFind, "find", "fd")
	register(handleSplit, "split", "csplit")
}

// yqVerbs are the subcommands that precede the expression.
var yqVerbs = []string{"e", "eval", "ea", "eval-all", "r", "read", "w", "write"}

// handleYq: yq's first positional is an expression, not a file, and -i edits
// the files it is given in place — as a plain reader it was a SafeRead that
// rewrote tracked files.
func handleYq(a *analyzer, name string, args []word) result {
	files := nonFlags(args)
	if len(files) > 0 && slices.Contains(yqVerbs, files[0].text) {
		files = files[1:]
	}
	if len(files) > 0 {
		files = files[1:] // the expression
	}
	if !hasFlag(args, "-i", "--inplace", "--in-place") {
		r := safe("yq evaluates an expression")
		a.readFiles(&r, files)
		return r
	}
	r := mutating("yq edits files in place")
	a.writeFiles(&r, files, false)
	return r
}

// handleUnset: unsetting PATH/IFS changes command resolution.
func handleUnset(a *analyzer, name string, args []word) result {
	for _, w := range nonFlags(args) {
		if dangerousVar(w.text) {
			return opaque("unsets " + w.text)
		}
	}
	return safe("")
}

// handleTee writes (truncating unless -a) every positional argument.
func handleTee(a *analyzer, name string, args []word) result {
	r := mutating("")
	append_ := hasFlag(args, "-a", "--append")
	a.writeFiles(&r, nonFlags(args), !append_)
	return r
}

// handleSplit writes chunk files into the working directory.
func handleSplit(a *analyzer, name string, args []word) result {
	r := mutating("writes split files")
	if nf := nonFlags(args); len(nf) > 0 {
		a.readFiles(&r, nf[:1])
	}
	return r
}

// handleFind: paths are reads; -delete is destructive; -exec runs another
// command whose class is folded in (never below mutating).
func handleFind(a *analyzer, name string, args []word) result {
	r := safe("")
	var paths []word
	i := 0
	for i < len(args) && !isFlag(args[i].text) && args[i].text != "(" && args[i].text != "!" {
		paths = append(paths, args[i])
		i++
	}
	a.readFiles(&r, paths)
	for i < len(args) {
		t := args[i].text
		switch t {
		case "-delete":
			r.raise(Destructive)
			r.unknown = true
			r.reason = joinReason(r.reason, "find -delete removes matches")
		case "-exec", "-execdir", "-ok", "-okdir":
			end := i + 1
			for end < len(args) && args[end].text != ";" && args[end].text != "+" {
				end++
			}
			if !a.foldExec(&r, args[i+1:end]) {
				r.unknown = true
			}
			i = end
		case "-fprint", "-fprintf", "-fls", "-fprint0":
			if i+1 < len(args) {
				a.writeFiles(&r, args[i+1:i+2], true)
				i++
			}
			r.unknown = true
		}
		i++
	}
	return r
}

// foldExec classifies the -exec payload ({} placeholders stripped) and merges
// it into r at no less than MutatingWorkspace. The payload's declared reads
// and writes are merged too: path deny rules match on the declared paths, and
// a payload whose paths never surfaced was invisible to them.
//
// safeReader reports a payload that is itself a recognised safe reader. Any
// other payload must not be coverable by a bare `find *` argv-prefix rule —
// the rule cannot see what the payload does — so handleFind marks it opaque.
func (a *analyzer) foldExec(r *result, payload []word) (safeReader bool) {
	inner := make([]word, 0, len(payload))
	for _, w := range payload {
		if w.text != "{}" {
			inner = append(inner, w)
		}
	}
	if len(inner) == 0 {
		return false
	}
	ir := a.classify(inner)
	safeReader = ir.class == SafeRead && !ir.unknown && ir.hardDeny == ""
	ir.raise(MutatingWorkspace)
	r.raise(ir.class)
	r.network = r.network || ir.network
	r.unknown = r.unknown || ir.unknown
	r.reads = append(r.reads, ir.reads...)
	r.writes = append(r.writes, ir.writes...)
	if r.hardDeny == "" {
		r.hardDeny = ir.hardDeny
	}
	r.reason = joinReason(r.reason, "find -exec "+inner[0].text+": "+ir.reason)
	return safeReader
}

// ---- build tools ----------------------------------------------------------------

func registerBuildTools() {
	register(handleGo, "go")
	register(handleCargo, "cargo")
	register(handleNpm, "npm", "pnpm", "yarn", "bun", "bunx", "npx", "corepack")
	register(handlePip, "pip", "pip3", "uv", "uvx", "pipx", "poetry", "pipenv", "conda", "mamba")
	register(handleMaven, "mvn", "mvnw")
	register(handleGradle, "gradle", "gradlew")
	register(handleDotnet, "dotnet")
	register(handleBrew, "brew")
	register(handleGem, "gem", "bundle", "bundler")
	register(handleComposer, "composer")
	register(handleSwiftPM, "swift", "flutter", "dart", "mix", "cabal", "stack", "opam", "bazel", "bazelisk", "sbt", "lein")
	register(func(a *analyzer, name string, args []word) result { return mutating("build tool") },
		"make", "gmake", "cmake", "ninja", "meson", "pytest", "tsc", "esbuild", "webpack", "vite", "rollup", "parcel",
		"eslint", "prettier", "black", "ruff", "mypy", "flake8", "isort", "pylint", "gofmt", "gofumpt", "goimports",
		"golangci-lint", "staticcheck", "revive", "jest", "vitest", "mocha", "rspec", "phpunit", "ctest", "rustc",
		"gcc", "cc", "g++", "c++", "clang", "clang++", "javac", "kotlinc", "swiftc", "zig", "tox", "nox",
		"xcodebuild", "buck", "buck2", "rebar3", "dune", "shellcheck", "shfmt", "hadolint", "yamllint", "markdownlint",
		"terraform-docs", "protoc", "buf", "wasm-pack", "emcc", "nasm", "as", "ld", "ar", "strip", "patch",
	)
	// These download a toolchain into the user's home (~/.rustup, ~/.nvm,
	// ~/.asdf…), so they install as well as fetch.
	register(func(a *analyzer, name string, args []word) result { return installer(name + " downloads a toolchain") },
		"rustup", "nvm", "fnm", "volta", "asdf", "mise", "sdk", "pyenv", "rbenv", "gvm")
	register(func(a *analyzer, name string, args []word) result {
		return network("govulncheck downloads the vulnerability database")
	}, "govulncheck")
}

// goExecFlags name go options whose value is a program the toolchain runs, or
// a file that replaces what it compiles.
var goExecFlags = []string{"-exec", "-toolexec", "-vettool", "-overlay", "-pkgdir"}

// goCompilerFlags pass options through to the compiler and linker, which have
// exec-injection options of their own (-toolexec, -extld).
var goCompilerFlags = []string{"-gcflags", "-ldflags", "-asmflags", "-gccgoflags"}

// goInjection reports the go option that would run a program of the caller's
// choosing. `go test -exec /tmp/evil ./...` ran it under the builtin
// `bash(go test *)` allow rule, which sees only the argv prefix.
func goInjection(args []word) (string, bool) {
	for i, w := range args {
		opt, val, hasEq := strings.Cut(w.text, "=")
		if slices.Contains(goExecFlags, opt) {
			return opt, true
		}
		if !slices.Contains(goCompilerFlags, opt) {
			continue
		}
		if !hasEq && i+1 < len(args) {
			val = args[i+1].text
		}
		if strings.Contains(val, "-toolexec") || strings.Contains(val, "-extld") {
			return opt, true
		}
	}
	return "", false
}

func handleGo(a *analyzer, name string, args []word) result {
	if opt, ok := goInjection(args); ok {
		// Opaque as well as Privilege: an argv-prefix allow rule must never
		// cover a command that names its own executor.
		return result{class: Privilege, unknown: true, reason: "go " + opt + " runs a program of the caller's choosing"}
	}
	sub := first(args)
	rest := nonFlags(args)
	switch sub {
	case "get":
		return network("go get downloads modules")
	case "install":
		// The binary lands in $GOBIN or $GOPATH/bin, outside the workspace.
		return installer("go install downloads and installs a binary")
	case "mod":
		if len(rest) > 1 && rest[1].text == "download" {
			return network("go mod download")
		}
		return mutating("go mod " + strings.Join(texts(rest[min(1, len(rest)):]), " "))
	case "env", "version", "list", "doc", "help", "bug":
		if sub == "env" && hasFlag(args, "-w", "-u") {
			return mutating("writes go env")
		}
		return safe("go " + sub)
	case "build", "test", "vet", "fmt", "run", "generate", "clean", "tool", "work", "fix", "telemetry":
		if sub == "telemetry" && len(rest) > 1 && rest[1].text == "on" {
			return network("enables Go telemetry")
		}
		return mutating("go " + sub)
	case "":
		return safe("go")
	}
	return mutating("go " + sub)
}

func handleCargo(a *analyzer, name string, args []word) result {
	v := verbs{
		"fetch": network("cargo fetch"), "install": installer("cargo install"), "add": network("cargo add"),
		"update": network("cargo update"), "publish": network("cargo publish"), "login": privilegeDeny("cargo login stores credentials"),
		"search": network("cargo search"), "yank": destructive("cargo yank"), "owner": network("cargo owner"),
		"version": safe("cargo version"), "metadata": safe("cargo metadata"), "tree": safe("cargo tree"),
		"clean": destructive("cargo clean removes target/"), "uninstall": uninstaller("cargo uninstall"),
	}
	return v.lookup(args, mutating("cargo "+first(args)))
}

func handleNpm(a *analyzer, name string, args []word) result {
	if name == "npx" || name == "bunx" {
		return network(name + " downloads and runs a package")
	}
	sub := first(args)
	// yarn and pnpm spell a global install `global add <pkg>`; the verb that
	// decides the class is the one after it.
	if rest := texts(nonFlags(args)); sub == "global" && len(rest) > 1 {
		sub = rest[1]
	}
	if r, ok := npmPackageVerb(name, sub, args); ok {
		return r
	}
	switch sub {
	case "publish", "audit", "outdated", "view", "info", "search",
		"dlx", "create", "init", "import", "dedupe", "prune", "pack", "x", "exec", "pm":
		return network(name + " " + sub)
	// `npm doctor` and `npm ping` both contact the registry; classifying them
	// as local reads meant they ran with no network and no prompt.
	case "doctor", "ping":
		return network(name + " " + sub + " contacts the registry")
	case "login", "adduser", "logout", "token", "whoami", "owner", "access", "profile":
		return privilegeDeny(name + " " + sub + " touches registry credentials")
	case "unpublish", "deprecate", "cache":
		return destructive(name + " " + sub)
	case "ls", "list", "why", "explain", "config", "get", "--version", "-v", "help", "bin", "root", "prefix", "":
		if sub == "config" && (hasFlag(args, "set", "delete") || slices.Contains(texts(args), "set")) {
			return mutating("npm config write")
		}
		return safe(name + " " + sub)
	}
	return mutating(name + " " + sub)
}

// npmPackageVerb classifies the verbs that add or remove packages. A global
// one writes the npm prefix, outside the workspace, and so needs the prefix
// mounted writable when the user approves it; a local one only fills
// node_modules and needs nothing but the network.
func npmPackageVerb(name, sub string, args []word) (result, bool) {
	switch sub {
	case "install", "i", "add", "ci", "update", "up", "upgrade", "link", "rebuild":
		if npmGlobal(args) {
			return installer(name + " " + sub + " --global"), true
		}
		return network(name + " " + sub), true
	case "uninstall", "remove", "rm", "un", "unlink":
		if npmGlobal(args) {
			return uninstaller(name + " " + sub + " --global"), true
		}
		return destructive(name + " " + sub), true
	}
	return result{}, false
}

// npmGlobal reports an install that writes the npm prefix instead of the
// workspace's node_modules: `-g`/`--global`, or yarn/pnpm's `global` verb.
func npmGlobal(args []word) bool {
	return hasFlag(args, "-g", "--global", "--location") || slices.Contains(texts(nonFlags(args)), "global")
}

// pipUserTools install into the user's home (~/.local/bin, ~/.local/pipx,
// the uv tool directory) rather than a project virtualenv, so their installs
// need the tool prefixes writable.
var pipUserTools = []string{"pipx", "uv", "uvx", "conda", "mamba"}

func handlePip(a *analyzer, name string, args []word) result {
	sub := first(args)
	switch sub {
	case "install", "sync", "add", "update", "upgrade", "tool", "self", "reinstall", "upgrade-all", "inject", "ensurepath", "python":
		if slices.Contains(pipUserTools, name) || hasFlag(args, "--user") {
			return installer(name + " " + sub)
		}
		return network(name + " " + sub)
	case "download", "lock", "publish", "search", "wheel", "index", "cache", "create", "env":
		return network(name + " " + sub)
	case "uninstall", "remove", "rm":
		if slices.Contains(pipUserTools, name) || hasFlag(args, "--user") {
			return uninstaller(name + " " + sub)
		}
		return destructive(name + " " + sub)
	case "list", "show", "freeze", "check", "config", "debug", "version", "--version", "tree", "export", "info", "":
		// `--outdated` turns the local listing into an index query.
		if hasFlag(args, "-o", "--outdated") {
			return network(name + " " + sub + " --outdated queries the index")
		}
		return safe(name + " " + sub)
	case "run", "shell":
		return mutating(name + " run")
	}
	if name == "uvx" {
		return network("uvx downloads and runs a tool")
	}
	return mutating(name + " " + sub)
}

func handleMaven(a *analyzer, name string, args []word) result {
	for _, t := range texts(nonFlags(args)) {
		switch {
		case t == "deploy" || t == "install" || strings.HasPrefix(t, "dependency:") || strings.HasPrefix(t, "versions:") || t == "release:prepare" || t == "release:perform":
			return network("mvn " + t)
		case t == "clean":
			continue
		}
	}
	return mutating("mvn build")
}

func handleGradle(a *analyzer, name string, args []word) result {
	for _, t := range texts(nonFlags(args)) {
		if strings.HasPrefix(t, "publish") || t == "dependencies" || t == "uploadArchives" {
			return network(name + " " + t)
		}
	}
	if hasFlag(args, "--refresh-dependencies") {
		return network(name + " --refresh-dependencies")
	}
	return mutating(name + " build")
}

func handleDotnet(a *analyzer, name string, args []word) result {
	nf := texts(nonFlags(args))
	sub := first(args)
	switch sub {
	case "restore", "nuget", "workload":
		return network("dotnet " + sub)
	case "add", "tool":
		if len(nf) > 1 && (nf[1] == "package" || nf[1] == "install" || nf[1] == "update") {
			return network("dotnet " + sub + " " + nf[1])
		}
		return mutating("dotnet " + sub)
	case "--version", "--info", "--list-sdks", "--list-runtimes", "help", "":
		return safe("dotnet " + sub)
	}
	return mutating("dotnet " + sub)
}

// brewLocalFlags are the query options that only print what is already on
// disk. They are options, not verbs, so `verbs.lookup` (which reads the first
// *positional* word) can never see them.
var brewLocalFlags = []string{"--prefix", "--cellar", "--caskroom", "--repository", "--repo", "--version", "--cache", "--env"}

// handleBrew: only a handful of brew verbs are local. `info`, `deps`,
// `outdated`, `search` and `doctor` all query the Homebrew API or a tap's git
// remote, so classifying them as safe reads gave them no network *and* no
// prompt at which to ask for it: `brew info fpc` failed with "Could not
// connect" after the user had allowed it.
func handleBrew(a *analyzer, name string, args []word) result {
	if hasFlag(args, brewLocalFlags...) {
		return safe("brew " + strings.Join(texts(args), " "))
	}
	v := verbs{
		// Local: these read the installed Cellar and the local taps only.
		"list": safe("brew list"), "ls": safe("brew ls"), "config": safe("brew config"),
		"leaves": safe("brew leaves"), "--version": safe("brew version"), "": safe("brew"),
		// Writes the Homebrew prefix and its cache.
		"install": installer("brew install"), "reinstall": installer("brew reinstall"),
		"upgrade": installer("brew upgrade"), "fetch": installer("brew fetch"),
		"tap": installer("brew tap"), "link": installer("brew link"), "unlink": installer("brew unlink"),
		// update rewrites Homebrew's own git repositories under the prefix,
		// so it needs the same writable prefix an install does.
		"update": installer("brew update"), "update-reset": installer("brew update-reset"),
		"pin": installer("brew pin"), "unpin": installer("brew unpin"),
		"uninstall": uninstaller("brew uninstall"), "remove": uninstaller("brew remove"),
		"rm": uninstaller("brew rm"), "cleanup": uninstaller("brew cleanup"),
		"autoremove": uninstaller("brew autoremove"), "untap": uninstaller("brew untap"),
	}
	return v.lookup(args, network("brew "+first(args)))
}

func handleGem(a *analyzer, name string, args []word) result {
	sub := first(args)
	switch sub {
	case "install", "update":
		if name == "gem" {
			// Gems land in GEM_HOME (~/.gem), outside the workspace; a
			// bundler install fills the project's vendor/bundle.
			return installer(name + " " + sub)
		}
		return network(name + " " + sub)
	case "push", "fetch", "add", "outdated", "lock", "search", "owner", "yank", "signin", "signout":
		return network(name + " " + sub)
	case "uninstall", "remove", "clean", "cleanup":
		if name == "gem" {
			return uninstaller(name + " " + sub)
		}
		return destructive(name + " " + sub)
	case "list", "info", "show", "env", "help", "version", "--version", "check", "contents", "which", "config", "":
		// `--remote`/`-r` asks rubygems.org instead of the local index.
		if hasFlag(args, "-r", "--remote", "--both") {
			return network(name + " " + sub + " --remote queries rubygems.org")
		}
		return safe(name + " " + sub)
	}
	return mutating(name + " " + sub)
}

func handleComposer(a *analyzer, name string, args []word) result {
	v := verbs{
		"install": network("composer install"), "update": network("composer update"), "require": network("composer require"),
		"create-project": network("composer create-project"), "outdated": network("composer outdated"),
		"remove": destructive("composer remove"),
		"show":   safe("composer show"), "validate": safe("composer validate"), "--version": safe("composer version"), "licenses": safe("composer licenses"),
	}
	return v.lookup(args, mutating("composer "+first(args)))
}

// handleSwiftPM covers language package managers with a shared verb shape.
func handleSwiftPM(a *analyzer, name string, args []word) result {
	nf := texts(nonFlags(args))
	joined := " " + strings.Join(nf, " ") + " "
	switch {
	case strings.Contains(joined, " pub get ") || strings.Contains(joined, " deps.get ") || strings.Contains(joined, " package resolve ") ||
		strings.Contains(joined, " package update ") || strings.Contains(joined, " install ") || strings.Contains(joined, " update ") || strings.Contains(joined, " fetch ") || strings.Contains(joined, " sync "):
		return network(name + " dependency fetch")
	case len(nf) > 0 && (nf[0] == "--version" || nf[0] == "version" || nf[0] == "info" || nf[0] == "list"):
		return safe(name + " " + nf[0])
	}
	return mutating(name + " build/test")
}
