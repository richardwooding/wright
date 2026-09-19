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

// reader builds a handler for commands that only read their file arguments;
// skip positional arguments (e.g. the grep pattern) are not treated as files.
func reader(skip int) handler {
	return func(a *analyzer, name string, args []word) result {
		r := safe("")
		files := nonFlags(args)
		if len(files) > skip {
			a.readFiles(&r, files[skip:])
		}
		return r
	}
}

// noFiles is for commands whose positional arguments are not files.
func noFiles(a *analyzer, name string, args []word) result { return safe("") }

func registerReaders() {
	register(reader(0),
		"cat", "head", "tail", "less", "more", "wc", "tac", "rev", "nl", "od", "hexdump", "xxd", "strings",
		"file", "stat", "md5sum", "sha1sum", "sha256sum", "sha512sum", "b2sum", "cksum", "sum",
		"sort", "uniq", "cut", "paste", "join", "comm", "column", "fold", "fmt", "expand", "unexpand",
		"diff", "cmp", "ls", "tree", "du", "df", "base64", "base32", "jq", "yq", "readelf", "objdump", "nm", "ldd", "gzip -l",
	)
	register(reader(1), "grep", "egrep", "fgrep", "rg", "ag", "ack", "awk", "gawk", "mawk", "nawk")
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

// handleUnset: unsetting PATH/IFS changes command resolution.
func handleUnset(a *analyzer, name string, args []word) result {
	for _, w := range nonFlags(args) {
		if dangerousVar(w.text) {
			return opaque("unsets " + w.text)
		}
	}
	return safe("")
}

// handleSed: -i edits files in place (mutating, protected targets denied);
// otherwise sed only reads.
func handleSed(a *analyzer, name string, args []word) result {
	inPlace := hasShort(args, 'i') || hasFlag(args, "--in-place")
	files := nonFlags(args)
	if !hasFlag(args, "-e", "-f", "--expression", "--file") && len(files) > 0 {
		files = files[1:] // the script
	}
	if !inPlace {
		r := safe("")
		a.readFiles(&r, files)
		return r
	}
	r := mutating("edits files in place")
	a.writeFiles(&r, files, false)
	return r
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
			r.reason = joinReason(r.reason, "find -delete removes matches")
		case "-exec", "-execdir", "-ok", "-okdir":
			end := i + 1
			for end < len(args) && args[end].text != ";" && args[end].text != "+" {
				end++
			}
			a.foldExec(&r, args[i+1:end])
			i = end
		case "-fprint", "-fprintf", "-fls", "-fprint0":
			if i+1 < len(args) {
				a.writeFiles(&r, args[i+1:i+2], true)
				i++
			}
		}
		i++
	}
	return r
}

// foldExec classifies the -exec payload ({} placeholders stripped) and merges
// it into r at no less than MutatingWorkspace.
func (a *analyzer) foldExec(r *result, payload []word) {
	inner := make([]word, 0, len(payload))
	for _, w := range payload {
		if w.text != "{}" {
			inner = append(inner, w)
		}
	}
	if len(inner) == 0 {
		return
	}
	ir := a.classify(inner)
	ir.raise(MutatingWorkspace)
	r.raise(ir.class)
	r.network = r.network || ir.network
	r.unknown = r.unknown || ir.unknown
	if r.hardDeny == "" {
		r.hardDeny = ir.hardDeny
	}
	r.reason = joinReason(r.reason, "find -exec "+inner[0].text+": "+ir.reason)
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
	register(func(a *analyzer, name string, args []word) result { return network("downloads toolchain") }, "rustup", "govulncheck", "nvm", "fnm", "volta", "asdf", "mise", "sdk", "pyenv", "rbenv", "gvm")
}

func handleGo(a *analyzer, name string, args []word) result {
	sub := first(args)
	rest := nonFlags(args)
	switch sub {
	case "get", "install":
		return network("go " + sub + " downloads modules")
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
		"fetch": network("cargo fetch"), "install": network("cargo install"), "add": network("cargo add"),
		"update": network("cargo update"), "publish": network("cargo publish"), "login": privilegeDeny("cargo login stores credentials"),
		"search": network("cargo search"), "yank": destructive("cargo yank"), "owner": network("cargo owner"),
		"version": safe("cargo version"), "metadata": safe("cargo metadata"), "tree": safe("cargo tree"),
		"clean": destructive("cargo clean removes target/"),
	}
	return v.lookup(args, mutating("cargo "+first(args)))
}

func handleNpm(a *analyzer, name string, args []word) result {
	if name == "npx" || name == "bunx" {
		return network(name + " downloads and runs a package")
	}
	sub := first(args)
	switch sub {
	case "install", "i", "add", "ci", "update", "up", "upgrade", "publish", "audit", "outdated", "view", "info", "search",
		"dlx", "create", "init", "link", "import", "dedupe", "prune", "rebuild", "pack", "x", "exec", "pm":
		return network(name + " " + sub)
	case "login", "adduser", "logout", "token", "whoami", "owner", "access", "profile":
		return privilegeDeny(name + " " + sub + " touches registry credentials")
	case "uninstall", "remove", "rm", "un", "unlink", "unpublish", "deprecate", "cache":
		return destructive(name + " " + sub)
	case "ls", "list", "why", "explain", "config", "get", "--version", "-v", "help", "bin", "root", "prefix", "doctor", "ping", "":
		if sub == "config" && (hasFlag(args, "set", "delete") || slices.Contains(texts(args), "set")) {
			return mutating("npm config write")
		}
		return safe(name + " " + sub)
	}
	return mutating(name + " " + sub)
}

func handlePip(a *analyzer, name string, args []word) result {
	sub := first(args)
	switch sub {
	case "install", "download", "sync", "add", "lock", "publish", "search", "update", "upgrade", "wheel", "index", "tool", "python", "self", "cache", "create", "env", "inject", "reinstall", "upgrade-all", "ensurepath":
		return network(name + " " + sub)
	case "uninstall", "remove", "rm":
		return destructive(name + " " + sub)
	case "list", "show", "freeze", "check", "config", "debug", "version", "--version", "tree", "export", "info", "":
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

func handleBrew(a *analyzer, name string, args []word) result {
	v := verbs{
		"list": safe("brew list"), "ls": safe("brew ls"), "info": safe("brew info"), "deps": safe("brew deps"),
		"config": safe("brew config"), "doctor": safe("brew doctor"), "--version": safe("brew version"),
		"--prefix": safe("brew prefix"), "outdated": safe("brew outdated"), "leaves": safe("brew leaves"),
		"uninstall": destructive("brew uninstall"), "remove": destructive("brew remove"), "rm": destructive("brew rm"),
		"cleanup": destructive("brew cleanup"), "autoremove": destructive("brew autoremove"),
	}
	return v.lookup(args, network("brew "+first(args)))
}

func handleGem(a *analyzer, name string, args []word) result {
	sub := first(args)
	switch sub {
	case "install", "update", "push", "fetch", "add", "outdated", "lock", "search", "owner", "yank", "signin", "signout":
		return network(name + " " + sub)
	case "uninstall", "remove", "clean", "cleanup":
		return destructive(name + " " + sub)
	case "list", "info", "show", "env", "help", "version", "--version", "check", "contents", "which", "config", "":
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
