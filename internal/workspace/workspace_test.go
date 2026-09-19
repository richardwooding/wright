package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/workspace"
)

// fixture builds: <tmp>/ws (root), <tmp>/outside, <tmp>/home, a symlink
// ws/escape → outside and ws/inner → ws/sub.
type fixture struct {
	root, outside, home string
	ws                  *workspace.Workspace
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	home := filepath.Join(base, "home")
	for _, d := range []string{filepath.Join(root, "sub"), outside, filepath.Join(home, ".ssh"), filepath.Join(root, ".git"), filepath.Join(root, "node_modules")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "inner")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n*.log\n!keep.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", ".gitignore"), []byte("build/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".wrightignore"), []byte("secrets/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("WRIGHT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{root: root, outside: outside, home: home, ws: ws}
}

func TestOpenUsesGitToplevel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "repo")
	deep := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	ws, err := workspace.Open(deep, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root() != repo {
		t.Errorf("Root = %s, want %s", ws.Root(), repo)
	}
	plain, err := workspace.Open(filepath.Join(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Root() != base {
		t.Errorf("non-repo Root = %s, want %s", plain.Root(), base)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := workspace.Open(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("expected error for missing root")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Open(f, nil); err == nil {
		t.Error("expected error for file root")
	}
	if _, err := workspace.Open(t.TempDir(), []string{"/nonexistent/extra"}); err == nil {
		t.Error("expected error for missing extra dir")
	}
}

func TestResolve(t *testing.T) {
	fx := newFixture(t)
	tests := []struct {
		name   string
		in     string
		want   string
		inside bool
	}{
		{"relative", "sub/x.go", filepath.Join(fx.root, "sub", "x.go"), true},
		{"root itself", ".", fx.root, true},
		{"workspace var", "$WORKSPACE/sub", filepath.Join(fx.root, "sub"), true},
		{"dotdot escape", "../outside/f", filepath.Join(fx.outside, "f"), false},
		{"dotdot inside", "sub/../sub/y", filepath.Join(fx.root, "sub", "y"), true},
		{"tilde", "~/.ssh/id_rsa", filepath.Join(fx.home, ".ssh", "id_rsa"), false},
		{"bare tilde", "~", fx.home, false},
		{"symlink escape", "escape/secret.txt", filepath.Join(fx.outside, "secret.txt"), false},
		{"symlink escape nonexistent tail", "escape/a/b/c", filepath.Join(fx.outside, "a", "b", "c"), false},
		{"symlink inside", "inner/z", filepath.Join(fx.root, "sub", "z"), true},
		{"absolute inside", filepath.Join(fx.root, "sub"), filepath.Join(fx.root, "sub"), true},
		// Resolve evaluates symlinks, and on macOS /etc is a link to
		// /private/etc, so the expectation has to be resolved the same way
		// rather than spelled out.
		{"absolute outside", "/etc/passwd", resolved(t, "/etc/passwd"), false},
		{"prefix trick", fx.root + "2/file", fx.root + "2/file", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, inside, err := fx.ws.Resolve(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || inside != tt.inside {
				t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", tt.in, got, inside, tt.want, tt.inside)
			}
		})
	}
}

func TestExtraRoots(t *testing.T) {
	fx := newFixture(t)
	ws, err := workspace.Open(fx.root, []string{fx.outside})
	if err != nil {
		t.Fatal(err)
	}
	if _, inside, _ := ws.Resolve(filepath.Join(fx.outside, "f")); !inside {
		t.Error("extra root should be inside")
	}
	if got := ws.Rel(filepath.Join(fx.outside, "f")); got != "f" {
		t.Errorf("Rel = %q", got)
	}
}

func TestRel(t *testing.T) {
	fx := newFixture(t)
	if got := fx.ws.Rel(filepath.Join(fx.root, "sub", "x")); got != "sub/x" {
		t.Errorf("Rel = %q", got)
	}
	if got := fx.ws.Rel(fx.root); got != "." {
		t.Errorf("Rel(root) = %q", got)
	}
	if got := fx.ws.Rel("/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("Rel(outside) = %q", got)
	}
}

func TestHash(t *testing.T) {
	fx := newFixture(t)
	h := fx.ws.Hash()
	if len(h) != 16 {
		t.Errorf("Hash len = %d", len(h))
	}
	if h != fx.ws.Hash() {
		t.Error("Hash not stable")
	}
}

func TestIgnoredAndHidden(t *testing.T) {
	fx := newFixture(t)
	tests := []struct {
		rel     string
		ignored bool
		hidden  bool
	}{
		{"main.go", false, false},
		{"node_modules/x/index.js", true, false},
		{"debug.log", true, false},
		{"keep.log", false, false},
		{"sub/build/out", true, false},
		{"build/out", false, false}, // sub/.gitignore does not apply at root
		{".git/config", true, false},
		{"secrets/token", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			abs := filepath.Join(fx.root, tt.rel)
			if got := fx.ws.Ignored(abs); got != tt.ignored {
				t.Errorf("Ignored = %v, want %v", got, tt.ignored)
			}
			if got := fx.ws.Hidden(abs); got != tt.hidden {
				t.Errorf("Hidden = %v, want %v", got, tt.hidden)
			}
		})
	}
	if fx.ws.Ignored(filepath.Join(fx.outside, "x.log")) {
		t.Error("paths outside every root are never ignored by workspace files")
	}
}

func TestIsSecretFile(t *testing.T) {
	fx := newFixture(t)
	tests := []struct {
		name string
		want bool
	}{
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{".env.example", false},
		{".env.sample", false},
		{".env.template", false},
		{"server.pem", true},
		{"private.key", true},
		{"keystore.jks", true},
		{"id_ed25519", true},
		{"id_ed25519.pub", false},
		{"vault.kdbx", true},
		{"credentials.json", true},
		{"credentials", true},
		{"service-account-prod.json", true},
		{"main.go", false},
		{"environment.go", false},
		{"keyboard.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fx.ws.IsSecretFile(filepath.Join(fx.root, "dir", tt.name)); got != tt.want {
				t.Errorf("IsSecretFile(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
	if !fx.ws.IsSensitive(filepath.Join(fx.root, "prod.tfvars")) || fx.ws.IsSensitive(filepath.Join(fx.root, "main.tf")) {
		t.Error("IsSensitive tfvars mismatch")
	}
}

func TestIsProtected(t *testing.T) {
	fx := newFixture(t)
	tests := []struct {
		path string
		want bool
	}{
		{filepath.Join(fx.home, ".ssh", "id_rsa"), true},
		{filepath.Join(fx.home, ".ssh"), true},
		{filepath.Join(fx.home, ".aws", "credentials"), true},
		{filepath.Join(fx.home, ".config", "gh", "hosts.yml"), true},
		{filepath.Join(fx.home, ".config", "wright", "config.json"), true},
		{filepath.Join(fx.home, ".gnupg", "x"), true},
		{filepath.Join(fx.home, ".kube", "config"), true},
		{filepath.Join(fx.home, ".docker", "config.json"), true},
		{filepath.Join(fx.home, ".docker", "other"), false},
		{filepath.Join(fx.home, ".netrc"), true},
		{filepath.Join(fx.home, ".npmrc"), true},
		{filepath.Join(fx.home, ".pypirc"), true},
		{filepath.Join(fx.home, ".bashrc"), true},
		{filepath.Join(fx.home, ".zshrc"), true},
		{filepath.Join(fx.home, "code", "x.go"), false},
		{"/etc/passwd", true},
		{"/etc", true},
		{"/etcetera", false},
		{filepath.Join(fx.root, ".wright", "settings.json"), true},
		{filepath.Join(fx.root, ".git", "hooks", "pre-commit"), true},
		{filepath.Join(fx.root, "main.go"), false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := fx.ws.IsProtected(tt.path); got != tt.want {
				t.Errorf("IsProtected(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestIsProtectedMatchesResolvedSpellings pins the hard floor against the
// spelling Resolve actually produces. Every path in a policy.Request is
// symlink-resolved, so a protected set that knows only the literal path stops
// protecting anything reached through a link: /etc on macOS (a symlink to
// /private/etc), a $HOME under a link, or a ~/.ssh and ~/.gitconfig managed
// by a dotfiles repository. The expectations are computed here at run time —
// a hardcoded /private/... string would assert nothing on Linux.
func TestIsProtectedMatchesResolvedSpellings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	store := filepath.Join(base, "elsewhere")
	root := filepath.Join(base, "ws")
	cfgReal := filepath.Join(base, "cfg-real")
	for _, d := range []string{home, filepath.Join(store, "ssh"), filepath.Join(store, "dotfiles"), root, cfgReal} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store, "dotfiles", "gitconfig"), []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgLink := filepath.Join(base, "cfg-link")
	links := map[string]string{
		filepath.Join(home, ".ssh"):       filepath.Join(store, "ssh"),
		filepath.Join(home, ".gitconfig"): filepath.Join(store, "dotfiles", "gitconfig"),
		cfgLink:                           cfgReal,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("WRIGHT_CONFIG_DIR", cfgLink)
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"system dir as resolved", resolved(t, "/etc/passwd"), true},
		{"system dir as written", "/etc/passwd", true},
		{"home credential dir through the link", filepath.Join(home, ".ssh", "id_rsa"), true},
		{"home credential dir as resolved", filepath.Join(store, "ssh", "id_rsa"), true},
		{"home rc file through the link", filepath.Join(home, ".gitconfig"), true},
		{"home rc file as resolved", filepath.Join(store, "dotfiles", "gitconfig"), true},
		{"config dir through the link", filepath.Join(cfgLink, "trust.json"), true},
		{"config dir as resolved", filepath.Join(cfgReal, "trust.json"), true},
		{"a neighbour of a resolved target", filepath.Join(store, "dotfiles", "vimrc"), false},
		{"an ordinary workspace file", filepath.Join(root, "main.go"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ws.IsProtected(tt.path); got != tt.want {
				t.Errorf("IsProtected(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestProtectedHonoursConfigDir(t *testing.T) {
	fx := newFixture(t)
	custom := filepath.Join(fx.outside, "wcfg")
	t.Setenv("WRIGHT_CONFIG_DIR", custom)
	ws, err := workspace.Open(fx.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.IsProtected(filepath.Join(custom, "trust.json")) {
		t.Error("WRIGHT_CONFIG_DIR should be protected")
	}
}

func FuzzResolve(f *testing.F) {
	for _, s := range []string{"a", "../x", "escape/x", "~/y", "$WORKSPACE/z", "/etc", "sub/../../q", "inner/../escape/w", "", ".", "..", "escape", "//", "a//b", "~", "$WORKSPACE"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if strings.ContainsRune(in, 0) {
			return
		}
		fx := newFixture(t)
		abs, inside, err := fx.ws.Resolve(in)
		if err != nil {
			return
		}
		if !filepath.IsAbs(abs) {
			t.Fatalf("Resolve(%q) returned relative %q", in, abs)
		}
		// Anything reached through the escape symlink must be reported outside.
		if inside && (strings.HasPrefix(abs, fx.outside+"/") || abs == fx.outside) {
			t.Fatalf("Resolve(%q) = %q reported inside but lives in outside dir", in, abs)
		}
		if inside && !strings.HasPrefix(abs, fx.root) {
			t.Fatalf("Resolve(%q) = %q inside but not under root", in, abs)
		}
	})
}

// resolved is the path after symlink evaluation, which is what Resolve
// returns. It keeps expectations portable between Linux and macOS.
func resolved(t *testing.T, path string) string {
	t.Helper()
	dir, base := filepath.Split(path)
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return path
	}
	return filepath.Join(real, base)
}
