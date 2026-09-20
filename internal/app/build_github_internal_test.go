package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/ghauth"
	"github.com/richardwooding/wright/internal/workspace"
)

const fakeToken = "gho_0123456789abcdef0123456789abcdef0123"

// phase runs workspaceAndConfig's *effects* without a workspace: the
// githubAuth phase only reads b.o, b.settings and the resolver, so a builder
// can be assembled directly. That keeps the test on the one thing it is
// about and needs no gh binary.
func phase(t *testing.T, o RunOptions, s config.Settings, res func() (ghauth.Result, error)) *builder {
	t.Helper()
	b := &builder{o: o, settings: s, ghResolve: res}
	if err := b.githubAuth(); err != nil {
		t.Fatalf("githubAuth: %v", err)
	}
	return b
}

// With the switch off nothing happens at all — no exec, no LookPath. That is
// what makes "off means byte-identical to before" true by construction
// rather than by assertion, so the test asserts the resolver was never even
// called.
func TestGitHubAuthOffIsInert(t *testing.T) {
	called := false
	b := phase(t, RunOptions{}, config.Settings{}, func() (ghauth.Result, error) {
		called = true
		return ghauth.Result{Token: fakeToken}, nil
	})
	if called {
		t.Error("the resolver ran with the switch off")
	}
	if b.githubToken != "" || b.githubEnv != nil {
		t.Errorf("a credential was built with the switch off: %q %v", b.githubToken, b.githubEnv)
	}
	if b.gitHub == nil || b.gitHub.On() {
		t.Errorf("holder = %v, want present but empty", b.gitHub)
	}
}

func TestGitHubAuthBuildsTokenAndHelper(t *testing.T) {
	for _, tt := range []struct {
		name string
		o    RunOptions
		s    config.Settings
	}{
		{name: "by flag", o: RunOptions{GitHubAuth: true}},
		{name: "by user settings", s: config.Settings{GitHub: config.GitHub{Auth: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := phase(t, tt.o, tt.s, func() (ghauth.Result, error) {
				return ghauth.Result{Token: fakeToken, Source: ghauth.SourceGhCLI, GhPath: "/usr/bin/gh"}, nil
			})
			if b.githubEnv["GH_TOKEN"] != fakeToken {
				t.Errorf("GH_TOKEN = %q", b.githubEnv["GH_TOKEN"])
			}
			var helper string
			for k, v := range b.githubEnv {
				if strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
					helper = v
				}
			}
			if !strings.HasPrefix(helper, "!'/usr/bin/gh'") {
				t.Errorf("credential helper = %q, want gh by absolute quoted path", helper)
			}
			// The count is the base environment's and must not be overridden
			// from here, or the block's bookkeeping moves.
			if _, ok := b.githubEnv["GIT_CONFIG_COUNT"]; ok {
				t.Error("githubEnv overrides GIT_CONFIG_COUNT")
			}
			if !b.gitHub.On() || b.gitHub.Source() != ghauth.SourceGhCLI {
				t.Errorf("holder On=%v Source=%q", b.gitHub.On(), b.gitHub.Source())
			}
		})
	}
}

// A missing or logged-out gh warns and the session continues: a credential
// nobody could resolve must not stop someone working.
func TestGitHubAuthWarnsAndCarriesOn(t *testing.T) {
	b := phase(t, RunOptions{GitHubAuth: true}, config.Settings{}, func() (ghauth.Result, error) {
		return ghauth.Result{}, ghauth.ErrNoGh
	})
	if b.githubToken != "" || b.gitHub.On() {
		t.Error("a failed resolution left a credential behind")
	}
	if len(b.warnings) != 1 || !strings.Contains(b.warnings[0], "gh is not installed") {
		t.Errorf("warnings = %v, want one naming the reason", b.warnings)
	}
}

// Warnings are built before the redactor exists and are printed unredacted,
// so nothing that touches a token may interpolate it.
func TestNoWarningEverCarriesTheToken(t *testing.T) {
	b := phase(t, RunOptions{GitHubAuth: true}, config.Settings{}, func() (ghauth.Result, error) {
		return ghauth.Result{Token: fakeToken, Source: ghauth.SourceGhCLI, GhPath: "/usr/bin/gh"}, nil
	})
	b.gitHubWarnings(true)
	if len(b.warnings) == 0 {
		t.Fatal("an authenticated session said nothing at startup")
	}
	for _, w := range b.warnings {
		if strings.Contains(w, fakeToken) {
			t.Errorf("a warning carries the token: %q", w)
		}
	}
	joined := strings.Join(b.warnings, "\n")
	if !strings.Contains(joined, ghauth.SourceGhCLI) {
		t.Errorf("no warning names the token's source: %v", b.warnings)
	}
	// The degenerate case has to be said out loud.
	if !strings.Contains(joined, "every command") {
		t.Errorf("a session where every call is networked did not say so: %v", b.warnings)
	}
}

// A resolver error carries gh's own words; they must not leak either.
func TestAResolverErrorDoesNotLeakItsOutput(t *testing.T) {
	b := phase(t, RunOptions{GitHubAuth: true}, config.Settings{}, func() (ghauth.Result, error) {
		return ghauth.Result{}, errors.New("unexpected: " + fakeToken)
	})
	for _, w := range b.warnings {
		if strings.Contains(w, fakeToken) {
			t.Errorf("a warning carries the token from an error: %q", w)
		}
	}
}

// TestAuthProjectsEnablesOneWorkspace is the "this project" scope. It lives
// in the user's own config rather than the project's, because a project file
// is committed: putting it there would ask everyone who clones the
// repository to hand over their own credential.
func TestAuthProjectsEnablesOneWorkspace(t *testing.T) {
	here := t.TempDir()
	elsewhere := t.TempDir()
	real, err := filepath.EvalSymlinks(here)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		root     string
		projects []string
		want     bool
	}{
		{name: "the named workspace", root: here, projects: []string{real}, want: true},
		{name: "another workspace", root: elsewhere, projects: []string{real}, want: false},
		{name: "none named", root: here, want: false},
		// A hand-edited entry must still match: the path is normalised on
		// both sides, the way trust normalises a project root.
		{name: "a trailing slash", root: here, projects: []string{real + "/"}, want: true},
		{name: "an unresolved spelling", root: here, projects: []string{here}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws, err := workspace.Open(tt.root, nil)
			if err != nil {
				t.Fatal(err)
			}
			b := &builder{ws: ws, settings: config.Settings{GitHub: config.GitHub{AuthProjects: tt.projects}}}
			if got := b.gitHubWanted(); got != tt.want {
				t.Errorf("gitHubWanted = %v, want %v", got, tt.want)
			}
		})
	}
}

// The flag and the every-project switch still work on their own.
func TestGitHubWantedFromFlagOrAlways(t *testing.T) {
	ws, err := workspace.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !(&builder{ws: ws, o: RunOptions{GitHubAuth: true}}).gitHubWanted() {
		t.Error("the flag did not enable it")
	}
	if !(&builder{ws: ws, settings: config.Settings{GitHub: config.GitHub{Auth: true}}}).gitHubWanted() {
		t.Error("the every-project switch did not enable it")
	}
	if (&builder{ws: ws}).gitHubWanted() {
		t.Error("enabled with nothing asking for it")
	}
}
