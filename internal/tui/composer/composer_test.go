package composer_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/composer"
)

func newComposer(files []string) composer.Model {
	m := composer.New(theme.New(true), composer.Options{
		Files:    func() []string { return files },
		Commands: []string{"help", "mode", "model", "quit"},
	})
	m.SetWidth(60)
	m.Focus()
	return m
}

func press(m composer.Model, keys ...string) (composer.Model, composer.Event) {
	var ev composer.Event
	for _, k := range keys {
		m, _, ev = m.Update(keyFor(k))
	}
	return m, ev
}

// keyFor builds the KeyPressMsg whose String() is s, for the keys the tests use.
func keyFor(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "shift+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "alt+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
	case "ctrl+j":
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func typeText(m composer.Model, s string) composer.Model {
	for _, r := range s {
		m, _ = press(m, string(r))
	}
	return m
}

func TestEnterSubmitsAndClears(t *testing.T) {
	m := typeText(newComposer(nil), "hello")
	m, ev := press(m, "enter")
	if !ev.Submitted || ev.Text != "hello" {
		t.Fatalf("event = %+v", ev)
	}
	if m.Value() != "" {
		t.Fatalf("box not cleared: %q", m.Value())
	}
	if _, ev := press(m, "enter"); ev.Submitted {
		t.Fatal("empty box submitted")
	}
}

func TestNewlineKeys(t *testing.T) {
	for _, k := range []string{"shift+enter", "alt+enter", "ctrl+j"} {
		m := typeText(newComposer(nil), "a")
		m, ev := press(m, k)
		m = typeText(m, "b")
		if ev.Submitted {
			t.Errorf("%s submitted", k)
		}
		if m.Value() != "a\nb" {
			t.Errorf("%s: value %q", k, m.Value())
		}
	}
}

func TestHistoryRecall(t *testing.T) {
	m := typeText(newComposer(nil), "first")
	m, _ = press(m, "enter")
	m = typeText(m, "second")
	m, _ = press(m, "enter")
	m = typeText(m, "draft")
	m, _ = press(m, "up")
	if m.Value() != "second" {
		t.Fatalf("after up: %q", m.Value())
	}
	m, _ = press(m, "up")
	if m.Value() != "first" {
		t.Fatalf("after up up: %q", m.Value())
	}
	m, _ = press(m, "down", "down")
	if m.Value() != "draft" {
		t.Fatalf("draft not restored: %q", m.Value())
	}
}

func TestPasteInsertsVerbatim(t *testing.T) {
	m := newComposer(nil)
	m, _, _ = m.Update(tea.PasteMsg{Content: "line1\nline2"})
	if m.Value() != "line1\nline2" {
		t.Fatalf("value %q", m.Value())
	}
}

func TestFileCompletion(t *testing.T) {
	m := typeText(newComposer([]string{"internal/tui/model.go", "README.md", "internal/theme/theme.go"}), "look at @")
	if m.Popup() != composer.PopupFiles {
		t.Fatal("popup not opened by @")
	}
	m = typeText(m, "theme")
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "internal/theme/theme.go") || strings.Contains(view, "README") {
		t.Fatalf("popup not filtered:\n%s", view)
	}
	m, _ = press(m, "tab")
	if m.Popup() != composer.PopupNone {
		t.Fatal("popup still open after tab")
	}
	if got := m.Value(); got != "look at internal/theme/theme.go " {
		t.Fatalf("value %q", got)
	}
	if m.Height() != 1 {
		t.Fatalf("height %d after popup closed", m.Height())
	}
}

func TestFilePopupEscAndBackspace(t *testing.T) {
	m := typeText(newComposer([]string{"a.go"}), "@a")
	m, _ = press(m, "esc")
	if m.Popup() != composer.PopupNone || m.Value() != "@a" {
		t.Fatalf("esc: popup %v value %q", m.Popup(), m.Value())
	}
	m = typeText(newComposer([]string{"a.go"}), "@a")
	m, _ = press(m, "backspace", "backspace")
	if m.Popup() != composer.PopupNone || m.Value() != "" {
		t.Fatalf("backspace: popup %v value %q", m.Popup(), m.Value())
	}
}

func TestCommandCompletion(t *testing.T) {
	m := typeText(newComposer(nil), "/mo")
	if m.Popup() != composer.PopupCommands {
		t.Fatal("popup not opened by /")
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "mode") || !strings.Contains(view, "model") || strings.Contains(view, "help") {
		t.Fatalf("popup not filtered:\n%s", view)
	}
	m, _ = press(m, "down", "enter")
	if got := m.Value(); got != "/model " {
		t.Fatalf("value %q", got)
	}
	// A slash mid-sentence is just a character.
	m = typeText(newComposer(nil), "a /b")
	if m.Popup() != composer.PopupNone {
		t.Fatal("popup opened for a mid-line slash")
	}
}

func TestCursorOffsetByPopup(t *testing.T) {
	m := typeText(newComposer([]string{"a.go", "b.go"}), "@")
	c := m.Cursor()
	if c == nil {
		t.Fatal("no cursor while focused")
	}
	if c.Y != 2 {
		t.Fatalf("cursor Y = %d, want 2 (below two popup rows)", c.Y)
	}
	if m.Height() != 3 {
		t.Fatalf("height %d, want 3", m.Height())
	}
}

func TestCtrlUClears(t *testing.T) {
	m := typeText(newComposer(nil), "abc")
	m, _ = press(m, "ctrl+u")
	if m.Value() != "" {
		t.Fatalf("value %q", m.Value())
	}
}

func TestGrowsToEightLines(t *testing.T) {
	m := newComposer(nil)
	for range 12 {
		m = typeText(m, "x")
		m, _ = press(m, "ctrl+j")
	}
	if m.Height() != 8 {
		t.Fatalf("height %d, want 8", m.Height())
	}
}
