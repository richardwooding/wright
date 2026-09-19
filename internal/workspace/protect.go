package workspace

import (
	"path/filepath"
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
func (w *Workspace) IsProtected(abs string) bool {
	if abs == "/etc" || strings.HasPrefix(abs, "/etc/") {
		return true
	}
	if under(abs, w.ConfigDir) {
		return true
	}
	if w.Home != "" {
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
	}
	for _, r := range w.Roots {
		if under(abs, filepath.Join(r, ".wright")) || under(abs, filepath.Join(r, ".git")) {
			return true
		}
	}
	return false
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
