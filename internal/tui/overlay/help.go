package overlay

import (
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/theme"
)

// Entry is one help row: the key or command and what it does.
type Entry struct {
	Name string
	Desc string
}

// Help lists keybindings and slash commands; it scrolls.
type Help struct {
	th       theme.Theme
	keys     []Entry
	commands []Entry
	scroll   int
}

// NewHelp builds the help overlay.
func NewHelp(th theme.Theme, keys, commands []Entry) *Help {
	return &Help{th: th, keys: keys, commands: commands}
}

// Title is the heading.
func (h *Help) Title() string { return h.th.Accented.Render("help") }

// Update scrolls; esc, q, enter and ? close.
func (h *Help) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return h, nil, false
	}
	switch key.String() {
	case keyEsc, "q", keyEnter, "?":
		return h, nil, true
	case keyUp, "k":
		h.scroll = max(h.scroll-1, 0)
	case keyDown, "j":
		h.scroll++
	case "pgup":
		h.scroll = max(h.scroll-10, 0)
	case "pgdown":
		h.scroll += 10
	}
	return h, nil, false
}

// View renders both tables in one scrollable column.
func (h *Help) View(width, height int) string {
	w := inner(width)
	lines := []string{h.th.Bold.Render("keys")}
	lines = append(lines, entries(h.keys, h.th, w)...)
	lines = append(lines, "", h.th.Bold.Render("commands"))
	lines = append(lines, entries(h.commands, h.th, w)...)
	room := max(height-frameRows-2-2, 1)
	body := scrollWindow(lines, h.scroll, room, h.th)
	body = append(body, "", h.th.Subtle.Render("↑↓ scroll · esc close"))
	return frame(h.th, h.Title(), body, width, height)
}

// entries aligns names in a column sized to the longest name.
func entries(es []Entry, th theme.Theme, w int) []string {
	col := 0
	for _, e := range es {
		col = max(col, len([]rune(e.Name)))
	}
	col = min(col, max(w/2, 1))
	out := make([]string, 0, len(es))
	for _, e := range es {
		name := ansiTrunc(e.Name, col)
		pad := col - len([]rune(name))
		out = append(out, ansiTrunc(th.Key.Render(name)+spaces(pad)+"  "+e.Desc, w))
	}
	return out
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}
