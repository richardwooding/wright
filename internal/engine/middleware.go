package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/sandbox"
)

// Middleware is the chain every tool call runs through, at any depth: panic
// recovery, the approval gate (which evaluates sub-agent calls against a
// child policy engine), the audit log and the untrusted-content fence. The
// app gives sub-agents the same chain, so a child's calls are approved,
// audited and fenced exactly like the main agent's.
func (e *Engine) Middleware() []agentkit.Middleware {
	return []agentkit.Middleware{agentkit.Recover(), agentkit.ApproveWith(e), e.grantMiddleware(), e.auditMiddleware(), e.untrustedMiddleware()}
}

// grantMiddleware hands the approved call the sandbox grant its approval
// earned. It sits directly inside the approval middleware because that is
// the only place the decision and the call's context meet: agentkit's
// ApproveWith calls the tool with the context it already had, so Approve
// itself cannot add to it. A call with no recorded grant is untouched — the
// sandbox is never widened by default.
func (e *Engine) grantMiddleware() agentkit.Middleware {
	return func(next agentkit.Tool) agentkit.Tool {
		def := next.Definition()
		return agentkit.Raw(def.Name, def.Description, def.Parameters,
			func(ctx context.Context, args json.RawMessage) (agentkit.Output, error) {
				c, _ := agentkit.CallFrom(ctx)
				if g, ok := e.takeGrant(c.Call.ID, args); ok {
					ctx = sandbox.WithGrant(ctx, sandbox.Grant{Network: g.Network, Writable: g.Writable})
				}
				return next.Call(ctx, args)
			})
	}
}

// maxAuditArgs bounds the argument text kept in the audit log.
const maxAuditArgs = 4096

// auditMiddleware records every tool call and result. It sits inside the
// approval middleware, so denied calls are logged by the decision path only.
func (e *Engine) auditMiddleware() agentkit.Middleware {
	return func(next agentkit.Tool) agentkit.Tool {
		return agentkit.Raw(next.Definition().Name, next.Definition().Description, next.Definition().Parameters,
			func(ctx context.Context, args json.RawMessage) (agentkit.Output, error) {
				c, _ := agentkit.CallFrom(ctx)
				e.audit(audit.Event{Kind: audit.KindToolCall, Run: c.RunID, Depth: c.Depth, Tool: toolRef(c, args)})
				start := e.now()
				// This middleware wraps the tool's actual execution, so it
				// is the one place that knows a call is running, at every
				// depth: a dump asking "what has not come back" is answered
				// from here. Deferred, because agentkit.Recover sits outside
				// this middleware and a panicking tool unwinds through here.
				defer e.startCall(c)()
				out, err := next.Call(ctx, args)
				rec := &audit.Result{IsError: out.IsError || err != nil, Bytes: len(out.Text()), DurationMS: e.now().Sub(start).Milliseconds()}
				ev := audit.Event{Kind: audit.KindToolResult, Run: c.RunID, Depth: c.Depth, Tool: toolRef(c, args), Result: rec}
				if err != nil {
					ev.Error = err.Error()
				}
				e.audit(ev)
				return out, err
			})
	}
}

// untrustedMiddleware fences every text part of a tool result as data and
// flags instruction-like content, so the model treats tool output, fetched
// pages and MCP results as observations rather than commands.
func (e *Engine) untrustedMiddleware() agentkit.Middleware {
	return func(next agentkit.Tool) agentkit.Tool {
		name := next.Definition().Name
		return agentkit.Raw(name, next.Definition().Description, next.Definition().Parameters,
			func(ctx context.Context, args json.RawMessage) (agentkit.Output, error) {
				out, err := next.Call(ctx, args)
				if err != nil || out.IsError {
					return out, err
				}
				c, _ := agentkit.CallFrom(ctx)
				for i, p := range out.Content {
					t, ok := p.(core.TextPart)
					if !ok {
						continue
					}
					if sigs := prompt.ScanInjection(t.Text); len(sigs) > 0 {
						e.flagInjection(c, name, sigs)
					}
					out.Content[i] = core.Text(prompt.WrapUntrusted(name, t.Text))
				}
				return out, err
			})
	}
}

func (e *Engine) flagInjection(c agentkit.Call, tool string, sigs []prompt.Signal) {
	kinds := make([]string, 0, len(sigs))
	for _, s := range sigs {
		kinds = append(kinds, s.Kind)
	}
	msg := fmt.Sprintf("%s result contains instruction-like content (%s); it is passed to the model as data only", tool, strings.Join(kinds, ", "))
	e.audit(audit.Event{Kind: audit.KindInjection, Run: c.RunID, Depth: c.Depth, Tool: &audit.Tool{Name: tool, CallID: c.Call.ID}, Text: strings.Join(kinds, ",")})
	e.emit(Event{Kind: KindInjection, RunID: c.RunID, Depth: c.Depth, Call: &c.Call, Text: msg})
}

func toolRef(c agentkit.Call, args json.RawMessage) *audit.Tool {
	a := string(args)
	if len(a) > maxAuditArgs {
		a = a[:maxAuditArgs] + "…"
	}
	return &audit.Tool{Name: c.Call.Name, CallID: c.Call.ID, Args: a}
}
