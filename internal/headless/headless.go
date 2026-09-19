// Package headless runs one prompt through the engine without a UI (`wright
// -p`) and reports the outcome as text, a single JSON object or a stream of
// JSON lines. It never approves anything: the engine denies every Ask with a
// hint, and the runner turns that into exit code 3 so scripts can tell "the
// agent needed a human" from "the agent failed".
package headless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
)

// Format selects how the run is reported.
type Format string

// Output formats.
const (
	FormatText       Format = "text"        // final output text only
	FormatJSON       Format = "json"        // one result object
	FormatStreamJSON Format = "stream-json" // one object per event, then the result
)

// ParseFormat validates a --output value.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON, FormatStreamJSON:
		return f, nil
	default:
		return "", fmt.Errorf("headless: unknown output format %q (text, json or stream-json)", s)
	}
}

// Exit codes. They are part of the headless contract (scripts branch on
// them), so their meaning never changes.
const (
	ExitOK               = 0
	ExitError            = 1   // provider or tool error, or the run stopped with an error
	ExitUsage            = 2   // bad invocation (no prompt, unknown format)
	ExitApprovalRequired = 3   // a call needed an approval that headless mode cannot give
	ExitBudget           = 4   // max steps, max tokens or deadline reached
	ExitInterrupted      = 130 // the context was cancelled (SIGINT/SIGTERM)
)

// Line is one stream-json record. Type is the engine event kind in
// snake_case, or "result" for the final line; every other field is set only
// when it applies.
type Line struct {
	Type       string        `json:"type"`
	Time       time.Time     `json:"time,omitzero"`
	RunID      string        `json:"run_id,omitempty"`
	Depth      int           `json:"depth,omitempty"`
	Step       int           `json:"step,omitempty"`
	Text       string        `json:"text,omitempty"`
	Tool       *ToolLine     `json:"tool,omitempty"`
	Result     *ResultLine   `json:"result,omitempty"`
	Usage      *UsageLine    `json:"usage,omitempty"`
	CostUSD    float64       `json:"cost_usd,omitempty"`
	ContextPct float64       `json:"context_pct,omitempty"`
	Approval   *ApprovalLine `json:"approval,omitempty"`
	Decision   *DecisionLine `json:"decision,omitempty"`
	Todos      []engine.Todo `json:"todos,omitempty"`
	Queued     int           `json:"queued,omitempty"`
	Error      string        `json:"error,omitempty"`

	// Result fields, set on the final "result" line only.
	Output     string `json:"output,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Steps      int    `json:"steps,omitempty"`
	ToolCalls  int    `json:"tool_calls,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ExitCode   int    `json:"exit_code"`
}

// ToolLine identifies a tool call.
type ToolLine struct {
	Name string          `json:"name"`
	ID   string          `json:"id,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ResultLine carries a tool result.
type ResultLine struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// UsageLine mirrors core.Usage with JSON names.
type UsageLine struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int `json:"reasoning_tokens,omitempty"`
}

// ApprovalLine is the shape of an approval request. Headless runs never
// produce one (the engine denies instead), but the schema is stable so a
// consumer of stream-json can handle both modes with one decoder.
type ApprovalLine struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Title    string `json:"title,omitempty"`
	Severity string `json:"severity,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// DecisionLine reports how an approval was answered.
type DecisionLine struct {
	Allow  bool   `json:"allow"`
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Run submits promptText (plus parts) to e and consumes events until the run
// finishes. It writes the report to w in format f, diagnostics to errw, and
// returns the exit code. Cancelling ctx interrupts the run through the
// engine and yields ExitInterrupted.
func Run(ctx context.Context, e *engine.Engine, promptText string, parts []core.Part, f Format, w, errw io.Writer, verbose bool) int {
	r := &runner{e: e, f: f, w: w, errw: errw, verbose: verbose}
	if err := e.Submit(promptText, parts...); err != nil {
		r.errText = err.Error()
		return r.finish(ExitUsage)
	}
	return r.loop(ctx)
}

type runner struct {
	e       *engine.Engine
	f       Format
	w, errw io.Writer
	verbose bool

	approval    bool // a tool result carried the headless-denial marker
	interrupted bool
	errText     string
	fin         *engine.Finish
}

func (r *runner) loop(ctx context.Context) int {
	events := r.e.Events()
	done := ctx.Done()
	for {
		select {
		case <-done:
			// Cancel once, then keep draining so the RunFinished line and the
			// audit run_end still get written.
			r.interrupted = true
			r.e.Cancel()
			done = nil
		case ev, ok := <-events:
			if !ok {
				return r.finish(r.exitCode())
			}
			r.observe(ev)
			r.write(ev)
			if ev.Kind == engine.KindRunFinished {
				return r.finish(r.exitCode())
			}
		}
	}
}

// observe updates the exit-code state from one event.
func (r *runner) observe(ev engine.Event) {
	switch ev.Kind {
	case engine.KindToolResult:
		if ev.Result != nil && ev.Result.IsError && strings.Contains(ev.Result.Text(), engine.HeadlessDenialMarker) {
			r.approval = true
		}
	case engine.KindError:
		r.errText = ev.Text
	case engine.KindRunFinished:
		r.fin = ev.Finish
	default:
	}
}

// write emits the event in the chosen format: every event for stream-json,
// tool one-liners on errw for verbose text, nothing otherwise.
func (r *runner) write(ev engine.Event) {
	switch {
	case r.f == FormatStreamJSON:
		if ev.Kind != engine.KindRunFinished { // the result line replaces it
			r.emit(lineFor(ev))
		}
	case r.verbose && ev.Kind == engine.KindToolCall && ev.Call != nil:
		fmt.Fprintf(r.errw, "[tool] %s %s\n", ev.Call.Name, clip(string(ev.Call.Arguments), 120))
	case r.verbose && ev.Kind == engine.KindToolResult && ev.Call != nil:
		status := "ok"
		if ev.Err != nil || (ev.Result != nil && ev.Result.IsError) {
			status = "error"
		}
		fmt.Fprintf(r.errw, "[tool] %s → %s (%s)\n", ev.Call.Name, status, ev.Duration.Round(time.Millisecond))
	case r.verbose && (ev.Kind == engine.KindNotice || ev.Kind == engine.KindError || ev.Kind == engine.KindRedacted):
		fmt.Fprintf(r.errw, "[%s] %s\n", ev.Kind, ev.Text)
	}
}

func (r *runner) finish(code int) int {
	res := r.resultLine(code)
	switch r.f {
	case FormatText:
		if res.Output != "" {
			fmt.Fprintln(r.w, res.Output)
		}
		if res.Error != "" {
			fmt.Fprintln(r.errw, "wright: "+res.Error)
		}
	default:
		r.emit(res)
	}
	return code
}

// exitCode maps the observed run onto the exit-code contract. Budget stops
// are checked before the generic error case because agentkit reports them
// with both a stop reason and an error.
func (r *runner) exitCode() int {
	fin := r.fin
	switch {
	case r.interrupted, fin != nil && fin.StopReason == "canceled", fin != nil && errors.Is(fin.Err, context.Canceled):
		return ExitInterrupted
	case fin != nil && (fin.StopReason == "max_steps" || fin.StopReason == "max_tokens" || fin.StopReason == "max_tool_calls" || fin.StopReason == "deadline"):
		return ExitBudget
	case fin == nil, fin.Err != nil, fin.StopReason == "error", r.errText != "":
		return ExitError
	case r.approval:
		return ExitApprovalRequired
	default:
		return ExitOK
	}
}

func (r *runner) resultLine(code int) Line {
	l := Line{Type: "result", SessionID: r.e.SessionID(), ExitCode: code, Error: r.errText}
	if fin := r.fin; fin != nil {
		l.Output, l.StopReason, l.Steps, l.ToolCalls = fin.Output, fin.StopReason, fin.Steps, fin.ToolCalls
		l.Usage, l.CostUSD, l.DurationMS = usageLine(fin.Usage), fin.Cost, fin.Duration.Milliseconds()
		if fin.Err != nil && l.Error == "" {
			l.Error = fin.Err.Error()
		}
	}
	if r.approval && l.Error == "" {
		l.Error = "a tool call " + engine.HeadlessDenialMarker
	}
	return l
}

func (r *runner) emit(l Line) {
	b, err := json.Marshal(l)
	if err != nil {
		fmt.Fprintf(r.errw, "wright: encode %s line: %v\n", l.Type, err)
		return
	}
	fmt.Fprintf(r.w, "%s\n", b)
}

// lineFor projects an engine event onto the stream-json shape.
func lineFor(ev engine.Event) Line {
	l := Line{Type: ev.Kind.String(), Time: ev.Time, RunID: ev.RunID, Depth: ev.Depth, Step: ev.Step, Text: ev.Text}
	if ev.Call != nil {
		l.Tool = &ToolLine{Name: ev.Call.Name, ID: ev.Call.ID, Args: ev.Call.Arguments}
	}
	switch ev.Kind {
	case engine.KindToolResult:
		l.Result = &ResultLine{IsError: ev.Err != nil}
		if ev.Result != nil {
			l.Result.Text, l.Result.IsError = ev.Result.Text(), l.Result.IsError || ev.Result.IsError
		}
		if ev.Err != nil {
			l.Error = ev.Err.Error()
		}
	case engine.KindUsage:
		l.Usage, l.CostUSD, l.ContextPct = usageLine(ev.Usage), ev.Cost, ev.ContextPct
	case engine.KindApprovalRequest:
		if a := ev.Approval; a != nil {
			l.Approval = &ApprovalLine{ID: a.ID, Tool: a.Tool, Title: a.Preview.Title, Severity: a.Severity.String(), Reason: a.Verdict.Reason}
		}
	case engine.KindApprovalDecided:
		if d := ev.Decision; d != nil {
			l.Decision = &DecisionLine{Allow: d.Allow, By: d.By, Reason: d.Reason}
		}
	case engine.KindTodos:
		l.Todos = ev.Todos
	case engine.KindQueued:
		l.Queued = ev.Queued
	case engine.KindRetry, engine.KindError:
		if ev.Err != nil {
			l.Error = ev.Err.Error()
		}
	default:
	}
	return l
}

func usageLine(u core.Usage) *UsageLine {
	if u == (core.Usage{}) {
		return nil
	}
	return &UsageLine{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
