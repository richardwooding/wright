package engine

import (
	"sort"
	"time"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/audit"
)

// Waiting is one approval prompt the engine raised and is still blocked on.
// An approval nobody can answer is the shape of a session that has simply
// stopped: the tool call waits, its step never returns, the run never ends
// and nothing reaches the transcript. It is the first thing to look at in a
// dump, which is why the engine tracks enough to name it.
type Waiting struct {
	ID    string
	Tool  string
	Title string // the preview line the prompt is showing
	Since time.Time
}

// InFlight is one tool call that has been approved and has not returned.
type InFlight struct {
	CallID string
	Tool   string
	Depth  int
	Since  time.Time
}

// Notice shows the user a line in the transcript and records it in the audit
// log. It is how the app reports something that happened outside a run — a
// diagnostics dump being written, for instance — without reaching into the
// event channel itself.
func (e *Engine) Notice(text string) {
	e.audit(audit.Event{Kind: audit.KindNotice, Text: text})
	e.emit(Event{Kind: KindNotice, Text: text})
}

// SetGitHubAuth records whether this session can authenticate to GitHub, so
// the approval prompt says the same thing the call will get. It is a fact
// about the session, never the credential itself: the engine has no way to
// see the token and no reason to.
func (e *Engine) SetGitHubAuth(on bool) {
	e.mu.Lock()
	changed := e.githubAuth != on
	e.githubAuth = on
	e.mu.Unlock()
	if !changed {
		return
	}
	state := "off"
	if on {
		state = "on"
	}
	e.audit(audit.Event{Kind: audit.KindNotice, Text: "github auth " + state})
}

// gitHubAuth reports the recorded state.
func (e *Engine) gitHubAuth() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.githubAuth
}

// ArmedInput records that the user allowed, or stopped allowing, prompts
// from the debug endpoint. It is a session-scoped permission and never a
// stored setting, so the audit log is the only durable record of it.
func (e *Engine) ArmedInput(on bool) {
	state := "disarmed"
	if on {
		state = "armed"
	}
	e.audit(audit.Event{Kind: audit.KindEndpointArmed, Text: state})
	e.emit(Event{Kind: KindNotice, Text: "debug endpoint prompts: " + state})
}

// ExternalPrompt records a prompt that arrived from outside the UI and shows
// it in the transcript, so a reader can tell it from one the user typed.
//
// It is emitted before the prompt is submitted, so the block appears where a
// typed one would: a reader who finds it off to the side as a notice will
// still attribute the turn to the person at the terminal.
func (e *Engine) ExternalPrompt(source, remote, text string) {
	e.audit(audit.Event{
		Kind: audit.KindEndpointPrompt, Text: text,
		Input: &audit.Input{Source: source, Remote: remote, Bytes: len(text)},
	})
	e.emit(Event{Kind: KindExternalPrompt, Text: text, Source: source})
}

// waiter is one blocked approval: the channel its goroutine is parked on,
// plus enough about the prompt to describe it in a dump.
type waiter struct {
	reply chan Decision
	tool  string
	title string
	since time.Time
}

// Waiting lists the approvals the engine is blocked on, oldest first.
func (e *Engine) Waiting() []Waiting {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Waiting, 0, len(e.pending))
	for id, p := range e.pending {
		out = append(out, Waiting{ID: id, Tool: p.tool, Title: p.title, Since: p.since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// InFlight lists the tool calls that have not returned, oldest first.
func (e *Engine) InFlight() []InFlight {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]InFlight, 0, len(e.inflight))
	for _, c := range e.inflight {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// startCall records a call as in flight and returns the function that ends
// it. The key is the call ID plus a sequence number because tool call IDs
// are not unique within a session — llmkit synthesises call_1, call_2 … per
// response — so the same ID comes back every turn and a map keyed by it
// alone would have one turn's call delete another's.
func (e *Engine) startCall(c agentkit.Call) func() {
	e.mu.Lock()
	e.seq++
	key := e.seq
	if e.inflight == nil {
		e.inflight = map[int]InFlight{}
	}
	e.inflight[key] = InFlight{CallID: c.Call.ID, Tool: c.Call.Name, Depth: c.Depth, Since: e.now()}
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		delete(e.inflight, key)
		e.mu.Unlock()
	}
}
