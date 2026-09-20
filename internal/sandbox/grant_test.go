package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/richardwooding/wright/internal/sandbox"
)

// clearToolEnv removes every variable ToolPrefixes reads, so a test measures
// what it sets up and not the developer's machine.
func clearToolEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"HOMEBREW_PREFIX", "HOMEBREW_CACHE", "NPM_CONFIG_PREFIX", "npm_config_prefix",
		"CARGO_HOME", "RUSTUP_HOME", "GOBIN", "GOPATH", "GEM_HOME", "XDG_CACHE_HOME",
	} {
		t.Setenv(name, "")
	}
	return home
}

// TestToolPrefixes pins what an approved install may write: an existing tool
// prefix, never a directory that does not exist, and never a system root or
// $HOME itself — a mistyped HOMEBREW_PREFIX must not become a read-write bind
// of half the machine.
func TestToolPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, home string) string // returns the path under test
		want   bool
		absent string
	}{
		{
			name: "existing homebrew prefix",
			setup: func(t *testing.T, _ string) string {
				dir := t.TempDir()
				t.Setenv("HOMEBREW_PREFIX", dir)
				return dir
			},
			want: true,
		},
		{
			name: "missing homebrew prefix",
			setup: func(t *testing.T, _ string) string {
				dir := filepath.Join(t.TempDir(), "nope")
				t.Setenv("HOMEBREW_PREFIX", dir)
				return dir
			},
			want: false,
		},
		{
			name: "filesystem root is refused",
			setup: func(t *testing.T, _ string) string {
				t.Setenv("HOMEBREW_PREFIX", "/")
				return "/"
			},
			want: false,
		},
		{
			name: "first-level system directory is refused",
			setup: func(t *testing.T, _ string) string {
				t.Setenv("HOMEBREW_PREFIX", "/usr")
				return "/usr"
			},
			want: false,
		},
		{
			name: "home itself is refused",
			setup: func(t *testing.T, home string) string {
				t.Setenv("GEM_HOME", home)
				return home
			},
			want: false,
		},
		{
			name: "go binary directory",
			setup: func(t *testing.T, home string) string {
				dir := filepath.Join(home, "go", "bin")
				mkdir(t, dir)
				return dir
			},
			want: true,
		},
		{
			name: "local bin",
			setup: func(t *testing.T, home string) string {
				dir := filepath.Join(home, ".local", "bin")
				mkdir(t, dir)
				return dir
			},
			want: true,
		},
		{
			name: "cargo home",
			setup: func(t *testing.T, home string) string {
				dir := filepath.Join(home, ".cargo")
				mkdir(t, dir)
				return dir
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := clearToolEnv(t)
			path := tt.setup(t, home)
			got := sandbox.ToolPrefixes()
			if slices.Contains(got, path) != tt.want {
				t.Errorf("ToolPrefixes() = %v, contains %q = %v, want %v", got, path, !tt.want, tt.want)
			}
			if slices.Contains(got, home) {
				t.Errorf("ToolPrefixes() returned $HOME itself: %v", got)
			}
			for i, p := range got {
				if slices.Index(got, p) != i {
					t.Errorf("ToolPrefixes() repeats %q: %v", p, got)
				}
			}
		})
	}
}

// TestGrantContext pins the carrier: nothing is granted unless the engine
// puts a grant on the context.
func TestGrantContext(t *testing.T) {
	if _, ok := sandbox.GrantFrom(context.Background()); ok {
		t.Error("a bare context must carry no grant")
	}
	if !(sandbox.Grant{}).Empty() {
		t.Error("the zero grant must be empty")
	}
	g := sandbox.Grant{Network: true, Writable: []string{"/opt/homebrew"}}
	got, ok := sandbox.GrantFrom(sandbox.WithGrant(context.Background(), g))
	if !ok || !got.Network || !slices.Equal(got.Writable, g.Writable) || got.Empty() {
		t.Errorf("GrantFrom = %+v, %v, want %+v", got, ok, g)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestGrantedPrefixIsWritableInTheRealSandbox closes the loop between the
// Spec and the OS: a tool prefix outside the workspace is unreachable inside
// the sandbox, and adding it to ReadWrite for one call makes the write land
// on the real directory — which is the whole point of the grant. It skips
// where bwrap cannot run, exactly as the other backend tests do.
func TestGrantedPrefixIsWritableInTheRealSandbox(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	prefix := t.TempDir() // stands in for the Homebrew prefix
	script := "touch " + prefix + "/installed"

	out, err := runSpec(t, b, sandbox.Spec{
		Argv: []string{"bash", "-c", script}, Dir: ws,
		Env: sandbox.Env(nil, nil), ReadWrite: []string{ws},
	})
	if err == nil {
		t.Errorf("a directory outside the workspace must not be writable: %s", out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "installed")); err == nil {
		t.Error("the ungranted call wrote to the prefix on the real filesystem")
	}

	out, err = runSpec(t, b, sandbox.Spec{
		Argv: []string{"bash", "-c", script}, Dir: ws,
		Env: sandbox.Env(nil, nil), ReadWrite: []string{ws, prefix},
	})
	if err != nil {
		t.Fatalf("granted prefix not writable: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "installed")); err != nil {
		t.Errorf("the granted write did not reach the real directory: %v", err)
	}
}
