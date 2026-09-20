package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/ghauth"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/trust"
)

// githubAuth resolves the GitHub credential, when this session asked for one.
//
// It runs after workspaceAndConfig (which settles the settings and trust) and
// before sandboxing, because a gh living under $HOME does not exist inside
// the sandbox and the spec has to learn about it before it is built.
//
// With the switch off it does nothing at all — no exec, no LookPath — which
// is what makes "off means byte-identical to before" true by construction.
func (b *builder) githubAuth() error {
	// The holder exists either way: /github on fills it mid-session.
	b.gitHub = tools.NewGitHubAuth()
	if !b.gitHubWanted() {
		return nil
	}
	res, err := b.resolveGitHub()
	if err != nil {
		// Never fatal. Making a missing credential stop the session turns an
		// optional convenience into a hard dependency on gh, in a harness
		// whose pitch is that it works offline. The user finds out at the
		// first push, with this warning already on screen.
		b.warn("github auth: %s; `git push` over HTTPS and `gh` commands will not be able to authenticate", githubFailure(err))
		return nil
	}
	b.githubToken, b.githubSource = res.Token, res.Source
	b.githubEnv = map[string]string{"GH_TOKEN": res.Token}
	if res.GhPath != "" {
		// Absolute and quoted: resolving "gh" through the sandbox PATH would
		// let a script put its own directory first and be handed the token
		// on stdin.
		maps.Copy(b.githubEnv, git.CredentialHelperKeys("!'"+res.GhPath+"' auth git-credential"))
		b.githubGhDir = filepath.Dir(res.GhPath)
	}
	b.gitHub.Enable(b.githubEnv, b.githubSource)
	return nil
}

// githubFailure is why resolution failed, in wright's own words.
//
// The raw error is deliberately not interpolated: warnings are built before
// the redactor exists and are printed unredacted, and an error that came back
// from another program is not something to trust with that. Each sentinel
// already carries an actionable message.
func githubFailure(err error) string {
	for _, known := range []error{ghauth.ErrNoGh, ghauth.ErrNotLoggedIn, ghauth.ErrNoToken} {
		if errors.Is(err, known) {
			return strings.TrimPrefix(known.Error(), "ghauth: ")
		}
	}
	return "the token could not be resolved"
}

// gitHubWanted reports whether this session asked for a credential: the
// flag, the user's "every project" switch, or this workspace being one of
// the projects they named.
//
// The project list is matched on the *normalised* path, the same way trust
// matches a project root — accepting under one spelling and checking under
// another is how project trust silently stopped working on macOS, and this
// is the same trap with the same shape.
func (b *builder) gitHubWanted() bool {
	if b.o.GitHubAuth || b.settings.GitHub.Auth {
		return true
	}
	return b.ws != nil && slices.Contains(normalizedRoots(b.settings.GitHub.AuthProjects), trust.NormalizeRoot(b.ws.Root()))
}

// normalizedRoots normalises a configured list once, so a hand-edited entry
// with a trailing slash or a symlink still matches.
func normalizedRoots(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, trust.NormalizeRoot(p))
	}
	return out
}

// resolveGitHub is the injection seam: tests replace it so no test needs a gh
// binary, and so the "off does nothing" property can be asserted by a
// resolver that records whether it was called at all.
func (b *builder) resolveGitHub() (ghauth.Result, error) {
	return resolveGitHub(b.ctx, b.ghResolve, b.env)
}

// resolveGitHub is shared by the startup phase and /github, so the command
// resolves a token exactly the way the flag does.
func resolveGitHub(ctx context.Context, override func() (ghauth.Result, error), env func(string) string) (ghauth.Result, error) {
	if override != nil {
		return override()
	}
	return ghauth.Resolve(ctx, ghauth.Deps{Env: env})
}

// resolveGitHub on a built session, for /github on.
func (b *Built) resolveGitHub() (ghauth.Result, error) {
	return resolveGitHub(context.Background(), b.ghResolve, b.opts.Env)
}

// gitHubCommand is /github: say what the session can do as you on GitHub,
// and turn it on or off for the rest of the session.
//
// Turning it on here never writes a settings file. Persisting the choice
// belongs to the user's own config, which this says rather than doing.
func (b *Built) gitHubCommand(args []string) (string, error) {
	switch {
	case len(args) == 0:
		return b.gitHubStatus(), nil
	case strings.EqualFold(args[0], "off"):
		if !b.gitHub.On() {
			return "GitHub authentication is already off.", nil
		}
		b.gitHub.Disable()
		b.setGitHubAuth(false)
		return "GitHub authentication is off for the rest of this session. Commands can still reach the network if they are allowed to; they just carry no credential." +
			"\n" + b.gitHubRemembered(), nil
	case strings.EqualFold(args[0], "on"):
		return b.enableGitHub(args[1:])
	default:
		return "", fmt.Errorf("usage: /github [on|off]")
	}
}

// Scopes for /github on. They are spelled as the user types them.
const (
	scopeSession = "session"
	scopeProject = "project"
	scopeAlways  = "always"
)

func (b *Built) enableGitHub(args []string) (string, error) {
	scope := scopeSession
	if len(args) > 0 {
		scope = strings.ToLower(args[0])
	}
	if scope != scopeSession && scope != scopeProject && scope != scopeAlways {
		return "", fmt.Errorf("usage: /github on [session|project|always]")
	}
	if b.gitHub.On() {
		// Already on for this session; the ask may still be to remember it.
		note, err := b.rememberGitHub(scope)
		if err != nil {
			return "", err
		}
		return b.gitHubStatus() + note, nil
	}
	res, err := b.resolveGitHub()
	if err != nil {
		return "", errors.New("GitHub authentication could not be turned on: " + githubFailure(err))
	}
	env := map[string]string{"GH_TOKEN": res.Token}
	if res.GhPath != "" {
		maps.Copy(env, git.CredentialHelperKeys("!'"+res.GhPath+"' auth git-credential"))
	}
	b.gitHub.Enable(env, res.Source)
	b.setGitHubAuth(true)
	remembered, err := b.rememberGitHub(scope)
	if err != nil {
		return "", err
	}
	// The token is not added to the redactor here: it is built once, at
	// startup, and a Redactor's pattern list is fixed after New. Say so
	// rather than implying a protection that is not there.
	note := ""
	if !b.tokenRedacted(res.Token) {
		note = "\n\nNote: this token is not registered with the redactor, which only accepts patterns when it is built at startup." +
			" It is still masked if it matches a known token shape. To have it registered, put " + userConfigSnippet +
			" in your user config, or start wright with --github-auth."
	}
	return "GitHub authentication is ON (token source: " + res.Source + ")." +
		"\nAny command that runs with network access can now act as you on GitHub." +
		remembered + note, nil
}

// rememberGitHub writes the choice, when the user asked for one that
// outlives the session. Both persistent scopes go in the *user's* config:
// a project's own settings file is committed, so putting it there would be
// asking everyone who clones the repository to hand over their credential —
// and effectiveSettings ignores the block from a project layer anyway.
func (b *Built) rememberGitHub(scope string) (string, error) {
	switch scope {
	case scopeAlways:
		if err := b.Layered.SaveUser(func(s *config.Settings) { s.GitHub.Auth = true }); err != nil {
			return "", err
		}
		return "\nRemembered for every project, in " + b.Layered.Paths.UserConfigFile() + ".", nil
	case scopeProject:
		root := trust.NormalizeRoot(b.WS.Root())
		if err := b.Layered.SaveUser(func(s *config.Settings) {
			s.GitHub.AuthProjects = addOnce(s.GitHub.AuthProjects, root)
		}); err != nil {
			return "", err
		}
		return "\nRemembered for " + root + ", in your user config (not the project's, which is shared).", nil
	}
	return "\nThis session only. `/github on project` or `/github on always` remembers it.", nil
}

// userConfigSnippet is the settings the user would paste to make the choice
// permanent. It is named once so the two places that suggest it agree.
const userConfigSnippet = `"github": {"auth": true}`

func (b *Built) gitHubStatus() string {
	if !b.gitHub.On() {
		return "GitHub authentication is off: `gh` commands and `git push` over HTTPS cannot authenticate." +
			"\n`/github on` turns it on for this session; `/github on project` or `/github on always` remembers it."
	}
	return "GitHub authentication is ON (token source: " + b.gitHub.Source() + ")." +
		"\nAny command that runs with network access carries a token that can act as you on GitHub." +
		"\n" + b.gitHubRemembered() +
		"\nTurn it off for the rest of this session with `/github off`."
}

// gitHubRemembered says what will turn it on again next session, because
// otherwise `/github off` looks like it did nothing when a saved setting
// brings it straight back.
func (b *Built) gitHubRemembered() string {
	switch {
	case b.Settings.GitHub.Auth:
		return "Remembered for every project in your user config."
	case b.WS != nil && slices.Contains(normalizedRoots(b.Settings.GitHub.AuthProjects), trust.NormalizeRoot(b.WS.Root())):
		return "Remembered for this project in your user config."
	case b.opts.GitHubAuth:
		return "This session was started with --github-auth."
	}
	return "Not remembered: it ends with this session."
}

// tokenRedacted reports whether the redactor already masks this token, so
// the command can say which of the two is true rather than guessing.
func (b *Built) tokenRedacted(token string) bool {
	if b.redactor == nil || token == "" {
		return false
	}
	out, _ := b.redactor.Redact(token)
	return out != token
}

// setGitHubAuth keeps the engine's display fact in step, so the approval
// prompt cannot say something different from what the call gets.
func (b *Built) setGitHubAuth(on bool) {
	if b.Engine != nil {
		b.Engine.SetGitHubAuth(on)
	}
}

// gitHubWarnings are the facts the user is told once, at startup: that the
// session is authenticated, where the token came from, and the two ways the
// exposure is wider than it first looks.
func (b *builder) gitHubWarnings(everyCallNetworked bool) {
	if b.githubToken == "" {
		return
	}
	msg := "github auth is ON: any shell command that runs with network access carries a GitHub token that can act as you" +
		" (read and write your repositories, open pull requests, create gists). Token source: " + b.githubSource
	if everyCallNetworked {
		msg += ". This session gives every command the network, so every command carries it"
	}
	b.warn("%s", msg)
	// A gh the sandbox cannot see produces "command not found" from inside
	// git, a long way from its cause.
	if b.githubGhDir != "" && b.ws != nil && b.ws.Home != "" && strings.HasPrefix(b.githubGhDir, b.ws.Home+string(filepath.Separator)) {
		b.warn("gh lives in %s, under your home directory, which the sandbox hides; it is mounted read-only so the git credential helper can find it", b.githubGhDir)
	}
}
