// Package shellclass classifies shell scripts by what they can do, using a
// real bash AST (mvdan.cc/sh) rather than string matching. Every simple
// command reached through pipes, &&/||, subshells, loops and functions is
// looked up in a command table with subcommand granularity; redirects are
// turned into declared writes and reads; constructs the analyser cannot see
// through (eval, $(…), sh -c "$X") mark the whole script Unknown so it can
// never match an allow rule; and a small set of catastrophic patterns
// (privilege escalation, curl|sh, rm -rf of $HOME or a root, force-pushing a
// protected branch) set HardDeny so no mode can run them.
//
// The package is a leaf: it depends only on the Workspace interface below,
// which internal/policy adapts from internal/workspace and internal/git.
package shellclass

import (
	"fmt"
	"strings"
)

// Class orders commands by the damage they can do. Higher is worse; a
// script's Class is the maximum over its commands.
type Class int

// The classes, from harmless to catastrophic.
const (
	// SafeRead reads files or state and has no side effects.
	SafeRead Class = iota
	// MutatingWorkspace changes files inside the workspace (build, format, edit).
	MutatingWorkspace
	// Network reaches outside the machine (fetch, push, install, API calls).
	Network
	// Destructive removes or irreversibly overwrites data.
	Destructive
	// Privilege escalates, touches the system, or executes remote code.
	Privilege
)

var classNames = [...]string{"safe-read", "mutating", "network", "destructive", "privilege"}

// String returns the lowercase class name.
func (c Class) String() string {
	if c < 0 || int(c) >= len(classNames) {
		return fmt.Sprintf("class(%d)", int(c))
	}
	return classNames[c]
}

// Command is one simple command after wrapper peeling.
type Command struct {
	// Argv is the literal argument vector; dynamic words appear as their
	// source text ($VAR, $(…)) and set Dynamic.
	Argv []string
	// Env holds NAME=value prefixes attached to the command.
	Env []string
	// Class is the command's own class.
	Class Class
	// Reason is a short human explanation of the class.
	Reason string
	// Dynamic is true when any argument depends on runtime expansion.
	Dynamic bool
	// Writes and Reads are the absolute paths the command declares it will
	// write (redirects, tee, cp destinations…) or read.
	Writes []string
	Reads  []string
	// Network is true when the command needs network access to succeed.
	Network bool
	// Installs is true when the command installs, upgrades or removes
	// software outside the workspace (a package manager's install verbs).
	// Approving such a call is what makes the tool prefixes writable for
	// that one call; nothing else widens the sandbox.
	Installs bool
}

// Analysis is the result for a whole script.
type Analysis struct {
	// Raw is the script as given.
	Raw string
	// Commands are the simple commands found, in evaluation order.
	Commands []Command
	// Class is the maximum command class (and any pattern-level floor).
	Class Class
	// Unknown is true when the script contains constructs whose effect cannot
	// be determined statically. Unknown scripts never match allow rules.
	Unknown bool
	// HardDeny, when non-empty, names a pattern that no mode may run.
	HardDeny string
	// Reasons collects the notable findings for the approval prompt.
	Reasons []string
	// NeedsNetwork is true when any command needs the network.
	NeedsNetwork bool
	// Installs is true when any command installs software outside the
	// workspace, so approving the script should make the tool prefixes
	// writable for that call.
	Installs bool
}

// Workspace is the slice of the workspace the classifier needs. It is an
// interface so this package stays a leaf; internal/policy adapts the real
// workspace and git packages.
type Workspace interface {
	// Resolve expands ~ and $WORKSPACE, resolves symlinks and reports whether
	// the path is inside the workspace.
	Resolve(string) (abs string, inside bool, err error)
	// IsSecretFile reports whether the path looks like a credential file.
	IsSecretFile(string) bool
	// IsProtected reports whether the path may never be written.
	IsProtected(string) bool
	// IsTracked reports whether the path is tracked by git.
	IsTracked(string) bool
	// Home is the user's home directory.
	Home() string
	// Roots are the workspace roots.
	Roots() []string
}

// BranchProtector is optionally implemented by a Workspace to override the
// default protected branch patterns (main, master, release/*).
type BranchProtector interface {
	ProtectedBranches() []string
}

// maxDepth bounds `sh -c` recursion.
const maxDepth = 3

// Analyze classifies script against ws. It never panics on any input; a
// script that does not parse is Unknown.
func Analyze(script string, ws Workspace) Analysis {
	a := &analyzer{ws: ws, out: &Analysis{Raw: script}}
	a.run(script)
	a.finish()
	return *a.out
}

// Summary renders a one-line description for prompts and audit logs.
func (a Analysis) Summary() string {
	var b strings.Builder
	b.WriteString(a.Class.String())
	if a.Unknown {
		b.WriteString(" (opaque)")
	}
	if a.HardDeny != "" {
		b.WriteString(" — hard deny: ")
		b.WriteString(a.HardDeny)
	}
	if len(a.Reasons) > 0 {
		b.WriteString(" — ")
		b.WriteString(strings.Join(dedupe(a.Reasons), "; "))
	}
	if a.NeedsNetwork {
		b.WriteString(" [needs network]")
	}
	if a.Installs {
		b.WriteString(" [installs software]")
	}
	return b.String()
}

// dedupe removes repeated reasons while keeping order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
