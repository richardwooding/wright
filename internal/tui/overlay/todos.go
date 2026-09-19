package overlay

import (
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/theme"
)

// Todos shows the model's task list (ctrl+t).
type Todos struct {
	th     theme.Theme
	todos  []engine.Todo
	scroll int
}

// NewTodos builds the overlay for the current list.
func NewTodos(todos []engine.Todo, th theme.Theme) *Todos {
	return &Todos{th: th, todos: todos}
}

// Title is the heading with a done/total count.
func (t *Todos) Title() string {
	done := 0
	for _, td := range t.todos {
		if td.Status == "completed" {
			done++
		}
	}
	return t.th.Accented.Render("todos") + t.th.Subtle.Render(" · "+itoa(done)+"/"+itoa(len(t.todos))+" done")
}

// Update scrolls; esc, q and ctrl+t close.
func (t *Todos) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return t, nil, false
	}
	switch key.String() {
	case keyEsc, "q", "ctrl+t", keyEnter:
		return t, nil, true
	case keyUp, "k":
		t.scroll = max(t.scroll-1, 0)
	case keyDown, "j":
		t.scroll++
	}
	return t, nil, false
}

// View renders one row per todo with a status glyph and word.
func (t *Todos) View(width, height int) string {
	w := inner(width)
	var lines []string
	for _, td := range t.todos {
		lines = append(lines, ansiTrunc(t.status(td.Status)+" "+td.Content, w))
	}
	if len(lines) == 0 {
		lines = []string{t.th.Subtle.Render("(no todos yet)")}
	}
	room := max(height-frameRows-2-2, 1)
	body := scrollWindow(lines, t.scroll, room, t.th)
	body = append(body, "", t.th.Subtle.Render("esc close"))
	return frame(t.th, t.Title(), body, width, height)
}

func (t *Todos) status(s string) string {
	switch s {
	case "completed":
		return t.th.Good.Render(theme.GlyphOK + " done")
	case "in_progress":
		return t.th.Warm.Render(theme.GlyphRunning + " doing")
	default:
		return t.th.Subtle.Render("○ todo")
	}
}
