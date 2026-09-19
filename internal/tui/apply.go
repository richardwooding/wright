package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/tui/diffview"
	"github.com/richardwooding/wright/internal/tui/overlay"
	"github.com/richardwooding/wright/internal/tui/transcript"
)

// onEvent applies one engine event, re-arms the pump and either coalesces
// (text deltas) or refreshes immediately (everything else).
func (m Model) onEvent(ev engine.Event) (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{m.waitEvent()}
	switch ev.Kind {
	case engine.KindText, engine.KindReasoning:
		m.applyDelta(ev)
		if !m.flushArmed {
			m.flushArmed = true
			cmds = append(cmds, tea.Tick(flushInterval, func(time.Time) tea.Msg { return flushMsg{} }))
		}
		return m, tea.Batch(cmds...)
	case engine.KindRunStarted:
		cmds = append(cmds, m.applyRunStarted())
	default:
		m.applyEvent(ev)
	}
	m.layout()
	m.refresh()
	return m, tea.Batch(cmds...)
}

// applyDelta appends streamed text to the live assistant block.
func (m *Model) applyDelta(ev engine.Event) {
	if m.live == nil {
		m.live = &transcript.Assistant{Live: true}
		m.tr.Append(m.live)
	}
	if ev.Kind == engine.KindReasoning {
		m.live.Reasoning += ev.Text
	} else {
		m.live.Text += ev.Text
	}
	m.tr.Invalidate(m.live)
}

// applyRunStarted opens a run: a fresh live block and a spinner.
func (m *Model) applyRunStarted() tea.Cmd {
	m.running = true
	m.pending = nil
	m.follow = true
	m.finishLive()
	m.live = &transcript.Assistant{Live: true}
	m.tr.Append(m.live)
	m.status = m.ctl.Status()
	return m.spin.Tick
}

// finishLive freezes the live block so its render is cached from now on.
func (m *Model) finishLive() {
	if m.live == nil {
		return
	}
	m.live.Live = false
	m.tr.Invalidate(m.live)
	m.live = nil
}

// applyEvent maps every non-streaming kind to a transcript change.
func (m *Model) applyEvent(ev engine.Event) {
	switch ev.Kind {
	case engine.KindToolCall:
		m.applyToolCall(ev)
	case engine.KindToolProgress:
		m.applyToolProgress(ev)
	case engine.KindToolResult:
		m.applyToolResult(ev)
	case engine.KindApprovalRequest:
		m.applyApprovalRequest(ev)
	case engine.KindApprovalDecided:
		m.applyApprovalDecided(ev)
	case engine.KindQuestion:
		m.applyQuestion(ev)
	case engine.KindRunFinished:
		m.applyRunFinished(ev)
	default:
		m.applyNotice(ev)
	}
}

// applyNotice covers the kinds that only add a line or update state.
func (m *Model) applyNotice(ev engine.Event) {
	switch ev.Kind {
	case engine.KindRetry:
		m.notice(fmt.Sprintf("retry %d in %s: %v", ev.Attempt, ev.Delay, ev.Err), transcript.LevelWarn)
	case engine.KindCompact:
		if c := ev.Compact; c != nil {
			m.notice(fmt.Sprintf("compacted context (%s): %s → %s tokens", c.Reason, formatTokens(c.Before), formatTokens(c.After)), transcript.LevelInfo)
		}
	case engine.KindUsage:
		m.status = m.ctl.Status()
	case engine.KindRedacted:
		m.badge("redacted")
		m.notice("redacted a secret in tool output: "+ev.Text, transcript.LevelInfo)
	case engine.KindInjection:
		m.badge("injection")
		m.notice("possible prompt injection in tool output: "+ev.Text, transcript.LevelWarn)
	case engine.KindTodos:
		m.todos = ev.Todos
		if _, open := m.ov.(*overlay.Todos); open {
			m.ov = overlay.NewTodos(m.todos, m.th)
		}
	case engine.KindQueued:
		m.queued = ev.Queued
		m.pending = nil
	case engine.KindError:
		m.tr.Append(&transcript.Error{Text: errText(ev)})
	case engine.KindNotice:
		m.notice(ev.Text, transcript.LevelInfo)
	case engine.KindStep:
	}
}

// badge marks the most recent tool card so the warning is visible where the
// output is, not only in a notice further down.
func (m *Model) badge(b string) {
	if m.lastCard == nil {
		return
	}
	m.lastCard.Badges = append(m.lastCard.Badges, b)
	m.tr.Invalidate(m.lastCard)
}

func (m *Model) applyToolCall(ev engine.Event) {
	if ev.Call == nil {
		return
	}
	m.finishLive() // text after the call goes in a new block, below the card
	card := &transcript.ToolCard{
		ID: ev.Call.ID, Name: ev.Call.Name, Args: ev.Call.Arguments,
		Status: transcript.StatusRunning, Depth: ev.Depth, Expanded: m.toolsExpanded,
	}
	m.cards[card.ID] = card
	m.lastCard = card
	m.tr.Append(card)
}

func (m *Model) applyToolProgress(ev engine.Event) {
	card := m.card(ev)
	if card == nil {
		return
	}
	card.Progress = append(card.Progress, ev.Text)
	m.tr.Invalidate(card)
}

// applyToolResult finishes a card. A denial arrives as an error result whose
// text starts with "not approved"; it gets its own status so the transcript
// distinguishes "refused" from "failed".
func (m *Model) applyToolResult(ev engine.Event) {
	card := m.card(ev)
	if card == nil {
		return
	}
	text := ""
	isErr := ev.Err != nil
	if ev.Result != nil {
		text = ev.Result.Text()
		isErr = isErr || ev.Result.IsError
	}
	if text == "" && ev.Err != nil {
		text = ev.Err.Error()
	}
	switch {
	case isErr && strings.HasPrefix(text, "not approved"):
		card.Status = transcript.StatusDenied
	case isErr:
		card.Status = transcript.StatusError
	default:
		card.Status = transcript.StatusOK
	}
	card.Output = text
	if card.Diff == "" && diffview.IsDiff(text) {
		card.Diff = text
	}
	card.Duration = ev.Duration
	m.tr.Invalidate(card)
}

// card finds the card for an event, creating one for results that arrive
// without a call (forwarded sub-agent output).
func (m *Model) card(ev engine.Event) *transcript.ToolCard {
	if ev.Call == nil {
		return m.lastCard
	}
	if c, ok := m.cards[ev.Call.ID]; ok {
		return c
	}
	m.applyToolCall(ev)
	return m.cards[ev.Call.ID]
}

func (m *Model) applyApprovalRequest(ev engine.Event) {
	if ev.Approval == nil {
		return
	}
	a := *ev.Approval
	if ev.Call != nil {
		if c, ok := m.cards[ev.Call.ID]; ok && a.Preview.Diff != "" {
			c.Diff = a.Preview.Diff
			m.tr.Invalidate(c)
		}
	}
	ctl, id := m.ctl, a.ID
	m.ov = overlay.NewApproval(a, m.th, func(d engine.Decision) { ctl.Reply(id, d) })
	m.follow = true
}

func (m *Model) applyApprovalDecided(ev engine.Event) {
	if ev.Decision == nil {
		return
	}
	if ap, ok := m.ov.(*overlay.Approval); ok && ev.Approval != nil && ap.ID() == ev.Approval.ID {
		m.ov = nil
	}
	d := ev.Decision
	block := &transcript.Approval{Allowed: d.Allow, By: d.By, Reason: d.Reason}
	if ev.Approval != nil {
		block.Tool = ev.Approval.Tool
		block.Summary = ev.Approval.Preview.Title
	}
	if ev.Call != nil {
		if block.Tool == "" {
			block.Tool = ev.Call.Name
		}
		if c, ok := m.cards[ev.Call.ID]; ok {
			if block.Summary == "" {
				block.Summary = c.Summary()
			}
			if !d.Allow {
				c.Status = transcript.StatusDenied
				m.tr.Invalidate(c)
			}
		}
	}
	if d.Grant != nil {
		block.Rule = d.Grant.Rule.String()
		block.Scope = d.Grant.Scope.String()
	}
	m.tr.Append(block)
}

func (m *Model) applyQuestion(ev engine.Event) {
	if ev.Question == nil {
		return
	}
	ctl, id := m.ctl, ev.Question.ID
	m.ov = overlay.NewQuestion(*ev.Question, m.th, func(a engine.Answer) { ctl.Answer(id, a) })
}

// applyRunFinished closes the run and records the summary shown at exit.
func (m *Model) applyRunFinished(ev engine.Event) {
	m.running = false
	m.finishLive()
	m.status = m.ctl.Status()
	f := ev.Finish
	if f == nil {
		return
	}
	if f.Err != nil {
		m.tr.Append(&transcript.Error{Text: f.Err.Error()})
	}
	summary := fmt.Sprintf("steps %d · tool calls %d · %s tok · %s · %s",
		f.Steps, f.ToolCalls, formatTokens(f.Usage.TotalTokens), formatCost(f.Cost, m.status.CostKnown), f.Duration.Round(100*1e6))
	if f.StopReason != "" {
		summary += " · " + f.StopReason
	}
	m.exitSummary = summary
	m.notice(summary, transcript.LevelInfo)
}

// errText prefers the error, then the text, so an Error event never renders blank.
func errText(ev engine.Event) string {
	if ev.Err != nil {
		return ev.Err.Error()
	}
	if ev.Text != "" {
		return ev.Text
	}
	return "unknown error"
}
