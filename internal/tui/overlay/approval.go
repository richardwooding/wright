package overlay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/diffview"
)

// approvalState is which page of the prompt is showing.
type approvalState uint8

const (
	stateMain    approvalState = iota
	stateGrants                // the "allow…" list of exact rules
	stateEdit                  // argument editor
	stateExplain               // Verdict.Explain trace
)

// Reasons shown to the model when the user declines.
const (
	reasonDenied = "denied by user"
	byUser       = "user"
)

// Approval is the permission prompt. It renders the request, a scrollable
// preview and a vertical option list, and reports the decision through the
// callback given to NewApproval.
type Approval struct {
	a      engine.Approval
	th     theme.Theme
	decide func(engine.Decision)

	state   approvalState
	opts    list
	grants  list
	scroll  int
	edit    textarea.Model
	editErr string
	preview []string // uncoloured preview lines, coloured at render time
}

// NewApproval builds the prompt for a. Focus starts on deny for destructive
// requests so Enter never destroys anything by reflex; "allow…" is only
// offered when the policy engine produced offers.
func NewApproval(a engine.Approval, th theme.Theme, decide func(engine.Decision)) *Approval {
	p := &Approval{a: a, th: th, decide: decide}
	p.opts.items = append(p.opts.items, listItem{key: "y", label: allowLabel(a.Grants)})
	if len(a.Offers) > 0 {
		p.opts.items = append(p.opts.items, listItem{key: "a", label: "allow… (choose a rule to remember)"})
	}
	p.opts.items = append(p.opts.items,
		listItem{key: "e", label: "edit arguments"},
		listItem{key: "n", label: "deny"},
		listItem{key: "x", label: "explain this decision"},
	)
	if a.Severity == engine.SeverityDestructive {
		p.opts.focus = p.opts.byKey("n")
	}
	for i, o := range a.Offers {
		p.grants.items = append(p.grants.items, listItem{
			key:   fmt.Sprint(i + 1),
			label: o.Label,
			desc:  fmt.Sprintf("rule %s · scope %s", o.Rule.String(), o.Scope),
		})
	}
	p.preview = p.previewLines()
	return p
}

// allowLabel spells out what "allow" hands over. A command the classifier
// says needs the network gets it when the user allows — so the option has to
// say so, rather than leaving the user to discover it from a command that
// failed after they said yes.
func allowLabel(g engine.CallGrant) string {
	switch {
	case g.Network && len(g.Writable) > 0:
		return "allow once (with network and the writes below)"
	case g.Network:
		return "allow once (with network access)"
	case len(g.Writable) > 0:
		return "allow once (with the writes below)"
	default:
		return "allow once"
	}
}

// grantFacts name what allowing hands over, path by path: consent has to be
// to something specific.
func (p *Approval) grantFacts(w int) []string {
	g := p.a.Grants
	if g.Empty() {
		return nil
	}
	var out []string
	if g.Network {
		out = append(out, p.th.Warm.Render(theme.GlyphWarn+" ")+"allowing runs this call with network access")
	}
	if len(g.Writable) > 0 {
		out = append(out, wrap(theme.GlyphWarn+" allowing also makes these writable for this call only: "+
			strings.Join(g.Writable, ", "), w)...)
	}
	return out
}

// ID is the approval's request ID.
func (p *Approval) ID() string { return p.a.ID }

// Title is the severity word, tool and preview title.
func (p *Approval) Title() string {
	glyph, word, style := p.severity()
	t := style.Render(glyph+" "+word) + " · " + p.th.Bold.Render(p.a.Tool)
	if p.a.Preview.Title != "" {
		t += " · " + p.a.Preview.Title
	}
	return t
}

// severity maps the level to glyph + word + colour; all three are shown.
func (p *Approval) severity() (glyph, word string, style lipgloss.Style) {
	switch p.a.Severity {
	case engine.SeverityDestructive:
		return theme.GlyphStop, "destructive", p.th.HotBold
	case engine.SeverityCaution:
		return theme.GlyphWarn, "caution", p.th.Warm
	default:
		return theme.GlyphAsk, "approval", p.th.Accented
	}
}

// Update handles keys for the current page.
func (p *Approval) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if p.state == stateEdit {
			var cmd tea.Cmd
			p.edit, cmd = p.edit.Update(msg)
			return p, cmd, false
		}
		return p, nil, false
	}
	switch p.state {
	case stateGrants:
		return p, nil, p.updateGrants(key)
	case stateEdit:
		return p.updateEdit(key)
	case stateExplain:
		if key.String() == keyEsc || key.String() == "x" || key.String() == keyEnter {
			p.state = stateMain
		}
		return p, nil, false
	default:
		return p, nil, p.updateMain(key)
	}
}

// updateMain handles the option list; it reports done once a decision is sent.
func (p *Approval) updateMain(key tea.KeyPressMsg) bool {
	switch s := key.String(); s {
	case keyUp, "k", "shift+tab":
		p.opts.move(-1)
	case keyDown, "j", "tab":
		p.opts.move(1)
	case "pgup":
		p.scroll = max(p.scroll-10, 0)
	case "pgdown":
		p.scroll += 10
	case keyEsc:
		return p.deny()
	case keyEnter:
		return p.choose(p.opts.items[p.opts.focus].key)
	default:
		if i := p.opts.byKey(s); i >= 0 {
			p.opts.focus = i
			return p.choose(s)
		}
	}
	return false
}

// choose runs the option bound to key.
func (p *Approval) choose(key string) bool {
	switch key {
	case "y":
		return p.send(engine.Decision{Allow: true, By: byUser})
	case "a":
		p.state = stateGrants
	case "e":
		p.openEditor()
	case "n":
		return p.deny()
	case "x":
		p.state = stateExplain
	}
	return false
}

func (p *Approval) deny() bool {
	return p.send(engine.Decision{Allow: false, Reason: reasonDenied, By: byUser})
}

func (p *Approval) send(d engine.Decision) bool {
	if p.decide != nil {
		p.decide(d)
	}
	return true
}

// updateGrants handles the "allow…" page: pick an exact rule or go back.
func (p *Approval) updateGrants(key tea.KeyPressMsg) bool {
	switch s := key.String(); s {
	case keyEsc:
		p.state = stateMain
	case keyUp, "k":
		p.grants.move(-1)
	case keyDown, "j":
		p.grants.move(1)
	case keyEnter:
		return p.grant(p.grants.focus)
	default:
		if i := p.grants.byKey(s); i >= 0 {
			return p.grant(i)
		}
	}
	return false
}

func (p *Approval) grant(i int) bool {
	if i < 0 || i >= len(p.a.Offers) {
		return false
	}
	offer := p.a.Offers[i]
	return p.send(engine.Decision{Allow: true, Grant: &offer, By: byUser})
}

// openEditor fills a textarea with the pretty-printed arguments.
func (p *Approval) openEditor() {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.SetValue(prettyJSON(p.a.Args))
	ta.Focus()
	p.edit = ta
	p.editErr = ""
	p.state = stateEdit
}

// updateEdit: esc goes back unchanged, ctrl+s accepts when the JSON is
// valid; everything else edits.
func (p *Approval) updateEdit(key tea.KeyPressMsg) (Overlay, tea.Cmd, bool) {
	switch key.String() {
	case keyEsc:
		p.state = stateMain
		return p, nil, false
	case "ctrl+s":
		raw := []byte(p.edit.Value())
		if !json.Valid(raw) {
			p.editErr = "not valid JSON — fix it or press esc"
			return p, nil, false
		}
		return p, nil, p.send(engine.Decision{Allow: true, Args: json.RawMessage(raw), By: byUser})
	}
	var cmd tea.Cmd
	p.edit, cmd = p.edit.Update(key)
	return p, cmd, false
}

// View lays out the page for the given size.
func (p *Approval) View(width, height int) string {
	w := inner(width)
	switch p.state {
	case stateGrants:
		return p.viewGrants(width, height, w)
	case stateEdit:
		return p.viewEdit(width, height, w)
	case stateExplain:
		body := wrapAll(p.a.Verdict.Explain, w)
		if len(body) == 0 {
			body = []string{p.th.Subtle.Render("(no explanation recorded)")}
		}
		body = append(body, "", p.th.Subtle.Render("esc back"))
		return frame(p.th, p.Title(), body, width, height)
	default:
		return p.viewMain(width, height, w)
	}
}

// viewMain: request facts, scrollable preview, option list, footer.
func (p *Approval) viewMain(width, height, w int) string {
	facts := p.facts(w)
	options := p.opts.render(p.th, w)
	footer := []string{p.th.Subtle.Render("↑↓ move · enter select · pgup/pgdn scroll · esc deny")}
	// The preview gets whatever is left after the fixed parts; never below 3 rows.
	fixed := frameRows + 2 + len(facts) + len(options) + len(footer) + 2
	room := max(height-fixed, 3)
	body := append([]string(nil), facts...)
	if len(p.preview) > 0 {
		body = append(body, scrollWindow(p.coloured(w), p.scroll, room, p.th)...)
		body = append(body, "")
	}
	body = append(body, options...)
	body = append(body, "")
	body = append(body, footer...)
	return frame(p.th, p.Title(), body, width, height)
}

// facts are the resolved paths, command class findings and the reason.
func (p *Approval) facts(w int) []string {
	var out []string
	req := p.a.Request
	if len(req.Writes) > 0 {
		out = append(out, wrap("writes: "+strings.Join(req.Writes, ", "), w)...)
	}
	if paths := exclude(req.Paths, req.Writes); len(paths) > 0 {
		out = append(out, wrap("reads: "+strings.Join(paths, ", "), w)...)
	}
	if req.URL != nil {
		out = append(out, wrap("url: "+req.URL.String(), w)...)
	}
	if req.Shell != nil {
		for _, r := range req.Shell.Reasons {
			out = append(out, p.th.Warm.Render(theme.GlyphWarn+" ")+r)
		}
		if req.Shell.Unknown {
			out = append(out, p.th.Warm.Render(theme.GlyphWarn+" contains constructs the analyser cannot see through"))
		}
		if n := req.Shell.Unrecognised; len(n) > 0 {
			// The script is perfectly readable; wright simply has no
			// description of these programs, so it cannot say what they
			// touch. Saying that plainly is the difference between "I
			// cannot read this" and "I can read it, I just do not know
			// this tool" — and it tells the user what remembering a rule
			// for it would actually mean.
			out = append(out, wrap(theme.GlyphWarn+" wright has no description of "+strings.Join(n, ", ")+
				", so it cannot tell what they read or write; the sandbox still confines them", w)...)
		}
	}
	out = append(out, p.grantFacts(w)...)
	if p.a.Verdict.Reason != "" {
		out = append(out, p.th.Subtle.Render(ansiTrunc(p.a.Verdict.Reason, w)))
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

// previewLines is the diff, command or body as plain lines.
func (p *Approval) previewLines() []string {
	switch {
	case p.a.Preview.Diff != "":
		return strings.Split(strings.TrimRight(p.a.Preview.Diff, "\n"), "\n")
	case p.a.Preview.Body != "":
		return strings.Split(strings.TrimRight(p.a.Preview.Body, "\n"), "\n")
	case p.a.Request.Shell != nil:
		return strings.Split(strings.TrimRight(p.a.Request.Shell.Raw, "\n"), "\n")
	}
	return nil
}

// coloured renders the preview: diffs through diffview, text as-is.
func (p *Approval) coloured(w int) []string {
	if p.a.Preview.Diff != "" {
		return diffview.Lines(p.a.Preview.Diff, p.th, w)
	}
	out := make([]string, len(p.preview))
	for i, l := range p.preview {
		out[i] = ansiTrunc(l, w)
	}
	return out
}

func (p *Approval) viewGrants(width, height, w int) string {
	body := []string{p.th.Subtle.Render("Remember a rule so this is not asked again. Each shows the exact rule text and where it is stored."), ""}
	body = append(body, p.grants.render(p.th, w)...)
	body = append(body, "", p.th.Subtle.Render("enter/number choose · esc back"))
	return frame(p.th, p.Title()+" · allow…", body, width, height)
}

func (p *Approval) viewEdit(width, height, w int) string {
	rows := max(height-frameRows-6, 3)
	p.edit.SetWidth(w)
	p.edit.SetHeight(rows)
	body := []string{p.th.Subtle.Render("Edit the JSON arguments; the tool runs with what you save."), ""}
	body = append(body, strings.Split(p.edit.View(), "\n")...)
	body = append(body, "")
	if p.editErr != "" {
		body = append(body, p.th.Hot.Render(theme.GlyphFail+" "+p.editErr))
	} else {
		body = append(body, p.th.Subtle.Render("ctrl+s save and allow · esc back"))
	}
	return frame(p.th, p.Title()+" · edit", body, width, height)
}

// prettyJSON indents raw, returning it unchanged when it is not JSON.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// exclude returns the paths not in writes, so a file is listed once.
func exclude(paths, writes []string) []string {
	var out []string
	for _, p := range paths {
		found := slices.Contains(writes, p)
		if !found {
			out = append(out, p)
		}
	}
	return out
}

func wrapAll(lines []string, w int) []string {
	var out []string
	for _, l := range lines {
		out = append(out, wrap(l, w)...)
	}
	return out
}
