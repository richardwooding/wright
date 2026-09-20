// Package tui is wright's interactive front end: a Bubble Tea v2 model with a
// transcript viewport, a composer, modal overlays and a status bar, plus the
// line-oriented --plain loop for pipes and screen readers. It talks to the
// engine only through the Controller interface and the Event channel, so a
// fake controller drives it completely in tests.
package tui

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/composer"
	"github.com/richardwooding/wright/internal/tui/markdown"
	"github.com/richardwooding/wright/internal/tui/overlay"
	"github.com/richardwooding/wright/internal/tui/transcript"
)

// Controller is what the UI needs from the engine. *engine.Engine satisfies
// it; tests use a recording fake.
type Controller interface {
	Submit(text string, parts ...core.Part) error
	Cancel()
	Reply(id string, d engine.Decision)
	Answer(id string, a engine.Answer)
	SetMode(policy.Mode) error
	Mode() policy.Mode
	SetModel(ctx context.Context, name string) error
	Compact() error
	Undo() (files []string, err error)
	Status() engine.Status
	Models(ctx context.Context) []ModelChoice
}

// ModelChoice is one row of the /model picker.
type ModelChoice struct {
	Name     string
	Provider string
	Known    bool // in the cost catalog
}

// SessionMeta is what /sessions lists.
type SessionMeta struct {
	ID      string
	Title   string
	Updated time.Time
	Turns   int
}

// SessionSource lists and exports sessions for /sessions, /resume and /export.
type SessionSource interface {
	List(ctx context.Context) ([]SessionMeta, error)
	Export(ctx context.Context, id, path string) error
}

// Options configures the UI.
type Options struct {
	Events        <-chan engine.Event
	Sessions      SessionSource                     // nil disables /sessions and /export
	Git           func(context.Context) git.Summary // polled every 5 s while idle
	Files         func() []string                   // "@" completion candidates
	Command       func(ctx context.Context, name string, args []string) (string, error)
	Version       string
	WorkspaceRoot string
	Plain         bool
	InitialPrompt string
	Warnings      []string // sandbox warnings shown as notices at start
	// DebugAddr is the diagnostics endpoint, empty when none is running. It
	// is shown in the status bar: a listening endpoint is a state the user
	// must be able to see, and an address they have to remember is one they
	// will not find when the session is the thing going wrong.
	DebugAddr string
	// In and Out are the --plain streams; nil means stdin and stdout.
	In  io.Reader
	Out io.Writer
}

// Timings. Deltas are coalesced so a fast stream re-renders the live block
// at most ~30 times a second; ctrl+c must be repeated within the window.
const (
	flushInterval = 33 * time.Millisecond
	gitInterval   = 5 * time.Second
	ctrlCWindow   = 1500 * time.Millisecond
)

// Messages internal to the UI.
type (
	eventMsg        engine.Event
	eventsClosedMsg struct{}
	flushMsg        struct{}
	gitTickMsg      struct{}
	gitMsg          git.Summary
	hintExpiredMsg  struct{}
	commandDoneMsg  struct {
		name string
		text string
		err  error
	}
	modelSetMsg struct {
		name string
		err  error
	}
	sessionsMsg struct {
		items  []SessionMeta
		resume bool // open the picker for /resume rather than /sessions
		err    error
	}
	exportedMsg struct {
		path string
		err  error
	}
)

// Model is the root Bubble Tea model.
type Model struct {
	ctx  context.Context
	ctl  Controller
	opts Options
	th   theme.Theme
	md   *markdown.Renderer

	width, height int
	vp            viewport.Model
	tr            *transcript.Model
	comp          composer.Model
	ov            overlay.Overlay
	// ovQueue holds overlays raised while another was open. Tool calls run
	// in parallel, so one step can raise several approvals at once, and each
	// one a caller is waiting on an answer for: an overlay that is replaced
	// instead of queued is a tool call that never gets an answer and a run
	// that never ends. See queueOverlay.
	ovQueue []overlay.Overlay
	spin    spinner.Model

	status engine.Status
	git    git.Summary
	todos  []engine.Todo

	running       bool
	live          *transcript.Assistant
	cards         map[string]*transcript.ToolCard
	lastCard      *transcript.ToolCard
	toolsExpanded bool
	queued        int      // as reported by the engine
	pending       []string // submitted while running, until the engine acknowledges

	flushArmed bool
	follow     bool // keep the viewport at the bottom
	mouse      bool // cell-motion tracking: off so the terminal keeps selection

	lastCtrlC   time.Time
	hint        string
	exitSummary string
	quitting    bool
}

// New builds the model. It renders nothing until the first WindowSizeMsg.
func New(ctl Controller, o Options) Model {
	th := theme.New(true) // dark until BackgroundColorMsg says otherwise
	md := markdown.New()
	m := Model{
		ctx:    context.Background(),
		ctl:    ctl,
		opts:   o,
		th:     th,
		md:     md,
		vp:     viewport.New(),
		tr:     transcript.New(th, md),
		cards:  map[string]*transcript.ToolCard{},
		follow: true,
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
	m.vp.MouseWheelEnabled = true
	m.vp.SoftWrap = false
	m.comp = composer.New(th, composer.Options{Files: o.Files, Commands: commandNames()})
	m.comp.Focus()
	if ctl != nil {
		m.status = ctl.Status()
	}
	for _, w := range o.Warnings {
		m.tr.Append(&transcript.Notice{Text: w, Level: transcript.LevelWarn})
	}
	return m
}

// Init starts the event pump, asks for the background colour and the first
// git summary, and queues the initial prompt.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitEvent(), tea.RequestBackgroundColor, m.gitTick(0)}
	if m.opts.InitialPrompt != "" {
		prompt := m.opts.InitialPrompt
		cmds = append(cmds, func() tea.Msg { return submitMsg(prompt) })
	}
	return tea.Batch(cmds...)
}

// submitMsg feeds the initial prompt through the same path as typed input.
type submitMsg string

// waitEvent blocks on the engine channel and re-arms itself on each message
// (gitlapse's channel pump). A closed channel stops the pump.
func (m Model) waitEvent() tea.Cmd {
	ch := m.opts.Events
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg(ev)
	}
}

// gitTick schedules the next git poll.
func (m Model) gitTick(after time.Duration) tea.Cmd {
	if m.opts.Git == nil {
		return nil
	}
	return tea.Tick(after, func(time.Time) tea.Msg { return gitTickMsg{} })
}

// ExitSummary is the text Run prints after the program exits: the last run's
// numbers and how to resume the session.
func (m Model) ExitSummary() string {
	var parts []string
	if m.exitSummary != "" {
		parts = append(parts, m.exitSummary)
	}
	if id := m.status.SessionID; id != "" {
		parts = append(parts, "session "+id+" — resume with wright --resume "+id)
	}
	return strings.Join(parts, "\n")
}

// View composes transcript (or overlay), queued strip, the rule-wrapped
// composer and the status bar, and places the terminal cursor in the
// composer.
func (m Model) View() tea.View {
	if m.width == 0 {
		v := tea.NewView("starting…")
		v.AltScreen = !m.opts.Plain
		v.WindowTitle = m.windowTitle()
		return v
	}
	vpH, stripH := m.viewportHeight()
	body := m.vp.View()
	if m.ov != nil {
		box := m.ov.View(max(m.width-2, 1), vpH)
		// An overlay has a minimum size (border, title) and Place does not
		// truncate, so on a short terminal the box can be taller than the
		// space it was given; without the clamp it pushes the composer and
		// the status bar off the screen.
		body = clampRows(lipgloss.Place(m.width, vpH, lipgloss.Center, lipgloss.Center, box), vpH)
	}
	parts := []string{body}
	if stripH > 0 {
		parts = append(parts, m.queuedStrip())
	}
	rule := m.rule()
	parts = append(parts, rule, m.comp.View(), rule, m.statusBar())
	v := tea.NewView(strings.Join(parts, "\n"))
	v.AltScreen = !m.opts.Plain
	// Cell-motion tracking hands every click and drag to the program, which
	// is what stops the terminal's own text selection and copy. It buys one
	// thing — wheel-scrolling the transcript — so it is opt-in (alt+m or
	// /mouse) and shift+up/shift+down scroll without it.
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	v.WindowTitle = m.windowTitle()
	if m.ov == nil {
		if c := m.comp.Cursor(); c != nil {
			// vpH + stripH rows above the composer, plus its own rule.
			c.Y += vpH + stripH + 1
			v.Cursor = c
		}
	}
	return v
}

// clampRows keeps at most n rows of a rendered block.
func clampRows(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// windowTitle names the workspace and marks a run in progress, so a wright
// left working in another tab says so from the window list.
func (m Model) windowTitle() string {
	title := "wright — " + filepath.Base(m.opts.WorkspaceRoot)
	if m.running {
		return theme.GlyphRunning + " " + title
	}
	return title
}

// rule is the divider drawn above and below the composer. U+2500 is one
// cell wide, so the rule is exactly the terminal's width — a lipgloss
// bordered box would add side columns the composer does not have.
func (m Model) rule() string {
	return m.th.Rule.Render(strings.Repeat("─", max(m.width, 1)))
}

// showOverlay puts ov on screen, or behind whatever is already there.
//
// Approvals and questions are raised by a goroutine blocked on the answer,
// so dropping one hangs that caller for as long as the session lives. Every
// overlay therefore queues rather than replaces; nextOverlay brings the next
// one up when the current one closes.
func (m *Model) showOverlay(ov overlay.Overlay) {
	if m.ov != nil {
		m.ovQueue = append(m.ovQueue, ov)
		return
	}
	m.ov = ov
}

// nextOverlay closes the current overlay and raises the next queued one.
func (m *Model) nextOverlay() {
	m.ov = nil
	if len(m.ovQueue) > 0 {
		m.ov, m.ovQueue = m.ovQueue[0], m.ovQueue[1:]
	}
}

// dropQueuedApproval removes an approval from the queue once it has been
// answered elsewhere — the engine decides for itself when a run is cancelled,
// and a stale prompt for a call that is already over would ask the user about
// something they can no longer affect.
func (m *Model) dropQueuedApproval(id string) {
	m.ovQueue = slices.DeleteFunc(m.ovQueue, func(ov overlay.Overlay) bool {
		ap, ok := ov.(*overlay.Approval)
		return ok && ap.ID() == id
	})
}

// viewportHeight is the transcript height after the fixed rows, and the
// queued strip's height (0 or 1). The fixed rows are the status bar and the
// two rules around the composer, which reports its own height.
func (m Model) viewportHeight() (vpH, stripH int) {
	if m.queued > 0 || len(m.pending) > 0 {
		stripH = 1
	}
	vpH = max(m.height-3-m.comp.Height()-stripH, 1)
	return vpH, stripH
}

// queuedStrip shows what is waiting for the current run to finish.
func (m Model) queuedStrip() string {
	n := max(m.queued, len(m.pending))
	text := "queued " + itoa(n)
	if len(m.pending) > 0 {
		text += ": " + strings.ReplaceAll(m.pending[0], "\n", " ")
	}
	return m.th.Warm.Render(truncate(text, m.width))
}

// layout resizes the viewport and composer for the current terminal size.
func (m *Model) layout() {
	m.comp.SetWidth(m.width)
	vpH, _ := m.viewportHeight()
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(vpH)
}

// refresh pushes the transcript into the viewport.
func (m *Model) refresh() {
	m.vp.SetContentLines(m.tr.Lines(m.width))
	if m.follow {
		m.vp.GotoBottom()
	}
}

// Run drives the UI to completion and returns the exit summary to print.
func Run(ctx context.Context, ctl Controller, o Options) (string, error) {
	if o.Plain {
		return runPlain(ctx, ctl, o, o.In, o.Out)
	}
	m := New(ctl, o)
	m.ctx = ctx
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil {
		return "", err
	}
	if fm, ok := final.(Model); ok {
		return fm.ExitSummary(), nil
	}
	return "", nil
}

// EventMsg wraps an engine event as the message Update consumes. The pump
// does this for the Events channel; it is exported so the app or a test can
// inject a synthetic event.
func EventMsg(ev engine.Event) tea.Msg { return eventMsg(ev) }

// FlushMsg is the coalescing tick that renders pending text deltas. Exported
// so tests can flush deterministically instead of waiting 33 ms.
func FlushMsg() tea.Msg { return flushMsg{} }
