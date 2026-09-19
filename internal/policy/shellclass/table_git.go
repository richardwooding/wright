package shellclass

import (
	"path"
	"strings"
)

// defaultProtectedBranches are the branches a forced or deleting push is
// hard-denied for unless the workspace overrides them.
var defaultProtectedBranches = []string{"main", "master", "release/*"}

var (
	gitSafeRead = map[string]bool{
		"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "describe": true,
		"shortlog": true, "ls-files": true, "ls-tree": true, "cat-file": true, "rev-list": true, "name-rev": true,
		"grep": true, "help": true, "version": true, "--version": true, "check-ignore": true, "check-attr": true,
		"merge-base": true, "diff-tree": true, "diff-index": true, "diff-files": true, "for-each-ref": true, "var": true,
		"count-objects": true, "fsck": true, "whatchanged": true, "range-diff": true, "show-ref": true, "verify-commit": true,
		"verify-tag": true, "cherry": true, "bisect": true, "annotate": true, "show-branch": true, "status-porcelain": true,
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
		switch {
		case t == "-c" && i+1 < len(args):
			if isHooksPath(args[i+1].text) {
				return i, privilegeDeny("git -c core.hooksPath overrides hooks"), true
			}
			i += 2
		case strings.HasPrefix(t, "-c") && isHooksPath(t[2:]):
			return i, privilegeDeny("git -c core.hooksPath overrides hooks"), true
		case t == "-C" || t == "--git-dir" || t == "--work-tree" || t == "--namespace" || t == "--exec-path":
			i += 2
		case strings.HasPrefix(t, "--exec-path="):
			return i, opaque("git --exec-path overrides the git binaries"), true
		default:
			i++
		}
	}
	return i, result{}, false
}

func isHooksPath(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	return strings.EqualFold(key, "core.hooksPath")
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
	for _, t := range nf {
		if isHooksPath(t) {
			return privilegeDeny("git config core.hooksPath overrides hooks")
		}
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

func gitUpdateRef(a *analyzer, rest []word) result {
	if hasFlag(rest, "-d", "--delete") {
		return destructive("git update-ref -d deletes a ref")
	}
	return mutating("git update-ref")
}
