package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/sandbox"
)

// maxHardDenials aborts a run that keeps hitting the hard-deny set; a model
// that is being steered by injected content should not get unlimited tries.
const maxHardDenials = 3

// bashTool is the one tool whose decision carries a sandbox grant. It is
// named here rather than imported from internal/tools, which sits below the
// engine and must not be pulled into it.
const bashTool = "bash"

// Approve implements agentkit.Approver. It runs on the tool goroutine: it
// evaluates policy, and for an Ask verdict emits an approval request and
// blocks until the UI replies or the run is cancelled.
func (e *Engine) Approve(ctx context.Context, c agentkit.Call) (agentkit.Decision, error) {
	req, preview, err := e.describe(c)
	if err != nil {
		e.auditDecision(c, policy.Verdict{Decision: policy.Deny, Reason: err.Error()}, "policy", nil, CallGrant{})
		return agentkit.Deny("could not evaluate this call: " + err.Error()), nil
	}
	req.Depth = c.Depth
	verdict := e.policyFor(c.Depth).Evaluate(req)
	switch verdict.Decision {
	case policy.Allow:
		e.auditDecision(c, verdict, "policy", nil, CallGrant{})
		return e.allowed(c, verdict, nil), nil
	case policy.Deny:
		e.auditDecision(c, verdict, "policy", nil, CallGrant{})
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
		// Headless never prompts: this call is denied, now. Recording the Ask
		// verdict that would have raised a prompt leaves a CI run's log
		// claiming nothing was denied, which is the same untruth outcomeOf
		// fixes for the two user branches. What the verdict *was* survives in
		// Class, Rule and Reason, which names the --allow rule that would let
		// it through.
		verdict.Decision = policy.Deny
		e.auditDecision(c, verdict, "headless", nil, CallGrant{})
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
	grant := callGrant(req, verdict)
	e.emit(Event{Kind: KindApprovalRequest, RunID: c.RunID, Depth: c.Depth, Call: &c.Call, Approval: &Approval{
		ID: id, Tool: c.Call.Name, Args: c.Call.Arguments, Request: req, Verdict: verdict,
		Preview: preview, Offers: verdict.Offers, Severity: severity(verdict, req), Grants: grant,
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
			e.auditDecision(c, verdict, "user", &d, CallGrant{})
			return agentkit.Deny(verdict.Reason), nil
		}
		if d.Grant != nil {
			if err := e.policy.Grant(*d.Grant); err != nil {
				e.emit(Event{Kind: KindNotice, Text: "could not record grant: " + err.Error()})
			}
		}
		// Approving grants what the command needs. Offering "allow" and
		// "allow with network" separately for a command that cannot work
		// without the network only produces a call that fails after the
		// user said yes — `brew info fpc` did exactly that.
		verdict.Network = verdict.Network || d.Network || grant.Network
		e.auditDecision(c, verdict, "user", &d, grant)
		return e.allowedWithGrant(c, verdict, d.Args, grant), nil
	}
}

// callGrant is what allowing this call will give it beyond the defaults. It
// is computed from the classifier, never from the model's arguments alone:
// Request.Network only matters once the user has answered the prompt it
// caused, and the writable prefixes only ever come from an approval.
func callGrant(req policy.Request, v policy.Verdict) CallGrant {
	sh := req.Shell
	if req.Tool != bashTool || sh == nil {
		return CallGrant{}
	}
	g := CallGrant{Network: v.Network || sh.NeedsNetwork || req.Network}
	if sh.Installs {
		// A package manager writes outside the workspace. The prompt names
		// these paths, so approving is consent to this exact list.
		g.Writable = sandbox.ToolPrefixes()
	}
	return g
}

// grantKeyFor identifies one tool call for the grant handover between
// Approve and the middleware that puts the grant on the context. agentkit's
// approval middleware runs the tool with the context it already had, so an
// Approver cannot add to it; the arguments are part of the key because they
// are what the middleware passes on after any edit.
func grantKeyFor(callID string, args json.RawMessage) string {
	return callID + "\x00" + string(args)
}

// allowedWithGrant is allowed(), plus the per-call sandbox grant the user's
// approval earned. Only a non-empty writable set is recorded: network
// already travels as the rewritten "network" argument.
func (e *Engine) allowedWithGrant(c agentkit.Call, verdict policy.Verdict, edited json.RawMessage, grant CallGrant) agentkit.Decision {
	d := e.allowed(c, verdict, edited)
	if len(grant.Writable) == 0 {
		return d
	}
	args := c.Call.Arguments
	if d.Arguments != nil {
		args = d.Arguments
	}
	e.mu.Lock()
	e.grants[grantKeyFor(c.Call.ID, args)] = grant
	e.mu.Unlock()
	return d
}

// takeGrant pops the grant recorded for a call, if any.
func (e *Engine) takeGrant(callID string, args json.RawMessage) (CallGrant, bool) {
	key := grantKeyFor(callID, args)
	e.mu.Lock()
	defer e.mu.Unlock()
	g, ok := e.grants[key]
	delete(e.grants, key)
	return g, ok
}

// allowed builds the Allow decision. For bash the "network" argument is
// always rewritten to the verdict, so the tool never acts on the model's own
// request: only a +net rule or the user's answer turns networking on.
func (e *Engine) allowed(c agentkit.Call, verdict policy.Verdict, edited json.RawMessage) agentkit.Decision {
	args := edited
	if c.Call.Name == bashTool {
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
	// A grant whose call never ran (a cancelled run) must not be waiting for
	// the next call that happens to carry the same arguments.
	clear(e.grants)
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

// auditDecision records one decision. grant is what allowing this call hands
// it; it is recorded only for an allow, because a call that did not run was
// given nothing.
func (e *Engine) auditDecision(c agentkit.Call, v policy.Verdict, by string, d *Decision, grant CallGrant) {
	outcome := outcomeOf(v, d)
	rec := &audit.Decision{Outcome: outcome, Class: v.Class.String(), Mode: e.Mode().String(), By: by, OffersShown: len(v.Offers), HardDeny: v.HardDeny, Reason: v.Reason}
	if v.Rule != nil {
		rec.Rule = v.Rule.String()
		rec.Source = string(v.Rule.Source)
	}
	if d != nil && d.Grant != nil {
		rec.Grant = d.Grant.Rule.String() + " (" + d.Grant.Scope.String() + ")"
	}
	if outcome == strings.ToLower(policy.Allow.String()) {
		// v.Network is the value Engine.allowed pins into the call's
		// arguments, so it is the network the call really gets.
		rec.GrantedNetwork = grant.Network || v.Network
		rec.GrantedWritable = grant.Writable
	}
	e.audit(audit.Event{Kind: audit.KindDecision, Run: c.RunID, Depth: c.Depth, Tool: &audit.Tool{Name: c.Call.Name, CallID: c.Call.ID, Args: string(c.Call.Arguments)}, Decision: rec})
}

// outcomeOf is what happened to the call, which is not always the verdict. A
// verdict the user answered is still Ask — Ask is what raised the prompt —
// so recording it leaves an allow and a deny indistinguishable in the log and
// counts a denial as "asked" in the summary. The user's answer is the outcome.
func outcomeOf(v policy.Verdict, d *Decision) string {
	if d == nil {
		return strings.ToLower(v.Decision.String())
	}
	if d.Allow {
		return strings.ToLower(policy.Allow.String())
	}
	return strings.ToLower(policy.Deny.String())
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
			flag := "--allow '" + o.Rule.String() + "'"
			if !slices.Contains(flags, flag) {
				flags = append(flags, flag)
			}
		}
		hint = "re-run with " + strings.Join(flags, " ")
	}
	why := ""
	if v.Reason != "" {
		why = ": " + v.Reason
	}
	return fmt.Sprintf("%s %s%s (%s)", c.Call.Name, HeadlessDenialMarker, why, hint)
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
