package policy

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/workspace"
)

// Engine evaluates requests against a mode and rule set. It is safe for
// concurrent use; parallel tool calls evaluate concurrently.
type Engine struct {
	mu          sync.Mutex
	ws          *workspace.Workspace
	mode        Mode
	rules       []Rule // every configured rule, all layers flattened
	grants      []Rule // session grants (and persisted ones applied live)
	persist     func(Rule) error
	hardDenials int
	parent      *Engine
	depth       int
}

// New builds an engine over the workspace with the given mode and rule
// layers (any order; the lattice does not rank layers).
func New(ws *workspace.Workspace, mode Mode, layers ...[]Rule) *Engine {
	e := &Engine{ws: ws, mode: mode}
	for _, l := range layers {
		e.rules = append(e.rules, l...)
	}
	return e
}

// SetPersist installs the callback that records project-local grants.
func (e *Engine) SetPersist(fn func(Rule) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persist = fn
}

// Mode returns the current mode.
func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode changes the mode. Bypass is refused here by design: it can only be
// set at construction from the command-line flag. A child engine cannot be
// made more permissive than its parent.
func (e *Engine) SetMode(m Mode) error {
	if m == ModeBypass {
		return ErrBypassNotSettable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.parent != nil && permissiveness(m) > permissiveness(e.parent.Mode()) {
		return ErrModeAboveParent
	}
	e.mode = m
	return nil
}

// HardDenials returns how many hard-deny verdicts this engine (and its
// children) has issued; the run is cancelled after three.
func (e *Engine) HardDenials() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hardDenials
}

// Depth returns the subagent nesting level (0 for the root engine).
func (e *Engine) Depth() int { return e.depth }

// Child derives an engine for a subagent: the same rules and a snapshot of
// the grants, mode clamped to the parent's (bypass is never inherited), and
// no ability to grant.
func (e *Engine) Child(depth int) *Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	mode := e.mode
	if mode == ModeBypass {
		mode = ModeDefault
	}
	return &Engine{
		ws:     e.ws,
		mode:   mode,
		rules:  e.rules,
		grants: slices.Clone(e.grants),
		parent: e,
		depth:  depth,
	}
}

// Grant records an offer the user accepted. Once records nothing; Session
// adds an in-memory rule; ProjectLocal persists it and applies it live.
func (e *Engine) Grant(offer GrantOffer) error {
	if e.parent != nil {
		return ErrChildGrant
	}
	rule := offer.Rule
	rule.Decision = Allow
	e.mu.Lock()
	defer e.mu.Unlock()
	switch offer.Scope {
	case ScopeOnce:
		return nil
	case ScopeSession:
		rule.Source = SourceSession
	case ScopeProjectLocal:
		if e.persist == nil {
			return ErrNoPersist
		}
		rule.Source = SourceProjectLocal
		if err := e.persist(rule); err != nil {
			return err
		}
	}
	e.grants = append(e.grants, rule)
	return nil
}

// Grants returns the session grants recorded so far.
func (e *Engine) Grants() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.grants)
}

// noteHardDenial bumps the counter here and in every ancestor.
func (e *Engine) noteHardDenial() {
	for cur := e; cur != nil; cur = cur.parent {
		cur.mu.Lock()
		cur.hardDenials++
		cur.mu.Unlock()
	}
}

// snapshot copies the mutable state so evaluation runs without the lock.
func (e *Engine) snapshot() (Mode, []Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	all := make([]Rule, 0, len(e.rules)+len(e.grants))
	all = append(all, e.rules...)
	all = append(all, e.grants...)
	return e.mode, all
}

// eval carries one evaluation.
type eval struct {
	e     *Engine
	mode  Mode
	rules []Rule
	req   Request
	kind  reqKind
	v     Verdict
}

// reqKind buckets tools for the mode table.
type reqKind int

const (
	kindRead reqKind = iota
	kindWrite
	kindBash
	kindWeb
	kindMCP
	kindOther
)

var (
	readTools  = map[string]bool{"read_file": true, "glob": true, "grep": true, "list_dir": true}
	writeTools = map[string]bool{"write_file": true, "edit_file": true, "multi_edit": true}
	// otherTools have no side effects of their own: activating a skill or
	// reading a file bundled with it only puts text in the transcript, and
	// anything the skill then *does* goes through bash, edit_file and the
	// rest, which are evaluated on their own.
	otherTools = map[string]bool{"explore": true, "skill": true, "skill_file": true, "todo_write": true, "ask_user": true}
)

func kindOf(tool string) reqKind {
	switch {
	case readTools[tool]:
		return kindRead
	case writeTools[tool]:
		return kindWrite
	case argvTools[tool]:
		return kindBash
	case webTools[tool]:
		return kindWeb
	case strings.HasPrefix(tool, "mcp:"):
		return kindMCP
	case otherTools[tool]:
		return kindOther
	}
	return kindOther
}

// Evaluate runs the lattice for req.
func (e *Engine) Evaluate(req Request) Verdict {
	mode, rules := e.snapshot()
	ev := &eval{e: e, mode: mode, rules: rules, req: req, kind: kindOf(req.Tool)}
	ev.v.Class = ev.class()
	ev.run()
	if ev.v.HardDeny {
		e.noteHardDenial()
	}
	return ev.v
}

// class is the request's class for the audit log and prompt severity.
func (ev *eval) class() shellclass.Class {
	switch ev.kind {
	case kindBash:
		if ev.req.Shell != nil {
			return ev.req.Shell.Class
		}
		return shellclass.Privilege
	case kindWrite:
		return shellclass.MutatingWorkspace
	case kindWeb, kindMCP:
		return shellclass.Network
	}
	return shellclass.SafeRead
}

func (ev *eval) explain(format string, args ...any) {
	ev.v.Explain = append(ev.v.Explain, fmt.Sprintf(format, args...))
}

func (ev *eval) decide(d Decision, reason string, rule *Rule) {
	ev.v.Decision, ev.v.Reason, ev.v.Rule = d, reason, rule
	ev.explain("→ %s: %s", d, reason)
}

// run walks the lattice in order and stops at the first decisive step.
//
// Ask rules come in two ranks. Explicit ones (user, project, flag) sit above
// allow rules: tightening is always honoured. The builtin ask list is the
// mode table's defaults written as rules ("edits ask", "web_fetch asks"),
// so it sits below allow rules — otherwise no allow rule for edit_file or
// web_fetch could ever take effect.
func (ev *eval) run() {
	if ev.hardDeny() || ev.hidden() {
		return
	}
	if ev.matchDecision(Deny, false) {
		return
	}
	if ev.matchDecision(Ask, false) && ev.mode != ModeBypass {
		ev.finishAsk()
		return
	}
	if ev.allowed() {
		return
	}
	if ev.matchDecision(Ask, true) && ev.mode != ModeBypass {
		ev.finishAsk()
		return
	}
	ev.modeTable()
}

// hardDeny applies the set that no mode overrides.
func (ev *eval) hardDeny() bool {
	reason := ""
	switch {
	case ev.req.Shell != nil && ev.req.Shell.HardDeny != "":
		reason = ev.req.Shell.HardDeny
	default:
		reason = ev.hardDenyPaths()
	}
	if reason == "" {
		return false
	}
	ev.v.HardDeny = true
	ev.explain("hard-deny set: %s", reason)
	ev.decide(Deny, reason+" (denied in every mode)", nil)
	return true
}

// hardDenyPaths checks secret-file reads and protected-path writes.
func (ev *eval) hardDenyPaths() string {
	for _, p := range ev.req.Paths {
		if ev.e.ws.IsSecretFile(p) {
			return "reads secret file " + ev.e.ws.Rel(p)
		}
	}
	for _, p := range ev.req.Writes {
		if ev.e.ws.IsProtected(p) || ev.e.ws.IsSecretFile(p) {
			return "writes protected path " + ev.e.ws.Rel(p)
		}
	}
	return ""
}

// hidden denies any touch of a .wrightignore path: invisible to all tools.
func (ev *eval) hidden() bool {
	for _, p := range ev.allPaths() {
		if ev.e.ws.Hidden(p) {
			ev.decide(Deny, "path hidden by .wrightignore: "+ev.e.ws.Rel(p), nil)
			return true
		}
	}
	return false
}

// allPaths is every path the request touches, including those declared by
// shell commands.
func (ev *eval) allPaths() []string {
	out := append(slices.Clone(ev.req.Paths), ev.req.Writes...)
	if ev.req.Shell != nil {
		for _, c := range ev.req.Shell.Commands {
			out = append(out, c.Reads...)
			out = append(out, c.Writes...)
		}
	}
	return out
}

// matchDecision looks for any rule of the given decision that applies;
// builtin selects the builtin rank of ask rules (see run).
func (ev *eval) matchDecision(d Decision, builtin bool) bool {
	for i := range ev.rules {
		r := &ev.rules[i]
		if r.Decision != d || !r.MatchesTool(ev.req.Tool) {
			continue
		}
		if d == Ask && (r.Source == SourceBuiltin) != builtin {
			continue
		}
		if what, ok := ev.ruleHits(r); ok {
			if d == Deny {
				ev.decide(Deny, fmt.Sprintf("%s rule %s [%s] matched %s", d, r, r.Source, what), r)
			} else {
				ev.v.Rule = r
				ev.explain("ask rule %s [%s] matched %s", r, r.Source, what)
			}
			return true
		}
	}
	return false
}

// ruleHits reports whether a deny/ask rule applies to any element of the
// request and names the element.
func (ev *eval) ruleHits(r *Rule) (string, bool) {
	switch {
	case r.IsBare():
		return "the tool", true
	case r.IsPath():
		for _, p := range ev.allPaths() {
			if r.MatchesPath(p, ev.e.ws.Home, ev.e.ws.Root()) {
				return ev.e.ws.Rel(p), true
			}
		}
	case r.IsBash():
		if ev.req.Shell == nil {
			return "", false
		}
		for _, c := range ev.req.Shell.Commands {
			if r.MatchesCommand(c.Argv) {
				return strings.Join(c.Argv, " "), true
			}
		}
	case r.IsDomain():
		if ev.req.URL != nil && r.MatchesHost(ev.req.URL.Hostname()) {
			return ev.req.URL.Hostname(), true
		}
	}
	return "", false
}

// finishAsk turns a matched ask rule into the verdict, with plan-mode and
// class adjustments.
func (ev *eval) finishAsk() {
	if ev.mode == ModePlan && !ev.planMayAsk() {
		ev.decide(Deny, "plan mode: only reads are allowed", ev.v.Rule)
		return
	}
	rule := ev.v.Rule
	ev.decide(Ask, fmt.Sprintf("ask rule %s [%s]", rule, rule.Source), rule)
	ev.v.Offers = ev.e.Suggest(ev.req)
}

// planMayAsk reports the kinds plan mode still prompts for rather than
// denying outright: reads, web access and read-only-annotated MCP tools.
func (ev *eval) planMayAsk() bool {
	return ev.kind == kindRead || ev.kind == kindWeb || (ev.kind == kindMCP && ev.req.ReadOnly)
}

// allowed applies allow rules and grants: every element must be covered.
func (ev *eval) allowed() bool {
	allow := make([]*Rule, 0, len(ev.rules))
	for i := range ev.rules {
		if ev.rules[i].Decision == Allow && ev.rules[i].MatchesTool(ev.req.Tool) {
			allow = append(allow, &ev.rules[i])
		}
	}
	if len(allow) == 0 {
		ev.explain("no allow rules for %s", ev.req.Tool)
		return false
	}
	covered, rule, why := ev.coverage(allow)
	if !covered {
		ev.explain("allow rules do not cover %s", why)
		return false
	}
	ev.v.Network = ev.allowNetwork(rule)
	ev.decide(Allow, fmt.Sprintf("allow rule %s [%s]", rule, rule.Source), rule)
	return true
}

// allowNetwork reports whether an allowed bash command may use the network.
func (ev *eval) allowNetwork(rule *Rule) bool {
	if ev.kind != kindBash {
		return false
	}
	// The model asking for network (Request.Network) never grants it; only a
	// +net rule or the user's answer to a prompt does.
	return rule.Net()
}

// coverage checks the allow rules against the request's elements. It returns
// the last rule used, or the uncovered element.
func (ev *eval) coverage(allow []*Rule) (covered bool, rule *Rule, why string) {
	switch ev.kind {
	case kindBash:
		return ev.coverBash(allow)
	case kindWeb:
		host := ""
		if ev.req.URL != nil {
			host = ev.req.URL.Hostname()
		}
		for _, r := range allow {
			if r.IsBare() || r.MatchesHost(host) {
				return true, r, ""
			}
		}
		return false, nil, "host " + host
	case kindRead, kindWrite:
		return ev.coverPaths(allow)
	default:
		// MCP and other tools: any matching (bare) rule suffices.
		for _, r := range allow {
			if r.IsBare() {
				return true, r, ""
			}
		}
		return false, nil, "the tool"
	}
}

func (ev *eval) coverBash(allow []*Rule) (bool, *Rule, string) {
	sh := ev.req.Shell
	if sh == nil {
		return false, nil, "an unanalysed command"
	}
	if sh.Unknown {
		return false, nil, "an opaque script (allow rules never match dynamic shell)"
	}
	var last *Rule
	for _, c := range sh.Commands {
		matched := false
		for _, r := range allow {
			if (r.IsBare() || r.IsBash()) && r.MatchesCommand(c.Argv) {
				matched, last = true, r
				if r.Net() {
					break // prefer remembering a +net rule
				}
			}
		}
		if !matched {
			return false, nil, "command `" + strings.Join(c.Argv, " ") + "`"
		}
		if (c.Network || ev.req.Network) && !last.Net() {
			return false, nil, "network access for `" + strings.Join(c.Argv, " ") + "` (the allow rule has no +net)"
		}
	}
	if last == nil {
		return false, nil, "an empty script"
	}
	return true, last, ""
}

func (ev *eval) coverPaths(allow []*Rule) (bool, *Rule, string) {
	paths := append(slices.Clone(ev.req.Paths), ev.req.Writes...)
	if len(paths) == 0 {
		return false, nil, "a call with no paths"
	}
	var last *Rule
	for _, p := range paths {
		matched := false
		for _, r := range allow {
			if r.IsBare() || r.MatchesPath(p, ev.e.ws.Home, ev.e.ws.Root()) {
				matched, last = true, r
				break
			}
		}
		if !matched {
			return false, nil, "path " + ev.e.ws.Rel(p)
		}
	}
	return true, last, ""
}

// modeTable is the final step: the default behaviour per mode and class.
func (ev *eval) modeTable() {
	switch ev.kind {
	case kindRead:
		ev.modeRead()
	case kindWrite:
		ev.modeWrite()
	case kindBash:
		ev.modeBash()
	case kindWeb:
		ev.modeFallback(Ask, "web access")
	case kindMCP:
		ev.modeMCP()
	default:
		ev.modeOther()
	}
	if ev.v.Decision == Ask {
		ev.v.Offers = ev.e.Suggest(ev.req)
	}
}

// outside classifies the request's paths relative to the workspace: the
// first protected path (deny) or the first outside/ignored path (ask).
func (ev *eval) outside(paths []string) (deny, ask string) {
	for _, p := range paths {
		switch {
		case ev.e.ws.IsProtected(p):
			return "protected path " + ev.e.ws.Rel(p), ""
		case !ev.e.ws.Inside(p):
			if ask == "" {
				ask = "path outside the workspace " + ev.e.ws.Rel(p)
			}
		case ev.e.ws.IsSensitive(p):
			if ask == "" {
				ask = "sensitive file " + ev.e.ws.Rel(p)
			}
		}
	}
	return "", ask
}

func (ev *eval) modeRead() {
	deny, ask := ev.outside(ev.req.Paths)
	switch {
	case deny != "":
		ev.decide(Deny, deny, nil)
	case ask != "" && ev.mode != ModeBypass:
		ev.decide(Ask, ask, nil)
	default:
		ev.decide(Allow, "reads inside the workspace are allowed in every mode", nil)
	}
}

func (ev *eval) modeWrite() {
	deny, ask := ev.outside(ev.req.Writes)
	if deny != "" {
		ev.decide(Deny, deny, nil)
		return
	}
	switch ev.mode {
	case ModePlan:
		ev.decide(Deny, "plan mode: no edits", nil)
	case ModeBypass:
		ev.decide(Allow, "bypass mode", nil)
	case ModeAutoEdit:
		if ask != "" {
			ev.decide(Ask, ask, nil)
			return
		}
		for _, p := range ev.req.Writes {
			if ev.e.ws.Ignored(p) {
				ev.decide(Ask, "auto-edit does not cover ignored file "+ev.e.ws.Rel(p), nil)
				return
			}
		}
		ev.decide(Allow, "auto-edit mode: edits inside the workspace", nil)
	default:
		if ask != "" {
			ev.decide(Ask, ask, nil)
			return
		}
		ev.decide(Ask, "edits need approval in default mode", nil)
	}
}

// modeBash applies the shell class table, after checking that declared
// reads and writes stay inside the workspace.
func (ev *eval) modeBash() {
	sh := ev.req.Shell
	if sh == nil {
		ev.decide(Deny, "command was not analysed", nil)
		return
	}
	summary := sh.Summary()
	if sh.Class == shellclass.Privilege {
		ev.decide(Deny, "privilege/system command: "+summary, nil)
		return
	}
	// Paths outside the workspace are checked in every mode. Plan mode is
	// meant to be the most restrictive one, so a read-only command must not
	// reach a protected file there just because writes are off; bypass turns
	// prompts off, not the hard-deny floor.
	deny, ask := ev.outsideShell(sh)
	if deny != "" {
		ev.decide(Deny, deny, nil)
		return
	}
	if ev.mode == ModePlan {
		ev.modeBashPlan(sh, summary, ask)
		return
	}
	if ev.mode == ModeBypass {
		ev.decide(Allow, "bypass mode (network stays off unless --allow-network): "+summary, nil)
		return
	}
	switch {
	case sh.Unknown:
		ev.decide(Ask, "opaque shell (cannot be auto-allowed): "+summary, nil)
	case ev.req.Network:
		ev.decide(Ask, "requests network access: "+summary, nil)
	case sh.Class == shellclass.SafeRead && ask == "":
		ev.decide(Allow, "read-only command: "+summary, nil)
	case ask != "":
		ev.decide(Ask, ask+": "+summary, nil)
	default:
		ev.decide(Ask, sh.Class.String()+" command: "+summary, nil)
	}
}

func (ev *eval) modeBashPlan(sh *shellclass.Analysis, summary, ask string) {
	switch {
	case sh.Class != shellclass.SafeRead || sh.Unknown:
		ev.decide(Deny, "plan mode: only read-only commands: "+summary, nil)
	case ask != "":
		// A read-only command touching paths outside the workspace prompts,
		// exactly as the read tools do in plan mode.
		ev.decide(Ask, ask+": "+summary, nil)
	default:
		ev.decide(Allow, "read-only command: "+summary, nil)
	}
}

// outsideShell applies the outside-workspace rules to declared shell paths.
func (ev *eval) outsideShell(sh *shellclass.Analysis) (deny, ask string) {
	var reads, writes []string
	for _, c := range sh.Commands {
		reads = append(reads, c.Reads...)
		writes = append(writes, c.Writes...)
	}
	deny, ask = ev.outside(reads)
	if deny != "" {
		return deny, ""
	}
	for _, w := range writes {
		if !ev.e.ws.Inside(w) {
			return "", "writes outside the workspace " + ev.e.ws.Rel(w)
		}
	}
	return "", ask
}

func (ev *eval) modeMCP() {
	switch ev.mode {
	case ModePlan:
		if ev.req.ReadOnly {
			ev.decide(Ask, "plan mode: MCP tool annotated read-only (annotation is advisory)", nil)
		} else {
			ev.decide(Deny, "plan mode: MCP tool is not annotated read-only", nil)
		}
	case ModeBypass:
		ev.decide(Allow, "bypass mode", nil)
	default:
		ev.decide(Ask, "MCP tools need approval", nil)
	}
}

// modeFallback handles kinds with a single default decision.
func (ev *eval) modeFallback(d Decision, what string) {
	if ev.mode == ModeBypass {
		ev.decide(Allow, "bypass mode", nil)
		return
	}
	ev.decide(d, what+" needs approval", nil)
}

// modeOther covers explore/skill/todo/ask_user (allowed: their effects go
// through other tools) and unknown tools (ask).
func (ev *eval) modeOther() {
	if otherTools[ev.req.Tool] {
		ev.decide(Allow, "built-in tool with no direct side effects", nil)
		return
	}
	ev.modeFallback(Ask, "unknown tool "+ev.req.Tool)
}

// Suggest proposes grants that would allow this request in future. Nothing
// is offered for destructive or opaque requests, and bash offers never widen
// beyond two argv words.
func (e *Engine) Suggest(req Request) []GrantOffer {
	var rules []Rule
	switch kindOf(req.Tool) {
	case kindBash:
		rules = suggestBash(req)
	case kindRead, kindWrite:
		rules = e.suggestPaths(req)
	case kindWeb:
		if req.URL != nil && req.URL.Hostname() != "" {
			if r, err := ParseRule(req.Tool+"(domain:"+req.URL.Hostname()+")", SourceSession); err == nil {
				rules = append(rules, r)
			}
		}
	case kindMCP:
		if r, err := ParseRule(req.Tool, SourceSession); err == nil {
			rules = append(rules, r)
		}
	}
	offers := make([]GrantOffer, 0, len(rules)*2)
	for _, r := range rules {
		offers = append(offers,
			GrantOffer{Rule: r, Scope: ScopeSession, Label: "allow " + r.String() + " for this session"},
			GrantOffer{Rule: r, Scope: ScopeProjectLocal, Label: "allow " + r.String() + " for this project (.wright/settings.local.json)"},
		)
	}
	return offers
}

func suggestBash(req Request) []Rule {
	sh := req.Shell
	if sh == nil || sh.Unknown || sh.HardDeny != "" || sh.Class >= shellclass.Destructive {
		return nil
	}
	var rules []Rule
	seen := map[string]bool{}
	for _, c := range sh.Commands {
		if len(c.Argv) == 0 || strings.HasPrefix(c.Argv[0], "(") {
			continue
		}
		words := c.Argv[:min(2, len(c.Argv))]
		text := "bash(" + strings.Join(words, " ") + " *)"
		if c.Network || sh.NeedsNetwork {
			text += " +net"
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		if r, err := ParseRule(text, SourceSession); err == nil {
			rules = append(rules, r)
		}
	}
	return rules
}

func (e *Engine) suggestPaths(req Request) []Rule {
	paths := append(slices.Clone(req.Paths), req.Writes...)
	var rules []Rule
	seen := map[string]bool{}
	for _, p := range paths {
		if e.ws.IsProtected(p) || e.ws.IsSecretFile(p) {
			continue
		}
		dir := filepath.Dir(p)
		var pat string
		switch {
		case e.ws.Inside(dir):
			rel := e.ws.Rel(dir)
			if rel == "." {
				pat = "$WORKSPACE/**"
			} else {
				pat = "$WORKSPACE/" + filepath.ToSlash(rel) + "/**"
			}
		case strings.HasPrefix(dir, e.ws.Home+string(filepath.Separator)):
			pat = "~/" + filepath.ToSlash(dir[len(e.ws.Home)+1:]) + "/**"
		default:
			pat = filepath.ToSlash(dir) + "/**"
		}
		text := req.Tool + "(" + pat + ")"
		if seen[text] {
			continue
		}
		seen[text] = true
		if r, err := ParseRule(text, SourceSession); err == nil {
			rules = append(rules, r)
		}
	}
	return rules
}
