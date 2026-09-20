package tools

import (
	"maps"
	"slices"
	"sort"
	"sync"
)

// GitHubAuth is the session's GitHub credential, if it asked for one.
//
// It is a holder rather than a plain map for the same reason CwdState is:
// /github on and /github off change it while a session is running, and tools
// run in parallel. A change takes effect on the next call, never on one
// already in flight.
//
// What it holds is the *whole* contribution — the token and the git
// credential helper that uses it — because the two are useless apart: a
// helper with no token produces nothing, and a token with no helper leaves
// `git push` unauthenticated.
type GitHubAuth struct {
	mu     sync.Mutex
	env    map[string]string
	source string
}

// NewGitHubAuth returns an empty holder: no credential until one is enabled.
func NewGitHubAuth() *GitHubAuth { return &GitHubAuth{} }

// Enable installs the environment contribution and names where the token
// came from. source is a name ("gh auth token"), never the token.
func (g *GitHubAuth) Enable(env map[string]string, source string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.env, g.source = maps.Clone(env), source
}

// Disable drops the credential for the rest of the session.
func (g *GitHubAuth) Disable() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.env, g.source = nil, ""
}

// On reports whether a credential is installed.
func (g *GitHubAuth) On() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.env) > 0
}

// Source names where the token came from, for a status line or a warning.
// It never contains the token.
func (g *GitHubAuth) Source() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.source
}

// env returns the contribution as KEY=VALUE entries, sorted so a command
// line is reproducible. The result is freshly allocated on every call: it
// ends up in a *exec.Cmd's environment and must not alias the holder.
func (g *GitHubAuth) entries() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.env) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.env))
	for k, v := range g.env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// gitHubEnvFor is the credential a single call gets: none unless the call
// runs with the network.
//
// A command with no network cannot use a credential, so it is not given one.
// That is the control this feature rests on — not redaction, which is a
// backstop a `base64` defeats. It also means an auto-allowed read, which
// runs with no prompt at all, never has the token in its environment.
func (d *Deps) gitHubEnvFor(network bool) []string {
	if !network {
		return nil
	}
	return d.GitHub.entries()
}

// withEnv appends entries to a copy of base, replacing any existing entry
// with the same name.
//
// Clone first: Deps.SandboxSpec is shared by every call *and* by the MCP
// stdio servers, so appending in place would hand the next command — and
// every MCP server — the last one's credential. The same trap withGranted
// documents for ReadWrite.
func withEnv(base, entries []string) []string {
	if len(entries) == 0 {
		return base
	}
	out := slices.Clone(base)
	for _, e := range entries {
		name, _, ok := cutEnv(e)
		if !ok {
			continue
		}
		out = slices.DeleteFunc(out, func(existing string) bool {
			n, _, ok := cutEnv(existing)
			return ok && n == name
		})
		out = append(out, e)
	}
	return out
}

// cutEnv splits a KEY=VALUE entry.
func cutEnv(entry string) (name, value string, ok bool) {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return "", "", false
}
