package overlay

import (
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/fuzzy"
)

// Item is one picker row. Value is what the caller gets back; Label is what
// the user sees and filters on.
type Item struct {
	Label string
	Desc  string
	Value string
}

// Picker is a filterable list used by /model, /sessions and /resume.
type Picker struct {
	title string
	th    theme.Theme
	items []Item
	pick  func(Item)
	query string
	hits  list
	shown []Item
}

// NewPicker builds a picker; pick is called with the chosen item.
func NewPicker(title string, items []Item, th theme.Theme, pick func(Item)) *Picker {
	p := &Picker{title: title, th: th, items: items, pick: pick}
	p.refilter()
	return p
}

// Title is the picker's heading.
func (p *Picker) Title() string { return p.th.Accented.Render(p.title) }

// Update filters on printable keys, moves on arrows and picks on enter.
func (p *Picker) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil, false
	}
	switch key.String() {
	case keyEsc:
		return p, nil, true
	case keyUp, "ctrl+p":
		p.hits.move(-1)
	case keyDown, "ctrl+n":
		p.hits.move(1)
	case keyEnter:
		if len(p.shown) > 0 {
			if p.pick != nil {
				p.pick(p.shown[p.hits.focus])
			}
			return p, nil, true
		}
	case "backspace":
		if p.query != "" {
			p.query = p.query[:len(p.query)-1]
			p.refilter()
		}
	default:
		if key.Text != "" {
			p.query += key.Text
			p.refilter()
		}
	}
	return p, nil, false
}

// refilter recomputes the visible rows from the query.
func (p *Picker) refilter() {
	labels := make([]string, len(p.items))
	byLabel := map[string]Item{}
	for i, it := range p.items {
		labels[i] = it.Label
		byLabel[it.Label] = it
	}
	p.shown = p.shown[:0]
	p.hits.items = p.hits.items[:0]
	for _, l := range fuzzy.Filter(labels, p.query) {
		it := byLabel[l]
		p.shown = append(p.shown, it)
		p.hits.items = append(p.hits.items, listItem{label: it.Label, desc: it.Desc})
	}
	p.hits.focus = 0
}

// View shows the filter line, the rows that fit and a hint. Scrolling is by
// item so a two-line row (label + description) never gets split.
func (p *Picker) View(width, height int) string {
	w := inner(width)
	body := []string{p.th.Bold.Render("filter: ") + p.query + "▏", ""}
	room := max((height-frameRows-2-len(body)-2)/2, 1)
	start := 0
	if p.hits.focus >= room {
		start = p.hits.focus - room + 1
	}
	end := min(start+room, len(p.hits.items))
	visible := list{items: p.hits.items[start:end], focus: p.hits.focus - start}
	rows := visible.render(p.th, w)
	if len(rows) == 0 {
		rows = []string{p.th.Subtle.Render("(no matches)")}
	}
	body = append(body, rows...)
	body = append(body, "", p.th.Subtle.Render("type to filter · ↑↓ move · enter choose · esc close"))
	return frame(p.th, p.Title(), body, width, height)
}
