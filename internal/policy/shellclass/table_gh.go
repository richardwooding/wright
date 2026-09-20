package shellclass

import "strings"

// The GitHub CLI reaches a service, not the file system, so every gh command
// is at least Network — which already sorts above MutatingWorkspace, making
// the unknown case the strict one. What the table below adds is the
// difference between reading a pull request and deleting a repository.
//
// It replaced a set of substring tests over the joined positionals. That was
// wrong at the edges in both directions: `gh pr create --delete-branch`
// escaped being called a deletion only because nonFlags dropped the option,
// any command with a positional spelled "delete" collided with it, and the
// deletion branch returned destructive(), which does not set network — so
// the audit record said a command that talks to GitHub needed none.

// ghDestructiveVerbs are the second words that destroy something that is not
// in the workspace, keyed by the noun they follow.
var ghDestructiveVerbs = map[string][]string{
	"release":   {"delete", "delete-asset"},
	"gist":      {"delete"},
	"cache":     {"delete"},
	"label":     {"delete"},
	"variable":  {"delete"},
	"run":       {"cancel", "delete"},
	"workflow":  {"disable"},
	"project":   {"delete", "item-delete", "field-delete"},
	"org":       {"delete"},
	"codespace": {"delete", "stop"},
}

// ghWriteVerbs change something on GitHub without destroying it.
var ghWriteVerbs = map[string][]string{
	"pr":       {"create", "edit", "merge", "close", "reopen", "comment", "review", "ready", "lock", "unlock"},
	"issue":    {"create", "edit", "close", "reopen", "comment", "pin", "unpin", "transfer", "lock", "unlock"},
	"release":  {"create", "edit", "upload"},
	"repo":     {"create", "fork", "clone", "rename", "edit", "archive", "unarchive", "sync", "set-default"},
	"run":      {"rerun"},
	"workflow": {"run", "enable"},
	"label":    {"create", "edit", "clone"},
	"gist":     {"create", "edit", "rename"},
	"project":  {"create", "edit", "item-create", "item-edit", "item-add", "field-create", "copy", "link", "unlink"},
	"variable": {"set"},
	"cache":    {},
	"org":      {},
}

// ghCredentialVerbs touch stored credentials or the keys that stand in for
// them. They are hard denies: the agent may never mint, read or install a
// credential, which is the line that keeps wright resolving the token on the
// host a different act from the agent asking for one.
var ghCredentialVerbs = map[string][]string{
	"auth":    {"token", "login", "refresh", "setup-git", "switch", "logout"},
	"secret":  {"set", "remove", "delete", "list"},
	"ssh-key": {"add", "delete"},
	"gpg-key": {"add", "delete"},
	// An alias whose expansion starts with "!" is a shell command, and
	// `gh alias set` would write one that a later, innocent-looking `gh x`
	// runs. An extension is third-party code gh downloads and executes.
	"alias":     {"set", "delete", "import"},
	"extension": {"install", "exec", "create", "remove", "upgrade"},
}

// handleGh classifies the GitHub CLI by noun and verb.
func handleGh(a *analyzer, name string, args []word) result {
	noun, verb := ghNounVerb(args)
	switch {
	case noun == "":
		// No subcommand at all: `gh --version`, `gh --help`.
		return network(name)
	case matchesVerb(ghCredentialVerbs, noun, verb):
		return privilegeDenyNet(name + " " + noun + " " + verb + " touches stored credentials")
	case noun == "repo" && verb == "delete":
		// Deleting a repository is not recoverable from here and is not
		// something an allow rule should ever reach, so it goes on the
		// floor beside `git push --force` to a protected branch.
		return hardDenyNet(name + " repo delete destroys a repository")
	case noun == "codespace" && verb == "ssh":
		return privilegeDenyNet(name + " codespace ssh opens a remote shell")
	case noun == "api":
		return ghAPI(a, name, args)
	case matchesVerb(ghDestructiveVerbs, noun, verb):
		return destructiveNet(name + " " + noun + " " + verb)
	case matchesVerb(ghWriteVerbs, noun, verb):
		return network(name + " " + noun + " " + verb + " writes to GitHub")
	}
	// A noun the table does not know, or a verb that is neither a write nor
	// a deletion: Network, which prompts. Drift costs a prompt, never a
	// silent allow.
	return network(strings.TrimSpace(name + " " + noun + " " + verb))
}

// ghAPI classifies `gh api`, where the method decides everything: the same
// command shape reads an issue or deletes a repository.
func ghAPI(a *analyzer, name string, args []word) result {
	method := ""
	if v, ok := flagValue(args, "-X", "--method"); ok && !v.dynamic {
		method = strings.ToUpper(v.text)
	} else if ok {
		return result{class: Network, network: true, unknown: true, reason: name + " api with a dynamic method"}
	}
	switch method {
	case "DELETE":
		return hardDenyNet(name + " api -X DELETE")
	case "", "GET", "HEAD":
		return network(name + " api reads the GitHub API")
	default:
		return network(name + " api -X " + method + " writes through the GitHub API")
	}
}

// ghNounVerb is the first and second positional words, which for gh are the
// command group and its action. Flags are skipped, so `gh --repo x pr list`
// reads as `pr list`.
func ghNounVerb(args []word) (noun, verb string) {
	nf := texts(nonFlags(args))
	if len(nf) > 0 {
		noun = nf[0]
	}
	if len(nf) > 1 {
		verb = nf[1]
	}
	return noun, verb
}

func matchesVerb(table map[string][]string, noun, verb string) bool {
	for _, v := range table[noun] {
		if v == verb {
			return true
		}
	}
	return false
}
