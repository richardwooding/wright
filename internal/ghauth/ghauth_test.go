package ghauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/ghauth"
)

// stub builds Deps from a fixed environment and a fixed `gh auth token`
// result, so no test needs a gh binary or an environment of its own.
func stub(env map[string]string, ghPath, out string, runErr error) ghauth.Deps {
	return ghauth.Deps{
		Env: func(k string) string { return env[k] },
		LookPath: func(string) (string, error) {
			if ghPath == "" {
				return "", errors.New("not found")
			}
			return ghPath, nil
		},
		Run: func(context.Context, string, ...string) (string, error) { return out, runErr },
	}
}

func TestResolve(t *testing.T) {
	const token = "gho_0123456789abcdef0123456789abcdef0123"
	tests := []struct {
		name       string
		env        map[string]string
		ghPath     string
		out        string
		runErr     error
		wantToken  string
		wantSource string
		wantErr    error
	}{
		{
			name: "GH_TOKEN wins", env: map[string]string{"GH_TOKEN": token, "GITHUB_TOKEN": "other-token"},
			ghPath: "/usr/bin/gh", wantToken: token, wantSource: ghauth.SourceGHToken,
		},
		{
			name: "GITHUB_TOKEN is the fallback", env: map[string]string{"GITHUB_TOKEN": token},
			ghPath: "/usr/bin/gh", wantToken: token, wantSource: ghauth.SourceGitHubToken,
		},
		{
			// The environment path must not need gh at all.
			name: "an exported token needs no gh", env: map[string]string{"GH_TOKEN": token},
			wantToken: token, wantSource: ghauth.SourceGHToken,
		},
		{
			name: "otherwise gh mints one", ghPath: "/usr/bin/gh", out: token + "\n",
			wantToken: token, wantSource: ghauth.SourceGhCLI,
		},
		{name: "no gh at all", wantErr: ghauth.ErrNoGh},
		{
			name: "gh is not logged in", ghPath: "/usr/bin/gh", runErr: errors.New("exit status 1"),
			wantErr: ghauth.ErrNotLoggedIn,
		},
		// Everything below is a gh that printed something that is not a
		// token. None of it may become an environment variable.
		{name: "a usage banner", ghPath: "/usr/bin/gh", out: "Usage: gh auth token\n\nFlags:\n  -h  help\n", wantErr: ghauth.ErrNoToken},
		{name: "whitespace", ghPath: "/usr/bin/gh", out: "   \n\t\n", wantErr: ghauth.ErrNoToken},
		{name: "nothing", ghPath: "/usr/bin/gh", out: "", wantErr: ghauth.ErrNoToken},
		{name: "too short to be a credential", ghPath: "/usr/bin/gh", out: "abc\n", wantErr: ghauth.ErrNoToken},
		{name: "two words", ghPath: "/usr/bin/gh", out: "token expired-please-login\n", wantErr: ghauth.ErrNoToken},
		{name: "an empty environment variable falls through", env: map[string]string{"GH_TOKEN": ""}, wantErr: ghauth.ErrNoGh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ghauth.Resolve(context.Background(), stub(tt.env, tt.ghPath, tt.out, tt.runErr))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got.Token != "" {
					t.Errorf("a failed resolution returned a token: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Token != tt.wantToken || got.Source != tt.wantSource {
				t.Errorf("Resolve = {Token:%q Source:%q}, want {%q %q}", got.Token, got.Source, tt.wantToken, tt.wantSource)
			}
			// The source is a name, and goes into warnings and the audit
			// log; it must never carry the value with it.
			if strings.Contains(got.Source, tt.wantToken) {
				t.Errorf("Source %q contains the token", got.Source)
			}
		})
	}
}

// The credential helper resolves gh by absolute path, because resolving it
// through the sandbox PATH lets a script put its own directory first.
func TestResolveReportsTheAbsolutePath(t *testing.T) {
	got, err := ghauth.Resolve(context.Background(), stub(nil, "/opt/homebrew/bin/gh", "gho_0123456789abcdef0123456789abcdef0123\n", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.GhPath != "/opt/homebrew/bin/gh" {
		t.Errorf("GhPath = %q", got.GhPath)
	}
}

// An error from gh carries gh's own words, which are written to stderr and
// must not be confused with stdout — the place a token comes from.
func TestResolveKeepsStderrOutOfTheToken(t *testing.T) {
	d := stub(nil, "/usr/bin/gh", "", errors.New("gh: To get started with GitHub CLI, please run: gh auth login"))
	got, err := ghauth.Resolve(context.Background(), d)
	if !errors.Is(err, ghauth.ErrNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
	if got.Token != "" {
		t.Errorf("token = %q, want empty", got.Token)
	}
}
