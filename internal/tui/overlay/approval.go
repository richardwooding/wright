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
	"github.com/richardwooding/wright/internal/policy"
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
		p.opts.items = append(p.opts.items, listItem{key: "a", label: "allow… (choose rules to remember)"})
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
	p.grants.multi()
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
	if g.GitHubAuth {
		out = append(out, wrap(theme.GlyphWarn+" allowing also lets this call authenticate to GitHub as you", w)...)
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

// updateGrants handles the "allow…" page: mark the rules to remember, or go
// back. A script routinely needs more than one — `cd X && fpc … && ./bin/t`
// wants a rule for each — and answering one prompt per rule means being asked
// again on the very next call.
func (p *Approval) updateGrants(key tea.KeyPressMsg) bool {
	switch s := key.String(); s {
	case keyEsc:
		p.state = stateMain
	case keyUp, "k":
		p.grants.move(-1)
	case keyDown, "j":
		p.grants.move(1)
	case "space":
		p.grants.toggle()
	case keyEnter:
		// Nothing marked means the focused row, so enter alone still works
		// the way it did when a prompt could only accept one rule.
		if marked := p.grants.markedItems(); len(marked) > 0 {
			return p.grant(marked...)
		}
		return p.grant(p.grants.focus)
	default:
		// A digit still picks that one row and answers immediately: it is
		// the fast path, and waiting for a second key would make the common
		// single-rule case slower than before.
		if i := p.grants.byKey(s); i >= 0 {
			return p.grant(i)
		}
	}
	return false
}

// grant sends an allow that remembers the offers at these indices.
func (p *Approval) grant(idx ...int) bool {
	var offers []policy.GrantOffer
	for _, i := range idx {
		if i >= 0 && i < len(p.a.Offers) {
			offers = append(offers, p.a.Offers[i])
		}
	}
	if len(offers) == 0 {
		return false
	}
	return p.send(engine.Decision{Allow: true, Grants: offers, By: byUser})
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
	out = append(out, p.coverageFacts(w)...)
	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

// previewLines is the diff, command or body as plain lines.
// coverageFacts answer the question this prompt actually raises for anyone who
// has been saving rules: "I approved this, why am I being asked again?"
//
// The wording matches policy's own explain("allow rules do not cover %s", …),
// so there is one sentence to maintain and the strings it produces were
// written as the tail of exactly that sentence.
func (p *Approval) coverageFacts(w int) []string {
	var out []string
	if u := p.a.Verdict.Uncovered; u != "" {
		out = append(out, wrap(p.th.Warm.Render(theme.GlyphAsk+" ")+"your allow rules do not cover "+u, w)...)
	}
	// With every command covered but an explicit ask rule deciding, there are
	// no offers at all — so [a] is hidden and the held rules have nowhere to
	// appear. Give the count here instead, and say plainly that they did not
	// decide: the call is still being refused pending an answer, and a line
	// reading as "this is allowed" on a prompt that is asking would be worse
	// than the noise this whole change removes.
	if len(p.a.Offers) == 0 && len(p.a.Verdict.Held) > 0 {
		out = append(out, wrap(p.th.Subtle.Render(fmt.Sprintf(
			"%d rule(s) you already have match this call; the decision above outranks them",
			len(p.a.Verdict.Held))), w)...)
	}
	return out
}

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

// viewGrants is the "allow…" page: the rules to remember, then the rules
// already in force.
//
// Nothing here may be appended to p.grants.items. That list is positionally
// identical to p.a.Offers — grant() indexes the offers by list index and the
// digits are fmt.Sprint(i+1) — so one extra row would make every digit below
// it save the wrong rule. The held block is therefore flat lines, the way
// Help.View builds its sections.
//
// It also budgets rows and windows the list, which it did not before: a
// six-command script yields eighteen offers at two lines each, and frame cuts
// from the bottom, so the footer hint was gone and the focus marker could sit
// off-screen — at any terminal height.
func (p *Approval) viewGrants(width, height, w int) string {
	// frame keeps height-frameRows body lines and spends two on the title and
	// the blank under it. What is left is shed in priority order, because a
	// short terminal cannot have everything: the key hints outrank the held
	// block, which outranks the explanatory intro. Losing the hints is what
	// this page did before it budgeted at all — frame cuts from the bottom,
	// so they went first, at every height including forty rows.
	foot := []string{"", p.th.Subtle.Render("space mark · enter apply · number apply one · esc back")}
	avail := max(height-frameRows-2-len(foot), 1)

	var head []string
	if avail >= 8 {
		head = []string{p.th.Subtle.Render("Remember rules so this is not asked again. Each shows the exact rule text and where it is stored."), ""}
		avail -= len(head)
	}
	heldRoom := 0
	if n := len(p.a.Verdict.Held); n > 0 && avail >= 6 {
		// Blank, heading and at least one rule; a third of the page at most.
		heldRoom = min(n+2, max(avail/3, 3))
	}
	body := append(head, p.grantRows(w, max(avail-heldRoom, 2))...)
	body = append(body, p.heldLines(w, heldRoom)...)
	return frame(p.th, p.Title()+" · allow…", append(body, foot...), width, height)
}

// grantRows renders the offers as a window that keeps the focused row visible.
// Each offer is two lines (label and rule/scope), so the window counts pairs.
func (p *Approval) grantRows(w, room int) []string {
	if len(p.grants.items)*2 <= room {
		return p.grants.render(p.th, w)
	}
	// One row of the budget goes to the "… more below" line.
	rows := max((room-1)/2, 1)
	start := 0
	if p.grants.focus >= rows {
		start = p.grants.focus - rows + 1
	}
	end := min(start+rows, len(p.grants.items))
	// marks must be sliced alongside items or box() reads another row's flag
	// and the [x] lands on the wrong rule.
	win := list{items: p.grants.items[start:end], focus: p.grants.focus - start, marks: p.grants.marks[start:end]}
	out := win.render(p.th, w)
	if end < len(p.grants.items) {
		out = append(out, p.th.Subtle.Render(fmt.Sprintf("  … %d more below", len(p.grants.items)-end)))
	}
	return out
}

// heldLines names the rules this prompt did not offer because the user
// already has one covering that command.
//
// Deliberately factual rather than reassuring: "you already have this" says
// what is true without implying the call is allowed, which it is not — it is
// still waiting for an answer.
func (p *Approval) heldLines(w, room int) []string {
	held := p.a.Verdict.Held
	if len(held) == 0 || room < 2 {
		return nil
	}
	out := []string{"", p.th.Bold.Render("already allowed — not offered again")}
	shown := min(len(held), room-2) // the blank and the heading are rows too
	if shown < len(held) {
		shown-- // and so is the "… and N more" line
	}
	if shown < 1 {
		return nil
	}
	for _, h := range held[:shown] {
		line := "  " + h.Rule.String() + "  ·  you already have this"
		if h.Rule.String() != h.By.String() {
			line = "  " + h.Rule.String() + "  ·  covered by " + h.By.String()
		}
		out = append(out, p.th.Subtle.Render(ansiTrunc(line, w)))
	}
	if n := len(held) - shown; n > 0 {
		out = append(out, p.th.Subtle.Render(fmt.Sprintf("  … and %d more you already have", n)))
	}
	return out
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
