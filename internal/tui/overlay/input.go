package overlay

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/theme"
)

// Input is a one-line prompt (the /export path).
type Input struct {
	title  string
	prompt string
	th     theme.Theme
	submit func(string) tea.Cmd
	value  string
}

// NewInput builds the prompt with an initial value; submit turns the trimmed
// text into a message for the caller's Update.
func NewInput(title, prompt, initial string, th theme.Theme, submit func(string) tea.Cmd) *Input {
	return &Input{title: title, prompt: prompt, th: th, submit: submit, value: initial}
}

// Title is the heading.
func (p *Input) Title() string { return p.th.Accented.Render(p.title) }

// Value is the current text.
func (p *Input) Value() string { return p.value }

// Update edits the line; enter submits, esc cancels, ctrl+u clears.
func (p *Input) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	switch m := msg.(type) {
	case tea.PasteMsg:
		p.value += strings.ReplaceAll(m.Content, "\n", "")
		return p, nil, false
	case tea.KeyPressMsg:
		cmd, done := p.key(m)
		return p, cmd, done
	}
	return p, nil, false
}

func (p *Input) key(key tea.KeyPressMsg) (tea.Cmd, bool) {
	switch key.String() {
	case keyEsc:
		return nil, true
	case keyEnter:
		v := strings.TrimSpace(p.value)
		if v == "" {
			return nil, false
		}
		if p.submit != nil {
			return p.submit(v), true
		}
		return nil, true
	case "ctrl+u":
		p.value = ""
	case "backspace":
		if r := []rune(p.value); len(r) > 0 {
			p.value = string(r[:len(r)-1])
		}
	default:
		if key.Text != "" {
			p.value += key.Text
		}
	}
	return nil, false
}

// View shows the prompt and the line being edited.
func (p *Input) View(width, height int) string {
	w := inner(width)
	body := wrap(p.prompt, w)
	body = append(body, "", ansiTrunc("› "+p.value+"▏", w), "", p.th.Subtle.Render("enter ok · esc cancel"))
	return frame(p.th, p.Title(), body, width, height)
}
