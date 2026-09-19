package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

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

// splitReadWrite turns the requested read-write paths into Landlock rule
// inputs that never grant write on a protected subpath. Landlock rules are
// additive — the kernel unions every rule matching an ancestor — so a
// subtree cannot be subtracted from a read-write rule. The only way to keep
// `.git/hooks` or `.wright` read-only is to grant read-write on each sibling
// instead of on their parent, which is what this does, level by level. The
// split roots are returned as read-only so everything stays *readable*.
func splitReadWrite(rw []string) (roDirs, rwDirs, rwFiles []string) {
	protected := ProtectedPaths(rw)
	for _, root := range rw {
		if slices.Contains(protected, root) {
			roDirs = append(roDirs, root) // asked for read-write, but protected
			continue
		}
		under := descendants(root, protected)
		if len(under) == 0 {
			rwDirs = append(rwDirs, root)
			continue
		}
		roDirs = append(roDirs, root)
		d, f := writableTree(root, under)
		rwDirs = append(rwDirs, d...)
		rwFiles = append(rwFiles, f...)
	}
	return roDirs, rwDirs, rwFiles
}

// writableTree lists the entries of dir that may be granted read-write given
// the protected paths under it: an entry that is protected is skipped, an
// entry that only *contains* a protected path is expanded one level deeper,
// and anything else is granted whole.
func writableTree(dir string, protected []string) (dirs, files []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // unreadable: grant nothing rather than too much
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if slices.Contains(protected, p) {
			continue
		}
		info, err := os.Stat(p) // follow symlinks: Landlock rules are per-inode
		if err != nil {
			continue
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		if under := descendants(p, protected); len(under) > 0 {
			d, f := writableTree(p, under)
			dirs, files = append(dirs, d...), append(files, f...)
			continue
		}
		dirs = append(dirs, p)
	}
	return dirs, files
}

// descendants returns the protected paths that lie strictly under dir.
func descendants(dir string, protected []string) []string {
	prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)
	var out []string
	for _, p := range protected {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
