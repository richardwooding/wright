package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/skillsdir"
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
		return b.mcpStatus(), nil
	case "skills":
		return b.skillsStatus(), nil
	case "agents":
		return b.agentsStatus(), nil
	default:
		return "", fmt.Errorf("unknown command /%s", name)
	}
}

// mcpStatus is /mcp: what this session connected, and what it did not.
func (b *Built) mcpStatus() string {
	if b.MCP == nil || len(b.MCP.Servers) == 0 {
		return "no MCP servers configured (add one with `wright mcp add`; they take effect next session)"
	}
	var lines []string
	for _, s := range b.MCP.Servers {
		switch {
		case s.Err != nil:
			lines = append(lines, fmt.Sprintf("✗ %s (%s): %v", s.Name, s.Transport, s.Err))
		case s.Trusted:
			lines = append(lines, fmt.Sprintf("✓ %s (%s): %d tool(s), accepted", s.Name, s.Transport, s.ToolCount))
		default:
			lines = append(lines, fmt.Sprintf("✓ %s (%s): %d tool(s), this session only", s.Name, s.Transport, s.ToolCount))
		}
	}
	return strings.Join(lines, "\n")
}

// skillsStatus is /skills: the catalog the model was given.
func (b *Built) skillsStatus() string {
	rows := skillsdir.Describe(b.Skills)
	if len(rows) == 0 {
		return "no skills found (looked in " + strings.Join(b.skillDirs(), ", ") + ")"
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s — %s\n  %s", r.Name, firstLine(r.Description), b.WS.Rel(r.Source)))
	}
	return strings.Join(lines, "\n")
}

// skillDirs is the search path, relative to the workspace where it helps.
func (b *Built) skillDirs() []string {
	dirs := skillsdir.Dirs(b.WS, b.Layered.Paths.UserConfig, b.Settings.Skills)
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, b.WS.Rel(d))
	}
	return out
}

// agentsStatus is /agents: the sub-agents the model can delegate to.
func (b *Built) agentsStatus() string {
	lines := []string{fmt.Sprintf("%s — %s (built in, read-only)", agents.ExploreName, "research the codebase and report back")}
	for _, d := range b.Agents {
		kind := "read-only"
		if !d.ReadOnly {
			kind = "read-write"
		}
		lines = append(lines, fmt.Sprintf("%s — %s (%s)\n  %s", d.Name, firstLine(d.Description), kind, b.WS.Rel(d.Source)))
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
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

// trustProject is /trust: it accepts the project's settings files and
// reports the workspace's own, separate trust state. The two are different
// questions — the settings are bytes the user reads, the workspace is a
// directory the user works in — so this command answers only the one it
// showed, and names the command that answers the other.
func (b *Built) trustProject() (string, error) {
	out, err := b.trustSettings()
	if err != nil {
		return "", err
	}
	return out + "\n\n" + b.workspaceTrustStatus(), nil
}

// workspaceTrustStatus says whether edits in this directory still prompt.
func (b *Built) workspaceTrustStatus() string {
	if b.WorkspaceTrusted {
		return b.WS.Root() + " is a trusted workspace: edits to files inside it do not ask (shell commands still do)."
	}
	return b.WS.Root() + " is not a trusted workspace: every edit asks." +
		" Run `wright trust accept` or start wright with --trust; it takes effect next session."
}

// trustSettings accepts the current project settings files. The layers were
// built before the acceptance, so the effect starts with the next session.
func (b *Built) trustSettings() (string, error) {
	path := b.Layered.Paths.ProjectSettingsFile()
	// Both project layers are trusted as a unit, so the hash covers
	// settings.local.json too.
	hash, err := ProjectHash(b.Layered.Paths)
	if err != nil {
		return "", err
	}
	if hash == "" {
		return "no project settings file (" + b.WS.Rel(path) + ") to trust", nil
	}
	if b.Trusted {
		return b.WS.Rel(b.Layered.Paths.ProjectDir) + " is already trusted", nil
	}
	store := trust.Open(b.Layered.Paths.TrustFile())
	if err := store.AcceptProject(b.WS.Root(), hash); err != nil {
		return "", err
	}
	return TrustPrompt(b.WS.Rel(b.Layered.Paths.ProjectDir), b.Layered.Project, b.Layered.ProjectLocal) +
		"\naccepted: its settings apply in full from the next session", nil
}
