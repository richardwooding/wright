package app

import (
	"errors"
	"fmt"
	"os"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/sandbox"
)

// Bypass refusals. They are errors, not warnings: a bypass that silently
// downgrades would be the "false confidence" the sandbox exists to prevent.
var (
	ErrBypassAsRoot      = errors.New("app: --bypass-permissions is refused as root outside a container")
	ErrBypassUnsandboxed = errors.New("app: --bypass-permissions needs an OS sandbox; pass --allow-unsandboxed-bypass to accept unconfined commands")
	ErrBypassNeedsFlag   = errors.New("app: mode \"bypass\" can only be enabled with --bypass-permissions")
)

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
	l := b.layered
	b.pol.SetPersist(func(r policy.Rule) error {
		return l.SaveProjectLocal(func(s *config.Settings) { appendRule(&s.Permissions, r) })
	})
	return nil
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

// ruleLayers parses builtin < user < project < project.local < flags. The
// project layer contributes allow rules only when trusted.
func (b *builder) ruleLayers() ([][]policy.Rule, error) {
	project := b.layered.Project.Permissions
	if !b.trusted {
		project.Allow = nil
	}
	steps := []struct {
		perms config.Permissions
		src   policy.Source
	}{
		{b.user.Permissions, policy.SourceUser},
		{project, policy.SourceProject},
		{b.layered.ProjectLocal.Permissions, policy.SourceProjectLocal},
		{config.Permissions{Allow: b.o.Allow, Deny: b.o.Deny}, policy.SourceFlag},
	}
	layers := [][]policy.Rule{policy.Builtin()}
	for _, st := range steps {
		rules, err := parsePermissions(st.perms, st.src)
		if err != nil {
			return nil, err
		}
		layers = append(layers, rules)
	}
	return layers, nil
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

// appendRule records a persisted grant in the matching list.
func appendRule(p *config.Permissions, r policy.Rule) {
	switch r.Decision {
	case policy.Allow:
		p.Allow = append(p.Allow, r.String())
	case policy.Ask:
		p.Ask = append(p.Ask, r.String())
	case policy.Deny:
		p.Deny = append(p.Deny, r.String())
	}
}
