package app

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/richardwooding/wright/internal/ghauth"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/tools"
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
	if !b.o.GitHubAuth && !b.settings.GitHub.Auth {
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
		for k, v := range git.CredentialHelperKeys("!'" + res.GhPath + "' auth git-credential") {
			b.githubEnv[k] = v
		}
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

// resolveGitHub is the injection seam: tests replace it so no test needs a gh
// binary, and so the "off does nothing" property can be asserted by a
// resolver that records whether it was called at all.
func (b *builder) resolveGitHub() (ghauth.Result, error) {
	if b.ghResolve != nil {
		return b.ghResolve()
	}
	return ghauth.Resolve(b.ctx, ghauth.Deps{Env: b.env})
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
