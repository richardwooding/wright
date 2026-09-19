package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"
	"github.com/richardwooding/ssrfguard"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

// fetchTimeout bounds one web_fetch request end to end.
const fetchTimeout = 30 * time.Second

// maxRedirects caps redirect chains in web_fetch.
const maxRedirects = 10

// toolsAndEngine builds the toolset, the adapters that let tools talk to the
// engine without importing it, loads the project instructions and opens the
// engine (which opens the model client, so a bad model fails here).
func (b *builder) toolsAndEngine() error {
	late := &lateEngine{}
	todos := &tools.TodoList{}
	deps := tools.Deps{
		WS:          b.ws,
		Sandbox:     b.backend,
		SandboxSpec: b.spec,
		Snap:        b.snaps,
		Redactor:    b.redactor,
		Fetch:       fetchClient(),
		SpillDir:    b.spillDir(),
		Cwd:         tools.NewCwd(b.cwd),
		Todos:       todos,
		OnRedacted:  late.redacted,
		Attribution: b.settings.AttributionEnabled(),
		Trailer:     b.settings.Git.Trailer,
		Version:     b.o.Version,
	}
	if !b.o.Print {
		deps.Asker = late // ask_user only makes sense with a human on the other end
	}
	base := tools.New(deps)
	instructions, err := b.loadInstructions()
	if err != nil {
		return err
	}
	settings := b.settings
	if b.o.Reasoning != "" {
		settings.Model.Reasoning = b.o.Reasoning
	}
	cwd := b.cwd
	eng, err := engine.New(b.ctx, engine.Options{
		Model:         b.choice,
		FastClient:    b.fastClient(),
		Settings:      settings,
		WS:            b.ws,
		Cwd:           cwd,
		Mode:          b.mode,
		Policy:        b.pol,
		Tools:         wrapTodos(base, todos, late),
		ReadOnlyTools: tools.ReadOnlyNames(),
		Describe:      describeFunc(base),
		Instructions:  instructions,
		ToolDocs:      toolDocs(),
		Store:         b.store,
		SessionID:     b.sessionID,
		Audit:         b.auditLog,
		Snapshots:     b.snaps,
		Sandbox:       b.backend.Name(),
		SandboxNet:    b.spec.Network,
		Warnings:      b.warnings,
		Headless:      b.o.Print,
		Git:           func(ctx context.Context) git.Summary { return git.Status(ctx, cwd) },
		ContextWindow: settings.Model.ContextWindow,
		MaxSteps:      b.o.MaxSteps,
		Bypass:        b.bypass,
		Version:       b.o.Version,
		Shell:         "bash",
	})
	if err != nil {
		return err
	}
	late.set(eng)
	b.eng = eng
	return nil
}

// fastClient opens the provider's cheap model for summaries and titles when
// it differs from the main choice. A failure is not fatal: the engine falls
// back to the main client.
func (b *builder) fastClient() core.Chatter {
	name := model.Fast(b.choice)
	if name == "" || name == b.choice.Model {
		return nil
	}
	a, err := agentkit.New(name)
	if err != nil {
		b.warn("fast model %s unavailable (%v); using %s for summaries", name, err, b.choice.Model)
		return nil
	}
	return a.Client()
}

// loadInstructions gathers AGENTS.md files. There is no way to ask about a
// CLAUDE.md fallback before the UI exists, so "ask" behaves as "no" with a
// warning that names the fix.
func (b *builder) loadInstructions() ([]prompt.Instruction, error) {
	ins, err := prompt.LoadInstructions(b.ws, b.cwd, b.settings.Instructions, b.layered.Paths.UserConfig, nil)
	if err != nil {
		return nil, err
	}
	root := b.ws.Root()
	_, agentsErr := os.Stat(filepath.Join(root, prompt.AgentsFile))
	_, claudeErr := os.Stat(filepath.Join(root, prompt.ClaudeFile))
	if fb := b.settings.Instructions.Fallback; agentsErr != nil && claudeErr == nil && (fb == "" || fb == "ask") {
		b.warn("%s found but no %s: it is not loaded; run /init (or `wright init`) to create %s, or set instructions.fallback to always/never", prompt.ClaudeFile, prompt.AgentsFile, prompt.AgentsFile)
	}
	return ins, nil
}

// spillDir is where truncated tool outputs are kept in full.
func (b *builder) spillDir() string {
	dir := filepath.Join(b.layered.Paths.UserCache, "spill", b.sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.warn("spill directory unavailable (%v); long outputs are truncated without a copy", err)
		return ""
	}
	return dir
}

func (b *builder) cleanup() {
	if b.eng == nil && b.auditLog != nil {
		_ = b.auditLog.Close()
	}
}

func (b *builder) built() *Built {
	return &Built{
		Engine: b.eng, Store: b.store, WS: b.ws, Layered: b.layered, Settings: b.settings,
		Warnings: b.warnings, Sandbox: b.backend, Choice: b.choice, SessionID: b.sessionID,
		Trusted: b.trusted, Close: b.eng.Close, opts: b.o,
	}
}

// lateEngine lets tool dependencies reference the engine before it exists:
// the tools are built first (the engine needs them), so the hooks resolve
// the engine at call time.
type lateEngine struct {
	mu sync.Mutex
	e  *engine.Engine
}

func (l *lateEngine) set(e *engine.Engine) {
	l.mu.Lock()
	l.e = e
	l.mu.Unlock()
}

func (l *lateEngine) get() *engine.Engine {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.e
}

// Ask implements tools.Asker.
func (l *lateEngine) Ask(ctx context.Context, q tools.Question) (tools.Answer, error) {
	e := l.get()
	if e == nil {
		return tools.Answer{}, errors.New("engine not ready")
	}
	ev := engine.QuestionEvent{Text: q.Text, Options: q.Options, FreeText: q.FreeText}
	if c, ok := agentkit.CallFrom(ctx); ok {
		call := c.Call
		ev.ToolCall = &call
	}
	a, err := e.Ask(ctx, ev)
	return tools.Answer{Text: a.Text, Index: a.Index}, err
}

func (l *lateEngine) redacted(_ context.Context, tool string, hits []redact.Hit) {
	e := l.get()
	if e == nil {
		return
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Name)
	}
	e.Redacted(tool, names)
}

func (l *lateEngine) setTodos(items []tools.Todo) {
	e := l.get()
	if e == nil {
		return
	}
	out := make([]engine.Todo, 0, len(items))
	for _, t := range items {
		out = append(out, engine.Todo{ID: t.ID, Content: t.Content, Status: t.Status})
	}
	e.SetTodos(out)
}

// wrapTodos replaces todo_write with a wrapper that pushes the new list to
// the engine after a successful call; the tools package has no callback of
// its own. Describe still resolves against the unwrapped toolset.
func wrapTodos(ts agentkit.Toolset, list *tools.TodoList, late *lateEngine) agentkit.Toolset {
	out := make(agentkit.Toolset, 0, len(ts))
	for _, t := range ts {
		def := t.Definition()
		if def.Name != tools.NameTodoWrite {
			out = append(out, t)
			continue
		}
		inner := t
		out = append(out, agentkit.Raw(def.Name, def.Description, def.Parameters, func(ctx context.Context, args json.RawMessage) (agentkit.Output, error) {
			res, err := inner.Call(ctx, args)
			if err == nil && !res.IsError {
				late.setTodos(list.Snapshot())
			}
			return res, err
		}))
	}
	return out
}

// describeFunc adapts the tools' Describers to the engine. MCP tools
// (mcp_<server>_<tool>) are addressed as mcp:<server>:<tool> so the builtin
// "mcp:*" ask rule and per-server rules match them.
func describeFunc(base agentkit.Toolset) engine.DescribeFunc {
	return func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
		if server, tool, ok := mcpName(name); ok {
			return policy.Request{Tool: "mcp:" + server + ":" + tool, Args: args}, engine.Preview{Title: name, Body: string(args)}, true, nil
		}
		d, ok := tools.Lookup(base, name)
		if !ok {
			return policy.Request{}, engine.Preview{}, false, nil
		}
		req, pv, err := d.Describe(args)
		if err != nil {
			return policy.Request{}, engine.Preview{}, false, err
		}
		return req, engine.Preview{Title: pv.Title, Diff: pv.Diff, Body: pv.Body}, true, nil
	}
}

func mcpName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, "mcp_")
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "_")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

func toolDocs() []prompt.ToolDoc {
	docs := tools.Docs()
	out := make([]prompt.ToolDoc, 0, len(docs))
	for _, d := range docs {
		out = append(out, prompt.ToolDoc{Name: d.Name, When: d.When})
	}
	return out
}

// fetchClient is the web_fetch client: ssrfguard validates every dial (which
// covers DNS rebinding) and CheckRedirect re-validates each hop's URL before
// it is followed, so a public page cannot bounce the agent to an internal
// address through a redirect chain.
func fetchClient() *http.Client {
	g := ssrfguard.New()
	c := g.Client()
	c.Timeout = fetchTimeout
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("web_fetch: too many redirects")
		}
		return g.ValidateURLContext(req.Context(), req.URL.String())
	}
	return c
}
