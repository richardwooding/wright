package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/catalog"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/prompt"
)

// ErrRunning is returned by operations that need an idle engine.
var ErrRunning = errors.New("engine: a run is in progress")

func catalogFor(name string) catalog.Model { return catalog.Lookup(name) }

// Submit starts a run with text (plus optional parts). While a run is in
// progress the message is queued for the running agent instead: it is
// appended before the next model call, so typing while the agent works
// steers it rather than waiting.
func (e *Engine) Submit(text string, parts ...core.Part) error {
	if strings.TrimSpace(text) == "" && len(parts) == 0 {
		return errors.New("engine: empty message")
	}
	select {
	case <-e.closed:
		// A caller that is not the UI — the debug endpoint's drain
		// goroutine — can still be holding a reference while the session
		// shuts down. Starting a run here would launch work on
		// context.Background() that nothing will ever stop.
		return errors.New("engine: the session is closed")
	default:
	}
	e.mu.Lock()
	if e.running {
		e.inbox.Post(append([]core.Part{core.Text(text)}, parts...)...)
		e.queued = e.inbox.Len()
		n := e.queued
		e.mu.Unlock()
		e.emit(Event{Kind: KindQueued, Queued: n, Text: text})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.cancel = cancel
	e.runDone = make(chan struct{})
	done := e.runDone
	e.mu.Unlock()
	go func() {
		defer close(done)
		e.run(ctx, text, parts)
	}()
	return nil
}

// Cancel interrupts the current run. Queued messages are dropped.
func (e *Engine) Cancel() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait blocks until the current run (if any) has finished.
func (e *Engine) Wait() {
	e.mu.Lock()
	done := e.runDone
	e.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (e *Engine) run(ctx context.Context, text string, parts []core.Part) {
	start := e.now()
	agent, err := e.buildAgent(ctx)
	if err != nil {
		e.finishRun(Finish{Err: err, StopReason: string(agentkit.StopError)}, start)
		return
	}
	e.mu.Lock()
	sessionID := e.session
	e.mu.Unlock()
	opts := []agentkit.RunOption{agentkit.WithSession(sessionID), agentkit.WithInbox(e.inbox)}
	if len(parts) > 0 {
		opts = append(opts, agentkit.WithParts(parts...))
	}
	var fin Finish
	for ev, err := range agent.Stream(ctx, text, opts...) {
		if ev.Kind == agentkit.EventFinish && ev.Depth == 0 {
			fin = e.finishFrom(ev, err)
			continue
		}
		e.translate(ev)
	}
	e.finishRun(fin, start)
}

func (e *Engine) finishFrom(ev agentkit.Event, err error) Finish {
	fin := Finish{Err: err}
	if ev.Result != nil {
		fin.Output = ev.Result.Output
		fin.StopReason = string(ev.Result.StopReason)
		fin.Steps = ev.Result.Steps
		fin.ToolCalls = ev.Result.ToolCalls
		fin.Usage = ev.Result.Usage
		fin.Duration = ev.Result.Duration
	}
	return fin
}

func (e *Engine) finishRun(fin Finish, start time.Time) {
	if fin.Duration == 0 {
		fin.Duration = e.now().Sub(start)
	}
	_, fin.Cost, _ = e.meter.Total()
	e.mu.Lock()
	e.running = false
	e.cancel = nil
	e.queued = 0
	e.meta.Turns++
	e.hardDeny = 0
	e.mu.Unlock()
	e.failPending()
	if fin.Err != nil && !errors.Is(fin.Err, context.Canceled) {
		e.audit(audit.Event{Kind: audit.KindError, Error: fin.Err.Error()})
	}
	e.audit(audit.Event{Kind: audit.KindRunEnd, Text: fin.StopReason, Model: &audit.Model{
		Name: e.choice.Model, InputTokens: fin.Usage.InputTokens, OutputTokens: fin.Usage.OutputTokens,
		CacheRead: fin.Usage.CachedInputTokens, CacheWrite: fin.Usage.CacheWriteTokens,
	}})
	if err := e.touch(context.Background()); err != nil {
		e.emit(Event{Kind: KindNotice, Text: "session metadata: " + err.Error()})
	}
	e.emit(Event{Kind: KindRunFinished, RunID: e.lastRun, Finish: &fin, Usage: fin.Usage, Cost: fin.Cost, Duration: fin.Duration, Err: fin.Err})
}

// buildAgent constructs a fresh immutable Agent for one run so that the mode
// (which filters tools), the model and the dynamic prompt block are current.
func (e *Engine) buildAgent(ctx context.Context) (*agentkit.Agent, error) {
	e.mu.Lock()
	client, fast, choice, mode := e.client, e.fast, e.choice, e.mode
	e.mu.Unlock()
	stable, dynamic := prompt.System(e.promptInputs(ctx, choice, mode))
	tools := e.opts.Tools
	if mode == policy.ModePlan {
		tools = filterTools(tools, e.opts.ReadOnlyTools)
	}
	opts := []agentkit.Option{
		agentkit.WithName("wright"),
		agentkit.WithInstructions(stable),
		agentkit.WithAdditionalInstructions(dynamic),
		agentkit.WithTools(tools...),
		agentkit.WithMiddleware(e.Middleware()...),
		agentkit.WithParallel(4),
		agentkit.WithBudget(agentkit.Budget{MaxSteps: e.opts.MaxSteps, Timeout: e.opts.Timeout}),
		agentkit.WithHooks(e.hooks()),
		agentkit.WithCache(core.CacheConfig{System: true, Turns: 1}),
	}
	if e.opts.Store != nil {
		opts = append(opts, agentkit.WithStore(e.opts.Store))
	}
	if e.ctxWin > 0 {
		opts = append(opts,
			agentkit.WithContextWindow(e.ctxWin),
			agentkit.WithCompactor(agentkit.Chain(agentkit.Summarize(fast, 4, agentkit.WithSummaryPrompt(prompt.Summary())), agentkit.StripReasoning())),
		)
	}
	if n := e.opts.Settings.Model.MaxOutputTokens; n > 0 {
		opts = append(opts, agentkit.WithMaxTokens(n))
	}
	if effort := e.opts.Settings.Model.Reasoning; effort != "" && choice.Info.Capabilities.Reasoning {
		opts = append(opts, agentkit.WithReasoning(core.ReasoningConfig{Effort: effort, Summary: "auto"}))
	}
	opts = append(opts, e.opts.Extra...)
	return agentkit.NewFromClient(client, opts...)
}

func filterTools(ts agentkit.Toolset, keep []string) agentkit.Toolset {
	var out agentkit.Toolset
	for _, t := range ts {
		if slices.Contains(keep, t.Definition().Name) {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) promptInputs(ctx context.Context, choice model.Choice, mode policy.Mode) prompt.Inputs {
	var gs git.Summary
	if e.opts.Git != nil {
		gs = e.opts.Git(ctx)
	}
	osName, arch, shell := e.opts.OS, e.opts.Arch, e.opts.Shell
	if osName == "" {
		osName, arch = runtime.GOOS, runtime.GOARCH
	}
	return prompt.Inputs{
		WS: e.opts.WS, Cwd: e.opts.Cwd, Model: choice, Mode: mode,
		Sandbox: e.opts.Sandbox, SandboxNet: e.opts.SandboxNet, Git: gs, Now: e.now(),
		Instructions: e.opts.Instructions, Tools: e.opts.ToolDocs,
		Attribution: e.opts.Settings.AttributionEnabled(), Trailer: e.opts.Settings.Git.Trailer,
		OS: osName, Arch: arch, Shell: shell,
	}
}

// translate maps one agentkit event onto the engine's event vocabulary.
func (e *Engine) translate(ev agentkit.Event) {
	out := Event{RunID: ev.RunID, Depth: ev.Depth, Agent: ev.Agent, Step: ev.Step, Time: e.now()}
	switch ev.Kind {
	case agentkit.EventText:
		out.Kind, out.Text = KindText, ev.Text
	case agentkit.EventReasoning:
		out.Kind, out.Text = KindReasoning, ev.Text
	case agentkit.EventStep:
		out.Kind = KindStep
		if ev.Depth == 0 {
			e.startRun(ev.RunID)
		}
	case agentkit.EventToolCall:
		out.Kind, out.Call = KindToolCall, ev.ToolCall
	case agentkit.EventToolProgress:
		out.Kind, out.Call, out.Text = KindToolProgress, ev.ToolCall, ev.Text
	case agentkit.EventToolResult:
		out.Kind, out.Call, out.Result, out.Err, out.Duration = KindToolResult, ev.ToolCall, ev.ToolResult, ev.Err, ev.Duration
	case agentkit.EventRetry:
		out.Kind, out.Err, out.Attempt, out.Delay = KindRetry, ev.Err, ev.Attempt, ev.Delay
		out.Text = fmt.Sprintf("retrying model call (attempt %d) in %s: %v", ev.Attempt, ev.Delay.Round(time.Millisecond), ev.Err)
	case agentkit.EventCompact:
		out.Kind = KindCompact
		out.Compact = &CompactInfo{Reason: "automatic", Before: ev.Before, After: ev.After}
		out.Text = fmt.Sprintf("context compacted: %s → %s tokens", tokens(ev.Before), tokens(ev.After))
	case agentkit.EventUsage:
		out.Kind = KindUsage
		if ev.Usage != nil {
			out.Usage = *ev.Usage
			out.Cost, out.ContextPct = e.recordUsage(ev.Depth, *ev.Usage)
		}
		out.Duration = ev.Duration
	case agentkit.EventApprovalRequest, agentkit.EventApprovalResult, agentkit.EventHandoff:
		return // the engine emits its own richer approval events
	case agentkit.EventFinish:
		if ev.Depth == 0 {
			return
		}
		out.Kind = KindNotice
		out.Text = "sub-agent " + ev.Agent + " finished"
	default:
		return
	}
	e.emit(out)
}

func (e *Engine) startRun(runID string) {
	e.mu.Lock()
	if e.lastRun == runID {
		e.mu.Unlock()
		return
	}
	e.lastRun = runID
	e.mu.Unlock()
	e.audit(audit.Event{Kind: audit.KindRunStart, Run: runID})
	e.emit(Event{Kind: KindRunStarted, RunID: runID})
}

func (e *Engine) recordUsage(depth int, u core.Usage) (usd, pct float64) {
	e.mu.Lock()
	name := e.choice.Model
	if depth == 0 {
		e.lastIn = u.InputTokens
	}
	e.meter.Add(name, u)
	_, usd, _ = e.meter.Total()
	if e.ctxWin > 0 {
		pct = float64(e.lastIn) / float64(e.ctxWin)
	}
	e.mu.Unlock()
	e.audit(audit.Event{Kind: audit.KindModelCall, Depth: depth, Model: &audit.Model{
		Name: name, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheRead: u.CachedInputTokens, CacheWrite: u.CacheWriteTokens,
	}})
	return usd, pct
}

func (e *Engine) hooks() agentkit.Hooks {
	return agentkit.Hooks{
		OnCompact: func(ci agentkit.CompactInfo) {
			e.audit(audit.Event{Kind: audit.KindCompaction, Run: ci.RunID, Text: fmt.Sprintf("%s %d→%d", ci.Reason, ci.Before, ci.After)})
		},
	}
}

func tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}
