// Package engine wraps an agentkit Agent for wright: it runs turns, fans every
// agentkit event into one channel the UIs consume, answers approval requests
// through the policy engine, and owns mode and model switching. This file is
// the contract the TUI and headless runner build against.
package engine

import (
	"encoding/json"
	"time"

	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/policy"
)

// Kind is the type of an Event.
type Kind uint8

// Event kinds, roughly in the order a run produces them.
const (
	KindRunStarted Kind = iota + 1
	KindText
	KindReasoning
	KindStep
	KindToolCall
	KindToolProgress
	KindToolResult
	KindApprovalRequest
	KindApprovalDecided
	KindQuestion
	KindRetry
	KindCompact
	KindUsage
	KindRedacted
	KindInjection
	KindTodos
	KindQueued
	KindRunFinished
	KindError
	KindNotice
)

var kindNames = [...]string{
	KindRunStarted: "run_started", KindText: "text", KindReasoning: "reasoning", KindStep: "step",
	KindToolCall: "tool_call", KindToolProgress: "tool_progress", KindToolResult: "tool_result",
	KindApprovalRequest: "approval_request", KindApprovalDecided: "approval_decided",
	KindQuestion: "question", KindRetry: "retry", KindCompact: "compact", KindUsage: "usage",
	KindRedacted: "redacted", KindInjection: "injection", KindTodos: "todos", KindQueued: "queued",
	KindRunFinished: "run_finished", KindError: "error", KindNotice: "notice",
}

// String returns the snake_case name used in stream-json output.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return "unknown"
}

// Event is one item on Engine.Events. Only the fields relevant to Kind are set.
type Event struct {
	Kind  Kind
	RunID string
	Depth int // 0 for the main agent, >0 for forwarded sub-agent events
	Agent string
	Step  int
	Time  time.Time

	Text string // Text, Reasoning, ToolProgress, Notice, Error, Redacted, Injection

	Call     *core.ToolCall   // ToolCall, ToolProgress, ToolResult, ApprovalRequest, ApprovalDecided
	Result   *core.ToolResult // ToolResult
	Err      error            // ToolResult (tool failure), Retry, Error
	Duration time.Duration    // ToolResult, Usage

	Approval *Approval      // ApprovalRequest
	Decision *Decision      // ApprovalDecided
	Question *QuestionEvent // Question

	Usage      core.Usage // Usage: this model call; RunFinished: whole run
	Cost       float64    // Usage, RunFinished: USD, 0 when the model is not in the catalog
	ContextPct float64    // Usage: last prompt size / context window, 0 when unknown

	Attempt int           // Retry
	Delay   time.Duration // Retry

	Compact *CompactInfo // Compact
	Todos   []Todo       // Todos
	Queued  int          // Queued: messages waiting in the inbox

	Finish *Finish // RunFinished
}

// Approval is a pending permission decision the UI must answer with
// Engine.Reply. ID is unique per request.
type Approval struct {
	ID       string
	Tool     string
	Args     json.RawMessage
	Request  policy.Request
	Verdict  policy.Verdict
	Preview  Preview
	Offers   []policy.GrantOffer // narrowest first; empty for destructive or opaque calls
	Severity Severity
}

// Severity drives the colour and the default focus of an approval prompt.
type Severity uint8

// Severity levels.
const (
	SeverityInfo        Severity = iota // reads, builds, tests
	SeverityCaution                     // writes inside the workspace, network
	SeverityDestructive                 // data loss possible; focus defaults to deny
)

// String returns the lower-case severity name.
func (s Severity) String() string {
	switch s {
	case SeverityCaution:
		return "caution"
	case SeverityDestructive:
		return "destructive"
	default:
		return "info"
	}
}

// Preview is what the approval prompt shows: a title line plus either a
// unified diff (edits), the command with its class summary (bash) or a body.
type Preview struct {
	Title string
	Diff  string
	Body  string
}

// Decision is the UI's answer to an Approval or a headless default.
type Decision struct {
	Allow   bool
	Grant   *policy.GrantOffer // accepted offer, when the user chose "allow for …"
	Args    json.RawMessage    // edited arguments, nil when unchanged
	Network bool               // run this bash call with network access
	Reason  string             // shown to the model when denied
	By      string             // "user", "policy", "headless"
}

// QuestionEvent is an ask_user call waiting for Engine.Reply with an Answer.
type QuestionEvent struct {
	ID       string
	Text     string
	Options  []string
	FreeText bool
	ToolCall *core.ToolCall
}

// Answer is the reply to a QuestionEvent, passed through Engine.Answer.
type Answer struct {
	Text  string
	Index int // -1 for free text
}

// CompactInfo describes one context compaction.
type CompactInfo struct {
	Reason string // "proactive", "context_length", "manual"
	Before int    // estimated tokens
	After  int
}

// Todo is one entry of the model's task list.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed
}

// Finish summarises a completed run.
type Finish struct {
	Output     string
	StopReason string
	Steps      int
	ToolCalls  int
	Usage      core.Usage
	Cost       float64
	Duration   time.Duration
	Err        error
}

// Status is the engine state rendered in the status bar and by /cost.
type Status struct {
	Model         string
	Provider      string
	Mode          policy.Mode
	Sandbox       string // backend name; "none" renders as a warning
	SandboxNet    bool   // network allowed inside the sandbox
	ContextWindow int
	ContextUsed   int // last prompt size in tokens
	Usage         core.Usage
	Cost          float64
	CostKnown     bool // false when the model is not in the catalog
	SessionID     string
	Running       bool
	Queued        int
	Bypass        bool
}

// ContextPct is the context-window utilisation in [0, 1], or 0 when unknown.
func (s Status) ContextPct() float64 {
	if s.ContextWindow <= 0 {
		return 0
	}
	return float64(s.ContextUsed) / float64(s.ContextWindow)
}
