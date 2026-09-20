// Package ghauth resolves a GitHub token on the host, for a session that
// asked for GitHub authentication.
//
// It runs as the user, before any model call, and that is the whole of its
// security story: `gh auth token` is hard-denied for the *agent*
// (internal/policy/shellclass), which may never mint or read a credential.
// wright may, once, on the user's instruction. What the agent gets is the use
// of a token whose provenance it cannot see and which it cannot re-mint.
//
// Nothing here ever logs, prints or returns the token except as Result.Token.
// Source is a name — "GH_TOKEN", "gh auth token" — so it can go in a warning,
// in /debug and in the audit log without carrying the value with it.
package ghauth

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds the call to gh. Resolution happens at startup, so a gh that
// hangs must not hang the session.
const Timeout = 5 * time.Second

// minTokenLen is the shortest thing this package will believe is a token. It
// exists to reject a gh that printed something else to stdout, not to
// validate any particular token format.
const minTokenLen = 8

// Sources, as they are named to the user.
const (
	SourceGHToken     = "GH_TOKEN"
	SourceGitHubToken = "GITHUB_TOKEN"
	SourceGhCLI       = "gh auth token"
)

// Errors a caller distinguishes. Each is a reason the user can act on.
var (
	ErrNoGh        = errors.New("ghauth: gh is not installed")
	ErrNotLoggedIn = errors.New("ghauth: gh has no credentials (run: gh auth login)")
	ErrNoToken     = errors.New("ghauth: gh produced no usable token")
)

// Result is a resolved credential plus where it came from.
type Result struct {
	// Token is the credential. Never log it, never put it in a warning:
	// warnings are built before the redactor exists.
	Token string
	// Source names the origin, never the value.
	Source string
	// GhPath is the absolute path of the gh binary, empty when the token
	// came from the environment and gh was not found. The credential helper
	// needs the absolute path: resolving "gh" through the sandbox PATH lets
	// a script put its own directory first.
	GhPath string
}

// Deps are the host facilities Resolve uses, injected so tests need no gh
// binary and no environment of their own.
type Deps struct {
	Env      func(string) string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, bin string, args ...string) (string, error)
}

// Resolve finds a token: GH_TOKEN, then GITHUB_TOKEN, then `gh auth token`.
//
// The environment comes first because a user who exported a token chose it
// deliberately, and because it is the one path that needs no gh at all.
func Resolve(ctx context.Context, d Deps) (Result, error) {
	d = withDefaults(d)
	for _, name := range []string{SourceGHToken, SourceGitHubToken} {
		if tok, ok := valid(d.Env(name)); ok {
			// gh may be absent; the token still works for gh-the-CLI's own
			// environment variable and for anything else that reads it.
			path, _ := d.LookPath("gh")
			return Result{Token: tok, Source: name, GhPath: path}, nil
		}
	}
	path, err := d.LookPath("gh")
	if err != nil {
		return Result{}, ErrNoGh
	}
	out, err := d.Run(ctx, path, "auth", "token")
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotLoggedIn, err)
	}
	tok, ok := valid(out)
	if !ok {
		return Result{}, ErrNoToken
	}
	return Result{Token: tok, Source: SourceGhCLI, GhPath: path}, nil
}

// valid accepts only something that could be a token: one line, no spaces,
// long enough. A gh that printed a usage banner, a warning or nothing at all
// must never become an environment variable.
func valid(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) < minTokenLen || strings.ContainsAny(s, " \t\n\r") {
		return "", false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return "", false
		}
	}
	return s, true
}

func withDefaults(d Deps) Deps {
	if d.Env == nil {
		d.Env = func(string) string { return "" }
	}
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.Run == nil {
		d.Run = run
	}
	return d
}

// run executes bin and returns stdout only. Stderr is deliberately kept
// separate: a gh that fails writes its explanation there, and that text must
// never be mistaken for a credential.
func run(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
