package shellclass

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// analyzer carries the state of one Analyze call. Sub-analyses (sh -c '…')
// share the same out but run at depth+1.
type analyzer struct {
	ws    Workspace
	out   *Analysis
	depth int
	// floor is a class lower bound raised by pattern-level findings (curl|sh)
	// that do not belong to a single command.
	floor Class
	// funcs are the function names this script defines; a call to one is
	// opaque rather than an unrecognised program.
	funcs []string
}

// word is an expanded shell word with its dynamic flag.
type word struct {
	text    string
	dynamic bool
}

// run parses and walks one script.
func (a *analyzer) run(script string) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(script), "")
	if err != nil {
		a.unknown("shell script does not parse: " + firstLine(err.Error()))
		return
	}
	a.walkStmts(file.Stmts)
}

// finish aggregates the per-command results into the analysis.
func (a *analyzer) finish() {
	out := a.out
	out.Class = max(out.Class, a.floor)
	for _, c := range out.Commands {
		out.Class = max(out.Class, c.Class)
		if c.Network {
			out.NeedsNetwork = true
		}
		if c.Installs {
			out.Installs = true
		}
	}
	out.Reasons = dedupe(out.Reasons)
}

// unknown marks the analysis opaque with a reason.
func (a *analyzer) unknown(reason string) {
	a.out.Unknown = true
	a.reason(reason)
}

// hardDeny records the first hard-deny reason and raises the class floor.
func (a *analyzer) hardDeny(reason string, class Class) {
	if a.out.HardDeny == "" {
		a.out.HardDeny = reason
	}
	a.floor = max(a.floor, class)
	a.reason(reason)
}

func (a *analyzer) reason(r string) {
	if r != "" {
		a.out.Reasons = append(a.out.Reasons, r)
	}
}

// add appends a classified command and mirrors its findings.
func (a *analyzer) add(c Command) {
	if c.Dynamic {
		a.out.Unknown = true
	}
	if c.Unrecognised && len(c.Argv) > 0 && slices.Contains(a.funcs, c.Argv[0]) {
		// A call to a function defined in this same script: opaque, as it
		// was before the unrecognised/opaque split.
		c.Unrecognised = false
		a.out.Unknown = true
	}
	if c.Unrecognised && len(c.Argv) > 0 && !slices.Contains(a.out.Unrecognised, c.Argv[0]) {
		a.out.Unrecognised = append(a.out.Unrecognised, c.Argv[0])
	}
	a.reason(c.Reason)
	a.out.Commands = append(a.out.Commands, c)
}

func (a *analyzer) walkStmts(stmts []*syntax.Stmt) {
	for _, s := range stmts {
		a.walkStmt(s)
	}
}

// walkStmt dispatches on the statement's command kind. Redirects attached to
// a simple command become that command's writes/reads; redirects on compound
// commands are attached to a synthetic "(redirect)" command.
func (a *analyzer) walkStmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	if call, ok := s.Cmd.(*syntax.CallExpr); ok {
		a.walkCall(call, s.Redirs)
		return
	}
	a.walkCompound(s.Cmd)
	if len(s.Redirs) > 0 {
		c := Command{Argv: []string{"(redirect)"}}
		a.redirects(s.Redirs, &c)
		a.add(c)
	}
}

// walkCompound descends into every compound command kind.
func (a *analyzer) walkCompound(node syntax.Command) {
	switch cmd := node.(type) {
	case *syntax.BinaryCmd:
		a.walkBinary(cmd)
	case *syntax.Subshell:
		a.walkStmts(cmd.Stmts)
	case *syntax.Block:
		a.walkStmts(cmd.Stmts)
	case *syntax.IfClause:
		a.walkIf(cmd)
	case *syntax.WhileClause:
		a.walkStmts(cmd.Cond)
		a.walkStmts(cmd.Do)
	case *syntax.ForClause:
		a.walkFor(cmd)
	case *syntax.CaseClause:
		a.expand(cmd.Word)
		for _, item := range cmd.Items {
			a.walkStmts(item.Stmts)
		}
	case *syntax.FuncDecl:
		// Remember the name before walking the body, so a later call to it
		// is not mistaken for an unrecognised *program*. A function name is
		// chosen by whoever wrote the script, so a rule naming one would
		// vouch for nothing; the body's own commands are analysed here and
		// have to be covered on their own terms.
		// Name is nil for a malformed declaration: "()0" parses as a
		// FuncDecl with no name, which the fuzzer found the moment this
		// started reading it.
		if cmd.Name != nil {
			a.funcs = append(a.funcs, cmd.Name.Value)
		}
		a.walkStmt(cmd.Body)
	case *syntax.TimeClause:
		a.walkStmt(cmd.Stmt)
	case *syntax.CoprocClause:
		a.walkStmt(cmd.Stmt)
	case *syntax.DeclClause:
		a.walkDecl(cmd)
	case *syntax.ArithmCmd, *syntax.LetClause, *syntax.TestClause:
		// Pure shell arithmetic/tests: no external effect.
	default:
		a.unknown("unsupported shell construct")
	}
}

func (a *analyzer) walkIf(cmd *syntax.IfClause) {
	for c := cmd; c != nil; c = c.Else {
		a.walkStmts(c.Cond)
		a.walkStmts(c.Then)
	}
}

func (a *analyzer) walkFor(cmd *syntax.ForClause) {
	if wi, ok := cmd.Loop.(*syntax.WordIter); ok {
		for _, w := range wi.Items {
			a.expand(w)
		}
	}
	a.walkStmts(cmd.Do)
}

// walkDecl handles export/declare/local/readonly/typeset. Exporting PATH or
// IFS changes what later commands resolve to, so it makes the script opaque.
func (a *analyzer) walkDecl(cmd *syntax.DeclClause) {
	for _, as := range cmd.Args {
		if as.Name != nil && dangerousVar(as.Name.Value) {
			a.unknown("overrides " + as.Name.Value)
		}
		if as.Value != nil {
			a.expand(as.Value)
		}
	}
}

// walkBinary handles &&, ||, | and |&. Pipelines are flattened so the
// remote-code and decode-to-shell patterns can look across stages.
func (a *analyzer) walkBinary(cmd *syntax.BinaryCmd) {
	if cmd.Op != syntax.Pipe && cmd.Op != syntax.PipeAll {
		a.walkStmt(cmd.X)
		a.walkStmt(cmd.Y)
		return
	}
	stages := flattenPipeline(cmd)
	a.checkPipeline(stages)
	for _, s := range stages {
		a.walkStmt(s)
	}
}

// flattenPipeline turns the left-nested BinaryCmd chain into its stages.
func flattenPipeline(cmd *syntax.BinaryCmd) []*syntax.Stmt {
	var stages []*syntax.Stmt
	var rec func(s *syntax.Stmt)
	rec = func(s *syntax.Stmt) {
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) && len(s.Redirs) == 0 {
			rec(b.X)
			rec(b.Y)
			return
		}
		stages = append(stages, s)
	}
	rec(cmd.X)
	rec(cmd.Y)
	return stages
}

// walkCall expands a simple command, classifies it and attaches redirects.
func (a *analyzer) walkCall(call *syntax.CallExpr, redirs []*syntax.Redirect) {
	env, envDanger := a.assigns(call.Assigns)
	words := a.expandAll(call.Args)
	if len(words) == 0 {
		// Bare assignment statement (X=1). Assigning PATH/IFS is opaque.
		if envDanger != "" {
			a.unknown("assigns " + envDanger)
		}
		if len(redirs) > 0 {
			c := Command{Argv: []string{"(redirect)"}, Env: env}
			a.redirects(redirs, &c)
			a.add(c)
		}
		return
	}
	c := a.classifyWords(words)
	c.Env = env
	if envDanger != "" {
		c.Dynamic = true
		c.Reason = joinReason(c.Reason, "overrides "+envDanger)
	}
	a.redirects(redirs, &c)
	// A redirect gives an otherwise inert command a path to act on, and it
	// is attached here rather than by the classifier, so the decision to
	// forgive its dynamic argument has to be revisited once it is known:
	// `echo "$X"` cannot act on the value, `echo "$X" > f` can.
	if c.dynamicArgs && (len(c.Writes) > 0 || len(c.Reads) > 0) {
		c.Dynamic = true
	}
	a.add(c)
}

// assigns renders NAME=value prefixes and reports a dangerous variable name.
func (a *analyzer) assigns(as []*syntax.Assign) (env []string, danger string) {
	for _, s := range as {
		if s.Name == nil {
			continue
		}
		val := ""
		if s.Value != nil {
			w := a.expand(s.Value)
			val = w.text
		}
		env = append(env, s.Name.Value+"="+val)
		if dangerousVar(s.Name.Value) {
			danger = s.Name.Value
		}
	}
	return env, danger
}

// loaderVars change command resolution or load code into every later
// process, whatever that process turns out to be.
var loaderVars = []string{
	"PATH", "IFS", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "PROMPT_COMMAND",
	"PYTHONSTARTUP", "NODE_OPTIONS", "SHELLOPTS", "BASHOPTS",
}

// gitProgramVars are the environment spelling of the git config keys that
// point git at a program (GIT_EXTERNAL_DIFF=… git diff runs that program on
// every file).
var gitProgramVars = []string{
	"GIT_SSH", "GIT_SSH_COMMAND", "GIT_EXEC_PATH", "GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS", "GIT_EDITOR",
	"GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_ASKPASS", "GIT_PROXY_COMMAND", "GIT_TEMPLATE_DIR",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_ATTR_NOSYSTEM",
}

// injectingEnvVars are the build and toolchain variables whose *value* is
// code, or names the program that compiles, links, wraps or fetches. They
// belong here rather than in the command table because an environment prefix
// never appears in the argv an allow rule matches: with
// `GOFLAGS=-toolexec=./pwn.sh go build ./...` the builtin `bash(go build *)`
// rule sees only `go build ./...`, so without this list the script ran with
// no prompt at all.
var injectingEnvVars = []string{
	// Go: -toolexec/-ldflags smuggled through GOFLAGS, a replacement
	// toolchain root, the module fetch path and the checksum switches.
	"GOFLAGS", "GOEXPERIMENT", "GOPROXY", "GOPRIVATE", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK",
	"GOROOT", "GCCGO", "GOGCCFLAGS",
	// The C/C++ toolchain, reached by cgo, make and every autotools build.
	"CC", "CXX", "CPP", "LD", "CFLAGS", "CXXFLAGS", "CPPFLAGS", "LDFLAGS",
	"CGO_CFLAGS", "CGO_CXXFLAGS", "CGO_CPPFLAGS", "CGO_LDFLAGS", "CGO_FFLAGS",
	// Rust and cargo (CARGO_TARGET_<triple>_RUNNER and _LINKER are matched by
	// prefix below).
	"RUSTFLAGS", "RUSTDOCFLAGS", "RUSTC", "RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER",
	"CARGO_BUILD_RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS",
	// make reads MAKEFLAGS as command line and MAKEFILES as extra rules.
	"MAKEFLAGS", "MAKEFILES",
	// Package managers: a registry of the caller's choosing supplies the code
	// the install step then executes.
	"PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL", "PIP_FIND_LINKS", "NPM_CONFIG_REGISTRY", "YARN_REGISTRY",
	// JVM: -javaagent in any of these starts a program inside the build.
	"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "JAVA_OPTS", "MAVEN_OPTS", "GRADLE_OPTS", "SBT_OPTS",
	// Interpreter option variables that run code before the script does.
	"PERL5OPT", "PERL5LIB", "RUBYOPT",
}

// InjectingEnvVars returns the environment variables whose assignment turns
// an otherwise ordinary build command into arbitrary code execution, sorted,
// as a copy the caller may modify. A variable that injects code into an
// allowed command is as good as an allowed command, so the sandbox strips the
// same names from a sandboxed command's environment; reading this list there
// too keeps the two from drifting apart.
func InjectingEnvVars() []string {
	out := slices.Clone(injectingEnvVars)
	slices.Sort(out)
	return out
}

// dangerousVar names variables whose assignment changes command resolution,
// loads code into every later process, points git at a program to run, or
// injects code into a build.
func dangerousVar(name string) bool {
	if slices.Contains(loaderVars, name) || slices.Contains(gitProgramVars, name) || slices.Contains(injectingEnvVars, name) {
		return true
	}
	return strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GIT_CONFIG") ||
		strings.HasPrefix(name, "CARGO_TARGET_")
}

func (a *analyzer) expandAll(ws []*syntax.Word) []word {
	out := make([]word, 0, len(ws))
	for _, w := range ws {
		out = append(out, a.expand(w))
	}
	return out
}

// expand renders a word literally where possible. Parameter expansions,
// command substitutions, arithmetic and globs make the word dynamic; the
// commands inside $(…) and <(…) are walked because they do execute.
func (a *analyzer) expand(w *syntax.Word) word {
	if w == nil {
		return word{}
	}
	return a.expandParts(w.Parts, false)
}

func (a *analyzer) expandParts(parts []syntax.WordPart, quoted bool) word {
	var b strings.Builder
	dynamic := false
	for _, p := range parts {
		part := a.expandPart(p, quoted)
		b.WriteString(part.text)
		dynamic = dynamic || part.dynamic
	}
	return word{text: b.String(), dynamic: dynamic}
}

func (a *analyzer) expandPart(p syntax.WordPart, quoted bool) word {
	switch x := p.(type) {
	case *syntax.Lit:
		return literal(x.Value, quoted)
	case *syntax.SglQuoted:
		return word{text: x.Value}
	case *syntax.DblQuoted:
		return a.expandParts(x.Parts, true)
	case *syntax.ParamExp:
		return a.param(x)
	case *syntax.CmdSubst:
		a.walkStmts(x.Stmts)
		return word{text: "$(…)", dynamic: true}
	case *syntax.ProcSubst:
		a.walkStmts(x.Stmts)
		return word{text: "<(…)", dynamic: true}
	case *syntax.ArithmExp:
		return word{text: "$((…))", dynamic: true}
	default:
		return word{text: "?", dynamic: true}
	}
}

// literal unescapes a bare literal and detects unquoted glob characters.
func literal(v string, quoted bool) word {
	if quoted {
		return word{text: unescape(v)}
	}
	return word{text: unescape(v), dynamic: isGlob(v)}
}

// isGlob reports whether an unquoted literal would be pathname-expanded:
// `*`, `?`, or a bracket expression. A lone `[` (the test builtin) is not.
func isGlob(v string) bool {
	if strings.ContainsAny(v, "*?") {
		return true
	}
	open := strings.IndexByte(v, '[')
	return open >= 0 && strings.IndexByte(v[open+1:], ']') >= 0
}

// unescape drops backslashes that quote the following character.
func unescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	esc := false
	for _, r := range v {
		if esc {
			b.WriteRune(r)
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// param renders $NAME. $HOME and $PWD are treated as literal because they
// are stable and needed to recognise `rm -rf $HOME`; anything else is dynamic.
func (a *analyzer) param(x *syntax.ParamExp) word {
	if x.Param == nil {
		return word{text: "${…}", dynamic: true}
	}
	name := x.Param.Value
	plain := x.Index == nil && x.Slice == nil && x.Repl == nil && x.Exp == nil && !x.Length && !x.Width && !x.Excl
	switch {
	case plain && name == "HOME":
		return word{text: a.ws.Home()}
	case plain && name == "PWD":
		return word{text: a.root()}
	}
	return word{text: "$" + name, dynamic: true}
}

// root is the primary workspace root ("" if none).
func (a *analyzer) root() string {
	if roots := a.ws.Roots(); len(roots) > 0 {
		return roots[0]
	}
	return ""
}

// redirect operators that write to their target.
func writesTarget(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob, syntax.RdrInOut, syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll:
		return true
	}
	return false
}

// truncates reports whether op discards the target's existing content.
func truncates(op syntax.RedirOperator) bool {
	switch op {
	case syntax.RdrOut, syntax.RdrClob, syntax.RdrAll, syntax.RdrAllClob:
		return true
	}
	return false
}

// redirects folds the statement's redirects into c: writes may be
// destructive (truncating a tracked file) or hard-denied (protected or
// secret target); `<` of a secret file is hard-denied.
func (a *analyzer) redirects(redirs []*syntax.Redirect, c *Command) {
	for _, r := range redirs {
		switch {
		case writesTarget(r.Op), r.Op == syntax.DplOut && !isFDWord(r.Word):
			a.redirectWrite(r, c)
		case r.Op == syntax.RdrIn:
			a.redirectRead(r, c)
		}
	}
}

// isFDWord reports whether a >& target is a file descriptor (2>&1, >&-).
func isFDWord(w *syntax.Word) bool {
	if w == nil {
		return true
	}
	t := w.Lit()
	if t == "-" {
		return true
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return t != ""
}

func (a *analyzer) redirectWrite(r *syntax.Redirect, c *Command) {
	w := a.expand(r.Word)
	if w.dynamic {
		c.Dynamic = true
		c.Reason = joinReason(c.Reason, "dynamic redirect target")
		return
	}
	if isDevSink(w.text) {
		return
	}
	a.declareWrite(c, w.text, truncates(r.Op))
}

// declareWrite records a write target and applies the tracked/protected rules.
func (a *analyzer) declareWrite(c *Command, target string, truncate bool) {
	abs, _, err := a.ws.Resolve(target)
	if err != nil {
		c.Dynamic = true
		return
	}
	c.Writes = append(c.Writes, abs)
	rel := a.display(abs)
	switch {
	case a.ws.IsProtected(abs) || a.ws.IsSecretFile(abs):
		a.hardDeny("writes protected path "+rel, Destructive)
		c.Class = max(c.Class, Destructive)
	case truncate && a.ws.IsTracked(abs):
		c.Class = max(c.Class, Destructive)
		c.Reason = joinReason(c.Reason, "truncates tracked file "+rel)
	default:
		c.Class = max(c.Class, MutatingWorkspace)
		c.Reason = joinReason(c.Reason, "writes "+rel)
	}
}

func (a *analyzer) redirectRead(r *syntax.Redirect, c *Command) {
	w := a.expand(r.Word)
	if w.dynamic {
		c.Dynamic = true
		return
	}
	a.declareRead(c, w.text)
}

// declareRead records a read target; reading a secret file is hard-denied.
func (a *analyzer) declareRead(c *Command, target string) {
	if target == "-" || isDevSink(target) {
		return
	}
	abs, _, err := a.ws.Resolve(target)
	if err != nil {
		c.Dynamic = true
		return
	}
	c.Reads = append(c.Reads, abs)
	if a.ws.IsSecretFile(abs) {
		a.hardDeny("reads secret file "+a.display(abs), Destructive)
		c.Class = max(c.Class, Destructive)
	}
}

// isDevSink reports targets that are not files.
func isDevSink(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin", "/dev/tty", "/dev/zero":
		return true
	}
	return strings.HasPrefix(p, "/dev/fd/") || strings.HasPrefix(p, "/proc/self/fd/")
}

// display renders abs relative to the workspace root when inside it.
func (a *analyzer) display(abs string) string {
	root := a.root()
	if root != "" && strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return abs[len(root)+1:]
	}
	if home := a.ws.Home(); home != "" && strings.HasPrefix(abs, home+string(filepath.Separator)) {
		return "~/" + abs[len(home)+1:]
	}
	return abs
}

func joinReason(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + "; " + add
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
