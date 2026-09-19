// Package agents builds the sub-agents the model can delegate to: the
// built-in read-only `explore` agent and any custom agents a project or the
// user defines as Markdown files with frontmatter.
//
// A sub-agent is an agentkit agent exposed as a tool, so the main agent calls
// it like any other tool and the child's events are forwarded with Depth 1
// for the UI to indent. Children are never a way around the permission
// engine: the app hands every sub-agent the same middleware chain as the main
// agent, and the engine evaluates a call at depth > 0 against a child policy
// engine, which clamps bypass back to the default mode and cannot grant.
// Read-only agents get the read-only toolset on top of that.
package agents

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/tools"
)

// ExploreName is the tool name of the built-in exploration agent.
const ExploreName = "explore"

// ExploreDoc is the system-prompt guidance for the explore tool.
const ExploreDoc = "Delegate a read-only research question about the codebase to a sub-agent (searching, reading, tracing a call path) and get a short report back. Use it to keep a long search out of the main transcript; it cannot change anything."

// exploreDescription is the tool description the model sees.
const exploreDescription = "Research the codebase read-only and report findings"

// Child budgets. A sub-agent is a bounded errand, not a second session: it
// stops well before the main agent's budget so a runaway child cannot spend
// the whole run.
const (
	childMaxSteps = 25
	childTimeout  = 10 * time.Minute
)

// childParallel is how many tools a child may call at once.
const childParallel = 4

// Deps is what building a sub-agent needs. The app fills it in; nothing here
// reaches the engine directly.
type Deps struct {
	// Client is the model client children use — the fast model, since a
	// sub-agent reads and summarises rather than designs.
	Client core.Chatter
	// ClientFor opens a client for a custom agent's own model. nil (or an
	// error) falls back to Client.
	ClientFor func(model string) (core.Chatter, error)
	// Tools is the full toolset; each agent gets a subset of it.
	Tools agentkit.Toolset
	// Middleware is the chain every child tool call runs through: the same
	// approval, audit and untrusted-content handling as the main agent.
	Middleware []agentkit.Middleware
	// MaxSteps and Timeout override the child budget when non-zero.
	MaxSteps int
	Timeout  time.Duration
}

// Explore builds the read-only exploration sub-agent as a tool.
func Explore(deps Deps) (agentkit.Tool, error) {
	return build(Definition{
		Name:         ExploreName,
		Description:  exploreDescription,
		Instructions: prompt.Explore(),
		ReadOnly:     true,
	}, deps)
}

// Build turns custom definitions into tools, one per definition. A
// definition that cannot be built becomes a Problem and costs only itself.
func Build(defs []Definition, deps Deps) (agentkit.Toolset, []Problem) {
	var (
		out      agentkit.Toolset
		problems []Problem
	)
	for _, def := range defs {
		t, err := build(def, deps)
		if err != nil {
			problems = append(problems, Problem{Path: def.Source, Err: err})
			continue
		}
		out = append(out, t)
	}
	return out, problems
}

// build makes one sub-agent tool.
func build(def Definition, deps Deps) (agentkit.Tool, error) {
	if def.Name == "" {
		return nil, errors.New("agents: agent needs a name")
	}
	client, err := clientFor(def, deps)
	if err != nil {
		return nil, err
	}
	budget := agentkit.Budget{MaxSteps: childMaxSteps, Timeout: childTimeout}
	if deps.MaxSteps > 0 {
		budget.MaxSteps = deps.MaxSteps
	}
	if deps.Timeout > 0 {
		budget.Timeout = deps.Timeout
	}
	child, err := agentkit.NewFromClient(client,
		agentkit.WithName(def.Name),
		agentkit.WithInstructions(def.Instructions),
		agentkit.WithTools(Toolset(def, deps.Tools)...),
		agentkit.WithMiddleware(deps.Middleware...),
		agentkit.WithParallel(childParallel),
		agentkit.WithBudget(budget),
	)
	if err != nil {
		return nil, fmt.Errorf("agents: %s: %w", def.Name, err)
	}
	return agentkit.AsTool(child, def.Name, def.Description, agentkit.WithForwardEvents()), nil
}

func clientFor(def Definition, deps Deps) (core.Chatter, error) {
	if def.Model != "" && deps.ClientFor != nil {
		c, err := deps.ClientFor(def.Model)
		if err != nil {
			return nil, fmt.Errorf("agents: %s: model %s: %w", def.Name, def.Model, err)
		}
		return c, nil
	}
	if deps.Client == nil {
		return nil, fmt.Errorf("agents: %s: no model client", def.Name)
	}
	return deps.Client, nil
}

// Toolset is the subset of ts a definition may use: exactly what `tools:`
// names when it names anything, the read-only tools for a read-only agent,
// and otherwise everything except bash — a custom agent that was not given a
// shell explicitly does not get one.
func Toolset(def Definition, ts agentkit.Toolset) agentkit.Toolset {
	if len(def.Tools) > 0 {
		out := make(agentkit.Toolset, 0, len(def.Tools))
		for _, t := range ts {
			if slices.Contains(def.Tools, t.Definition().Name) {
				out = append(out, t)
			}
		}
		return out
	}
	if def.ReadOnly {
		return tools.ReadOnly(ts)
	}
	out := make(agentkit.Toolset, 0, len(ts))
	for _, t := range ts {
		if t.Definition().Name != tools.NameBash {
			out = append(out, t)
		}
	}
	return out
}

// Names lists every sub-agent tool: explore plus the custom ones. The app
// turns these into allow rules, because *calling* a sub-agent has no effect
// of its own — everything the child then does is evaluated again, one level
// deeper, against a child policy engine.
func Names(defs []Definition) []string {
	out := []string{ExploreName}
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// ReadOnlyNames lists the agents plan mode keeps: explore and every custom
// agent that declares itself read-only.
func ReadOnlyNames(defs []Definition) []string {
	out := []string{ExploreName}
	for _, d := range defs {
		if d.ReadOnly {
			out = append(out, d.Name)
		}
	}
	return out
}

// Doc is the system-prompt guidance for one agent.
type Doc struct {
	Name string
	When string
}

// Docs returns the guidance lines for the explore agent and the custom ones,
// so the system prompt lists sub-agents beside the ordinary tools.
func Docs(defs []Definition) []Doc {
	out := []Doc{{Name: ExploreName, When: ExploreDoc}}
	for _, d := range defs {
		when := d.Description
		if d.ReadOnly {
			when += " (read-only sub-agent)"
		} else {
			when += " (sub-agent)"
		}
		out = append(out, Doc{Name: d.Name, When: when})
	}
	return out
}
