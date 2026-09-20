// Package policy decides whether a tool call may run. It combines the
// permission mode, the layered rule lists (builtin < user < project <
// project-local < flags < session grants) and the shell classifier into one
// verdict per request, evaluated as a lattice with no specificity scoring:
//
//	hard-deny set → deny rules → ask rules → allow rules / grants → mode table
//
// The hard-deny set applies in every mode, including bypass. A shell script
// is allowed by rules only when every one of its commands matches an allow
// rule and nothing in it is opaque to static analysis.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/richardwooding/wright/internal/policy/shellclass"
)

// Mode is the permission mode. The zero value is the safe default.
type Mode int

// The four modes. Plan is the most restrictive; Bypass the least and only
// reachable through the --bypass-permissions flag.
const (
	ModeDefault Mode = iota
	ModePlan
	ModeAutoEdit
	ModeBypass
)

var modeNames = map[Mode]string{ModeDefault: "default", ModePlan: "plan", ModeAutoEdit: "auto-edit", ModeBypass: "bypass"}

// String returns the mode's CLI name.
func (m Mode) String() string {
	if s, ok := modeNames[m]; ok {
		return s
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// ParseMode parses a CLI or settings mode name.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default":
		return ModeDefault, nil
	case "plan":
		return ModePlan, nil
	case "auto-edit", "autoedit", "auto_edit", "accept-edits":
		return ModeAutoEdit, nil
	case "bypass", "bypass-permissions":
		return ModeBypass, nil
	}
	return ModeDefault, fmt.Errorf("policy: unknown mode %q (default, plan, auto-edit)", s)
}

// permissiveness orders modes from most to least restrictive so a child
// engine can be clamped to no more than its parent.
func permissiveness(m Mode) int {
	switch m {
	case ModePlan:
		return 0
	case ModeDefault:
		return 1
	case ModeAutoEdit:
		return 2
	case ModeBypass:
		return 3
	}
	return 1
}

// Decision is the outcome of a rule or a verdict. Deny is the zero value so
// an uninitialised decision fails closed.
type Decision int

// Decisions, in increasing permissiveness.
const (
	Deny Decision = iota
	Ask
	Allow
)

var decisionNames = [...]string{"deny", "ask", "allow"}

// String returns "deny", "ask" or "allow".
func (d Decision) String() string {
	if d < 0 || int(d) >= len(decisionNames) {
		return fmt.Sprintf("decision(%d)", int(d))
	}
	return decisionNames[d]
}

// Source records where a rule came from, for explanations and trust.
type Source string

// Rule sources in precedence order (later layers are consulted alongside
// earlier ones; the lattice, not the source, decides).
const (
	SourceBuiltin      Source = "builtin"
	SourceUser         Source = "user"
	SourceProject      Source = "project"
	SourceProjectLocal Source = "project.local"
	SourceFlag         Source = "flag"
	SourceSession      Source = "session"
	// SourceTrust is not a rule source: no rule ever carries it. It names
	// the workspace-trust baseline in a verdict's Reason so the approval
	// prompt and the audit log can say which decision stopped the prompt —
	// see Engine.TrustWorkspace.
	SourceTrust Source = "workspace-trust"
)

// Scope says how long a granted rule lives.
type Scope int

// Grant scopes.
const (
	ScopeOnce         Scope = iota // this call only; nothing is recorded
	ScopeSession                   // until the process exits
	ScopeProjectLocal              // written to .wright/settings.local.json
)

// String returns a short label for the scope.
func (s Scope) String() string {
	switch s {
	case ScopeOnce:
		return "once"
	case ScopeSession:
		return "session"
	case ScopeProjectLocal:
		return "project"
	}
	return fmt.Sprintf("scope(%d)", int(s))
}

// GrantOffer is a rule the user may accept to avoid being asked again.
type GrantOffer struct {
	Rule  Rule
	Scope Scope
	Label string
}

// Request describes one tool call for evaluation. Paths and Writes are
// absolute, symlink-resolved paths; Shell is set for the bash tool; URL for
// web tools. Depth is the subagent nesting level.
type Request struct {
	Tool   string
	Args   json.RawMessage
	Paths  []string
	Writes []string
	Shell  *shellclass.Analysis
	URL    *url.URL
	Depth  int
	// Network is set when the model explicitly asked for network access.
	Network bool
	// ReadOnly carries an MCP server's readOnlyHint annotation. It is
	// advisory: it only decides whether plan mode asks or denies.
	ReadOnly bool
	// Cwd is the directory a shell command will run in. A recursive reader
	// given no path operand reads it, and nothing in the command names it,
	// so the credential scan needs it to see what the command would sweep up.
	Cwd string
}

// Verdict is the evaluation result.
type Verdict struct {
	Decision Decision
	// Rule is the rule that decided, when one did.
	Rule *Rule
	// Class is the request's shell class (or the equivalent for other tools).
	Class shellclass.Class
	// Reason is the one-line explanation shown with the decision.
	Reason string
	// Explain is the full trace through the lattice.
	Explain []string
	// Offers are grants that would make the same request pass next time.
	Offers []GrantOffer
	// HardDeny is true when the hard-deny set decided; counted per run.
	HardDeny bool
	// Network is true when the command should run with network access if it
	// runs at all (a +net rule matched, or the request asked and was allowed).
	Network bool
	// Installs is true when a +install rule allowed an installing command,
	// so the caller should also make the package-manager prefixes writable
	// for that one call. The paths themselves live in the sandbox package;
	// policy names the grant, not the directories.
	Installs bool
}

// Errors returned by Engine methods.
var (
	ErrBypassNotSettable = errors.New("policy: bypass mode can only be enabled with --bypass-permissions")
	ErrChildGrant        = errors.New("policy: subagents cannot grant permissions")
	ErrModeAboveParent   = errors.New("policy: subagent mode cannot exceed the parent's")
	ErrNoPersist         = errors.New("policy: no persistence configured for project grants")
)
