package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/cost"
	"github.com/richardwooding/wright/internal/git"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/snapshot"
	"github.com/richardwooding/wright/internal/workspace"
)

// DescribeFunc turns a tool call into a policy request and a preview before
// the call runs. The app layer supplies it so the engine never imports the
// tools package; ok is false for tools it does not know (MCP tools, for
// example, are described by their prefix).
type DescribeFunc func(name string, args []byte) (req policy.Request, preview Preview, ok bool, err error)

// Options configure an Engine. Client is for tests and custom providers; when
// nil the model is opened through llmkit by name.
type Options struct {
	Model      model.Choice
	Client     core.Chatter
	FastClient core.Chatter // summaries and titles; nil = same as Client
	Settings   config.Settings
	WS         *workspace.Workspace
	Cwd        string
	// CwdNow reports the shell's *live* working directory, which bash's `cd`
	// moves. Cwd is where it started and the fallback for callers that track
	// nothing. It is a func because the tracker lives in tools, which must
	// never be imported here; app owns both and wires them together.
	//
	// Without it the environment block told the model a directory that was
	// snapshotted at session build and never moved again — so a model that
	// had been told "the working directory persists" was handed a value it
	// could not rely on, and re-established it with a `cd` on every call.
	CwdNow        func() string
	Mode          policy.Mode
	Policy        *policy.Engine
	Tools         agentkit.Toolset
	ReadOnlyTools []string // tool names plan mode keeps
	Describe      DescribeFunc
	Extra         []agentkit.Option // skills.Use, MCP tools, anything app-level
	Instructions  []prompt.Instruction
	ToolDocs      []prompt.ToolDoc
	Store         *session.Store
	SessionID     string
	Audit         *audit.Log
	Snapshots     *snapshot.Store
	Sandbox       string
	SandboxNet    bool
	Warnings      []string
	Headless      bool
	Git           func(ctx context.Context) git.Summary
	ContextWindow int
	MaxSteps      int
	Timeout       time.Duration
	Bypass        bool
	Now           func() time.Time
	Version       string
	OS, Arch      string
	Shell         string
}

// Engine owns the agentkit Agent, fans its events into one channel, answers
// approval requests and tracks cost, context use and session metadata.
type Engine struct {
	opts Options

	mu       sync.Mutex
	client   core.Chatter
	fast     core.Chatter
	choice   model.Choice
	mode     policy.Mode
	policy   *policy.Engine
	session  string
	meta     session.Meta
	running  bool
	cancel   context.CancelFunc
	runDone  chan struct{}
	lastRun  string
	lastIn   int // last prompt size in tokens
	queued   int
	inbox    *agentkit.Inbox
	todos    []Todo
	pending  map[string]*waiter
	inflight map[int]InFlight
	// githubAuth is whether this session can authenticate to GitHub. It is
	// shown in the approval prompt; the credential itself lives in the
	// tools layer and never reaches here.
	githubAuth bool
	answers    map[string]chan Answer
	grants     map[string]CallGrant
	previews   map[string]Preview
	seq        int
	ctxWin     int
	hardDeny   int

	meter  cost.Meter
	events chan Event
	closed chan struct{}
	now    func() time.Time
}

// New builds an Engine. It opens the model client (unless Options.Client is
// set) so a bad model name fails here, before any UI starts.
func New(ctx context.Context, o Options) (*Engine, error) {
	if o.WS == nil {
		return nil, errors.New("engine: workspace is required")
	}
	if o.Policy == nil {
		o.Policy = policy.New(o.WS, o.Mode, policy.Builtin())
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxSteps == 0 {
		o.MaxSteps = 200
	}
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Hour
	}
	e := &Engine{
		opts:     o,
		client:   o.Client,
		fast:     o.FastClient,
		choice:   o.Model,
		mode:     o.Mode,
		policy:   o.Policy,
		session:  o.SessionID,
		inbox:    agentkit.NewInbox(),
		pending:  map[string]*waiter{},
		inflight: map[int]InFlight{},
		answers:  map[string]chan Answer{},
		grants:   map[string]CallGrant{},
		previews: map[string]Preview{},
		events:   make(chan Event, 256),
		closed:   make(chan struct{}),
		now:      o.Now,
	}
	if e.client == nil {
		c, err := openClient(o.Model.Model)
		if err != nil {
			return nil, err
		}
		e.client = c
	}
	if e.fast == nil {
		e.fast = e.client
	}
	e.ctxWin, _ = model.ContextWindow(o.Model, o.ContextWindow)
	if o.Store != nil && o.SessionID != "" {
		if m, ok, err := o.Store.Get(ctx, o.SessionID); err == nil && ok {
			e.meta = m
		}
	}
	if e.meta.ID == "" {
		e.meta = session.Meta{ID: o.SessionID, Model: o.Model.Model, Workspace: o.WS.Roots[0], Created: e.now()}
	}
	// Record the session before the first run rather than after it. The
	// sidecar was written only when a run ended, so for the whole of the
	// first run the session the status bar was naming did not exist as far
	// as `/sessions` and `/export` were concerned: one listed nothing and
	// the other reported "no such session" for the id on screen. A session
	// exists once it has been started. (Its transcript is agentkit's to
	// write, and lands a completed step at a time — see CLAUDE.md.)
	if err := e.touch(ctx); err != nil {
		e.emit(Event{Kind: KindNotice, Text: "session metadata: " + err.Error()})
	}
	e.audit(audit.Event{Kind: audit.KindSessionStart, Text: "model " + o.Model.Model + " mode " + o.Mode.String() + " sandbox " + o.Sandbox})
	for _, w := range o.Warnings {
		e.emit(Event{Kind: KindNotice, Text: w})
	}
	return e, nil
}

func openClient(name string) (core.Chatter, error) {
	a, err := agentkit.New(name)
	if err != nil {
		return nil, fmt.Errorf("engine: open model %q: %w", name, err)
	}
	return a.Client(), nil
}

// Events is the stream the UIs consume. It is closed by Close.
func (e *Engine) Events() <-chan Event { return e.events }

// SessionID returns the current session.
func (e *Engine) SessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session
}

// Mode returns the permission mode.
func (e *Engine) Mode() policy.Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode switches the permission mode for the next run. Bypass is rejected;
// it is only set at construction from an explicit flag.
func (e *Engine) SetMode(m policy.Mode) error {
	if err := e.policy.SetMode(m); err != nil {
		return err
	}
	e.mu.Lock()
	e.mode = m
	e.meta.Mode = m.String()
	e.mu.Unlock()
	// Record it now. The sidecar was only rewritten when a run ended, so a
	// mode changed before the first run — the usual moment, with shift+tab
	// before typing — was lost if that run never finished, and the session
	// came back in the wrong mode.
	e.touchQuietly()
	e.audit(audit.Event{Kind: audit.KindNotice, Text: "mode " + m.String()})
	e.emit(Event{Kind: KindNotice, Text: "permission mode: " + m.String()})
	return nil
}

// SetModel switches the model for the next run on the same session. The
// transcript is kept; reasoning blocks from the old model are stripped when
// the history is compacted.
func (e *Engine) SetModel(_ context.Context, name string) error {
	c, err := openClient(name)
	if err != nil {
		return err
	}
	provider, _ := model.Resolve(name)
	choice := model.Choice{Model: name, Provider: provider, Info: catalogFor(name), Source: model.SourceFlag}
	e.mu.Lock()
	e.client = c
	e.choice = choice
	e.ctxWin, _ = model.ContextWindow(choice, e.opts.ContextWindow)
	e.meta.Model = name
	e.mu.Unlock()
	e.touchQuietly()
	e.audit(audit.Event{Kind: audit.KindNotice, Text: "model " + name})
	e.emit(Event{Kind: KindNotice, Text: "model: " + name})
	return nil
}

// Model returns the current model choice.
func (e *Engine) Model() model.Choice {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.choice
}

// Status snapshots the engine state for the status bar.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	usage, usd, known := e.meter.Total()
	return Status{
		Model:         e.choice.Model,
		Provider:      e.choice.Provider,
		Mode:          e.mode,
		Sandbox:       e.opts.Sandbox,
		SandboxNet:    e.opts.SandboxNet,
		ContextWindow: e.ctxWin,
		ContextUsed:   e.lastIn,
		Usage:         usage,
		Cost:          usd,
		CostKnown:     known,
		SessionID:     e.session,
		Running:       e.running,
		Queued:        e.queued,
		Bypass:        e.opts.Bypass,
	}
}

// Todos returns the model's current task list.
func (e *Engine) Todos() []Todo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Todo(nil), e.todos...)
}

// SetTodos replaces the task list and tells the UI. The todo tool calls it
// through the app layer.
func (e *Engine) SetTodos(todos []Todo) {
	e.mu.Lock()
	e.todos = append([]Todo(nil), todos...)
	e.mu.Unlock()
	e.emit(Event{Kind: KindTodos, Todos: todos})
}

// Notify pushes an app-level event (a redaction notice, a warning) into the
// stream.
func (e *Engine) Notify(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	e.emit(ev)
}

// Redacted reports secret redactions in a tool result; the tools package
// calls it through the app layer.
func (e *Engine) Redacted(tool string, names []string) {
	e.audit(audit.Event{Kind: audit.KindRedaction, Tool: &audit.Tool{Name: tool}, Text: fmt.Sprint(names)})
	e.emit(Event{Kind: KindRedacted, Text: fmt.Sprintf("%s: %d secret(s) redacted before reaching the model (%v)", tool, len(names), names)})
}

// Close cancels any run, flushes session metadata and closes the event
// channel.
func (e *Engine) Close() error {
	e.Cancel()
	e.mu.Lock()
	done := e.runDone
	e.mu.Unlock()
	if done != nil {
		<-done
	}
	select {
	case <-e.closed:
		return nil
	default:
		close(e.closed)
	}
	err := e.touch(context.Background())
	if e.opts.Audit != nil {
		if cerr := e.opts.Audit.Close(); err == nil {
			err = cerr
		}
	}
	close(e.events)
	return err
}

func (e *Engine) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	select {
	case <-e.closed:
	case e.events <- ev:
	}
}

func (e *Engine) audit(ev audit.Event) {
	if e.opts.Audit == nil {
		return
	}
	e.mu.Lock()
	ev.Session = e.session
	if ev.Run == "" {
		ev.Run = e.lastRun
	}
	e.mu.Unlock()
	if err := e.opts.Audit.Write(ev); err != nil {
		e.emit(Event{Kind: KindNotice, Text: "audit log: " + err.Error()})
	}
}

// touchQuietly records the session metadata for a change the user made
// directly. Failing to write it is worth saying but must not fail the change
// itself: the mode is already set in the engine either way.
func (e *Engine) touchQuietly() {
	if err := e.touch(context.Background()); err != nil {
		e.emit(Event{Kind: KindNotice, Text: "session metadata: " + err.Error()})
	}
}

func (e *Engine) touch(ctx context.Context) error {
	if e.opts.Store == nil {
		return nil
	}
	e.mu.Lock()
	m := e.meta
	m.Updated = e.now()
	m.Usage, m.CostUSD, _ = e.meter.Total()
	m.Mode = e.mode.String()
	m.Model = e.choice.Model
	e.mu.Unlock()
	return e.opts.Store.Touch(ctx, m)
}
