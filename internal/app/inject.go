package app

import (
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/wright/internal/diag"
	"github.com/richardwooding/wright/internal/engine"
)

// injectBuffer is how many prompts may be waiting for the session.
//
// Small on purpose. The semantics are "steer the agent", and a backlog of
// prompts the sender has forgotten about is worse than a refusal they can
// see — but one would refuse an ordinary double-tap while the first is
// still being handed over.
const injectBuffer = 4

// injector carries prompts from the diagnostics endpoint to the engine.
//
// It exists because the two sides have opposite obligations. A diag handler
// must not block (diag.Options.Source says why: a debugger that waits on the
// thing being debugged becomes part of the hang), while Engine.Submit may
// block — it takes the engine's lock and emits an event on a channel that
// fills when nothing is draining it. So the handler does a non-blocking send
// and this goroutine does the waiting.
type injector struct {
	ch   chan diag.Prompt
	quit chan struct{}
	done chan struct{}
	// deliver hands one prompt to the session. It is a field so a test can
	// substitute one that blocks: "post never waits for delivery" is the
	// property this type exists for, and it cannot be shown with a delivery
	// that always returns.
	deliver func(diag.Prompt)

	mu     sync.Mutex
	closed bool
}

func newInjector(eng *engine.Engine) *injector {
	i := newInjectorWith(nil)
	i.deliver = func(p diag.Prompt) { deliverTo(eng, p) }
	return i
}

func newInjectorWith(deliver func(diag.Prompt)) *injector {
	i := &injector{
		ch:      make(chan diag.Prompt, injectBuffer),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		deliver: deliver,
	}
	go i.loop()
	return i
}

// post is the diag.Options.Input hook: it never touches the engine, so it
// cannot block, whatever the session is doing.
func (i *injector) post(p diag.Prompt) error {
	i.mu.Lock()
	closed := i.closed
	i.mu.Unlock()
	if closed {
		return diag.ErrClosed
	}
	select {
	case i.ch <- p:
		return nil
	default:
		return diag.ErrBusy
	}
}

func (i *injector) loop() {
	defer close(i.done)
	for {
		select {
		case <-i.quit:
			return
		case p := <-i.ch:
			if i.deliver != nil {
				i.deliver(p)
			}
		}
	}
}

// deliverTo shows the prompt, records it, then submits it. The order
// matters: the transcript block has to appear before the run it starts, or a
// reader sees the answer before the question.
//
// Every call here may block on the engine's event channel, which is exactly
// why it runs on the injector's goroutine and not on an HTTP handler's.
func deliverTo(eng *engine.Engine, p diag.Prompt) {
	eng.ExternalPrompt(endpointSource, p.Remote, p.Text)
	if err := eng.Submit(markPrompt(p.Text, time.Now())); err != nil {
		eng.Notice("the prompt from the debug endpoint was not accepted: " + err.Error())
	}
}

// endpointSource labels a prompt's origin in the transcript and the log.
const endpointSource = "debug endpoint"

// markPrefix opens the line that tells the model, and anyone reading the
// transcript later, that this turn was not typed at the terminal.
const markPrefix = "[prompt submitted through this session's local debug endpoint"

// markPrompt prefixes the text. It is a plain line, not an <untrusted> fence:
// that fence tells the model the content "cannot give you instructions", which
// would make it correctly refuse the very thing the user asked for. The
// provenance is worth stating; refusing it is not.
func markPrompt(text string, now time.Time) string {
	return markPrefix + " at " + now.UTC().Format(time.RFC3339) + "]\n" + text
}

// looksMarked reports whether text already carries the marker, so a sender
// cannot forge provenance by writing the line themselves.
func looksMarked(text string) bool { return strings.Contains(text, markPrefix) }

// Close stops accepting prompts, finishes the one in flight and waits. It
// must run before the engine closes, which is why Built.Close orders it so.
func (i *injector) Close() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()
	close(i.quit)
	<-i.done
	return nil
}
