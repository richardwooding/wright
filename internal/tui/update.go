package tui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/composer"
	"github.com/richardwooding/wright/internal/tui/overlay"
	"github.com/richardwooding/wright/internal/tui/transcript"
)

// Update routes each message to a handler; the per-type handlers keep this
// function flat and each of them small.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.tr.InvalidateAll()
		m.refresh()
		return m, nil
	case tea.BackgroundColorMsg:
		m.setTheme(theme.New(msg.IsDark()))
		return m, nil
	case tea.KeyPressMsg:
		return m.onKey(msg)
	case tea.PasteMsg:
		return m.onPaste(msg)
	case tea.MouseWheelMsg:
		return m.onWheel(msg)
	case eventMsg:
		return m.onEvent(engine.Event(msg))
	case eventsClosedMsg:
		m.opts.Events = nil
		return m, nil
	case submitMsg:
		return m.submit(string(msg))
	}
	return m.onInternal(msg)
}

// onInternal handles ticks and the results of asynchronous commands.
func (m Model) onInternal(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case flushMsg:
		m.flushArmed = false
		m.refresh()
		return m, nil
	case gitTickMsg:
		return m.onGitTick()
	case gitMsg:
		m.git = git.Summary(msg)
		return m, m.gitTick(gitInterval)
	case hintExpiredMsg:
		if time.Since(m.lastCtrlC) >= ctrlCWindow {
			m.hint = ""
		}
		return m, nil
	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case commandDoneMsg:
		return m.onCommandDone(msg)
	case modelSetMsg:
		return m.onModelSet(msg)
	case sessionsMsg:
		return m.onSessions(msg)
	case exportedMsg:
		return m.onExported(msg)
	case pickedMsg:
		return m.onPicked(msg)
	}
	return m, nil
}

// setTheme swaps every themed component after BackgroundColorMsg.
func (m *Model) setTheme(th theme.Theme) {
	m.th = th
	m.tr.SetTheme(th)
	m.comp.SetTheme(th)
	m.refresh()
}

// onGitTick polls git only while idle: a run already keeps git busy.
func (m Model) onGitTick() (tea.Model, tea.Cmd) {
	if m.opts.Git == nil {
		return m, nil
	}
	if m.running {
		return m, m.gitTick(gitInterval)
	}
	ctx, fn := m.ctx, m.opts.Git
	return m, func() tea.Msg { return gitMsg(fn(ctx)) }
}

// onKey: an open overlay takes every key; otherwise global bindings, then
// the composer.
func (m Model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.ov != nil {
		return m.onOverlayMsg(msg)
	}
	if handled, model, cmd := m.onGlobalKey(msg); handled {
		return model, cmd
	}
	var cmd tea.Cmd
	var ev composer.Event
	m.comp, cmd, ev = m.comp.Update(msg)
	m.layout()
	if ev.Submitted {
		model, scmd := m.submit(ev.Text)
		return model, tea.Batch(cmd, scmd)
	}
	return m, cmd
}

// onGlobalKey handles bindings that work regardless of the composer's state.
func (m Model) onGlobalKey(msg tea.KeyPressMsg) (bool, tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		model, cmd := m.onCtrlC()
		return true, model, cmd
	case "ctrl+d":
		if strings.TrimSpace(m.comp.Value()) == "" {
			m.quitting = true
			return true, m, tea.Quit
		}
	case "esc":
		if m.comp.Popup() == composer.PopupNone && m.running {
			m.ctl.Cancel()
			m.notice("cancelling…", transcript.LevelWarn)
			return true, m, nil
		}
	case "ctrl+o":
		m.toolsExpanded = !m.toolsExpanded
		m.tr.SetToolsExpanded(m.toolsExpanded)
		m.refresh()
		return true, m, nil
	case "ctrl+t":
		m.showOverlay(overlay.NewTodos(m.todos, m.th))
		return true, m, nil
	case "shift+tab":
		m.cycleMode()
		return true, m, nil
	case "ctrl+l":
		return true, m, tea.ClearScreen
	case "alt+m":
		m.toggleMouse()
		return true, m, nil
	case "pgup", "pgdown", "shift+up", "shift+down":
		return true, m.scroll(msg.String()), nil
	}
	return false, m, nil
}

// toggleMouse turns cell-motion tracking on or off and says which way, since
// the cost (no terminal selection) and the benefit (wheel scroll) are both
// invisible in the frame itself.
func (m *Model) toggleMouse() {
	m.mouse = !m.mouse
	if m.mouse {
		m.notice("mouse on: wheel scrolls the transcript, but the terminal's own text selection is disabled", transcript.LevelInfo)
		return
	}
	m.notice("mouse off: select and copy with the terminal; scroll with shift+up/shift+down or pgup/pgdn", transcript.LevelInfo)
}

// onCtrlC quits on the second press within the window; the first press only
// shows a hint so a stray ctrl+c never loses a session.
func (m Model) onCtrlC() (tea.Model, tea.Cmd) {
	now := time.Now()
	if !m.lastCtrlC.IsZero() && now.Sub(m.lastCtrlC) < ctrlCWindow {
		m.quitting = true
		return m, tea.Quit
	}
	m.lastCtrlC = now
	m.hint = "press ctrl+c again to quit"
	return m, tea.Tick(ctrlCWindow, func(time.Time) tea.Msg { return hintExpiredMsg{} })
}

// scroll moves the transcript and stops following the tail until the user
// scrolls back to the bottom. shift+up/shift+down are the line-granularity
// pair that replaces the wheel when the mouse is off (the default).
func (m Model) scroll(key string) Model {
	switch key {
	case "pgup":
		m.vp.PageUp()
	case "pgdown":
		m.vp.PageDown()
	case "shift+up":
		m.vp.ScrollUp(1)
	case "shift+down":
		m.vp.ScrollDown(1)
	}
	m.follow = m.vp.AtBottom()
	return m
}

func (m Model) onWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	if m.ov != nil {
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	m.follow = m.vp.AtBottom()
	return m, cmd
}

func (m Model) onPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.ov != nil {
		return m.onOverlayMsg(msg)
	}
	var cmd tea.Cmd
	m.comp, cmd, _ = m.comp.Update(msg)
	m.layout()
	return m, cmd
}

// onOverlayMsg forwards to the overlay and closes it when it says so.
func (m Model) onOverlayMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	ov, cmd, done := m.ov.Update(msg)
	m.ov = ov
	if done {
		m.nextOverlay()
	}
	return m, cmd
}

// cycleMode walks default → auto-edit → plan → default. Bypass is never in
// the cycle; it is a flag, not a keystroke.
func (m *Model) cycleMode() {
	var next policy.Mode
	switch m.ctl.Mode() {
	case policy.ModeDefault:
		next = policy.ModeAutoEdit
	case policy.ModeAutoEdit:
		next = policy.ModePlan
	default:
		next = policy.ModeDefault
	}
	m.setMode(next)
}

// setMode asks the controller and reports the outcome in the transcript.
func (m *Model) setMode(mode policy.Mode) {
	if err := m.ctl.SetMode(mode); err != nil {
		m.notice("mode: "+err.Error(), transcript.LevelError)
		return
	}
	m.status = m.ctl.Status()
	m.notice("mode: "+mode.String(), transcript.LevelInfo)
}

// submit sends typed text: slash commands are run locally, anything else
// goes to the engine (which queues it when a run is active).
func (m Model) submit(text string) (tea.Model, tea.Cmd) {
	text = strings.TrimSpace(text)
	if text == "" {
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		return m.runCommand(text)
	}
	m.tr.Append(&transcript.User{Text: text})
	m.follow = true
	if err := m.ctl.Submit(text); err != nil {
		m.tr.Append(&transcript.Error{Text: err.Error()})
	} else if m.running {
		m.pending = append(m.pending, text)
	}
	m.layout()
	m.refresh()
	return m, nil
}

// notice appends a notice block and refreshes.
func (m *Model) notice(text string, level transcript.Level) {
	m.tr.Append(&transcript.Notice{Text: text, Level: level})
	m.refresh()
}
