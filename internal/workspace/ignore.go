package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	gitignore "github.com/sabhiram/go-gitignore"
)

// Ignore files honoured at every directory level. .wrightignore additionally
// hides paths from all tools (see Hidden); the AI-specific names are the
// cross-tool conventions other agents already respect.
var (
	ignoreFiles = []string{".gitignore", ".wrightignore", ".aiignore", ".aiexclude"}
	hiddenFiles = []string{".wrightignore"}
)

// ignoreCache memoises compiled ignore files per directory so repeated checks
// during a glob or grep do not re-read the same files.
type ignoreCache struct {
	mu    sync.Mutex
	files map[string]*gitignore.GitIgnore // key: absolute ignore-file path; nil when absent
}

func newIgnoreCache() *ignoreCache {
	return &ignoreCache{files: map[string]*gitignore.GitIgnore{}}
}

// get compiles (or fetches) the ignore file at path; nil when it does not exist.
func (c *ignoreCache) get(path string) *gitignore.GitIgnore {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gi, ok := c.files[path]; ok {
		return gi
	}
	var gi *gitignore.GitIgnore
	if _, err := os.Stat(path); err == nil {
		if compiled, err := gitignore.CompileIgnoreFile(path); err == nil {
			gi = compiled
		}
	}
	c.files[path] = gi
	return gi
}

// Ignored reports whether abs (already resolved) is excluded by a .gitignore,
// .wrightignore, .aiignore or .aiexclude in the containing root or any
// directory between the root and the path. The .git directory itself is
// always ignored, as git does.
func (w *Workspace) Ignored(abs string) bool {
	if isGitDir(abs) {
		return true
	}
	return w.matchesIgnore(abs, ignoreFiles)
}

// Hidden reports whether abs is listed in a .wrightignore — such paths are
// invisible to every tool, not merely skipped by globbing.
func (w *Workspace) Hidden(abs string) bool {
	return w.matchesIgnore(abs, hiddenFiles)
}

// matchesIgnore walks from the containing root down to the parent of abs,
// consulting the named ignore files in each directory. Deeper files are
// checked last so a negation nearer the path wins, matching git's order.
func (w *Workspace) matchesIgnore(abs string, names []string) bool {
	root := w.rootOf(abs)
	if root == "" {
		return false
	}
	rel := abs[len(root):]
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	if rel == "" {
		return false
	}
	isDir := false
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		isDir = true
	}
	parts := strings.Split(rel, string(filepath.Separator))
	ignored := false
	dir := root
	for i := range parts {
		sub := filepath.ToSlash(filepath.Join(parts[i:]...))
		if isDir {
			sub += "/"
		}
		for _, name := range names {
			gi := w.ignores.get(filepath.Join(dir, name))
			if gi == nil {
				continue
			}
			if m, pat := gi.MatchesPathHow(sub); m && pat != nil {
				ignored = !pat.Negate
			}
		}
		dir = filepath.Join(dir, parts[i])
	}
	return ignored
}

// rootOf returns the root containing abs, or "".
func (w *Workspace) rootOf(abs string) string {
	for _, r := range w.Roots {
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return r
		}
	}
	return ""
}

// isGitDir reports whether abs is a .git directory or inside one.
func isGitDir(abs string) bool {
	return slices.Contains(strings.Split(abs, string(filepath.Separator)), ".git")
}
