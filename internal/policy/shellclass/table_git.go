package shellclass

import (
	"path"
	"slices"
	"strings"
)

// defaultProtectedBranches are the branches a forced or deleting push is
// hard-denied for unless the workspace overrides them.
var defaultProtectedBranches = []string{"main", "master", "release/*"}

var (
	gitSafeRead = map[string]bool{
		"status": true, "diff": true, "log": true, "show": true, "rev-parse": true, "describe": true,
		"shortlog": true, "ls-files": true, "ls-tree": true, "cat-file": true, "rev-list": true, "name-rev": true,
		"help": true, "version": true, "--version": true, "check-ignore": true, "check-attr": true,
		"merge-base": true, "diff-tree": true, "diff-index": true, "diff-files": true, "for-each-ref": true, "var": true,
		"count-objects": true, "fsck": true, "whatchanged": true, "range-diff": true, "show-ref": true, "verify-commit": true,
		"verify-tag": true, "cherry": true, "show-branch": true, "status-porcelain": true,
	}
	gitMutating = map[string]bool{
		"add": true, "commit": true, "switch": true, "merge": true, "rebase": true, "cherry-pick": true, "revert": true,
		"mv": true, "rm": true, "apply": true, "am": true, "init": true, "notes": true, "update-index": true,
		"replace": true, "commit-tree": true, "write-tree": true, "read-tree": true, "mktree": true, "hash-object": true,
		"fast-import": true, "fast-export": true, "format-patch": true, "archive": true, "bundle": true, "mailinfo": true,
		"mergetool": true, "difftool": true, "rerere": true, "sparse-checkout": true, "maintenance": true, "symbolic-ref": true,
		"pack-refs": true, "repack": true, "lfs": true,
	}
	gitNetwork = map[string]bool{
		"fetch": true, "pull": true, "clone": true, "ls-remote": true, "request-pull": true, "svn": true, "send-email": true,
	}
	gitDestructive = map[string]bool{
		"restore": true, "prune": true, "filter-branch": true, "filter-repo": true, "prune-packed": true,
	}
)

func registerGit() { register(handleGit, "git") }

// handleGit peels global options (-C dir, -c key=value, --git-dir) and
// dispatches on the subcommand. `-c core.hooksPath=…` is hard-denied because
// a hook path pointed at the workspace turns the next `git commit` into
// arbitrary code execution.
func handleGit(a *analyzer, name string, args []word) result {
	i, r, done := gitGlobalOptions(args)
	if done {
		return r
	}
	if i >= len(args) {
		return safe("git")
	}
	sub := args[i].text
	rest := args[i+1:]
	if h, ok := gitSubcommands[sub]; ok {
		return h(a, rest)
	}
	switch {
	case gitSafeRead[sub]:
		return safe("git " + sub)
	case gitNetwork[sub]:
		return network("git " + sub)
	case gitDestructive[sub]:
		return destructive("git " + sub + " discards data")
	case gitMutating[sub]:
		return mutating("git " + sub)
	}
	return opaque("unknown git subcommand " + sub)
}

// gitGlobalOptions skips git's global options and returns the index of the
// subcommand. done is true when an option itself decided the outcome.
func gitGlobalOptions(args []word) (i int, r result, done bool) {
	for i < len(args) && isFlag(args[i].text) {
		t := args[i].text
		if setting, n, ok := gitConfigArg(args, i); ok {
			if r, bad := gitConfigOption(setting); bad {
				return i, r, true
			}
			i += n
			continue
		}
		switch {
		case t == "--exec-path" || strings.HasPrefix(t, "--exec-path="):
			return i, opaque("git --exec-path overrides the git binaries"), true
		case t == "-C" || t == "--git-dir" || t == "--work-tree" || t == "--namespace":
			i += 2
		default:
			i++
		}
	}
	return i, result{}, false
}

// gitConfigArg recognises the `-c key=value` forms (separate, glued and
// --config-env) and returns the setting plus how many argv words it takes. A
// missing value is reported as dynamic, which makes the command opaque.
func gitConfigArg(args []word, i int) (word, int, bool) {
	t := args[i].text
	switch {
	case t == "-c" || t == "--config-env":
		if i+1 < len(args) {
			return args[i+1], 2, true
		}
		return word{dynamic: true}, 1, true
	case strings.HasPrefix(t, "-c") && len(t) > 2:
		return word{text: t[2:], dynamic: args[i].dynamic}, 1, true
	case strings.HasPrefix(t, "--config-env="):
		return word{text: strings.TrimPrefix(t, "--config-env="), dynamic: args[i].dynamic}, 1, true
	}
	return word{}, 0, false
}

// gitConfigOption judges one `-c key=value` (or `--config-env key=ENV`). bad
// reports that the setting alone decides the command's class.
func gitConfigOption(kv word) (result, bool) {
	key, _, _ := strings.Cut(kv.text, "=")
	switch {
	case kv.dynamic:
		return opaque("git -c with a dynamic setting"), true
	case isHooksPath(key):
		return privilegeDeny("git -c core.hooksPath overrides hooks"), true
	case gitConfigRunsProgram(key):
		return result{class: Privilege, unknown: true, reason: "git -c " + key + " points git at a program to run"}, true
	case gitConfigInert(key):
		return result{}, false
	}
	// An unrecognised key may name a program git will run, so the safe
	// default is opaque rather than "probably harmless".
	return opaque("git -c " + key + " is not a known inert setting"), true
}

func isHooksPath(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	return strings.EqualFold(key, "core.hooksPath")
}

// gitConfigSections are whole config sections whose keys name commands, hooks
// or extra config to load.
var gitConfigSections = []string{
	"alias", "uploadpack", "receive", "includeif", "pager", "url", "instaweb",
	"guitool", "difftool", "mergetool", "browser", "man", "web", "svn-remote", "trace2",
}

// gitConfigLastSegment maps a section to the trailing key names in it that
// name a program (the middle segment is the driver/filter name).
var gitConfigLastSegment = map[string][]string{
	"core":        {"hookspath", "sshcommand", "editor", "pager", "fsmonitor", "askpass", "gitproxy", "alternaterefscommand", "editor"},
	"diff":        {"external", "textconv", "command"},
	"merge":       {"driver"},
	"filter":      {"clean", "smudge", "process"},
	"protocol":    {"allow"},
	"gpg":         {"program"},
	"credential":  {"helper", "username"},
	"http":        {"proxy", "sslcainfo"},
	"include":     {"path"},
	"ssh":         {"variant"},
	"sequence":    {"editor"},
	"init":        {"templatedir"},
	"interactive": {"difffilter"},
	"blame":       {"markunblamablelines"},
	"safe":        {"directory"},
}

// gitConfigRunsProgram reports whether setting key can make git execute a
// program of the caller's choosing, or read configuration that can.
func gitConfigRunsProgram(key string) bool {
	key = strings.ToLower(key)
	section, rest, ok := strings.Cut(key, ".")
	if !ok {
		return false
	}
	if slices.Contains(gitConfigSections, section) {
		return true
	}
	last := rest
	if i := strings.LastIndex(rest, "."); i >= 0 {
		last = rest[i+1:]
	}
	return slices.Contains(gitConfigLastSegment[section], last)
}

// gitConfigInertPrefixes are sections that only affect presentation.
var gitConfigInertPrefixes = []string{"color.", "advice.", "status.", "format."}

// gitConfigInertKeys are the settings an agent legitimately passes with -c
// that cannot change what git executes. Everything outside this list is
// opaque: an allowlist is the only safe default when an unknown key may name
// a program.
var gitConfigInertKeys = map[string]bool{
	"user.name": true, "user.email": true, "user.useconfigonly": true, "user.signingkey": true,
	"core.autocrlf": true, "core.ignorecase": true, "core.quotepath": true, "core.filemode": true,
	"core.abbrev": true, "core.safecrlf": true, "core.symlinks": true, "core.logallrefupdates": true,
	"core.precomposeunicode": true, "core.longpaths": true, "core.bare": true, "core.commitgraph": true,
	"core.untrackedcache": true, "core.sparsecheckout": true, "core.whitespace": true, "core.eol": true,
	"diff.noprefix": true, "diff.algorithm": true, "diff.renames": true, "diff.context": true,
	"diff.mnemonicprefix": true, "diff.indentheuristic": true, "diff.colormoved": true,
	"diff.submodule": true, "diff.wordregex": true, "diff.relative": true, "diff.orderfile": true,
	"log.date": true, "log.decorate": true, "log.follow": true, "log.abbrevcommit": true, "log.mailmap": true,
	"push.default": true, "push.autosetupremote": true, "pull.rebase": true, "pull.ff": true,
	"merge.conflictstyle": true, "merge.ff": true, "rebase.autosquash": true, "rebase.autostash": true,
	"fetch.prune": true, "fetch.parallel": true, "commit.gpgsign": true, "tag.gpgsign": true,
	"init.defaultbranch": true, "gc.auto": true, "grep.linenumber": true, "grep.patterntype": true,
	"apply.whitespace": true, "am.threeway": true, "branch.autosetupmerge": true,
	"submodule.recurse": true, "versionsort.suffix": true,
}

// gitConfigInert reports whether key is a known presentation-only setting.
func gitConfigInert(key string) bool {
	key = strings.ToLower(key)
	for _, p := range gitConfigInertPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return gitConfigInertKeys[key]
}

// gitSubcommands need argument inspection.
var gitSubcommands = map[string]func(a *analyzer, rest []word) result{
	"push":       gitPush,
	"config":     gitConfig,
	"branch":     gitBranch,
	"checkout":   gitCheckout,
	"reset":      gitReset,
	"clean":      gitClean,
	"stash":      gitStash,
	"reflog":     gitReflog,
	"gc":         gitGC,
	"remote":     gitRemote,
	"submodule":  gitSubmodule,
	"worktree":   gitWorktree,
	"tag":        gitTag,
	"update-ref": gitUpdateRef,
	"bisect":     gitBisect,
	"grep":       gitGrep,
	"blame":      gitBlame,
	"annotate":   gitBlame,
}

// gitPush: a plain push is Network(+mutating remote). Forced or deleting
// pushes are Destructive, and hard-denied when the target branch is protected.
func gitPush(a *analyzer, rest []word) result {
	forced := hasFlag(rest, "--force", "-f", "--force-with-lease", "--force-if-includes", "--mirror") || hasShort(rest, 'f')
	deleting := hasFlag(rest, "--delete", "-d") || hasShort(rest, 'd')
	nf := texts(nonFlags(rest))
	var refspecs []string
	if len(nf) > 1 {
		refspecs = nf[1:]
	}
	targets, f, d := pushTargets(refspecs)
	forced, deleting = forced || f, deleting || d
	if !forced && !deleting {
		return network("git push")
	}
	kind := "force-push"
	if deleting {
		kind = "branch deletion"
	}
	for _, t := range targets {
		if a.protectedBranch(t) {
			return result{class: Destructive, reason: "git push " + kind + " to protected branch " + t, hardDeny: "git push " + kind + " to protected branch " + t}
		}
	}
	if len(targets) == 0 && forced && !deleting && hasFlag(rest, "--mirror") {
		return result{class: Destructive, reason: "git push --mirror rewrites every remote ref", hardDeny: "git push --mirror rewrites every remote ref"}
	}
	return destructive("git push " + kind)
}

// pushTargets extracts the remote branches from refspecs and whether any
// refspec itself forces (+ref) or deletes (:ref).
func pushTargets(refspecs []string) (targets []string, forced, deleting bool) {
	for _, spec := range refspecs {
		src, dst, hasColon := strings.Cut(spec, ":")
		switch {
		case strings.HasPrefix(spec, "+"):
			forced = true
			if hasColon {
				targets = append(targets, dst)
			} else {
				targets = append(targets, strings.TrimPrefix(src, "+"))
			}
		case hasColon && src == "":
			deleting = true
			targets = append(targets, dst)
		case hasColon:
			targets = append(targets, dst)
		default:
			targets = append(targets, spec)
		}
	}
	return targets, forced, deleting
}

// protectedBranch matches a ref (refs/heads/ stripped) against the patterns.
func (a *analyzer) protectedBranch(ref string) bool {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	patterns := defaultProtectedBranches
	if bp, ok := a.ws.(BranchProtector); ok && len(bp.ProtectedBranches()) > 0 {
		patterns = bp.ProtectedBranches()
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, ref); ok {
			return true
		}
	}
	return false
}

// gitConfig: reads are safe; writes are mutating; global/system writes and
// anything touching core.hooksPath are hard-denied.
func gitConfig(a *analyzer, rest []word) result {
	nf := texts(nonFlags(rest))
	if slices.ContainsFunc(nf, isHooksPath) {
		return privilegeDeny("git config core.hooksPath overrides hooks")
	}
	readFlag := hasFlag(rest, "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l", "--show-origin", "--show-scope")
	writeFlag := hasFlag(rest, "--unset", "--unset-all", "--replace-all", "--add", "--remove-section", "--rename-section", "--edit", "-e", "set", "unset")
	isWrite := writeFlag || (!readFlag && len(nf) >= 2) || (len(nf) >= 1 && (nf[0] == "set" || nf[0] == "unset"))
	if !isWrite {
		return safe("git config read")
	}
	if hasFlag(rest, "--global", "--system", "--worktree") {
		return privilegeDeny("git config writes outside the repository")
	}
	return mutating("git config write")
}

func gitBranch(a *analyzer, rest []word) result {
	switch {
	case hasFlag(rest, "-D") || (hasFlag(rest, "-d", "--delete") && hasFlag(rest, "-f", "--force")):
		return destructive("git branch -D force-deletes a branch")
	case hasFlag(rest, "-d", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-u", "--set-upstream-to", "--unset-upstream", "--edit-description", "-f", "--force"):
		return mutating("git branch modification")
	case len(nonFlags(rest)) == 0 || hasFlag(rest, "--list", "-l", "-a", "-r", "-v", "-vv", "--show-current", "--contains", "--merged", "--no-merged"):
		return safe("git branch list")
	}
	return mutating("git branch create")
}

// gitCheckout: switching branches is mutating; checking out paths discards
// working-tree changes and is destructive.
func gitCheckout(a *analyzer, rest []word) result {
	nf := texts(nonFlags(rest))
	pathy := hasFlag(rest, "--", "-p", "--patch", "--ours", "--theirs")
	for _, t := range nf {
		if t == "." || strings.HasPrefix(t, "./") || strings.Contains(t, "/") && !strings.HasPrefix(t, "origin/") && !strings.HasPrefix(t, "refs/") {
			pathy = true
		}
	}
	if pathy || len(nf) > 1 && !hasFlag(rest, "-b", "-B", "--orphan", "-t", "--track") {
		return destructive("git checkout of paths discards local changes")
	}
	return mutating("git checkout branch")
}

func gitReset(a *analyzer, rest []word) result {
	if hasFlag(rest, "--hard", "--merge", "--keep") {
		return destructive("git reset --hard discards local changes")
	}
	return mutating("git reset")
}

func gitClean(a *analyzer, rest []word) result {
	if hasFlag(rest, "-n", "--dry-run") {
		return safe("git clean dry run")
	}
	if hasShort(rest, 'f') || hasFlag(rest, "--force") {
		return destructive("git clean -f deletes untracked files")
	}
	return safe("git clean without -f does nothing")
}

func gitStash(a *analyzer, rest []word) result {
	switch first(rest) {
	case "list", "show":
		return safe("git stash " + first(rest))
	case "drop", "clear":
		return destructive("git stash " + first(rest) + " discards stashed changes")
	}
	return mutating("git stash")
}

func gitReflog(a *analyzer, rest []word) result {
	switch first(rest) {
	case "expire", "delete":
		return destructive("git reflog " + first(rest) + " removes recovery points")
	}
	return safe("git reflog")
}

func gitGC(a *analyzer, rest []word) result {
	if hasFlag(rest, "--prune") || hasFlag(rest, "--aggressive") {
		return destructive("git gc --prune removes unreachable objects")
	}
	return mutating("git gc")
}

func gitRemote(a *analyzer, rest []word) result {
	switch first(rest) {
	case "", "-v", "show", "get-url":
		return safe("git remote read")
	case "update":
		return network("git remote update")
	case "remove", "rm", "prune":
		return destructive("git remote " + first(rest))
	}
	return mutating("git remote " + first(rest))
}

func gitSubmodule(a *analyzer, rest []word) result {
	switch first(rest) {
	case "", "status", "summary":
		return safe("git submodule status")
	case "foreach":
		return opaque("git submodule foreach runs arbitrary commands")
	case "deinit":
		return destructive("git submodule deinit")
	case "update", "add", "sync", "init", "set-url":
		return network("git submodule " + first(rest))
	}
	return mutating("git submodule " + first(rest))
}

func gitWorktree(a *analyzer, rest []word) result {
	switch first(rest) {
	case "", "list":
		return safe("git worktree list")
	case "remove", "prune":
		return destructive("git worktree " + first(rest))
	}
	return mutating("git worktree " + first(rest))
}

func gitTag(a *analyzer, rest []word) result {
	switch {
	case hasFlag(rest, "-d", "--delete"):
		return destructive("git tag -d")
	case len(nonFlags(rest)) == 0 || hasFlag(rest, "-l", "--list", "-n", "--contains", "--points-at"):
		return safe("git tag list")
	}
	return mutating("git tag create")
}

// gitBlameFiles are the blame options whose value is a file blame opens:
// --contents supplies the text to annotate and -S a file of revisions.
var gitBlameFiles = []string{"--contents", "-S"}

// gitBlame: `git blame --contents <path> HEAD -- <tracked>` prints every line
// of <path> with the annotation, whatever and wherever <path> is — a private
// key, an .env file, anything outside the workspace. The handler never parsed
// the option, so the path was not a declared read and neither the
// secret-file hard deny nor the containment check ever saw it. Declaring it
// puts both back in the way. `annotate` is the same command under its older
// name.
func gitBlame(a *analyzer, rest []word) result {
	r := safe("git blame")
	for _, opt := range gitBlameFiles {
		if v, ok := flagValue(rest, opt); ok {
			a.readFiles(&r, []word{v})
		}
	}
	return r
}

// gitGrepSpec reads git grep's options with the reader machinery so that an
// option value is never mistaken for a pathspec, and names the option that
// runs a program.
var gitGrepSpec = readerSpec{
	skip:    1,
	pattern: []string{"-e", "-f", "--regexp", "--file"},
	value: vals("-e", "-f", "--regexp", "--file", "-m", "--max-count", "-A", "-B", "-C",
		"--after-context", "--before-context", "--context", "--threads", "--max-depth"),
	exec: []string{"--open-files-in-pager"},
}

// gitGrep: `--open-files-in-pager=<cmd>` (and its glued `-O<cmd>` spelling)
// runs that program on every matching file, so a search classified safe-read
// was arbitrary code execution. The operands are pathspecs of tracked
// content, except after a `--` separator or with `--no-index`, which searches
// the working tree instead of the index and so will happily print a file the
// repository never tracked: `git grep --no-index -e . -- .env` read a secret
// the declared-read checks never saw.
func gitGrep(a *analyzer, rest []word) result {
	files, _, exec := gitGrepSpec.split(rest)
	if exec == "" && gitGrepPagerShort(rest) {
		exec = "-O"
	}
	if exec != "" {
		return result{class: Privilege, unknown: true, reason: "git grep " + exec + " runs a program of the caller's choosing"}
	}
	r := safe("git grep")
	a.readFiles(&r, gitGrepPaths(rest, files))
	return r
}

// gitGrepPagerShort reports the `-O[<pager>]` form. Its argument is optional,
// so git only accepts it glued to the option, where it never looks like a
// known option name.
func gitGrepPagerShort(args []word) bool {
	for _, w := range args {
		t := w.text
		if strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t[1:], 'O') {
			return true
		}
	}
	return false
}

// gitGrepPaths are the operands that name files on disk: everything after a
// `--` separator, and, under --no-index, the positional operands left after
// the pattern. Without either, the operands are revisions and pathspecs
// resolved against the object database, not paths to open.
func gitGrepPaths(args, positional []word) []word {
	for i, w := range args {
		if w.text == "--" {
			return args[i+1:]
		}
	}
	if hasFlag(args, "--no-index") {
		return positional
	}
	return nil
}

// gitBisectSafe are the bisect subcommands that only report the search's
// state, and gitBisectMoves those that move HEAD to another commit.
var (
	gitBisectSafe  = []string{"", "log", "view", "visualize", "terms", "help"}
	gitBisectMoves = []string{"start", "good", "bad", "new", "old", "skip", "reset"}
)

// gitBisect: `git bisect run <cmd>` runs a program of the caller's choosing
// at every step of the search, so it is arbitrary code execution wearing the
// name of a history command — it was auto-allowed as a safe read. It is
// opaque as well as Privilege: an argv-prefix allow rule must never cover a
// command that names its own executor. The rest of bisect checks commits out,
// which is a working-tree change.
func gitBisect(a *analyzer, rest []word) result {
	sub := first(rest)
	label := strings.TrimRight("git bisect "+sub, " ")
	switch {
	case sub == "run":
		return result{class: Privilege, unknown: true, reason: label + " executes a program at every step"}
	case slices.Contains(gitBisectSafe, sub):
		return safe(label)
	case sub == "replay":
		r := mutating(label)
		if nf := nonFlags(rest); len(nf) > 1 {
			a.readFiles(&r, nf[1:2])
		}
		return r
	case slices.Contains(gitBisectMoves, sub):
		return mutating(label)
	}
	return opaque("unknown git bisect subcommand " + sub)
}

func gitUpdateRef(a *analyzer, rest []word) result {
	if hasFlag(rest, "-d", "--delete") {
		return destructive("git update-ref -d deletes a ref")
	}
	return mutating("git update-ref")
}
