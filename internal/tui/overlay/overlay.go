// Package overlay holds the modal prompts drawn over the transcript: the
// approval prompt, ask_user questions, pickers, help, todos, a typed-word
// confirmation and a path prompt. An open overlay receives every key; esc
// always closes it, which for an approval means deny — the safe default is
// never more than one key away.
package overlay

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
)

// Overlay is a modal prompt. Update reports done when it should be closed.
type Overlay interface {
	Update(msg tea.Msg) (Overlay, tea.Cmd, bool)
	View(width, height int) string
	Title() string
}

// Key names as tea.KeyPressMsg.String() spells them.
const (
	keyEsc   = "esc"
	keyEnter = "enter"
	keyUp    = "up"
	keyDown  = "down"
)

// Box geometry: the frame never exceeds the viewport and stays readable on a
// 40-column terminal (2 border + 2 padding columns leave 36 for text).
const (
	maxBoxWidth = 100
	frameCols   = 4 // border + padding on both sides
	frameRows   = 2 // top and bottom border
)

// frame draws title and body inside the themed border, clipped to the
// available size. body lines are truncated, never wrapped, so callers wrap
// text at inner(width) themselves when they want it to flow.
func frame(th theme.Theme, title string, body []string, width, height int) string {
	outer := min(width, maxBoxWidth)
	inner := max(outer-frameCols, 1)
	rows := max(height-frameRows, 1)
	lines := make([]string, 0, rows)
	if title != "" {
		lines = append(lines, ansi.Truncate(title, inner, "…"), "")
	}
	for _, l := range body {
		if len(lines) >= rows {
			break
		}
		lines = append(lines, ansi.Truncate(l, inner, "…"))
	}
	// v2 Width is the whole block, border and padding included.
	return th.Overlay.Width(outer).Render(strings.Join(lines, "\n"))
}

// inner is the text width available inside a frame drawn at width.
func inner(width int) int {
	return max(min(width, maxBoxWidth)-frameCols, 1)
}

// wrap hard-wraps text to width and splits it into lines.
func wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	return strings.Split(strings.TrimRight(ansi.Wrap(text, width, ""), "\n"), "\n")
}

// list is a vertical option list with one focused row.
type list struct {
	items []listItem
	focus int
	// marks, when non-nil, makes the list multi-select: it is one flag per
	// item and every row is drawn with a [x]/[ ] box. Left nil, the list is
	// the single-pick list every other overlay uses and renders as before.
	marks []bool
}

// multi turns the list into a multi-select one, sized to its items.
func (l *list) multi() { l.marks = make([]bool, len(l.items)) }

// toggle flips the mark on the focused row.
func (l *list) toggle() {
	if l.marks != nil && l.focus < len(l.marks) {
		l.marks[l.focus] = !l.marks[l.focus]
	}
}

// markedItems are the indices marked, in list order.
func (l *list) markedItems() []int {
	var out []int
	for i, m := range l.marks {
		if m {
			out = append(out, i)
		}
	}
	return out
}

// listItem is one row: an optional single-key shortcut and a label.
type listItem struct {
	key   string
	label string
	desc  string
}

func (l *list) move(delta int) {
	if n := len(l.items); n > 0 {
		l.focus = ((l.focus+delta)%n + n) % n
	}
}

// byKey returns the index of the item bound to key, or -1.
func (l *list) byKey(key string) int {
	for i, it := range l.items {
		if it.key != "" && it.key == key {
			return i
		}
	}
	return -1
}

// render draws the rows, marking the focused one; the whole row is styled
// so the focus is visible without colour (reverse video plus the "›" mark).
func (l *list) render(th theme.Theme, width int) []string {
	out := make([]string, 0, len(l.items))
	for i, it := range l.items {
		var b strings.Builder
		if i == l.focus {
			b.WriteString("› ")
		} else {
			b.WriteString("  ")
		}
		box := l.box(i)
		b.WriteString(box)
		if it.key != "" {
			b.WriteString("[" + it.key + "] ")
		}
		b.WriteString(it.label)
		row := ansi.Truncate(b.String(), width, "…")
		if i == l.focus {
			row = th.Selected.Render(row)
		} else if it.key != "" {
			row = "  " + box + th.Key.Render("["+it.key+"]") + " " + ansi.Truncate(it.label, max(width-len(it.key)-len(box)-5, 1), "…")
		}
		out = append(out, row)
		if it.desc != "" {
			out = append(out, th.Subtle.Render(ansi.Truncate("      "+it.desc, width, "…")))
		}
	}
	return out
}

// box is the mark for a row: empty unless the list is multi-select. It is a
// word-shaped glyph pair rather than colour, so the state is readable in a
// terminal without it.
func (l *list) box(i int) string {
	if l.marks == nil {
		return ""
	}
	if i < len(l.marks) && l.marks[i] {
		return "[x] "
	}
	return "[ ] "
}

// scrollWindow returns lines[offset:offset+size] clamped, plus an indicator
// line when there is more above or below.
func scrollWindow(lines []string, offset, size int, th theme.Theme) []string {
	if size < 1 || len(lines) == 0 {
		return nil
	}
	offset = max(0, min(offset, len(lines)-size))
	end := min(offset+size, len(lines))
	out := append([]string(nil), lines[offset:end]...)
	if end < len(lines) && len(out) > 0 {
		out[len(out)-1] = th.Subtle.Render("… " + strconv.Itoa(len(lines)-end) + " more (pgdn)")
	}
	return out
}

// ansiTrunc truncates styled text to w cells with an ellipsis.
func ansiTrunc(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }

// itoa is strconv.Itoa; it keeps the call sites short.
func itoa(n int) string { return strconv.Itoa(n) }
