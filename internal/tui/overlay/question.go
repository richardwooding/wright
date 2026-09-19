package overlay

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/theme"
)

// Question is an ask_user prompt: numbered options and, when the tool allows
// it, a free-text box. Tab moves between the two.
type Question struct {
	q      engine.QuestionEvent
	th     theme.Theme
	answer func(engine.Answer)
	opts   list
	text   textarea.Model
	inText bool
}

// NewQuestion builds the prompt; answer receives the reply exactly once.
func NewQuestion(q engine.QuestionEvent, th theme.Theme, answer func(engine.Answer)) *Question {
	p := &Question{q: q, th: th, answer: answer}
	for i, o := range q.Options {
		p.opts.items = append(p.opts.items, listItem{key: strconv.Itoa(i + 1), label: o})
	}
	if q.FreeText {
		ta := textarea.New()
		ta.ShowLineNumbers = false
		ta.Prompt = "› "
		ta.Placeholder = "type an answer"
		ta.SetHeight(3)
		p.text = ta
		if len(q.Options) == 0 {
			p.inText = true
			p.text.Focus()
		}
	}
	return p
}

// ID is the question's request ID.
func (p *Question) ID() string { return p.q.ID }

// Title names the prompt.
func (p *Question) Title() string {
	return p.th.Accented.Render(theme.GlyphAsk+" question") + p.th.Subtle.Render(" · ask_user")
}

// Update handles keys; esc sends an empty free-text answer so the tool
// returns instead of hanging the run.
func (p *Question) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil, false
	}
	switch key.String() {
	case keyEsc:
		return p, nil, p.send(engine.Answer{Index: -1})
	case "tab":
		if p.q.FreeText && len(p.q.Options) > 0 {
			p.toggleFocus()
		}
		return p, nil, false
	}
	if p.inText {
		return p.updateText(key)
	}
	return p, nil, p.updateOptions(key)
}

func (p *Question) toggleFocus() {
	p.inText = !p.inText
	if p.inText {
		p.text.Focus()
	} else {
		p.text.Blur()
	}
}

func (p *Question) updateOptions(key tea.KeyPressMsg) bool {
	switch s := key.String(); s {
	case keyUp, "k":
		p.opts.move(-1)
	case keyDown, "j":
		p.opts.move(1)
	case keyEnter:
		return p.pick(p.opts.focus)
	default:
		if i := p.opts.byKey(s); i >= 0 {
			return p.pick(i)
		}
	}
	return false
}

func (p *Question) pick(i int) bool {
	if i < 0 || i >= len(p.q.Options) {
		return false
	}
	return p.send(engine.Answer{Text: p.q.Options[i], Index: i})
}

func (p *Question) updateText(key tea.KeyPressMsg) (Overlay, tea.Cmd, bool) {
	if key.String() == keyEnter {
		text := strings.TrimSpace(p.text.Value())
		if text == "" {
			return p, nil, false
		}
		return p, nil, p.send(engine.Answer{Text: text, Index: -1})
	}
	var cmd tea.Cmd
	p.text, cmd = p.text.Update(key)
	return p, cmd, false
}

func (p *Question) send(a engine.Answer) bool {
	if p.answer != nil {
		p.answer(a)
	}
	return true
}

// View shows the question text, the options and the free-text box.
func (p *Question) View(width, height int) string {
	w := inner(width)
	body := wrap(p.q.Text, w)
	if len(p.opts.items) > 0 {
		body = append(body, "")
		body = append(body, p.opts.render(p.th, w)...)
	}
	if p.q.FreeText {
		p.text.SetWidth(w)
		body = append(body, "")
		body = append(body, strings.Split(p.text.View(), "\n")...)
	}
	hint := "enter choose · esc skip"
	if p.q.FreeText && len(p.q.Options) > 0 {
		hint = "tab switch list/text · " + hint
	}
	body = append(body, "", p.th.Subtle.Render(hint))
	return frame(p.th, p.Title(), body, width, height)
}
