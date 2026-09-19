package tui_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/policy"
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
	if f.events != nil {
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
			for _, line := range strings.Split(m.View().Content, "\n") {
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
	for _, line := range strings.Split(m.View().Content, "\n") {
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
