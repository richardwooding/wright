package workspace

import (
	"path/filepath"
	"slices"
	"strings"
)

// secretBaseGlobs match file names that hold credentials. They are checked
// against the base name so they apply anywhere in the tree.
var secretBaseGlobs = []string{
	".env", ".env.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks",
	"id_*", "*.kdbx",
	"credentials*", "service-account*.json",
}

// secretExceptions are the templated/public variants that look like secrets
// but are meant to be read.
var secretExceptions = []string{".example", ".sample", ".template", ".pub"}

// sensitiveBaseGlobs are files that are not secrets outright but often carry
// them (Terraform state/vars); tools should ask before reading them.
var sensitiveBaseGlobs = []string{"*.tfvars", "*.tfstate", "*.tfstate.backup"}

// protectedSystemDirs are the system locations no tool may write to, whoever
// the user is. They are spelled as the system spells them; Open records the
// symlink-resolved spelling of each one as well, because that is the form
// Resolve produces (on macOS /etc is a symlink to /private/etc).
var protectedSystemDirs = []string{"/etc"}

// protectedHomeDirs are directories under $HOME that no tool may write to
// (and that reads are denied for, outside the hard-deny set).
var protectedHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".kube",
	filepath.Join(".config", "gh"),
	filepath.Join(".config", "fish"),
}

// protectedHomeFiles are single files under $HOME with the same treatment,
// including shell rc files (a modified rc file runs on the user's next login).
var protectedHomeFiles = []string{
	filepath.Join(".docker", "config.json"),
	".netrc", ".npmrc", ".pypirc", ".gitconfig",
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zprofile", ".zshenv", ".zlogin", ".zlogout",
}

// IsSecretFile reports whether abs looks like a credential file by name.
func (w *Workspace) IsSecretFile(abs string) bool {
	base := filepath.Base(abs)
	for _, exc := range secretExceptions {
		if strings.HasSuffix(base, exc) {
			return false
		}
	}
	return matchesAny(secretBaseGlobs, base)
}

// IsSensitive reports whether abs is a file tools should ask before reading
// (Terraform state and variable files).
func (w *Workspace) IsSensitive(abs string) bool {
	return matchesAny(sensitiveBaseGlobs, filepath.Base(abs))
}

// IsProtected reports whether abs is a location no tool may write to:
// credential directories and rc files in $HOME, wright's own config, /etc,
// and the workspace's .wright and .git directories.
//
// Both spellings are checked: the path as configured, and the
// symlink-resolved form recorded by Open. Every path in a policy.Request has
// been through Resolve, so on macOS /etc/passwd arrives as
// /private/etc/passwd and ~/.ssh as whatever that link points at — a
// literal-only set would match neither, which is a hole in a floor no mode
// overrides.
func (w *Workspace) IsProtected(abs string) bool {
	if w.protectedLiterally(abs) {
		return true
	}
	for _, d := range w.resolvedDirs {
		if under(abs, d) {
			return true
		}
	}
	if slices.Contains(w.resolvedFiles, abs) {
		return true
	}
	// Any .git or .wright directory is protected, not just the roots': a
	// monorepo's submodule or a vendored checkout carries hooks that run
	// outside the sandbox just as the top-level one does.
	for seg := range strings.SplitSeq(filepath.ToSlash(abs), "/") {
		if seg == ".git" || seg == ".wright" {
			return true
		}
	}
	return false
}

// protectedLiterally applies the protected set as it is configured. It needs
// no precomputed state, so a Workspace built without Open still denies these
// writes; the resolved spellings in IsProtected are an addition to it, never
// a replacement.
func (w *Workspace) protectedLiterally(abs string) bool {
	for _, d := range protectedSystemDirs {
		if under(abs, d) {
			return true
		}
	}
	if under(abs, w.ConfigDir) {
		return true
	}
	if w.Home == "" {
		return false
	}
	for _, d := range protectedHomeDirs {
		if under(abs, filepath.Join(w.Home, d)) {
			return true
		}
	}
	for _, f := range protectedHomeFiles {
		if abs == filepath.Join(w.Home, f) {
			return true
		}
	}
	return false
}

// protectedPaths lists the protected directory prefixes and single files as
// they are configured, for Open to resolve.
func protectedPaths(home, configDir string) (dirs, files []string) {
	dirs = append(dirs, protectedSystemDirs...)
	if configDir != "" {
		dirs = append(dirs, configDir)
	}
	if home == "" {
		return dirs, files
	}
	for _, d := range protectedHomeDirs {
		dirs = append(dirs, filepath.Join(home, d))
	}
	for _, f := range protectedHomeFiles {
		files = append(files, filepath.Join(home, f))
	}
	return dirs, files
}

// resolvedSpellings returns the symlink-resolved form of every path whose
// resolved form differs from the literal one — /etc on macOS, a $HOME that
// sits under a link, a ~/.ssh or ~/.gitconfig managed by a dotfiles
// repository. Paths that do not exist (or cannot be resolved) contribute
// nothing: resolveSymlinkSafe already returns them unchanged.
func resolvedSpellings(paths []string) []string {
	var out []string
	for _, p := range paths {
		real, err := resolveSymlinkSafe(p)
		if err != nil || real == p || slices.Contains(out, real) {
			continue
		}
		out = append(out, real)
	}
	return out
}

// under reports whether abs equals dir or is beneath it.
func under(abs, dir string) bool {
	return dir != "" && (abs == dir || strings.HasPrefix(abs, dir+string(filepath.Separator)))
}

func matchesAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}
