package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/trust"
)

// Command runs the slash commands that need app-level services rather than
// the engine: /diff /audit /init /redaction /trust /mcp /skills. The name is
// given without the slash. Unknown names are an error so the UI can say so.
func (b *Built) Command(ctx context.Context, name string, args []string) (string, error) {
	switch strings.TrimPrefix(strings.ToLower(name), "/") {
	case "diff":
		staged := len(args) > 0 && (args[0] == "--staged" || args[0] == "--cached")
		out, err := git.Diff(ctx, b.WS.Root(), staged)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(out) == "" {
			return "no changes", nil
		}
		return out, nil
	case "audit":
		return b.auditSummary()
	case "init":
		return b.initProject()
	case "redaction":
		return b.redaction(args)
	case "trust":
		return b.trustProject()
	case "mcp":
		return "MCP servers are not configured yet (Phase 3).", nil
	case "skills":
		return "Skills are not configured yet (Phase 3).", nil
	default:
		return "", fmt.Errorf("unknown command /%s", name)
	}
}

func (b *Built) auditSummary() (string, error) {
	path := b.Store.AuditPath(b.Engine.SessionID())
	s, err := audit.Summarize(audit.Read(path))
	if err != nil {
		return "", err
	}
	return s.String() + "\nlog: " + path, nil
}

func (b *Built) initProject() (string, error) {
	written, err := InitProject(b.WS.Root())
	if err != nil {
		return "", err
	}
	if len(written) == 0 {
		return "nothing to do: AGENTS.md and .wright/settings.json already exist", nil
	}
	return "created " + strings.Join(written, ", ") + " (restart to load the new instructions)", nil
}

// redaction reports the switch; flipping it would need the toolset rebuilt,
// which the engine cannot do mid-session, so the answer names the setting.
func (b *Built) redaction(args []string) (string, error) {
	state := "on"
	if !b.Settings.RedactionEnabled() {
		state = "off"
	}
	if len(args) == 0 {
		return "secret redaction is " + state, nil
	}
	return fmt.Sprintf("secret redaction is %s; changing it at runtime is not supported yet — set \"redaction\": %v in .wright/settings.local.json and restart", state, args[0] == "on"), nil
}

// trustProject accepts the current project settings file. The layers were
// built before the acceptance, so the effect starts with the next session.
func (b *Built) trustProject() (string, error) {
	path := b.Layered.Paths.ProjectSettingsFile()
	hash, err := trust.HashFile(path)
	if err != nil {
		return "", err
	}
	if hash == "" {
		return "no project settings file (" + b.WS.Rel(path) + ") to trust", nil
	}
	if b.Trusted {
		return b.WS.Rel(path) + " is already trusted", nil
	}
	store := trust.Open(b.Layered.Paths.TrustFile())
	if err := store.AcceptProject(b.WS.Root(), hash); err != nil {
		return "", err
	}
	return TrustPrompt(b.WS.Rel(path), b.Layered.Project) + "\naccepted: its allow rules, extra directories, env passthrough and MCP servers apply from the next session", nil
}
