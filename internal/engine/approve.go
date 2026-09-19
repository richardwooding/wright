package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
)

// maxHardDenials aborts a run that keeps hitting the hard-deny set; a model
// that is being steered by injected content should not get unlimited tries.
const maxHardDenials = 3

// Approve implements agentkit.Approver. It runs on the tool goroutine: it
// evaluates policy, and for an Ask verdict emits an approval request and
// blocks until the UI replies or the run is cancelled.
func (e *Engine) Approve(ctx context.Context, c agentkit.Call) (agentkit.Decision, error) {
	req, preview, err := e.describe(c)
	if err != nil {
		e.auditDecision(c, policy.Verdict{Decision: policy.Deny, Reason: err.Error()}, "policy", nil)
		return agentkit.Deny("could not evaluate this call: " + err.Error()), nil
	}
	req.Depth = c.Depth
	verdict := e.policyFor(c.Depth).Evaluate(req)
	switch verdict.Decision {
	case policy.Allow:
		e.auditDecision(c, verdict, "policy", nil)
		return e.allowed(c, verdict, nil), nil
	case policy.Deny:
		e.auditDecision(c, verdict, "policy", nil)
		if verdict.HardDeny {
			e.countHardDeny(ctx)
		}
		return agentkit.Deny(denialText(verdict)), nil
	default:
		return e.ask(ctx, c, req, verdict, preview)
	}
}

// policyFor returns the engine that decides a call at depth. Depth 0 is the
// main agent and uses the session's engine; a sub-agent call is evaluated by
// a child engine, which clamps bypass back to the default mode and refuses
// grants, so a child can never be more permissive than its parent. The child
// is derived per call so grants made meanwhile are visible to it.
func (e *Engine) policyFor(depth int) *policy.Engine {
	if depth <= 0 {
		return e.policy
	}
	return e.policy.Child(depth)
}

func (e *Engine) describe(c agentkit.Call) (policy.Request, Preview, error) {
	if e.opts.Describe != nil {
		req, preview, ok, err := e.opts.Describe(c.Call.Name, c.Call.Arguments)
		if err != nil {
			return policy.Request{}, Preview{}, err
		}
		if ok {
			return req, preview, nil
		}
	}
	// Unknown tools (sub-agents, MCP without a describer) are evaluated by
	// name only; the builtin rules ask for anything they do not recognise.
	return policy.Request{Tool: c.Call.Name, Args: c.Call.Arguments},
		Preview{Title: c.Call.Name, Body: string(c.Call.Arguments)}, nil
}

func (e *Engine) ask(ctx context.Context, c agentkit.Call, req policy.Request, verdict policy.Verdict, preview Preview) (agentkit.Decision, error) {
	if e.opts.Headless {
		verdict.Reason = headlessReason(c, verdict)
		e.auditDecision(c, verdict, "headless", nil)
		return agentkit.Deny(verdict.Reason), nil
	}
	e.mu.Lock()
	e.seq++
	id := fmt.Sprintf("%s-%d", c.Call.ID, e.seq)
	reply := make(chan Decision, 1)
	e.pending[id] = reply
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
	}()
	e.emit(Event{Kind: KindApprovalRequest, RunID: c.RunID, Depth: c.Depth, Call: &c.Call, Approval: &Approval{
		ID: id, Tool: c.Call.Name, Args: c.Call.Arguments, Request: req, Verdict: verdict,
		Preview: preview, Offers: verdict.Offers, Severity: severity(verdict, req),
	}})
	select {
	case <-ctx.Done():
		return agentkit.Decision{}, ctx.Err()
	case d := <-reply:
		d.By = "user"
		e.emit(Event{Kind: KindApprovalDecided, RunID: c.RunID, Depth: c.Depth, Call: &c.Call, Decision: &d, Text: id})
		if !d.Allow {
			verdict.Reason = d.Reason
			if verdict.Reason == "" {
				verdict.Reason = "the user declined this call"
			}
			e.auditDecision(c, verdict, "user", &d)
			return agentkit.Deny(verdict.Reason), nil
		}
		if d.Grant != nil {
			if err := e.policy.Grant(*d.Grant); err != nil {
				e.emit(Event{Kind: KindNotice, Text: "could not record grant: " + err.Error()})
			}
		}
		verdict.Network = verdict.Network || d.Network
		e.auditDecision(c, verdict, "user", &d)
		return e.allowed(c, verdict, d.Args), nil
	}
}

// allowed builds the Allow decision. For bash the "network" argument is
// always rewritten to the verdict, so the tool never acts on the model's own
// request: only a +net rule or the user's answer turns networking on.
func (e *Engine) allowed(c agentkit.Call, verdict policy.Verdict, edited json.RawMessage) agentkit.Decision {
	args := edited
	if c.Call.Name == "bash" {
		src := args
		if src == nil {
			src = c.Call.Arguments
		}
		if pinned, err := setJSONBool(src, "network", verdict.Network); err == nil {
			args = pinned
		}
	}
	if args == nil {
		return agentkit.Allow()
	}
	return agentkit.AllowWith(args)
}

// Reply answers a pending approval. Unknown IDs are ignored (the run may have
// been cancelled meanwhile).
func (e *Engine) Reply(id string, d Decision) {
	e.mu.Lock()
	ch, ok := e.pending[id]
	e.mu.Unlock()
	if ok {
		select {
		case ch <- d:
		default:
		}
	}
}

// Ask relays an ask_user question to the UI and blocks for the answer.
func (e *Engine) Ask(ctx context.Context, q QuestionEvent) (Answer, error) {
	if e.opts.Headless {
		return Answer{}, errors.New("ask_user needs an interactive session")
	}
	e.mu.Lock()
	e.seq++
	q.ID = fmt.Sprintf("q-%d", e.seq)
	reply := make(chan Answer, 1)
	e.answers[q.ID] = reply
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.answers, q.ID)
		e.mu.Unlock()
	}()
	e.emit(Event{Kind: KindQuestion, Question: &q, Call: q.ToolCall})
	select {
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	case a := <-reply:
		return a, nil
	}
}

// Answer resolves a pending question.
func (e *Engine) Answer(id string, a Answer) {
	e.mu.Lock()
	ch, ok := e.answers[id]
	e.mu.Unlock()
	if ok {
		select {
		case ch <- a:
		default:
		}
	}
}

// failPending unblocks nothing (the run's ctx does that) but drops stale
// entries so a late Reply cannot leak into a future run.
func (e *Engine) failPending() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.pending {
		delete(e.pending, id)
	}
	for id := range e.answers {
		delete(e.answers, id)
	}
}

func (e *Engine) countHardDeny(ctx context.Context) {
	e.mu.Lock()
	e.hardDeny++
	n := e.hardDeny
	cancel := e.cancel
	e.mu.Unlock()
	if n >= maxHardDenials && cancel != nil {
		e.audit(audit.Event{Kind: audit.KindHardDenyAbort, Text: fmt.Sprintf("%d hard denials in one run", n)})
		e.emit(Event{Kind: KindError, Text: fmt.Sprintf("run stopped: %d hard-denied calls in one turn", n)})
		cancel()
		<-ctx.Done()
	}
}

func (e *Engine) auditDecision(c agentkit.Call, v policy.Verdict, by string, d *Decision) {
	rec := &audit.Decision{Outcome: strings.ToLower(v.Decision.String()), Class: v.Class.String(), Mode: e.Mode().String(), By: by, OffersShown: len(v.Offers), HardDeny: v.HardDeny, Reason: v.Reason}
	if v.Rule != nil {
		rec.Rule = v.Rule.String()
		rec.Source = string(v.Rule.Source)
	}
	if d != nil && d.Grant != nil {
		rec.Grant = d.Grant.Rule.String() + " (" + d.Grant.Scope.String() + ")"
	}
	e.audit(audit.Event{Kind: audit.KindDecision, Run: c.RunID, Depth: c.Depth, Tool: &audit.Tool{Name: c.Call.Name, CallID: c.Call.ID, Args: string(c.Call.Arguments)}, Decision: rec})
}

func denialText(v policy.Verdict) string {
	s := "denied by policy"
	if v.Rule != nil {
		s += " rule " + v.Rule.String()
	}
	if v.Reason != "" {
		s += ": " + v.Reason
	}
	return s + ". Do not retry the same action another way; tell the user what you needed and why."
}

// HeadlessDenialMarker is the phrase every headless denial carries. The
// headless runner scans tool results for it to report "approval required"
// (exit 3) rather than a plain tool failure.
const HeadlessDenialMarker = "requires interactive approval in headless mode"

func headlessReason(c agentkit.Call, v policy.Verdict) string {
	hint := "re-run interactively"
	if len(v.Offers) > 0 {
		flags := make([]string, 0, len(v.Offers))
		for _, o := range v.Offers {
			flags = append(flags, "--allow '"+o.Rule.String()+"'")
		}
		hint = "re-run with " + strings.Join(flags, " ")
	}
	return fmt.Sprintf("%s %s (%s, or use --mode auto-edit for edits)", c.Call.Name, HeadlessDenialMarker, hint)
}

func severity(v policy.Verdict, req policy.Request) Severity {
	switch {
	case v.HardDeny, v.Class >= shellclass.Destructive:
		return SeverityDestructive
	case req.Shell != nil && req.Shell.Unknown:
		return SeverityDestructive
	case len(req.Writes) > 0, v.Class == shellclass.Network, req.URL != nil, v.Class == shellclass.MutatingWorkspace:
		return SeverityCaution
	default:
		return SeverityInfo
	}
}

func setJSONBool(src json.RawMessage, key string, val bool) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(src, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	m[key] = val
	return json.Marshal(m)
}
