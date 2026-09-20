package policy

// The builtin rule lists. They mirror internal/config/defaults.json exactly
// (a test asserts it) so the compiled-in floor and the documented defaults
// can never drift apart.
var (
	builtinAllow = []string{
		"read_file($WORKSPACE/**)",
		"glob($WORKSPACE/**)",
		"grep($WORKSPACE/**)",
		"list_dir($WORKSPACE/**)",
		"bash(git status *)",
		"bash(git diff *)",
		"bash(git log *)",
		"bash(git show *)",
		"bash(git blame *)",
		"bash(git branch --list *)",
		"bash(git remote -v)",
		"bash(git rev-parse *)",
		"bash(git stash list *)",
		"bash(git worktree list *)",
		"bash(go build *)",
		"bash(go test *)",
		"bash(go vet *)",
		"bash(go fmt *)",
		"bash(rg *)",
		"bash(grep *)",
		"bash(find *)",
		"bash(ls *)",
		"bash(cat *)",
		"bash(head *)",
		"bash(tail *)",
		"bash(wc *)",
	}
	builtinAsk = []string{
		"write_file($WORKSPACE/**)",
		"edit_file($WORKSPACE/**)",
		"bash(git push *)",
		"web_fetch",
		"web_search",
		"mcp:*",
	}
	builtinDeny = []string{
		"*(**/.env)",
		"*(**/*.pem)",
		"*(**/*.key)",
		"*(**/*.p12)",
		"*(**/*.pfx)",
		"*(**/*.jks)",
		"*(**/*.kdbx)",
		"*(**/credentials*)",
		"*(**/service-account*.json)",
		"*(~/.ssh/**)",
		"*(~/.aws/**)",
		"*(~/.gnupg/**)",
		"*(~/.kube/**)",
		"*(~/.config/gh/**)",
		"*(~/.config/wright/**)",
		"*(~/.docker/config.json)",
		"*(~/.netrc)",
		"*(~/.npmrc)",
		"*(~/.pypirc)",
		"*($WORKSPACE/.wright/**)",
		"*($WORKSPACE/.git/**)",
		"bash(sudo *)",
		"bash(su *)",
		"bash(doas *)",
		"bash(pkexec *)",
		"bash(crontab *)",
		"bash(systemctl *)",
		"bash(launchctl *)",
		"web_fetch(domain:169.254.169.254)",
		"web_fetch(domain:localhost)",
		"web_fetch(domain:127.*)",
	}
)

// BuiltinTexts returns the raw builtin lists (allow, ask, deny).
func BuiltinTexts() (allow, ask, deny []string) {
	return append([]string(nil), builtinAllow...), append([]string(nil), builtinAsk...), append([]string(nil), builtinDeny...)
}

// Builtin returns the parsed builtin rules as one layer.
func Builtin() []Rule {
	var out []Rule
	out = append(out, MustParseRules(builtinDeny, Deny, SourceBuiltin)...)
	out = append(out, MustParseRules(builtinAsk, Ask, SourceBuiltin)...)
	out = append(out, MustParseRules(builtinAllow, Allow, SourceBuiltin)...)
	return out
}
