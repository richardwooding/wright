package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Grant is the extra reach one approved call gets beyond the base Spec. It
// exists because "allowed" and "able to work" are not the same thing: a
// command the classifier says needs the network, or that installs software
// outside the workspace, fails in a way the user did not choose unless the
// approval that let it run also gives it what it needs.
//
// A Grant is per call. Nothing here changes the base Spec, and only the
// engine puts one on a context — a tool never grants itself anything.
type Grant struct {
	// Network runs this call with network access.
	Network bool
	// Writable are directories bound read-write for this call only. The
	// approval prompt names them, so the user consents to these paths and
	// not to a vague widening.
	Writable []string
}

// Empty reports a grant that changes nothing.
func (g Grant) Empty() bool { return !g.Network && len(g.Writable) == 0 }

// grantKey carries a Grant on a call's context.
type grantKey struct{}

// WithGrant attaches g to ctx for the tool call about to run.
func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// GrantFrom reports the grant attached with WithGrant, if any.
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}

// ToolPrefixes returns the directories a package manager installs into on
// this machine: the Homebrew prefix and cache, the npm global prefix, the
// cargo and rustup homes, the Go binary directory, the user's ~/.local tool
// directories and the gem home. Only directories that already exist are
// returned, in the same spirit as Caches().
//
// They are never in the base Spec — an agent that can write the directories
// on the user's PATH can hand the user a binary to run outside the sandbox,
// which is the same escalation as a git hook. They are mounted read-write
// only for a single call the user approved after being shown this list.
func ToolPrefixes() []string {
	home, _ := os.UserHomeDir()
	var out []string
	add := func(paths ...string) {
		for _, p := range paths {
			if !usableToolPrefix(p, home) {
				continue
			}
			p = filepath.Clean(p)
			if slices.Contains(out, p) {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				out = append(out, p)
			}
		}
	}
	add(homebrewPrefix(), homebrewCache(home))
	add(envOr("NPM_CONFIG_PREFIX", os.Getenv("npm_config_prefix")),
		filepath.Join(home, ".npm-global"), filepath.Join(home, ".npm-packages"))
	add(envOr("CARGO_HOME", filepath.Join(home, ".cargo")))
	add(envOr("RUSTUP_HOME", filepath.Join(home, ".rustup")))
	add(goBin(home))
	add(filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "pipx"),
		filepath.Join(home, ".local", "share", "uv"))
	add(envOr("GEM_HOME", filepath.Join(home, ".gem")))
	return out
}

// homebrewPrefix is $HOMEBREW_PREFIX, or the standard location for this
// platform. /usr/local is only Homebrew's when it holds a Homebrew install —
// on every other machine it is an ordinary system directory that must stay
// read-only.
func homebrewPrefix() string {
	if p := os.Getenv("HOMEBREW_PREFIX"); p != "" {
		return p
	}
	for _, p := range []string{"/opt/homebrew", "/home/linuxbrew/.linuxbrew"} {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p
		}
	}
	if info, err := os.Stat("/usr/local/Cellar"); err == nil && info.IsDir() {
		return "/usr/local"
	}
	return ""
}

// homebrewCache is where downloaded bottles land.
func homebrewCache(home string) string {
	if p := os.Getenv("HOMEBREW_CACHE"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "Homebrew")
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "Homebrew")
}

// goBin is where `go install` puts a binary.
func goBin(home string) string {
	if p := os.Getenv("GOBIN"); p != "" {
		return p
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(home, "go")
	}
	return filepath.Join(gopath, "bin")
}

// usableToolPrefix refuses anything that would widen far more than a tool
// prefix: a relative path, a filesystem root, $HOME itself or a first-level
// directory such as /usr or /opt. A misconfigured HOMEBREW_PREFIX must not
// become a read-write bind of half the system.
func usableToolPrefix(p, home string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	if p == home {
		return false
	}
	// At least two path segments: "/opt/homebrew" yes, "/opt" and "/" no.
	return strings.Count(strings.Trim(p, string(filepath.Separator)), string(filepath.Separator)) >= 1
}
