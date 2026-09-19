package overlay

import (
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/theme"
)

// Confirm asks the user to type a word before something dangerous happens
// (bypass mode). A single keypress is deliberately not enough.
type Confirm struct {
	title   string
	prompt  string
	word    string
	th      theme.Theme
	confirm func() tea.Cmd
	typed   string
	wrong   bool
}

// NewConfirm builds the prompt; confirm runs only when word is typed exactly
// and returns the message that carries out the action.
func NewConfirm(title, prompt, word string, th theme.Theme, confirm func() tea.Cmd) *Confirm {
	return &Confirm{title: title, prompt: prompt, word: word, th: th, confirm: confirm}
}

// Title is the heading, in the hot style because it guards something hot.
func (c *Confirm) Title() string { return c.th.HotBold.Render(theme.GlyphStop + " " + c.title) }

// Update collects typed text; enter checks it, esc cancels.
func (c *Confirm) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return c, nil, false
	}
	switch key.String() {
	case keyEsc:
		return c, nil, true
	case keyEnter:
		if c.typed == c.word {
			var cmd tea.Cmd
			if c.confirm != nil {
				cmd = c.confirm()
			}
			return c, cmd, true
		}
		c.wrong = true
		c.typed = ""
	case "backspace":
		if c.typed != "" {
			c.typed = c.typed[:len(c.typed)-1]
		}
	default:
		if key.Text != "" {
			c.typed += key.Text
		}
	}
	return c, nil, false
}

// View shows the prompt and the typed text.
func (c *Confirm) View(width, height int) string {
	w := inner(width)
	body := wrap(c.prompt, w)
	body = append(body, "", "type "+c.th.Bold.Render(c.word)+" to confirm: "+c.typed+"▏")
	if c.wrong {
		body = append(body, c.th.Hot.Render(theme.GlyphFail+" that is not \""+c.word+"\""))
	}
	body = append(body, "", c.th.Subtle.Render("enter confirm · esc cancel"))
	return frame(c.th, c.Title(), body, width, height)
}
