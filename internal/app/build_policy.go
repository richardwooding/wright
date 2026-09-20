package app

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/trust"
)

// Bypass refusals. They are errors, not warnings: a bypass that silently
// downgrades would be the "false confidence" the sandbox exists to prevent.
var (
	ErrBypassAsRoot      = errors.New("app: --bypass-permissions is refused as root outside a container")
	ErrBypassUnsandboxed = errors.New("app: --bypass-permissions needs an OS sandbox; pass --allow-unsandboxed-bypass to accept unconfined commands")
	ErrBypassNeedsFlag   = errors.New("app: mode \"bypass\" can only be enabled with --bypass-permissions")
	// ErrWorkspaceNotTrusted ends a run the user declined at the startup
	// trust prompt. It is never returned for a headless run, which is never
	// asked in the first place.
	ErrWorkspaceNotTrusted = errors.New("app: this directory was not trusted")
)

// ExitTrustDeclined is the process exit code for a session the user ended by
// declining to trust the workspace. It has its own code because "you said
// no" is not a failure, and a wrapper script needs to tell it apart from
// one. cli re-exports it beside the other exit codes.
const ExitTrustDeclined = 5

// permissions resolves the mode and builds the policy engine from the rule
// layers, each tagged with its source so approval prompts and the audit log
// can say which file decided.
func (b *builder) permissions() error {
	mode, err := b.resolveMode()
	if err != nil {
		return err
	}
	b.mode = mode
	layers, err := b.ruleLayers()
	if err != nil {
		return err
	}
	b.pol = policy.New(b.ws, mode, layers...)
	// The workspace baseline is not a rule layer: it is applied inside the
	// policy mode table, after every floor, so that an ordinary edit inside
	// an accepted directory stops prompting while a sensitive file, an
	// ignored file, a path outside and the hard-deny set still do not.
	if b.workspaceTrusted {
		b.pol.TrustWorkspace()
	}
	l := b.layered
	b.pol.SetPersist(func(r policy.Rule) error {
		if err := l.SaveProjectLocal(func(s *config.Settings) { appendRule(&s.Permissions, r) }); err != nil {
			return err
		}
		return b.retrustLocal()
	})
	// The user's own config needs no re-trust: it is not part of any
	// project's hashed unit, and it was never gated in the first place.
	b.pol.SetPersistUser(func(r policy.Rule) error {
		return l.SaveUser(func(s *config.Settings) { appendRule(&s.Permissions, r) })
	})
	return nil
}

// retrustLocal re-records the project's trust hash after wright itself wrote
// settings.local.json. The file is part of the hashed unit, so a persisted
// grant would otherwise make the project untrusted next session — but only a
// project that was already trusted is re-accepted, so writing the file can
// never promote an untrusted project.
func (b *builder) retrustLocal() error {
	if !b.trusted {
		return nil
	}
	hash, err := ProjectHash(b.layered.Paths)
	if err != nil || hash == "" {
		return err
	}
	return trust.Open(b.layered.Paths.TrustFile()).AcceptProject(b.ws.Root(), hash)
}

// resolveMode applies flag > WRIGHT_MODE/settings > default, then the bypass
// rules: only the flag enables bypass, never as root outside a container,
// and never without a sandbox unless the user accepted that explicitly.
func (b *builder) resolveMode() (policy.Mode, error) {
	name := b.o.Mode
	if name == "" {
		name = b.settings.Permissions.Mode
	}
	mode := policy.ModeDefault
	if name != "" {
		m, err := policy.ParseMode(name)
		if err != nil {
			return 0, err
		}
		mode = m
	}
	if mode == policy.ModeBypass && !b.o.Bypass {
		return 0, ErrBypassNeedsFlag
	}
	if !b.o.Bypass {
		return mode, nil
	}
	inContainer := sandbox.InContainer()
	switch {
	case os.Geteuid() == 0 && !inContainer:
		return 0, ErrBypassAsRoot
	case b.backend.Name() == sandbox.NameNone && !b.o.AllowUnsandboxedBypass && !inContainer:
		return 0, ErrBypassUnsandboxed
	}
	b.bypass = true
	b.warn("permission bypass is ON for this process: nothing is asked; the hard-deny set still applies")
	return policy.ModeBypass, nil
}

// ruleLayers parses builtin < user < project < project.local < flags. Both
// project layers contribute allow rules only when trusted: settings.local.json
// is a file in the repository too, so it is gated with the shared one.
func (b *builder) ruleLayers() ([][]policy.Rule, error) {
	project, local := b.layered.Project.Permissions, b.layered.ProjectLocal.Permissions
	if !b.trusted {
		project.Allow, local.Allow = nil, nil
	}
	steps := []struct {
		perms config.Permissions
		src   policy.Source
	}{
		{b.user.Permissions, policy.SourceUser},
		{project, policy.SourceProject},
		{local, policy.SourceProjectLocal},
		{config.Permissions{Allow: b.o.Allow, Deny: b.o.Deny}, policy.SourceFlag},
	}
	layers := [][]policy.Rule{policy.Builtin(), subAgentRules(b.agentDefs)}
	for _, st := range steps {
		rules, err := parsePermissions(st.perms, st.src)
		if err != nil {
			return nil, err
		}
		layers = append(layers, rules)
	}
	return layers, nil
}

// subAgentRules allows the sub-agent tools themselves. Delegating is not an
// effect: the child's every call is evaluated again, one level deeper, by a
// child policy engine that cannot inherit bypass or grant anything. An
// explicit ask or deny rule for an agent's name still outranks this, because
// tightening always wins.
func subAgentRules(defs []agents.Definition) []policy.Rule {
	names := agents.Names(defs)
	rules := make([]policy.Rule, 0, len(names))
	for _, n := range names {
		r, err := policy.ParseRule(n, policy.SourceBuiltin)
		if err != nil {
			continue // a name that cannot be a rule simply gets asked about
		}
		r.Decision = policy.Allow
		rules = append(rules, r)
	}
	return rules
}

func parsePermissions(p config.Permissions, src policy.Source) ([]policy.Rule, error) {
	var out []policy.Rule
	for _, l := range []struct {
		texts []string
		d     policy.Decision
	}{{p.Deny, policy.Deny}, {p.Ask, policy.Ask}, {p.Allow, policy.Allow}} {
		rules, err := policy.ParseRules(l.texts, l.d, src)
		if err != nil {
			return nil, fmt.Errorf("%s permission rule: %w", src, err)
		}
		out = append(out, rules...)
	}
	return out, nil
}

// appendRule records a persisted grant in the matching list, once. Accepting
// the same offer again is an ordinary thing to do — the rule may not have
// matched the next call for some other reason — and appending unconditionally
// put three copies of `bash(fpc *)` in one user's settings.local.json.
// config.Merge dedupes *across* layers; this is the within-one-file case.
func appendRule(p *config.Permissions, r policy.Rule) {
	text := r.String()
	// Every write rewrites the whole file, so this is also the moment to
	// clear up duplicates that are already in it — one real settings file
	// accumulated four copies of the same rule before addOnce existed, and
	// nothing would ever have removed them.
	p.Allow, p.Ask, p.Deny = dedupe(p.Allow), dedupe(p.Ask), dedupe(p.Deny)
	switch r.Decision {
	case policy.Allow:
		p.Allow = addOnce(p.Allow, text)
	case policy.Ask:
		p.Ask = addOnce(p.Ask, text)
	case policy.Deny:
		p.Deny = addOnce(p.Deny, text)
	}
}

// dedupe keeps the first occurrence of each rule, preserving order: the list
// is read by people, and reordering it would make a diff say more than
// happened.
func dedupe(list []string) []string {
	if len(list) < 2 {
		return list
	}
	seen := make(map[string]bool, len(list))
	out := list[:0:0]
	for _, s := range list {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// addOnce appends text unless the list already has it.
func addOnce(list []string, text string) []string {
	if slices.Contains(list, text) {
		return list
	}
	return append(list, text)
}
