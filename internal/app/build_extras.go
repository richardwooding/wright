package app

import (
	"context"

	"github.com/richardwooding/agentkit"
	akskills "github.com/richardwooding/agentkit/skills"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/skillsdir"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/trust"
)

// loadSkills discovers Agent Skills for this workspace. Skills are
// instructions and files, not permissions: whatever a skill tells the model
// to run still goes through bash, the permission engine and the sandbox.
func (b *builder) loadSkills() *akskills.Set {
	set, problems, err := skillsdir.Load(b.ws, b.layered.Paths.UserConfig, b.settings.Skills)
	if err != nil {
		b.warn("skills: %v", err)
		return nil
	}
	for _, p := range problems {
		b.warn("skill %s: %v", b.ws.Rel(p.Path), p.Err)
	}
	b.skills = set
	return set
}

// engineExtras are the agentkit options the app adds to every run. The skill
// catalog goes into the system prompt and the skill tools are registered
// there, so the engine never has to know what a skill is.
func (b *builder) engineExtras(set *akskills.Set) []agentkit.Option {
	if set.Len() == 0 {
		return nil
	}
	return []agentkit.Option{akskills.Use(set)}
}

// connectMCP connects the configured MCP servers. Only a trusted settings
// layer can contribute servers in the first place (effectiveSettings drops
// mcpServers from an untrusted project), and each server still needs its own
// acceptance before its tools are registered.
func (b *builder) connectMCP() {
	set, err := mcpclient.Connect(b.ctx, b.settings.MCPServers, mcpclient.Deps{
		Trust:      b.trustStore(),
		Consent:    b.mcpConsent(),
		Env:        b.spec.Env,
		Sandbox:    b.backend,
		Spec:       b.spec,
		Workspace:  b.ws,
		HTTPClient: fetchClient(),
		Getenv:     b.env,
	})
	if err != nil {
		b.warn("mcp: %v", err)
		set = &mcpclient.Set{}
	}
	b.mcp = set
	for _, s := range set.Servers {
		if s.Err != nil {
			b.warn("mcp server %q not connected: %v", s.Name, s.Err)
			continue
		}
		if !s.Trusted {
			b.warn("mcp server %q connected for this session: %d tool(s)", s.Name, s.ToolCount)
		}
	}
}

// trustStore is where accepted projects and MCP servers are remembered. It
// lives in the user's config directory, never in the project, so a
// repository cannot vouch for itself.
func (b *builder) trustStore() *trust.Store {
	return trust.Open(b.layered.Paths.TrustFile())
}

// mcpConsent is how a new or changed server is accepted. Headless runs never
// accept anything new (nil denies). Interactive runs do not have a consent
// form yet, so they decline as well, but they say exactly what was proposed
// and how to accept it — silently skipping a configured server would be the
// worse failure.
func (b *builder) mcpConsent() mcpclient.ConsentFunc {
	if b.headless() {
		return nil
	}
	return func(_ context.Context, p mcpclient.Proposal) (mcpclient.Choice, error) {
		b.warn("%s\n  not connected: the interactive consent prompt is not built yet — add \"trusted\": true to this server in .wright/settings.json (or your user config) to accept it", p.String())
		return mcpclient.Deny, nil
	}
}

// subAgents builds the explore agent and the custom ones over the full
// toolset (wright's tools plus any MCP tools). Children run on the fast
// model and through the engine's own middleware chain, resolved late because
// the tools exist before the engine does.
func (b *builder) subAgents(full agentkit.Toolset, late *lateEngine, fast core.Chatter) agentkit.Toolset {
	client, err := b.childClient(fast)
	if err != nil {
		b.warn("sub-agents unavailable: %v", err)
		return nil
	}
	deps := agents.Deps{
		Client:     client,
		ClientFor:  openClient,
		Tools:      full,
		Middleware: []agentkit.Middleware{late.middleware()},
	}
	explore, err := agents.Explore(deps)
	if err != nil {
		b.warn("explore sub-agent unavailable: %v", err)
		return nil
	}
	out := agentkit.Toolset{explore}
	custom, problems := agents.Build(b.agentDefs, deps)
	for _, p := range problems {
		b.warn("agent %s: %v", b.ws.Rel(p.Path), p.Err)
	}
	return append(out, custom...)
}

// childClient is the model sub-agents run on: the fast model when the
// session has one, otherwise the session's own model.
func (b *builder) childClient(fast core.Chatter) (core.Chatter, error) {
	if fast != nil {
		return fast, nil
	}
	return openClient(b.choice.Model)
}

// openClient opens a client for a custom agent's own model.
func openClient(name string) (core.Chatter, error) {
	a, err := agentkit.New(name)
	if err != nil {
		return nil, err
	}
	return a.Client(), nil
}

// readOnlyTools is the set plan mode keeps: wright's read-only tools, the
// skill tools (which only produce text), the MCP tools their server
// annotates read-only, and the read-only sub-agents.
func (b *builder) readOnlyTools() []string {
	out := tools.ReadOnlyNames()
	if b.skills.Len() > 0 {
		out = append(out, akskills.ToolName, akskills.FileToolName)
	}
	out = append(out, b.mcp.ReadOnlyNames()...)
	return append(out, agents.ReadOnlyNames(b.agentDefs)...)
}

// toolDocs is the "when to use" guidance in the system prompt: the built-in
// tools plus one line per sub-agent. MCP tools and skills describe
// themselves (the server's descriptions and the skill catalog).
func (b *builder) toolDocs() []prompt.ToolDoc {
	docs := tools.Docs()
	out := make([]prompt.ToolDoc, 0, len(docs)+len(b.agentDefs)+1)
	for _, d := range docs {
		out = append(out, prompt.ToolDoc{Name: d.Name, When: d.When})
	}
	for _, d := range agents.Docs(b.agentDefs) {
		out = append(out, prompt.ToolDoc{Name: d.Name, When: d.When})
	}
	return out
}
