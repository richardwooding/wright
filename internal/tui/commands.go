package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/tui/overlay"
	"github.com/richardwooding/wright/internal/tui/transcript"
)

// command is one slash command. run mutates the model in place and may
// return a command for asynchronous work.
type command struct {
	name string
	args string
	help string
	run  func(m *Model, args []string) tea.Cmd
}

// commands is the table behind "/" completion, /help and dispatch. Commands
// the engine has not wired yet go through Options.Command so the app can
// attach them without touching the UI.
var commands []command

// init fills the table; a var initialiser would form a cycle through
// cmdHelp → commandHelp → commands.
func init() {
	commands = []command{
		{"help", "", "show keys and commands", (*Model).cmdHelp},
		{"clear", "", "clear the transcript", (*Model).cmdClear},
		{"compact", "", "compact the context now", (*Model).cmdCompact},
		{"cost", "", "tokens, cost and context use", (*Model).cmdCost},
		{"diff", "", "show the workspace diff", hook("diff")},
		{"mode", "[plan|default|auto-edit]", "show or switch the permission mode", (*Model).cmdMode},
		{"model", "[name]", "pick or switch the model", (*Model).cmdModel},
		{"sessions", "", "list sessions", (*Model).cmdSessions},
		{"resume", "<id>", "resume a session", (*Model).cmdResume},
		{"export", "[path]", "export this session as markdown", (*Model).cmdExport},
		{"undo", "", "revert the last run's file changes", (*Model).cmdUndo},
		{"init", "", "write an AGENTS.md for this project", hook("init")},
		{"mcp", "", "MCP servers", hook("mcp")},
		{"skills", "", "loaded skills", hook("skills")},
		{"ps", "", "what is running right now", (*Model).cmdPs},
		{"jobs", "", "background commands this session started", hook("jobs")},
		{"debug", "[dump]", "diagnostics endpoint, and write a dump of this session", hook("debug")},
		{"agents", "", "the sub-agents this session can call", hook("agents")},
		{"todos", "", "show the task list", (*Model).cmdTodos},
		{"mouse", "", "toggle wheel scrolling (off keeps terminal selection)", (*Model).cmdMouse},
		{"audit", "", "audit log summary", hook("audit")},
		{"redaction", "on|off", "toggle secret redaction", hook("redaction")},
		{"reasoning", "", "show or hide model reasoning", (*Model).cmdReasoning},
		{"trust", "", "trusted project settings and servers", hook("trust")},
		{"plain", "", "plain mode is a start-up flag", (*Model).cmdPlain},
		{"quit", "", "exit wright", (*Model).cmdQuit},
	}
}

// commandNames feeds the composer's "/" completion.
func commandNames() []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.name
	}
	return names
}

// commandHelp is the /help table.
func commandHelp() []overlay.Entry {
	out := make([]overlay.Entry, len(commands))
	for i, c := range commands {
		name := "/" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		out[i] = overlay.Entry{Name: name, Desc: c.help}
	}
	return out
}

// keyHelp is the /help key table. Related keys share a row: the overlay is
// as tall as the transcript, so the table has to earn its rows.
var keyHelp = []overlay.Entry{
	{Name: "enter", Desc: "send (queued while a run is active)"},
	{Name: "shift+enter / alt+enter / ctrl+j", Desc: "newline"},
	{Name: "esc", Desc: "cancel the run · close an overlay (deny)"},
	{Name: "ctrl+c ctrl+c / ctrl+d", Desc: "quit (twice within 1.5 s) · quit on an empty box"},
	{Name: "ctrl+o", Desc: "expand / collapse all tool cards"},
	{Name: "ctrl+t", Desc: "todos"},
	{Name: "shift+tab", Desc: "cycle mode default → auto-edit → plan"},
	{Name: "pgup / pgdn · shift+↑ / shift+↓", Desc: "scroll the transcript a page · a line"},
	{Name: "alt+m or /mouse", Desc: "wheel scroll; off by default so the terminal can select text"},
	{Name: "ctrl+u / ctrl+l", Desc: "clear the box · redraw"},
	{Name: "@ / /", Desc: "file · command completion"},
}

// runCommand parses "/name args…" and dispatches it.
func (m Model) runCommand(text string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(strings.TrimPrefix(text, "/"))
	if len(fields) == 0 {
		return m, nil
	}
	name, args := fields[0], fields[1:]
	for _, c := range commands {
		if c.name == name {
			cmd := c.run(&m, args)
			m.layout()
			m.refresh()
			return m, cmd
		}
	}
	m.notice("unknown command /"+name+" — /help lists them", transcript.LevelWarn)
	return m, nil
}

// hook routes a command to Options.Command and shows the reply as a notice.
func hook(name string) func(*Model, []string) tea.Cmd {
	return func(m *Model, args []string) tea.Cmd {
		if m.opts.Command == nil {
			m.notice("/"+name+" is not available in this build", transcript.LevelWarn)
			return nil
		}
		ctx, fn := m.ctx, m.opts.Command
		return func() tea.Msg {
			text, err := fn(ctx, name, args)
			return commandDoneMsg{name: name, text: text, err: err}
		}
	}
}

func (m Model) onCommandDone(msg commandDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice("/"+msg.name+": "+msg.err.Error(), transcript.LevelError)
		return m, nil
	}
	if msg.text != "" {
		m.notice(msg.text, transcript.LevelInfo)
	}
	return m, nil
}

func (m *Model) cmdHelp([]string) tea.Cmd {
	m.showOverlay(overlay.NewHelp(m.th, keyHelp, commandHelp()))
	return nil
}

func (m *Model) cmdClear([]string) tea.Cmd {
	m.tr.Clear()
	m.live, m.lastCard = nil, nil
	m.cards = map[string]*transcript.ToolCard{}
	return nil
}

func (m *Model) cmdCompact([]string) tea.Cmd {
	if err := m.ctl.Compact(); err != nil {
		m.notice("compact: "+err.Error(), transcript.LevelError)
	}
	return nil
}

func (m *Model) cmdCost([]string) tea.Cmd {
	s := m.ctl.Status()
	m.status = s
	u := s.Usage
	text := fmt.Sprintf("tokens: %s in (%s cached) · %s out · %s total · cost %s",
		formatTokens(u.InputTokens), formatTokens(u.CachedInputTokens), formatTokens(u.OutputTokens),
		formatTokens(u.TotalTokens), formatCost(s.Cost, s.CostKnown))
	if s.ContextWindow > 0 {
		text += fmt.Sprintf(" · context %s of %s (%d%%)", formatTokens(s.ContextUsed), formatTokens(s.ContextWindow), int(s.ContextPct()*100+0.5))
	}
	if !s.CostKnown {
		text += " · price unknown for this model"
	}
	m.notice(text, transcript.LevelInfo)
	return nil
}

// cmdMode switches modes. Bypass needs the typed word and is still refused
// by the policy engine unless the process started with the flag; the
// refusal is shown, not hidden.
func (m *Model) cmdMode(args []string) tea.Cmd {
	if len(args) == 0 {
		m.notice("mode: "+m.ctl.Mode().String(), transcript.LevelInfo)
		return nil
	}
	mode, err := policy.ParseMode(args[0])
	if err != nil {
		m.notice(err.Error(), transcript.LevelError)
		return nil
	}
	if mode == policy.ModeBypass {
		m.showOverlay(overlay.NewConfirm("bypass permissions",
			"Bypass runs every tool without asking. The hard-deny set still applies and the sandbox stays on. Only a process started with --bypass-permissions can enter it.",
			"yes", m.th, func() tea.Cmd { return pick(pickBypass, "") }))
		return nil
	}
	m.setMode(mode)
	return nil
}

// cmdModel opens the picker or switches directly.
func (m *Model) cmdModel(args []string) tea.Cmd {
	if len(args) > 0 {
		return m.switchModel(args[0])
	}
	choices := m.ctl.Models(m.ctx)
	if len(choices) == 0 {
		m.notice("no models available — check provider credentials", transcript.LevelWarn)
		return nil
	}
	items := make([]overlay.Item, len(choices))
	for i, c := range choices {
		desc := c.Provider
		if !c.Known {
			desc += " · price unknown"
		}
		items[i] = overlay.Item{Label: c.Name, Desc: desc, Value: c.Name}
	}
	m.showOverlay(overlay.NewPicker("model", items, m.th, func(it overlay.Item) tea.Cmd { return pick(pickModel, it.Value) }))
	return nil
}

// switchModel runs SetModel off the UI goroutine (it may probe a provider).
func (m *Model) switchModel(name string) tea.Cmd {
	ctx, ctl := m.ctx, m.ctl
	return func() tea.Msg { return modelSetMsg{name: name, err: ctl.SetModel(ctx, name)} }
}

func (m Model) onModelSet(msg modelSetMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice("model: "+msg.err.Error(), transcript.LevelError)
		return m, nil
	}
	m.status = m.ctl.Status()
	m.notice("model: "+msg.name, transcript.LevelInfo)
	return m, nil
}

func (m *Model) cmdSessions([]string) tea.Cmd { return m.listSessions(false) }

func (m *Model) cmdResume(args []string) tea.Cmd {
	if len(args) == 0 {
		return m.listSessions(true)
	}
	return m.resume(args[0])
}

// listSessions fetches the list off the UI goroutine and opens a picker.
func (m *Model) listSessions(resume bool) tea.Cmd {
	if m.opts.Sessions == nil {
		m.notice("sessions are not available in this build", transcript.LevelWarn)
		return nil
	}
	ctx, src := m.ctx, m.opts.Sessions
	return func() tea.Msg {
		items, err := src.List(ctx)
		return sessionsMsg{items: items, resume: resume, err: err}
	}
}

func (m Model) onSessions(msg sessionsMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice("sessions: "+msg.err.Error(), transcript.LevelError)
		return m, nil
	}
	if len(msg.items) == 0 {
		m.notice("no sessions yet", transcript.LevelInfo)
		return m, nil
	}
	items := make([]overlay.Item, len(msg.items))
	for i, s := range msg.items {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		items[i] = overlay.Item{
			Label: s.ID + "  " + title,
			Desc:  fmt.Sprintf("%s · %d turns", s.Updated.Format("2006-01-02 15:04"), s.Turns),
			Value: s.ID,
		}
	}
	title := "sessions"
	if msg.resume {
		title = "resume"
	}
	m.showOverlay(overlay.NewPicker(title, items, m.th, func(it overlay.Item) tea.Cmd { return pick(pickResume, it.Value) }))
	return m, nil
}

// resume hands the ID to the app through the hook; the UI cannot swap
// transcripts by itself.
func (m *Model) resume(id string) tea.Cmd {
	return hook("resume")(m, []string{id})
}

func (m *Model) cmdExport(args []string) tea.Cmd {
	if m.opts.Sessions == nil {
		m.notice("export is not available in this build", transcript.LevelWarn)
		return nil
	}
	if len(args) > 0 {
		return m.export(args[0])
	}
	def := "wright-" + m.status.SessionID + ".md"
	m.showOverlay(overlay.NewInput("export session", "Write the transcript as markdown to:", def, m.th, func(path string) tea.Cmd { return pick(pickExport, path) }))
	return nil
}

func (m *Model) export(path string) tea.Cmd {
	ctx, src, id := m.ctx, m.opts.Sessions, m.status.SessionID
	return func() tea.Msg { return exportedMsg{path: path, err: src.Export(ctx, id, path)} }
}

func (m Model) onExported(msg exportedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice("export: "+msg.err.Error(), transcript.LevelError)
		return m, nil
	}
	m.notice("exported to "+msg.path, transcript.LevelInfo)
	return m, nil
}

func (m *Model) cmdUndo([]string) tea.Cmd {
	files, err := m.ctl.Undo()
	if err != nil {
		m.notice("undo: "+err.Error(), transcript.LevelError)
		return nil
	}
	if len(files) == 0 {
		m.notice("undo: nothing to revert", transcript.LevelInfo)
		return nil
	}
	m.notice(fmt.Sprintf("undo: reverted %d file(s): %s — shell side effects are not reverted", len(files), strings.Join(files, ", ")), transcript.LevelWarn)
	return nil
}

// cmdMouse is the same toggle as alt+m, for terminals where alt is awkward.
func (m *Model) cmdMouse([]string) tea.Cmd {
	m.toggleMouse()
	return nil
}

func (m *Model) cmdTodos([]string) tea.Cmd {
	m.showOverlay(overlay.NewTodos(m.todos, m.th))
	return nil
}

func (m *Model) cmdReasoning([]string) tea.Cmd {
	show := !m.tr.ShowReasoning()
	m.tr.SetShowReasoning(show)
	if show {
		m.notice("reasoning shown", transcript.LevelInfo)
	} else {
		m.notice("reasoning hidden", transcript.LevelInfo)
	}
	return nil
}

func (m *Model) cmdPlain([]string) tea.Cmd {
	m.notice("plain mode is chosen at start-up: run wright --plain (implied by NO_COLOR, TERM=dumb or a pipe)", transcript.LevelInfo)
	return nil
}

func (m *Model) cmdQuit([]string) tea.Cmd {
	m.quitting = true
	return tea.Quit
}

// pickKind says which overlay produced a pickedMsg.
type pickKind uint8

const (
	pickModel pickKind = iota
	pickResume
	pickExport
	pickBypass
)

// pickedMsg carries an overlay's result back into Update: overlays cannot
// mutate the root model, so they answer with a message.
type pickedMsg struct {
	kind  pickKind
	value string
}

// pick builds the command an overlay returns with its result.
func pick(kind pickKind, value string) tea.Cmd {
	return func() tea.Msg { return pickedMsg{kind: kind, value: value} }
}

// onPicked completes the action an overlay started.
func (m Model) onPicked(msg pickedMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg.kind {
	case pickModel:
		cmd = m.switchModel(msg.value)
	case pickResume:
		cmd = m.resume(msg.value)
	case pickExport:
		cmd = m.export(msg.value)
	case pickBypass:
		m.setMode(policy.ModeBypass)
	}
	m.refresh()
	return m, cmd
}

// cmdPs answers "what is it doing right now?" — the question a session that
// has gone quiet raises, and the one nothing else in the UI answers. A tool
// card shows a spinner but not how long it has been spinning, and a run
// waiting on the model looks identical to a run waiting on a tool.
func (m *Model) cmdPs([]string) tea.Cmd {
	var b strings.Builder
	running := m.runningCards()
	switch {
	case !m.running:
		b.WriteString("Not running.")
	case len(running) == 0:
		// Every tool call has returned, so the step is over and the run is
		// waiting on the model. Saying so is the whole point: this is the
		// state most easily mistaken for a stuck command.
		b.WriteString("Running — waiting for the model (no tool call in flight).")
	default:
		fmt.Fprintf(&b, "Running — %s in flight:", countOf(len(running), "tool call"))
		for _, c := range running {
			fmt.Fprintf(&b, "\n  %s%-10s %s", strings.Repeat("  ", c.Depth), c.Name, formatDuration(time.Since(c.Started)))
			if s := c.Summary(); s != "" {
				fmt.Fprintf(&b, "  %s", s)
			}
		}
	}
	if n := len(m.ovQueue); n > 0 {
		fmt.Fprintf(&b, "\n\n%s waiting behind the one on screen.", countOf(n, "more prompt"))
	}
	if m.queued > 0 {
		fmt.Fprintf(&b, "\n%s queued to send after this run.", countOf(m.queued, "message"))
	}
	b.WriteString("\n\nBackground commands started with the job tool are listed by /jobs.")
	m.notice(b.String(), transcript.LevelInfo)
	return nil
}

// runningCards lists the tool calls that have not returned, oldest first.
func (m Model) runningCards() []*transcript.ToolCard {
	var out []*transcript.ToolCard
	for _, c := range m.cards {
		if c.Status == transcript.StatusRunning {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *transcript.ToolCard) int { return a.Started.Compare(b.Started) })
	return out
}

// countOf renders "1 tool call" / "3 tool calls".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// formatDuration renders an elapsed time that is being read, not measured.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
