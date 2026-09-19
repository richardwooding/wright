// Package workspace defines the directories the agent may touch and answers
// the questions every tool asks before acting: is this path inside the
// workspace (after symlinks), is it ignored, hidden, a secret file, or a
// protected system/config location.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrOutside is returned by callers that refuse paths outside the workspace.
var ErrOutside = errors.New("path is outside the workspace")

// gitTimeout bounds the `git rev-parse` call so a slow filesystem or a hung
// git never blocks startup.
const gitTimeout = 2 * time.Second

// Workspace is the set of roots the agent may work in plus the home and
// config directories needed to recognise protected paths.
type Workspace struct {
	// Roots are absolute, symlink-resolved directories; Roots[0] is the
	// primary root (git toplevel or the starting directory).
	Roots []string
	// Home is the user's home directory, symlink-resolved.
	Home string
	// ConfigDir is wright's own config directory (protected from writes).
	ConfigDir string

	// resolvedDirs and resolvedFiles are the protected locations in their
	// symlink-resolved spelling, recorded once at Open because that is the
	// spelling Resolve hands to IsProtected.
	resolvedDirs  []string
	resolvedFiles []string

	ignores *ignoreCache
}

// Open discovers the workspace for start (a directory inside a git repo uses
// the repo toplevel) and adds extra roots (--add-dir). Every root is made
// absolute and symlink-resolved so later containment checks are exact.
func Open(start string, extra []string) (*Workspace, error) {
	root, err := canonical(start)
	if err != nil {
		return nil, err
	}
	if top := gitToplevel(root); top != "" {
		root = top
	}
	roots := []string{root}
	for _, e := range extra {
		abs, err := canonical(e)
		if err != nil {
			return nil, err
		}
		roots = append(roots, abs)
	}
	home, _ := os.UserHomeDir()
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	w := &Workspace{
		Roots:     roots,
		Home:      home,
		ConfigDir: configDir(home),
		ignores:   newIgnoreCache(),
	}
	// The protected set is resolved here, once, rather than on every check:
	// it names system and home locations that do not move during a run.
	dirs, files := protectedPaths(w.Home, w.ConfigDir)
	w.resolvedDirs, w.resolvedFiles = resolvedSpellings(dirs), resolvedSpellings(files)
	return w, nil
}

// configDir mirrors config.DefaultPaths without importing it (workspace is a
// leaf package).
func configDir(home string) string {
	if v := os.Getenv("WRIGHT_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "wright")
	}
	return filepath.Join(home, ".config", "wright")
}

// canonical returns the absolute, symlink-resolved form of an existing directory.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace root is not a directory: " + p)
	}
	return real, nil
}

// gitToplevel returns the repository toplevel containing dir, or "".
func gitToplevel(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(top); err == nil {
		return real
	}
	return top
}

// Root is the primary root.
func (w *Workspace) Root() string { return w.Roots[0] }

// Hash identifies the workspace for per-project storage: sha256(root)[:16].
func (w *Workspace) Hash() string {
	sum := sha256.Sum256([]byte(w.Root()))
	return hex.EncodeToString(sum[:])[:16]
}

// Expand replaces a leading "~/" or "$WORKSPACE/" and makes relative paths
// workspace-relative. It does not touch the filesystem.
func (w *Workspace) Expand(p string) string {
	switch {
	case p == "~":
		return w.Home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(w.Home, p[2:])
	case p == "$WORKSPACE":
		return w.Root()
	case strings.HasPrefix(p, "$WORKSPACE/"):
		return filepath.Join(w.Root(), p[len("$WORKSPACE/"):])
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	default:
		return filepath.Join(w.Root(), p)
	}
}

// Resolve expands p, resolves symlinks on the deepest existing ancestor and
// reports whether the result lies under any root. Non-existent leaves are
// fine (a file about to be created), but a symlink anywhere on the existing
// part of the path is followed, so a link out of the workspace is caught.
func (w *Workspace) Resolve(p string) (abs string, inside bool, err error) {
	abs, err = resolveSymlinkSafe(w.Expand(p))
	if err != nil {
		return "", false, err
	}
	return abs, w.contains(abs), nil
}

// resolveSymlinkSafe returns EvalSymlinks(deepest existing ancestor) + rest.
func resolveSymlinkSafe(abs string) (string, error) {
	abs = filepath.Clean(abs)
	existing := abs
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// Nothing exists (not even "/"?) — cannot happen on a real FS, but
			// be defensive rather than loop forever.
			return abs, nil
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{real}, rest...)...), nil
}

// contains reports whether abs is one of the roots or beneath one.
func (w *Workspace) contains(abs string) bool {
	for _, r := range w.Roots {
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Rel returns abs relative to the root that contains it, or abs unchanged
// when it is outside every root.
func (w *Workspace) Rel(abs string) string {
	for _, r := range w.Roots {
		if abs == r {
			return "."
		}
		if strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return abs[len(r)+1:]
		}
	}
	return abs
}

// Inside reports whether an already-resolved absolute path is in the workspace.
func (w *Workspace) Inside(abs string) bool { return w.contains(abs) }

// IsHome reports whether abs is the home directory itself.
func (w *Workspace) IsHome(abs string) bool { return abs == w.Home }
