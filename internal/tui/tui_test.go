package tui_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui"
)

// fakeController records every call the UI makes.
type fakeController struct {
	mu      sync.Mutex
	submits []string
	replies []reply
	answers []answer
	modes   []policy.Mode
	models  []string
	mode    policy.Mode
	status  engine.Status
	cancels int
	compact int
	undo    int
	events  chan engine.Event // when set, Submit and Reply script a run
	// onSubmit replaces script for a test that needs a different run.
	onSubmit func()
}

type reply struct {
	id string
	d  engine.Decision
}

type answer struct {
	id string
	a  engine.Answer
}

func (f *fakeController) Submit(text string, _ ...core.Part) error {
	f.mu.Lock()
	f.submits = append(f.submits, text)
	f.mu.Unlock()
	switch {
	case f.onSubmit != nil:
		f.onSubmit()
	case f.events != nil:
		f.script(text)
	}
	return nil
}

func (f *fakeController) Cancel() { f.mu.Lock(); f.cancels++; f.mu.Unlock() }

func (f *fakeController) Reply(id string, d engine.Decision) {
	f.mu.Lock()
	f.replies = append(f.replies, reply{id, d})
	f.mu.Unlock()
	if f.events != nil {
		f.events <- engine.Event{Kind: engine.KindApprovalDecided, Approval: &engine.Approval{ID: id, Tool: "bash"}, Call: call("c1", "bash", `{"command":"go test ./..."}`), Decision: &d}
		f.events <- engine.Event{Kind: engine.KindToolResult, Call: call("c1", "bash", ""), Result: &core.ToolResult{Content: []core.Part{core.Text("ok\n")}}, Duration: 1200 * time.Millisecond}
		f.events <- engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{Steps: 2, ToolCalls: 1, Duration: 2 * time.Second}}
	}
}

func (f *fakeController) Answer(id string, a engine.Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answer{id, a})
}

func (f *fakeController) SetMode(m policy.Mode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m == policy.ModeBypass {
		return policy.ErrBypassNotSettable
	}
	f.modes = append(f.modes, m)
	f.mode = m
	f.status.Mode = m
	return nil
}

func (f *fakeController) Mode() policy.Mode { f.mu.Lock(); defer f.mu.Unlock(); return f.mode }

func (f *fakeController) SetModel(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append(f.models, name)
	return nil
}

func (f *fakeController) Compact() error { f.mu.Lock(); f.compact++; f.mu.Unlock(); return nil }

func (f *fakeController) Undo() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undo++
	return []string{"a.go"}, nil
}

func (f *fakeController) Status() engine.Status { f.mu.Lock(); defer f.mu.Unlock(); return f.status }

func (f *fakeController) Models(context.Context) []tui.ModelChoice {
	return []tui.ModelChoice{{Name: "claude-opus", Provider: "anthropic", Known: true}, {Name: "gpt-5", Provider: "openai"}}
}

// script plays a run that asks for approval, for the plain and teatest tests.
func (f *fakeController) script(text string) {
	f.events <- engine.Event{Kind: engine.KindRunStarted}
	f.events <- engine.Event{Kind: engine.KindText, Text: "hello from model, you said " + text + "\n"}
	f.events <- engine.Event{Kind: engine.KindToolCall, Call: call("c1", "bash", `{"command":"go test ./..."}`)}
	f.events <- engine.Event{Kind: engine.KindApprovalRequest, Call: call("c1", "bash", ""), Approval: &engine.Approval{
		ID: "ap1", Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`),
		Preview: engine.Preview{Title: "go test ./...", Body: "go test ./..."}, Severity: engine.SeverityInfo,
	}}
}

// scriptWithOffers is script, plus the rules the policy engine would offer.
func (f *fakeController) scriptWithOffers(t *testing.T, texts ...string) {
	t.Helper()
	var offers []policy.GrantOffer
	for _, text := range texts {
		r, err := policy.ParseRule(text, policy.SourceSession)
		if err != nil {
			t.Fatal(err)
		}
		offers = append(offers, policy.GrantOffer{Rule: r, Scope: policy.ScopeSession, Label: "allow " + text})
	}
	f.events <- engine.Event{Kind: engine.KindRunStarted}
	f.events <- engine.Event{Kind: engine.KindApprovalRequest, Call: call("c1", "bash", ""), Approval: &engine.Approval{
		ID: "ap1", Tool: "bash", Args: json.RawMessage(`{"command":"fpc x.pas && ./bin/t"}`),
		Preview:  engine.Preview{Title: "fpc x.pas && ./bin/t", Body: "fpc x.pas && ./bin/t"},
		Severity: engine.SeverityInfo, Offers: offers,
	}}
}

// scriptWithHeld is scriptWithOffers plus rules the engine suppressed because
// the user already holds one covering that command.
func (f *fakeController) scriptWithHeld(t *testing.T, offers, held []string) {
	t.Helper()
	rule := func(text string) policy.Rule {
		t.Helper()
		r, err := policy.ParseRule(text, policy.SourceSession)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var os []policy.GrantOffer
	for _, text := range offers {
		os = append(os, policy.GrantOffer{Rule: rule(text), Scope: policy.ScopeSession, Label: "allow " + text})
	}
	var hs []policy.HeldRule
	for _, text := range held {
		hs = append(hs, policy.HeldRule{Rule: rule(text), By: rule(text)})
	}
	f.events <- engine.Event{Kind: engine.KindRunStarted}
	f.events <- engine.Event{Kind: engine.KindApprovalRequest, Call: call("c1", "bash", ""), Approval: &engine.Approval{
		ID: "ap1", Tool: "bash", Args: json.RawMessage(`{"command":"gofmt -l . && ./out"}`),
		Preview:  engine.Preview{Title: "gofmt -l . && ./out", Body: "gofmt -l . && ./out"},
		Severity: engine.SeverityInfo, Offers: os,
		Verdict: policy.Verdict{Held: hs, Uncovered: "command `./out`"},
	}}
}

func call(id, name, args string) *core.ToolCall {
	return &core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func newModel(t *testing.T, ctl *fakeController, width, height int) tui.Model {
	t.Helper()
	m := tui.New(ctl, tui.Options{WorkspaceRoot: "/ws/wright", Warnings: []string{"sandbox: bwrap unavailable"}})
	m = update(m, tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

// update applies one message and returns the concrete model.
func update(m tui.Model, msg tea.Msg) tui.Model {
	next, _ := m.Update(msg)
	return next.(tui.Model)
}

func updateCmd(m tui.Model, msg tea.Msg) (tui.Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(tui.Model), cmd
}

func event(m tui.Model, ev engine.Event) tui.Model {
	return update(m, wrapEvent(ev))
}

func content(m tui.Model) string { return ansi.Strip(m.View().Content) }

func wrapEvent(ev engine.Event) tea.Msg { return tui.EventMsg(ev) }

func flushMessage() tea.Msg { return tui.FlushMsg() }

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	case "ctrl+t":
		return tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}
	case "ctrl+j":
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	case "alt+m":
		return tea.KeyPressMsg{Code: 'm', Mod: tea.ModAlt}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "shift+up":
		return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
	case "shift+down":
		return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func typeText(m tui.Model, s string) tui.Model {
	for _, r := range s {
		m = update(m, key(string(r)))
	}
	return m
}

// runEvents plays a full run with a tool call into the model.
func runEvents(m tui.Model) tui.Model {
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	for _, t := range []string{"Hello ", "**world**", "! Done."} {
		m = event(m, engine.Event{Kind: engine.KindText, Text: t})
	}
	m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "bash", `{"command":"go test ./..."}`)})
	m = event(m, engine.Event{Kind: engine.KindToolResult, Call: call("c1", "bash", ""), Result: &core.ToolResult{Content: []core.Part{core.Text("ok\n")}}, Duration: 1200 * time.Millisecond})
	m = event(m, engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{Steps: 2, ToolCalls: 1, Usage: core.Usage{TotalTokens: 41234}, Duration: 3 * time.Second}})
	return m
}

func TestStreamedTextAndCardRender(t *testing.T) {
	ctl := &fakeController{status: engine.Status{Model: "claude-opus", Sandbox: "bwrap", SessionID: "s1", CostKnown: true, Cost: 0.38}}
	m := runEvents(newModel(t, ctl, 80, 30))
	out := content(m)
	for _, want := range []string{"Hello", "world", "Done.", "▸ bash", "go test ./...", "1.2s", "✓ ok", "steps 2", "41.2k tok", "sandbox: bwrap unavailable", "claude-opus", "$0.38", "bwrap ⊘net", "sess s1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in view:\n%s", want, out)
		}
	}
	if !strings.Contains(m.ExitSummary(), "steps 2") || !strings.Contains(m.ExitSummary(), "--resume s1") {
		t.Errorf("exit summary %q", m.ExitSummary())
	}
}

func TestTextDeltasCoalesceUntilFlush(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	m, cmd := updateCmd(m, wrapEvent(engine.Event{Kind: engine.KindText, Text: "streamed words"}))
	if cmd == nil {
		t.Fatal("no flush tick armed")
	}
	if strings.Contains(content(m), "streamed words") {
		t.Fatal("delta rendered before the flush tick")
	}
	m = update(m, flushMessage())
	if !strings.Contains(content(m), "streamed words") {
		t.Fatalf("delta missing after flush:\n%s", content(m))
	}
}

func TestApprovalOverlayAllowAndDeny(t *testing.T) {
	tests := []struct {
		name     string
		severity engine.Severity
		keys     []string
		allow    bool
	}{
		{"y allows", engine.SeverityCaution, []string{"y"}, true},
		{"n denies", engine.SeverityCaution, []string{"n"}, false},
		{"esc denies", engine.SeverityInfo, []string{"esc"}, false},
		{"enter on destructive denies", engine.SeverityDestructive, []string{"enter"}, false},
		{"enter on info allows", engine.SeverityInfo, []string{"enter"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := &fakeController{}
			m := newModel(t, ctl, 80, 30)
			m = event(m, engine.Event{Kind: engine.KindRunStarted})
			m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "edit_file", `{"path":"x.go"}`)})
			m = event(m, engine.Event{Kind: engine.KindApprovalRequest, Call: call("c1", "edit_file", ""), Approval: &engine.Approval{
				ID: "ap1", Tool: "edit_file", Preview: engine.Preview{Title: "x.go", Diff: "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n"}, Severity: tt.severity,
			}})
			if v := content(m); !strings.Contains(v, "[y] allow once") || !strings.Contains(v, "+new") {
				t.Fatalf("overlay not shown:\n%s", v)
			}
			for _, k := range tt.keys {
				m = update(m, key(k))
			}
			if len(ctl.replies) != 1 || ctl.replies[0].id != "ap1" || ctl.replies[0].d.Allow != tt.allow {
				t.Fatalf("replies = %+v, want allow=%v", ctl.replies, tt.allow)
			}
			if strings.Contains(content(m), "[y] allow once") {
				t.Fatal("overlay still open after the decision")
			}
			// The decided event records the outcome in the transcript.
			d := ctl.replies[0].d
			m = event(m, engine.Event{Kind: engine.KindApprovalDecided, Call: call("c1", "edit_file", ""), Approval: &engine.Approval{ID: "ap1", Tool: "edit_file"}, Decision: &d})
			want := "✓ allowed"
			if !tt.allow {
				want = "✗ denied"
			}
			if v := content(m); !strings.Contains(v, "? approval") || !strings.Contains(v, want) {
				t.Fatalf("approval block missing %q:\n%s", want, v)
			}
		})
	}
}

func TestQuestionOverlay(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 24)
	m = event(m, engine.Event{Kind: engine.KindQuestion, Question: &engine.QuestionEvent{ID: "q1", Text: "Pick one", Options: []string{"a", "b"}}})
	if !strings.Contains(content(m), "Pick one") {
		t.Fatal("question not shown")
	}
	m = update(m, key("2"))
	if len(ctl.answers) != 1 || ctl.answers[0].a.Index != 1 {
		t.Fatalf("answers = %+v", ctl.answers)
	}
	if strings.Contains(content(m), "Pick one") {
		t.Fatal("question still open")
	}
}

func TestShiftTabCyclesMode(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 24)
	m = update(m, key("shift+tab"))
	m = update(m, key("shift+tab"))
	m = update(m, key("shift+tab"))
	want := []policy.Mode{policy.ModeAutoEdit, policy.ModePlan, policy.ModeDefault}
	if len(ctl.modes) != 3 || ctl.modes[0] != want[0] || ctl.modes[1] != want[1] || ctl.modes[2] != want[2] {
		t.Fatalf("modes = %v, want %v", ctl.modes, want)
	}
	if !strings.Contains(content(m), "mode: auto-edit") {
		t.Fatal("mode change not announced")
	}
}

func TestCtrlCTwiceQuits(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	m, cmd := updateCmd(m, key("ctrl+c"))
	if cmd == nil {
		t.Fatal("first ctrl+c returned no hint timer")
	}
	if !strings.Contains(content(m), "press ctrl+c again") {
		t.Fatal("no hint after first ctrl+c")
	}
	_, cmd = updateCmd(m, key("ctrl+c"))
	if !isQuit(cmd) {
		t.Fatal("second ctrl+c did not quit")
	}
}

func TestCtrlDQuitsOnlyWhenEmpty(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	m = typeText(m, "x")
	if _, cmd := updateCmd(m, key("ctrl+d")); isQuit(cmd) {
		t.Fatal("ctrl+d quit with text in the box")
	}
	m = newModel(t, &fakeController{}, 80, 24)
	if _, cmd := updateCmd(m, key("ctrl+d")); !isQuit(cmd) {
		t.Fatal("ctrl+d did not quit on an empty box")
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestComposerEnterSubmits(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 24)
	m = typeText(m, "fix the bug")
	m = update(m, key("enter"))
	if len(ctl.submits) != 1 || ctl.submits[0] != "fix the bug" {
		t.Fatalf("submits = %v", ctl.submits)
	}
	if !strings.Contains(content(m), "› fix the bug") {
		t.Fatalf("user block missing:\n%s", content(m))
	}
}

func TestSubmitWhileRunningShowsQueuedStrip(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 24)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	m = typeText(m, "also do this")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "queued 1: also do this") {
		t.Fatalf("queued strip missing:\n%s", v)
	}
	m = event(m, engine.Event{Kind: engine.KindQueued, Queued: 1})
	if v := content(m); !strings.Contains(v, "queued 1") {
		t.Fatalf("engine count not shown:\n%s", v)
	}
	m = event(m, engine.Event{Kind: engine.KindQueued, Queued: 0})
	if v := content(m); strings.Contains(v, "queued") {
		t.Fatalf("strip still shown after the engine drained the inbox:\n%s", v)
	}
}

func TestEscCancelsRun(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 24)
	m = update(m, key("esc"))
	if ctl.cancels != 0 {
		t.Fatal("esc cancelled while idle")
	}
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	update(m, key("esc"))
	if ctl.cancels != 1 {
		t.Fatal("esc did not cancel the run")
	}
}

func TestSlashCommands(t *testing.T) {
	ctl := &fakeController{status: engine.Status{Sandbox: "none"}}
	m := newModel(t, ctl, 80, 30)

	m = typeText(m, "/mode plan")
	m = update(m, key("enter"))
	if len(ctl.modes) != 1 || ctl.modes[0] != policy.ModePlan {
		t.Fatalf("modes = %v", ctl.modes)
	}

	m = typeText(m, "/model")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "filter:") || !strings.Contains(v, "claude-opus") {
		t.Fatalf("picker not open:\n%s", v)
	}
	m = update(m, key("esc"))

	m = typeText(m, "/nonsense")
	m = update(m, key("enter"))
	if !strings.Contains(content(m), "unknown command /nonsense") {
		t.Fatal("unknown command not reported")
	}

	m = typeText(m, "/undo")
	m = update(m, key("enter"))
	if ctl.undo != 1 || !strings.Contains(content(m), "reverted 1 file") {
		t.Fatal("undo not run")
	}

	m = typeText(m, "/compact")
	m = update(m, key("enter"))
	if ctl.compact != 1 {
		t.Fatal("compact not run")
	}

	m = typeText(m, "/help")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "/clear") || !strings.Contains(v, "shift+tab") {
		t.Fatalf("help missing entries:\n%s", v)
	}
	m = update(m, key("esc"))

	m = typeText(m, "/mode bypass")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "type yes to confirm") {
		t.Fatalf("bypass confirmation missing:\n%s", v)
	}
	m = typeText(m, "yes")
	m, cmd := updateCmd(m, key("enter"))
	if cmd == nil {
		t.Fatal("confirm produced no command")
	}
	m = update(m, cmd())
	if !strings.Contains(content(m), policy.ErrBypassNotSettable.Error()) {
		t.Fatalf("bypass refusal not shown:\n%s", content(m))
	}

	m = typeText(m, "/mcp")
	m = update(m, key("enter"))
	if !strings.Contains(content(m), "/mcp is not available") {
		t.Fatal("unwired hook not reported")
	}
}

func TestCommandHookResultShownAsNotice(t *testing.T) {
	ctl := &fakeController{}
	m := tui.New(ctl, tui.Options{Command: func(_ context.Context, name string, args []string) (string, error) {
		if name == "audit" {
			return "42 events, chain ok", nil
		}
		return "", errors.New("nope")
	}})
	m = update(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = typeText(m, "/audit")
	m, cmd := updateCmd(m, key("enter"))
	if cmd == nil {
		t.Fatal("hook produced no command")
	}
	m = update(m, cmd())
	if !strings.Contains(content(m), "42 events, chain ok") {
		t.Fatalf("hook result missing:\n%s", content(m))
	}
	m = typeText(m, "/trust")
	m, cmd = updateCmd(m, key("enter"))
	m = update(m, cmd())
	if !strings.Contains(content(m), "/trust: nope") {
		t.Fatalf("hook error missing:\n%s", content(m))
	}
}

func TestStatusBarHonesty(t *testing.T) {
	ctl := &fakeController{status: engine.Status{Model: "local-llm", Sandbox: "none", Bypass: true, CostKnown: false, ContextWindow: 100, ContextUsed: 23}}
	m := newModel(t, ctl, 100, 24)
	v := content(m)
	for _, want := range []string{"sandbox off", "⛔ bypass", " — ", "ctx 23%"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in status bar:\n%s", want, v)
		}
	}
}

func TestCtrlOExpandsCards(t *testing.T) {
	m := runEvents(newModel(t, &fakeController{}, 80, 30))
	if strings.Contains(content(m), "▾ bash") {
		t.Fatal("card expanded by default")
	}
	m = update(m, key("ctrl+o"))
	if v := content(m); !strings.Contains(v, "▾ bash") || !strings.Contains(v, `"command": "go test ./..."`) {
		t.Fatalf("card not expanded:\n%s", v)
	}
}

func TestCtrlTOpensTodos(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	m = event(m, engine.Event{Kind: engine.KindTodos, Todos: []engine.Todo{{Content: "write the TUI", Status: "in_progress"}}})
	m = update(m, key("ctrl+t"))
	if v := content(m); !strings.Contains(v, "write the TUI") || !strings.Contains(v, "● doing") {
		t.Fatalf("todos overlay:\n%s", v)
	}
}

func TestNoticesForEveryKind(t *testing.T) {
	m := newModel(t, &fakeController{}, 100, 40)
	m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "read_file", `{"path":"secrets.env"}`)})
	events := []engine.Event{
		{Kind: engine.KindRetry, Attempt: 2, Delay: time.Second, Err: errors.New("429")},
		{Kind: engine.KindCompact, Compact: &engine.CompactInfo{Reason: "manual", Before: 40000, After: 12000}},
		{Kind: engine.KindRedacted, Text: "aws_access_key…AB12"},
		{Kind: engine.KindInjection, Text: "ignore previous instructions"},
		{Kind: engine.KindError, Err: errors.New("provider down")},
		{Kind: engine.KindNotice, Text: "model switched"},
		{Kind: engine.KindToolResult, Call: call("c1", "read_file", ""), Result: &core.ToolResult{IsError: true, Content: []core.Part{core.Text("not approved: secret file")}}},
	}
	for _, ev := range events {
		m = event(m, ev)
	}
	v := content(m)
	for _, want := range []string{"retry 2", "40.0k → 12.0k", "redacted a secret", "prompt injection", "✗ provider down", "ℹ model switched", "✗ denied", "⚠ injection", "⚠ redacted"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
}

func TestNoLineWiderThanTerminal(t *testing.T) {
	for _, width := range []int{40, 60, 100, 200} {
		ctl := &fakeController{status: engine.Status{Model: "claude-opus-4-1-20250805", Sandbox: "bwrap", SessionID: "20260919-153012-a1b2", CostKnown: true, Cost: 1.2345, ContextWindow: 200000, ContextUsed: 50000}}
		m := runEvents(newModel(t, ctl, width, 24))
		m = update(m, key("ctrl+o"))
		m = typeText(m, strings.Repeat("long input ", 12))
		check := func(stage string) {
			t.Helper()
			for line := range strings.SplitSeq(m.View().Content, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width %d %s: %d-wide line %q", width, stage, w, ansi.Strip(line))
				}
			}
		}
		check("transcript")
		m = event(m, engine.Event{Kind: engine.KindApprovalRequest, Approval: &engine.Approval{
			ID: "ap1", Tool: "bash", Preview: engine.Preview{Title: strings.Repeat("title ", 20), Body: strings.Repeat("x", 300)},
		}})
		check("overlay")
	}
}

func TestResizeRerendersAtNewWidth(t *testing.T) {
	m := newModel(t, &fakeController{}, 120, 24)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	m = event(m, engine.Event{Kind: engine.KindText, Text: strings.Repeat("wide words ", 10)})
	m = update(m, flushMessage())
	m = event(m, engine.Event{Kind: engine.KindRunFinished})
	words := strings.Count(content(m), "wide")
	m = update(m, tea.WindowSizeMsg{Width: 40, Height: 24})
	v := content(m)
	if strings.Count(v, "wide") != words {
		t.Fatalf("text lost on resize:\n%s", v)
	}
	for line := range strings.SplitSeq(m.View().Content, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Fatalf("cached wide line survived the resize: %q", ansi.Strip(line))
		}
	}
}

func TestBackgroundColorSwitchesTheme(t *testing.T) {
	m := runEvents(newModel(t, &fakeController{}, 80, 24))
	m = update(m, tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	if !strings.Contains(content(m), "Hello") {
		t.Fatal("transcript lost on theme change")
	}
}

func TestViewMetadata(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	v := m.View()
	if !v.AltScreen {
		t.Error("alt screen not requested")
	}
	if v.WindowTitle != "wright — wright" {
		t.Errorf("title %q", v.WindowTitle)
	}
	if v.Cursor == nil {
		t.Error("no cursor for the composer")
	}
	m = update(m, key("ctrl+t"))
	if m.View().Cursor != nil {
		t.Error("cursor shown under an overlay")
	}
}

// ruleRows is the indices of the full-width divider rows in a rendered view.
func ruleRows(lines []string, width int) []int {
	rule := strings.Repeat("─", width)
	var out []int
	for i, line := range lines {
		if line == rule {
			out = append(out, i)
		}
	}
	return out
}

func TestRulesSurroundTheComposer(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		m := newModel(t, &fakeController{}, width, 24)
		lines := strings.Split(content(m), "\n")
		rows := ruleRows(lines, width)
		if len(rows) != 2 {
			t.Fatalf("width %d: %d rule rows, want one above and one below the composer", width, len(rows))
		}
		box := strings.Join(lines[rows[0]+1:rows[1]], "\n")
		if !strings.Contains(box, "Ask wright…") {
			t.Errorf("width %d: composer is not between the rules:\n%s", width, box)
		}
		if rows[1] != len(lines)-2 {
			t.Errorf("width %d: lower rule at %d, want it just above the status bar (%d rows)", width, rows[1], len(lines))
		}
		// The cursor must land in the composer, not on the rule above it.
		if c := m.View().Cursor; c == nil || c.Y <= rows[0] || c.Y >= rows[1] {
			t.Errorf("width %d: cursor %v outside the rules at %v", width, c, rows)
		}
	}
}

// TestViewFitsTerminalHeight pins the row budget: transcript + queued strip
// + two rules + composer + status bar is exactly the terminal's height. The
// heights start at 8 because viewportHeight clamps the transcript to one row
// below that and the chrome then overflows by design.
func TestViewFitsTerminalHeight(t *testing.T) {
	stages := []struct {
		name string
		prep func(tui.Model) tui.Model
	}{
		{"idle", func(m tui.Model) tui.Model { return m }},
		{"transcript", runEvents},
		{"queued strip", func(m tui.Model) tui.Model {
			m = event(m, engine.Event{Kind: engine.KindRunStarted})
			return event(m, engine.Event{Kind: engine.KindQueued, Queued: 2, Text: "and then this"})
		}},
		{"overlay", func(m tui.Model) tui.Model { return update(m, key("ctrl+t")) }},
		{"overlay over the strip", func(m tui.Model) tui.Model {
			m = event(m, engine.Event{Kind: engine.KindRunStarted})
			m = event(m, engine.Event{Kind: engine.KindQueued, Queued: 1, Text: "later"})
			return update(m, key("ctrl+t"))
		}},
		{"multi-line composer", func(m tui.Model) tui.Model {
			m = typeText(m, "one")
			m = update(m, key("ctrl+j"))
			return typeText(m, "two")
		}},
		// A collapsed card is no longer always one row: a small edit shows a
		// short diff and a failure shows why. The viewport still has to clamp.
		{"collapsed cards with diffs", func(m tui.Model) tui.Model {
			m = event(m, engine.Event{Kind: engine.KindRunStarted})
			for i := range 12 {
				id := "e" + strconv.Itoa(i)
				m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call(id, "edit_file", `{"path":"a.go"}`)})
				m = event(m, engine.Event{
					Kind: engine.KindToolResult, Call: call(id, "edit_file", ""),
					Result:  &core.ToolResult{Content: []core.Part{core.Text("Edited a.go: replaced 1 occurrence at line 3")}},
					Preview: &engine.Preview{Diff: "--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n ctx\n-old\n+new\n+more\n"},
				})
			}
			return m
		}},
		{"collapsed failures", func(m tui.Model) tui.Model {
			m = event(m, engine.Event{Kind: engine.KindRunStarted})
			for i := range 10 {
				id := "b" + strconv.Itoa(i)
				m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call(id, "bash", `{"command":"go build ./..."}`)})
				m = event(m, engine.Event{
					Kind: engine.KindToolResult, Call: call(id, "bash", ""),
					Result: &core.ToolResult{IsError: true, Content: []core.Part{core.Text("x.go:1:1: a\nx.go:2:1: b\nx.go:3:1: c\n[exit code 2, 1s]")}},
				})
			}
			return m
		}},
	}
	for _, height := range []int{8, 12, 24, 50} {
		for _, st := range stages {
			t.Run(st.name+"/h"+strconv.Itoa(height), func(t *testing.T) {
				m := st.prep(newModel(t, &fakeController{}, 80, height))
				if rows := strings.Count(m.View().Content, "\n") + 1; rows != height {
					t.Fatalf("%d rows in a %d-row terminal:\n%s", rows, height, content(m))
				}
			})
		}
	}
}

func TestWindowTitleSaysWhenWorking(t *testing.T) {
	// Before the first WindowSizeMsg the view is a placeholder, but the
	// window still deserves a name.
	if title := tui.New(&fakeController{}, tui.Options{WorkspaceRoot: "/ws/wright"}).View().WindowTitle; title != "wright — wright" {
		t.Errorf("title before the first resize = %q", title)
	}
	m := newModel(t, &fakeController{}, 80, 24)
	if title := m.View().WindowTitle; title != "wright — wright" {
		t.Errorf("idle title = %q", title)
	}
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	running := m.View().WindowTitle
	if !strings.HasSuffix(running, " wright — wright") || running == "wright — wright" {
		t.Errorf("running title = %q, want a mark before the name", running)
	}
	if !slices.ContainsFunc(theme.MoonFrames, func(f string) bool { return strings.HasPrefix(running, f) }) {
		t.Errorf("running title = %q, want one of the animation frames", running)
	}
	m = event(m, engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{Steps: 1}})
	if title := m.View().WindowTitle; title != "wright — wright" {
		t.Errorf("title after the run = %q", title)
	}
}

// TestWindowTitleAnimatesWhileWorking is the complaint this answers: the
// title said "running" with a dot that never moved, because nothing
// recomputed it between state changes. It rides the spinner's own tick, so
// there is no second timer to keep in step.
func TestWindowTitleAnimatesWhileWorking(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})

	seen := map[string]bool{m.View().WindowTitle: true}
	for range 40 {
		m = update(m, spinner.TickMsg{})
		seen[m.View().WindowTitle] = true
	}
	if len(seen) < 3 {
		t.Errorf("the title took %d distinct values over 40 ticks, want it to animate: %v", len(seen), seen)
	}

	// One frame must last several ticks: a title is an escape sequence
	// written to the terminal, and 12 a second is wasteful for something
	// the eye reads as movement at a third of that.
	m2 := newModel(t, &fakeController{}, 80, 24)
	m2 = event(m2, engine.Event{Kind: engine.KindRunStarted})
	before := m2.View().WindowTitle
	m2 = update(m2, spinner.TickMsg{})
	if m2.View().WindowTitle != before {
		t.Errorf("the title advanced on a single tick (%q → %q); it should be throttled", before, m2.View().WindowTitle)
	}

	// Idle is still a plain name: a wright that is not working must not
	// decorate the user's tab list.
	m = event(m, engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{Steps: 1}})
	idle := m.View().WindowTitle
	for range 5 {
		m = update(m, spinner.TickMsg{})
		if got := m.View().WindowTitle; got != idle {
			t.Errorf("an idle title changed: %q → %q", idle, got)
		}
	}
}

func TestMouseOffByDefaultAndToggles(t *testing.T) {
	m := newModel(t, &fakeController{}, 80, 24)
	if mode := m.View().MouseMode; mode != tea.MouseModeNone {
		t.Fatalf("mouse mode %v at start: tracking the mouse disables the terminal's own selection", mode)
	}
	m = update(m, key("alt+m"))
	if mode := m.View().MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("alt+m left the mouse mode at %v", mode)
	}
	if v := content(m); !strings.Contains(v, "mouse on") {
		t.Errorf("no notice for the toggle:\n%s", v)
	}
	m = update(m, key("alt+m"))
	if mode := m.View().MouseMode; mode != tea.MouseModeNone {
		t.Fatalf("alt+m did not turn the mouse back off: %v", mode)
	}
	m = typeText(m, "/mouse")
	m = update(m, key("enter"))
	if mode := m.View().MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("/mouse left the mouse mode at %v", mode)
	}
}

// TestShiftArrowsScrollByLine covers the keyboard replacement for the wheel:
// the mouse is off by default, and pgup/pgdn only move whole pages.
func TestShiftArrowsScrollByLine(t *testing.T) {
	const width = 60
	m := newModel(t, &fakeController{}, width, 12)
	for i := range 40 {
		m = event(m, engine.Event{Kind: engine.KindNotice, Text: "notice " + strconv.Itoa(i)})
	}
	// Everything above the upper rule is the transcript; scrolling a line
	// shifts every row of it.
	transcript := func(m tui.Model) string {
		lines := strings.Split(content(m), "\n")
		rows := ruleRows(lines, width)
		if len(rows) == 0 {
			t.Fatal("no rule row to find the transcript by")
		}
		return strings.Join(lines[:rows[0]], "\n")
	}
	bottom := transcript(m)
	m = update(m, key("shift+up"))
	up := transcript(m)
	if up == bottom {
		t.Fatalf("shift+up did not scroll:\n%s", bottom)
	}
	m = update(m, key("shift+up"))
	if transcript(m) == up {
		t.Fatal("the second shift+up did not scroll")
	}
	m = update(m, key("shift+down"))
	if got := transcript(m); got != up {
		t.Fatalf("shift+down did not go one line back:\n%s\nwant:\n%s", got, up)
	}
	m = update(m, key("shift+down"))
	if got := transcript(m); got != bottom {
		t.Fatalf("not back at the tail:\n%s\nwant:\n%s", got, bottom)
	}
}

// promptWriter collects output and closes seen once marker has been printed,
// so a scripted stdin can answer a prompt only after it has appeared.
type promptWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	marker string
	seen   chan struct{}
	done   bool
}

func (w *promptWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if !w.done && strings.Contains(w.buf.String(), w.marker) {
		w.done = true
		close(w.seen)
	}
	return len(p), nil
}

func (w *promptWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

// afterReader delays its reads until ch is closed.
type afterReader struct {
	ch <-chan struct{}
	r  io.Reader
}

func (a afterReader) Read(p []byte) (int, error) { <-a.ch; return a.r.Read(p) }

func TestPlainMode(t *testing.T) {
	events := make(chan engine.Event, 32)
	ctl := &fakeController{events: events}
	out := &promptWriter{marker: "n) deny", seen: make(chan struct{})}
	in := io.MultiReader(strings.NewReader("hello\n"), afterReader{ch: out.seen, r: strings.NewReader("1\n")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary, err := tui.Run(ctx, ctl, tui.Options{Plain: true, Events: events, In: in, Out: out, Warnings: []string{"sandbox off"}})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"[warning] sandbox off", "wright> ", "hello from model, you said hello", "[tool] bash: go test ./...", "? approval (info) bash: go test ./...", "1) allow once", "n) deny", "[tool] bash → ok (1.2s)", "[done] steps 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in plain output:\n%s", want, got)
		}
	}
	if len(ctl.replies) != 1 || !ctl.replies[0].d.Allow {
		t.Fatalf("replies = %+v", ctl.replies)
	}
	if !strings.Contains(summary, "steps 2") {
		t.Errorf("summary %q", summary)
	}
}

func TestPlainModeDeniesOnEOF(t *testing.T) {
	events := make(chan engine.Event, 32)
	ctl := &fakeController{events: events}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tui.Run(ctx, ctl, tui.Options{Plain: true, Events: events, In: strings.NewReader("go\n"), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if len(ctl.replies) != 1 || ctl.replies[0].d.Allow {
		t.Fatalf("EOF did not deny: %+v", ctl.replies)
	}
}

// TestConcurrentApprovalsAreAllAnswered is the hang a user hit: the agent
// runs tools in parallel, so one step can raise several approvals at once,
// and the overlay was a single field that each new request overwrote. The
// replaced request was never shown and never answered, so the tool call
// waiting on it blocked its step forever — the run stopped dead with no
// error, no prompt and nothing in the transcript, because the step never
// completed and so was never persisted.
func TestConcurrentApprovalsAreAllAnswered(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 30)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	for _, id := range []string{"ap1", "ap2", "ap3"} {
		m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call(id, "bash", `{"command":"echo"}`)})
		m = event(m, engine.Event{Kind: engine.KindApprovalRequest, Call: call(id, "bash", ""), Approval: &engine.Approval{
			ID: id, Tool: "bash", Preview: engine.Preview{Title: "echo " + id},
		}})
	}
	// Answer whatever is on screen until nothing is left to answer.
	for range 10 {
		if !strings.Contains(content(m), "[y] allow once") {
			break
		}
		m = update(m, key("y"))
	}
	if len(ctl.replies) != 3 {
		t.Fatalf("answered %d of 3 approvals: %+v — the unanswered tool calls block their step forever", len(ctl.replies), ctl.replies)
	}
	got := map[string]bool{}
	for _, r := range ctl.replies {
		got[r.id] = true
	}
	for _, id := range []string{"ap1", "ap2", "ap3"} {
		if !got[id] {
			t.Errorf("approval %s was never answered", id)
		}
	}
}

// TestRunEndDropsItsPrompts pins that prompts belonging to a finished run go
// away with it. The engine abandons its pending approvals when a run ends, so
// one still on screen asks about a call that is already over and an answer to
// it reaches nobody — while the user's own overlays are theirs to close.
func TestRunEndDropsItsPrompts(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 80, 30)
	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	for _, id := range []string{"ap1", "ap2"} {
		m = event(m, engine.Event{Kind: engine.KindApprovalRequest, Call: call(id, "bash", ""), Approval: &engine.Approval{
			ID: id, Tool: "bash", Preview: engine.Preview{Title: "echo " + id},
		}})
	}
	m = event(m, engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{StopReason: "cancelled"}})
	if v := content(m); strings.Contains(v, "[y] allow once") {
		t.Fatalf("an approval for a finished run is still on screen:\n%s", v)
	}
	// Typing must not answer a prompt that is gone.
	update(m, key("y"))
	if len(ctl.replies) != 0 {
		t.Errorf("replied to a finished run's approval: %+v", ctl.replies)
	}
}

// TestPsSaysWhatIsRunning covers the question a session that has gone quiet
// raises. A tool card spins but never says for how long, and a run waiting on
// the model looks exactly like a run waiting on a command — which is how a
// stalled session got read as a frozen bash process.
func TestPsSaysWhatIsRunning(t *testing.T) {
	ctl := &fakeController{}
	m := newModel(t, ctl, 100, 40)

	m = update(m, key("/"))
	m = typeText(m, "ps")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "Not running") {
		t.Fatalf("/ps before a run:\n%s", v)
	}

	m = event(m, engine.Event{Kind: engine.KindRunStarted})
	m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "bash", `{"command":"sleep 600"}`)})
	m = update(m, key("/"))
	m = typeText(m, "ps")
	m = update(m, key("enter"))
	v := content(m)
	if !strings.Contains(v, "bash") || !strings.Contains(v, "sleep 600") {
		t.Errorf("/ps does not name the running call:\n%s", v)
	}
	if !strings.Contains(v, "in flight") {
		t.Errorf("/ps does not say a call is in flight:\n%s", v)
	}

	// With the call finished, the run is waiting on the model — the state
	// most easily mistaken for a stuck command.
	m = event(m, engine.Event{Kind: engine.KindToolResult, Call: call("c1", "bash", ""), Result: &core.ToolResult{Content: []core.Part{core.Text("done")}}})
	m = update(m, key("/"))
	m = typeText(m, "ps")
	m = update(m, key("enter"))
	if v := content(m); !strings.Contains(v, "waiting for the model") {
		t.Errorf("/ps after the call returned:\n%s", v)
	}
}

// TestPlainModeAcceptsSeveralRules is plain mode's half of the multi-select
// grants page: a script needs a rule per command, and answering one prompt
// per rule means being asked again on the very next call.
func TestPlainModeAcceptsSeveralRules(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   []string // rule texts, nil when the answer must deny
	}{
		{name: "one number", answer: "2", want: []string{"bash(fpc *)"}},
		{name: "several numbers", answer: "2,3", want: []string{"bash(fpc *)", "bash(./bin/t *)"}},
		{name: "spaces are tolerated", answer: "3, 2", want: []string{"bash(./bin/t *)", "bash(fpc *)"}},
		// A typo must never allow, and applying the half that parsed would
		// save a rule the user did not mean.
		{name: "a bad number in the list denies", answer: "2,9"},
		{name: "nonsense denies", answer: "2,x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := make(chan engine.Event, 32)
			ctl := &fakeController{events: events}
			out := &promptWriter{marker: "n) deny", seen: make(chan struct{})}
			in := io.MultiReader(strings.NewReader("hello\n"), afterReader{ch: out.seen, r: strings.NewReader(tt.answer + "\n")})
			ctl.onSubmit = func() { ctl.scriptWithOffers(t, "bash(fpc *)", "bash(./bin/t *)") }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := tui.Run(ctx, ctl, tui.Options{Plain: true, Events: events, In: in, Out: out}); err != nil {
				t.Fatal(err)
			}
			if len(ctl.replies) != 1 {
				t.Fatalf("replies = %+v", ctl.replies)
			}
			d := ctl.replies[0].d
			if tt.want == nil {
				if d.Allow || len(d.Grants) != 0 {
					t.Fatalf("%q allowed: %+v", tt.answer, d)
				}
				return
			}
			var got []string
			for _, g := range d.Grants {
				got = append(got, g.Rule.String())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("grants = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStatusBarNamesTheDebugEndpoint pins the discoverability half of the
// diagnostics endpoint. A user who started one while chasing a hang should
// not have to remember the address, or remember that /debug exists, to find
// it — and a process that is listening should say so on every frame.
func TestStatusBarNamesTheDebugEndpoint(t *testing.T) {
	ctl := &fakeController{}
	m := tui.New(ctl, tui.Options{WorkspaceRoot: "/ws/wright", DebugAddr: "127.0.0.1:6060"})
	m = update(m, tea.WindowSizeMsg{Width: 200, Height: 30})
	if got := content(m); !strings.Contains(got, "debug 127.0.0.1:6060") {
		t.Errorf("the status bar does not name the endpoint:\n%s", got)
	}

	// With no endpoint there is nothing to say, and the bar is too narrow to
	// spend a segment on a state that is off.
	off := newModel(t, &fakeController{}, 200, 30)
	if got := content(off); strings.Contains(got, "debug ") {
		t.Errorf("a debug segment appears with no endpoint running:\n%s", got)
	}
}

// flatten makes an overlay's rendered text searchable: the frame's border
// characters sit between the words of a wrapped sentence, so they have to go
// before the line breaks are collapsed.
func flatten(view string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if strings.ContainsRune("│─╭╮╰╯", r) {
			return ' '
		}
		return r
	}, view)), " ")
}

// TestGitHubCommandAsksBeforeHandingOverACredential pins the ceremony.
// /github on hands a live credential to a model-driven process, so it asks
// the way entering bypass mode does; status and /github off widen nothing and
// go straight through.
func TestGitHubCommandAsksBeforeHandingOverACredential(t *testing.T) {
	tests := []struct {
		name     string
		typed    string
		wantAsk  bool
		wantCall bool // the hook ran without any further input
	}{
		{name: "status goes straight through", typed: "/github", wantCall: true},
		{name: "off goes straight through", typed: "/github off", wantCall: true},
		{name: "on asks first", typed: "/github on", wantAsk: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called []string
			ctl := &fakeController{}
			m := tui.New(ctl, tui.Options{
				WorkspaceRoot: "/ws",
				Command: func(_ context.Context, name string, args []string) (string, error) {
					called = append(called, name+" "+strings.Join(args, " "))
					return "ok", nil
				},
			})
			m = update(m, tea.WindowSizeMsg{Width: 100, Height: 30})
			m = typeText(m, tt.typed)
			m, cmd := updateCmd(m, key("enter"))
			if cmd != nil {
				cmd()
			}
			view := content(m)
			asked := strings.Contains(view, "authenticate to GitHub as you")
			if asked != tt.wantAsk {
				t.Fatalf("asked = %v, want %v:\n%s", asked, tt.wantAsk, view)
			}
			if got := len(called) > 0; got != tt.wantCall {
				t.Fatalf("hook ran = %v, want %v (%v)", got, tt.wantCall, called)
			}
			if !tt.wantAsk {
				return
			}
			// The prompt has to say what is being handed over before the
			// word is typed. The overlay wraps to the terminal, so compare
			// against the text with its line breaks flattened.
			flat := flatten(view)
			for _, want := range []string{"act as you", "network access", "/github off"} {
				if !strings.Contains(flat, want) {
					t.Errorf("the prompt does not mention %q:\n%s", want, view)
				}
			}
			// Anything but the word cancels.
			wrong := typeText(m, "no")
			if _, c := updateCmd(wrong, key("enter")); c != nil {
				c()
			}
			if len(called) != 0 {
				t.Errorf("a wrong word turned it on: %v", called)
			}
			// The typed word is not the end of it: the scope picker comes
			// next, and only choosing there runs the command.
			right := typeText(m, "yes")
			right, c := updateCmd(right, key("enter"))
			if c != nil {
				if msg := c(); msg != nil {
					right = update(right, msg)
				}
			}
			if len(called) != 0 {
				t.Fatalf("the command ran before a scope was chosen: %v", called)
			}
			view = content(right)
			for _, want := range []string{"this session", "this project", "every project"} {
				if !strings.Contains(view, want) {
					t.Errorf("the scope picker does not offer %q:\n%s", want, view)
				}
			}
			right, c = updateCmd(right, key("enter")) // take the focused row
			if c != nil {
				if msg := c(); msg != nil {
					if _, c2 := updateCmd(right, msg); c2 != nil {
						c2()
					}
				}
			}
			if len(called) != 1 || !strings.HasPrefix(called[0], "github on ") {
				t.Errorf("the confirmed command was %v, want one carrying a scope", called)
			}
		})
	}
}

// TestScrollingTheTranscript covers the complaint directly: a full
// transcript that the user cannot get back to. The composer reports the
// intent at its edges and the root model moves the viewport.
func TestScrollingTheTranscript(t *testing.T) {
	fill := func(t *testing.T) tui.Model {
		t.Helper()
		m := newModel(t, &fakeController{}, 80, 12)
		m = event(m, engine.Event{Kind: engine.KindRunStarted})
		// Notices render one line each; streamed text would be reflowed
		// into a paragraph and fit on screen, leaving nothing to scroll.
		for i := range 60 {
			m = event(m, engine.Event{Kind: engine.KindNotice, Text: fmt.Sprintf("line %d", i)})
		}
		return event(m, engine.Event{Kind: engine.KindRunFinished, Finish: &engine.Finish{Steps: 1}})
	}

	t.Run("up scrolls back and end returns", func(t *testing.T) {
		m := fill(t)
		bottom := content(m)
		for range 5 {
			m = update(m, key("up"))
		}
		if got := content(m); got == bottom {
			t.Fatalf("up did not move the transcript:\n%s", got)
		}
		if !strings.Contains(content(m), "scrolled") {
			t.Errorf("no marker while scrolled back:\n%s", content(m))
		}
		m = update(m, key("end"))
		if content(m) != bottom {
			t.Errorf("end did not return to the bottom")
		}
		if strings.Contains(content(m), "↑ scrolled") {
			t.Errorf("the scrolled marker survived end")
		}
	})

	// The regression this also fixes: an approval arriving mid-run used to
	// snap the transcript back to the bottom, throwing away a scroll made
	// on purpose. The overlay is modal, so it is shown either way.
	t.Run("an approval does not yank the viewport back", func(t *testing.T) {
		m := fill(t)
		for range 5 {
			m = update(m, key("up"))
		}
		scrolled := content(m)
		m = event(m, engine.Event{Kind: engine.KindApprovalRequest, Call: call("c1", "bash", ""), Approval: &engine.Approval{
			ID: "a1", Tool: "bash", Preview: engine.Preview{Title: "x", Body: "x"},
		}})
		m = update(m, key("esc")) // answer it; the overlay closes
		if got := content(m); got != scrolled {
			t.Errorf("the approval re-pinned the transcript:\nwant %q\ngot  %q",
				lastLine(scrolled), lastLine(got))
		}
	})

	// A new run is the one thing that should return to following: the user
	// just asked for something and wants to watch it happen.
	t.Run("a new run follows again", func(t *testing.T) {
		m := fill(t)
		for range 5 {
			m = update(m, key("up"))
		}
		m = event(m, engine.Event{Kind: engine.KindRunStarted})
		if strings.Contains(content(m), "↑ scrolled") {
			t.Errorf("a new run did not resume following:\n%s", content(m))
		}
	})
}

// lastLine is the final non-empty line, for a readable failure message.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	return lines[len(lines)-1]
}

// expandCards opens every tool card, so a test can assert on a body rather
// than on the one-line collapsed form.
func expandCards(m tui.Model) tui.Model {
	return update(m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
}

// TestTranscriptHidesTheUntrustedFence asserts the reader sees the tool's
// output and not the envelope the model was handed. The fence is addressed to
// the model; the human already knows tool output is tool output.
func TestTranscriptHidesTheUntrustedFence(t *testing.T) {
	m := newModel(t, &fakeController{}, 100, 30)
	m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "bash", `{"command":"ls"}`)})
	m = event(m, engine.Event{
		Kind: engine.KindToolResult, Call: call("c1", "bash", ""),
		Result: &core.ToolResult{Content: []core.Part{core.Text(prompt.WrapUntrusted("bash", "a.go\nb.go"))}},
	})
	out := content(expandCards(m))
	if strings.Contains(out, "untrusted") {
		t.Errorf("the fence reached the screen:\n%s", out)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("the output did not:\n%s", out)
	}
}

// TestBashDiffOutputIsColoured is the second half of stripping the fence: the
// fence was the string's prefix, so diffview.IsDiff could never match a
// successful result and `git diff` through bash has never been coloured.
func TestBashDiffOutputIsColoured(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new\n"
	m := newModel(t, &fakeController{}, 100, 30)
	m = event(m, engine.Event{Kind: engine.KindToolCall, Call: call("c1", "bash", `{"command":"git diff"}`)})
	m = event(m, engine.Event{
		Kind: engine.KindToolResult, Call: call("c1", "bash", ""),
		Result: &core.ToolResult{Content: []core.Part{core.Text(prompt.WrapUntrusted("bash", diff))}},
	})
	out := content(expandCards(m))
	// +1 −1 in the headline is what only the diff path can produce.
	if !strings.Contains(out, "+1") || !strings.Contains(out, "−1") {
		t.Errorf("bash diff output was not recognised as a diff:\n%s", out)
	}
}

// TestPlainModeNumbersOnlyActionableOffers: held rules are shown in plain mode
// too, but unnumbered. The offer numbers are rendered i+2 and parsed n-2 in
// two separate places, so anything numbered inserted into that block would
// silently shift which rule an answer saves.
func TestPlainModeNumbersOnlyActionableOffers(t *testing.T) {
	events := make(chan engine.Event, 32)
	ctl := &fakeController{events: events}
	out := &promptWriter{marker: "n) deny", seen: make(chan struct{})}
	in := io.MultiReader(strings.NewReader("hello\n"), afterReader{ch: out.seen, r: strings.NewReader("2\n")})
	ctl.onSubmit = func() {
		ctl.scriptWithHeld(t, []string{"bash(./out *)"}, []string{"bash(gofmt *)", "bash(go vet *)"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tui.Run(ctx, ctl, tui.Options{Plain: true, Events: events, In: in, Out: out}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"your allow rules do not cover command `./out`",
		"already allowed, not offered: bash(gofmt *)",
		"already allowed, not offered: bash(go vet *)",
		"1) allow once",
		"2) allow and remember: bash(./out *)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in plain output:\n%s", want, got)
		}
	}
	// The two held rules must not have taken 3) and 4).
	if strings.Contains(got, "3)") {
		t.Errorf("a held rule consumed an offer number:\n%s", got)
	}
	if len(ctl.replies) != 1 {
		t.Fatalf("replies = %+v", ctl.replies)
	}
	d := ctl.replies[0].d
	if !d.Allow || len(d.Grants) != 1 || d.Grants[0].Rule.String() != "bash(./out *)" {
		t.Fatalf("answering 2 did not grant the single offer: %+v", d)
	}
}
