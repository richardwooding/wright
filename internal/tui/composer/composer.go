// Package composer wraps the bubbles textarea as wright's prompt box: Enter
// submits, shift+enter / alt+enter / ctrl+j insert a newline (ctrl+j always
// works because shift+enter needs the kitty keyboard protocol), up/down walk
// the history from the first/last line, and "@" / "/" open completion popups
// for files and slash commands.
package composer

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/fuzzy"
)

// Height limits: the box grows with its content from one line to eight.
const (
	minHeight = 1
	maxHeight = 8
	popupMax  = 8 // completion rows shown above the box
)

// Options configures completion sources.
type Options struct {
	// Files returns completion candidates for "@"; nil disables the popup.
	Files func() []string
	// Commands are slash command names (without the slash) for "/".
	Commands []string
	// Placeholder is shown when the box is empty.
	Placeholder string
}

// Event is what a keystroke produced beyond editing: a submitted message.
type Event struct {
	Submitted bool
	Text      string
}

// PopupKind identifies which completion popup is open.
type PopupKind uint8

// Popup kinds.
const (
	PopupNone PopupKind = iota
	PopupFiles
	PopupCommands
)

type popup struct {
	kind  PopupKind
	query string
	all   []string
	items []string
	sel   int
}

// Model is the composer state. It is a value type like every bubble.
type Model struct {
	ta      textarea.Model
	th      theme.Theme
	opts    Options
	width   int
	history []string
	histIdx int // len(history) when not browsing
	draft   string
	popup   *popup
}

// New builds a composer with the given theme.
func New(th theme.Theme, o Options) Model {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = minHeight
	ta.MaxHeight = maxHeight
	ta.SetVirtualCursor(false)
	ta.Placeholder = o.Placeholder
	if ta.Placeholder == "" {
		ta.Placeholder = "Ask wright… (/ for commands, @ for files, ctrl+j for a newline)"
	}
	// Enter is ours (submit); the newline keys are handled explicitly.
	ta.KeyMap.InsertNewline.SetEnabled(false)
	m := Model{ta: ta, opts: o}
	m.SetTheme(th)
	return m
}

// SetTheme applies textarea styles for the theme.
func (m *Model) SetTheme(th theme.Theme) {
	m.th = th
	s := textarea.DefaultStyles(th.IsDark)
	plain := lipgloss.NewStyle()
	s.Focused.Base, s.Focused.CursorLine, s.Focused.Text, s.Focused.EndOfBuffer = plain, plain, plain, plain
	s.Focused.Prompt = th.UserPrompt
	s.Focused.Placeholder = th.Subtle
	s.Blurred = s.Focused
	m.ta.SetStyles(s)
}

// Focus focuses the textarea and returns its blink command.
func (m *Model) Focus() tea.Cmd { return m.ta.Focus() }

// SetWidth sets the total width including the prompt.
func (m *Model) SetWidth(w int) {
	m.width = w
	m.ta.SetWidth(w)
}

// Value returns the current text.
func (m Model) Value() string { return m.ta.Value() }

// SetValue replaces the text and moves the cursor to the end.
func (m *Model) SetValue(s string) {
	m.ta.SetValue(s)
	m.ta.MoveToEnd()
}

// Reset clears the box and closes any popup.
func (m *Model) Reset() {
	m.ta.Reset()
	m.popup = nil
}

// PushHistory records a submitted message for up/down recall.
func (m *Model) PushHistory(s string) {
	if s == "" || (len(m.history) > 0 && m.history[len(m.history)-1] == s) {
		m.histIdx = len(m.history)
		return
	}
	m.history = append(m.history, s)
	m.histIdx = len(m.history)
}

// Popup reports which completion popup is open.
func (m Model) Popup() PopupKind {
	if m.popup == nil {
		return PopupNone
	}
	return m.popup.kind
}

// ClosePopup dismisses the completion popup.
func (m *Model) ClosePopup() { m.popup = nil }

// Height is the number of rows View occupies (popup + textarea).
func (m Model) Height() int { return m.popupHeight() + m.ta.Height() }

// Cursor is the textarea cursor offset by the popup rows above it.
func (m Model) Cursor() *tea.Cursor {
	c := m.ta.Cursor()
	if c != nil {
		c.Y += m.popupHeight()
	}
	return c
}

// View renders the popup (if any) above the textarea.
func (m Model) View() string {
	if m.popup == nil || len(m.popup.items) == 0 {
		return m.ta.View()
	}
	return m.popupView() + "\n" + m.ta.View()
}

func (m Model) popupHeight() int {
	if m.popup == nil {
		return 0
	}
	return min(len(m.popup.items), popupMax)
}

func (m Model) popupView() string {
	rows := make([]string, 0, popupMax)
	for i, it := range m.popup.items[:m.popupHeight()] {
		line := " " + it + " "
		if i == m.popup.sel {
			line = m.th.Selected.Render(line)
		} else {
			line = m.th.Subtle.Render(line)
		}
		rows = append(rows, ansi.Truncate(line, max(m.width, 1), "…"))
	}
	return strings.Join(rows, "\n")
}

// Update handles one message. Keys the composer owns never reach the textarea.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd, Event) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd, Event{}
	}
	if m.popup != nil {
		return m.updatePopup(key)
	}
	return m.updateKey(key)
}

// updateKey handles keys with no popup open.
func (m Model) updateKey(key tea.KeyPressMsg) (Model, tea.Cmd, Event) {
	switch key.String() {
	case "enter":
		return m.submit()
	case "shift+enter", "alt+enter", "ctrl+j":
		m.ta.InsertString("\n")
		return m, nil, Event{}
	case "ctrl+u":
		m.Reset()
		return m, nil, Event{}
	case "up":
		if m.ta.Line() == 0 && m.historyStep(-1) {
			return m, nil, Event{}
		}
	case "down":
		if m.ta.Line() == m.ta.LineCount()-1 && m.historyStep(1) {
			return m, nil, Event{}
		}
	}
	wasEmpty := strings.TrimSpace(m.ta.Value()) == ""
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(key)
	m.maybeOpenPopup(key, wasEmpty)
	return m, cmd, Event{}
}

// submit emits the trimmed text and clears the box; blank input is ignored.
func (m Model) submit() (Model, tea.Cmd, Event) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return m, nil, Event{}
	}
	m.PushHistory(text)
	m.Reset()
	return m, nil, Event{Submitted: true, Text: text}
}

// historyStep moves through history; delta -1 is older. It reports whether
// the key was consumed.
func (m *Model) historyStep(delta int) bool {
	next := m.histIdx + delta
	if len(m.history) == 0 || next < 0 || next > len(m.history) {
		return false
	}
	if m.histIdx == len(m.history) {
		m.draft = m.ta.Value() // keep what was being typed
	}
	m.histIdx = next
	if next == len(m.history) {
		m.SetValue(m.draft)
	} else {
		m.SetValue(m.history[next])
	}
	return true
}

// maybeOpenPopup opens the file popup after "@" and the command popup after
// a "/" typed into an empty box.
func (m *Model) maybeOpenPopup(key tea.KeyPressMsg, wasEmpty bool) {
	switch key.Text {
	case "@":
		if m.opts.Files != nil {
			m.openPopup(PopupFiles, m.opts.Files())
		}
	case "/":
		if wasEmpty && len(m.opts.Commands) > 0 {
			m.openPopup(PopupCommands, m.opts.Commands)
		}
	}
}

func (m *Model) openPopup(kind PopupKind, all []string) {
	p := &popup{kind: kind, all: all}
	p.items = fuzzy.Filter(all, "")
	m.popup = p
}

// updatePopup routes keys while a completion popup is open: navigation and
// acceptance are consumed, everything else edits the text and the query.
func (m Model) updatePopup(key tea.KeyPressMsg) (Model, tea.Cmd, Event) {
	p := m.popup
	switch key.String() {
	case "esc":
		m.popup = nil
		return m, nil, Event{}
	case "tab":
		m.accept()
		return m, nil, Event{}
	case "enter":
		if m.completes() {
			m.accept()
			return m, nil, Event{}
		}
		m.popup = nil
		return m.submit()
	case "up", "ctrl+p":
		p.sel = (p.sel + len(p.items) - 1) % max(len(p.items), 1)
		return m, nil, Event{}
	case "down", "ctrl+n":
		p.sel = (p.sel + 1) % max(len(p.items), 1)
		return m, nil, Event{}
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(key)
	m.refilter(key)
	return m, cmd, Event{}
}

// refilter updates the query from the key just applied to the textarea.
func (m *Model) refilter(key tea.KeyPressMsg) {
	p := m.popup
	switch {
	case key.String() == "backspace":
		if p.query == "" {
			m.popup = nil
			return
		}
		p.query = p.query[:len(p.query)-1]
	case key.Text == "" || key.Text == " ":
		m.popup = nil
		return
	default:
		p.query += key.Text
	}
	p.items = fuzzy.Filter(p.all, p.query)
	p.sel = 0
}

// completes reports whether Enter should complete rather than submit: there
// is something to complete and, for commands, the user has not already
// typed the whole name ("/help⏎" must run /help, not add a space).
func (m Model) completes() bool {
	p := m.popup
	if len(p.items) == 0 {
		return false
	}
	return p.kind != PopupCommands || !strings.EqualFold(p.items[p.sel], p.query)
}

// accept replaces the trigger and query with the selected completion.
func (m *Model) accept() {
	p := m.popup
	m.popup = nil
	if len(p.items) == 0 {
		return
	}
	choice := p.items[p.sel]
	value := m.ta.Value()
	switch p.kind {
	case PopupCommands:
		m.SetValue("/" + choice + " ")
	case PopupFiles:
		needle := "@" + p.query
		if i := strings.LastIndex(value, needle); i >= 0 {
			m.SetValue(value[:i] + choice + " " + value[i+len(needle):])
		}
	case PopupNone:
	}
}
