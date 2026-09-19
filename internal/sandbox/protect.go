package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// ErrNoMountProtection reports that the read-only bind mounts that keep
// `.git/hooks` and `.wright` unwritable could not be made — no mount
// namespace here. It is deliberately not fatal: refusing to run any command
// would be worse, and the backend's Warnings() has already told the user
// that those paths rest on the permission layer alone.
var ErrNoMountProtection = errors.New("sandbox: read-only bind mounts unavailable")

// gitProtected are the entries of a `.git` directory that must never be
// writable from inside the sandbox: they are configuration and scripts that
// *git itself runs outside the sandbox* on the user's next command. The rest
// of `.git` stays writable so `git add` and `git commit` keep working inside.
var gitProtected = []string{"hooks", "config", "config.worktree"}

// ProtectedIn returns the existing paths under root that must stay read-only
// even though root is bound read-write: the git hook and config surfaces, and
// `.wright`, which holds the settings and trust state that decide what wright
// is allowed to do in the *next* session.
func ProtectedIn(root string) []string {
	var out []string
	if p := filepath.Join(root, ".wright"); exists(p) {
		out = append(out, p)
	}
	git := filepath.Join(root, ".git")
	info, err := os.Lstat(git)
	switch {
	case err != nil:
		return out // no .git at all: a plain directory must still work
	case !info.IsDir():
		// A `.git` *file* (worktree or submodule) names the real git
		// directory; rewriting it redirects every later git command.
		return append(out, git)
	}
	for _, name := range gitProtected {
		if p := filepath.Join(git, name); exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// EnsureProtected creates the protected paths of each workspace root that do
// not exist yet. A path that is absent cannot be bound read-only or denied,
// so without this the protection is only as good as the directory listing at
// the moment the sandbox was built: a payload simply runs `mkdir .wright`
// and writes the settings file into the directory it just made.
//
// Only the workspace roots are touched, never the caches or
// sandbox.extraReadWrite (which may name "/"), and only inside structures
// that already exist: `.git/hooks` and `.git/config` are created when `.git`
// is a directory, never a `.git` of wright's own invention, which would turn
// a plain directory into something git treats as a repository.
//
// It is best effort. Where the root is read-only the creation fails — and so
// would the payload's.
func EnsureProtected(roots []string) {
	for _, root := range roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		_ = os.MkdirAll(filepath.Join(root, ".wright"), 0o700)
		git := filepath.Join(root, ".git")
		if info, err := os.Lstat(git); err != nil || !info.IsDir() {
			continue // no repository, or a .git file, which is protected as it is
		}
		_ = os.MkdirAll(filepath.Join(git, "hooks"), 0o700)
		// An empty local config is what git assumes when the file is
		// missing, so creating it changes nothing — except that
		// core.hooksPath can no longer be introduced from inside.
		if cfg := filepath.Join(git, "config"); !exists(cfg) {
			if f, err := os.OpenFile(cfg, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
				_ = f.Close()
			}
		}
	}
}

// UserDirs returns wright's own configuration and state directories
// ($WRIGHT_CONFIG_DIR / $XDG_CONFIG_HOME/wright and $WRIGHT_DATA_DIR /
// $XDG_DATA_HOME/wright). They hold trust.json, the settings and the audit
// logs, so a sandboxed command that could write them would decide what
// wright may do next time. Only existing directories are returned.
func UserDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		wrightDir("WRIGHT_CONFIG_DIR", "XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		wrightDir("WRIGHT_DATA_DIR", "XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d != "" && exists(d) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// wrightDir mirrors config.DefaultPaths without importing config, which sits
// above this package. TestUserDirsMatchConfigPaths pins the two together.
func wrightDir(override, xdgName, fallback string) string {
	if v := os.Getenv(override); v != "" {
		return v
	}
	base := os.Getenv(xdgName)
	if base == "" {
		base = fallback
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "wright")
}

// ProtectedPaths returns every path that must stay read-only inside the
// sandbox although a read-write root contains it, for the read-write set rw.
func ProtectedPaths(rw []string) []string {
	var out []string
	for _, root := range rw {
		for _, p := range ProtectedIn(root) {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	for _, d := range UserDirs() {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
