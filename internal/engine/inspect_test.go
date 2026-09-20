package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
)

// blockingTool holds its call until release is closed, so a test can look at
// a session that is genuinely mid-call rather than one that has finished.
type blockingTool struct {
	agentkit.Tool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingTool(name string) *blockingTool {
	b := &blockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	b.Tool = agentkit.Func(name, "blocks", func(ctx context.Context, _ echoArgs) (string, error) {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	return b
}

// TestWaitingNamesTheApprovalTheSessionIsBlockedOn is the diagnostic the
// frozen-session report needed. An approval with nobody to answer it blocks
// its tool call, which blocks the step, which blocks the run — and none of
// that reaches the transcript, so the engine has to be able to say it.
func TestWaitingNamesTheApprovalTheSessionIsBlockedOn(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`),
		enginetest.TextResp("done"),
	}}
	f := newFixture(t, client, nil)
	if err := f.eng.Submit("edit a.go"); err != nil {
		t.Fatal(err)
	}
	var seen []engine.Waiting
	f.collect(t, func(ev engine.Event) {
		if ev.Kind != engine.KindApprovalRequest {
			return
		}
		// Read it while the prompt is genuinely outstanding: after the
		// reply the map is empty again, which is the answer to a different
		// question.
		seen = f.eng.Waiting()
		f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: true})
	})
	if len(seen) != 1 {
		t.Fatalf("Waiting() = %+v, want exactly the one prompt on screen", seen)
	}
	w := seen[0]
	if w.Tool != "edit_file" || w.ID == "" || w.Since.IsZero() {
		t.Errorf("Waiting()[0] = %+v, want the edit_file prompt with an id and a start time", w)
	}
	// The label has to identify the call. A preview title falls back to the
	// tool's own name, which beside the tool's name says nothing, so the
	// body's first line stands in — for bash, the command itself.
	if w.Title == "" || w.Title == w.Tool {
		t.Errorf("Waiting()[0].Title = %q; a dump could say only that something is waiting", w.Title)
	}
	if after := f.eng.Waiting(); len(after) != 0 {
		t.Errorf("Waiting() = %+v after the answer, want none", after)
	}
}

// TestInFlightNamesTheToolCallThatHasNotReturned is the other half: the
// user's original report was a bash call that never came back.
func TestInFlightNamesTheToolCallThatHasNotReturned(t *testing.T) {
	block := newBlockingTool("read_file")
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "read_file", `{"path":"a.go"}`),
		enginetest.TextResp("done"),
	}}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Tools = agentkit.Toolset{block, echoTool("edit_file")}
	})
	if err := f.eng.Submit("read a.go"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-block.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool was never called")
	}
	got := f.eng.InFlight()
	if len(got) != 1 {
		t.Fatalf("InFlight() = %+v, want the one call that is running", got)
	}
	if got[0].Tool != "read_file" || got[0].CallID == "" || got[0].Since.IsZero() {
		t.Errorf("InFlight()[0] = %+v", got[0])
	}
	close(block.release)
	f.collect(t, nil)
	if got := f.eng.InFlight(); len(got) != 0 {
		t.Errorf("InFlight() = %+v after the call returned, want none", got)
	}
}

// A session with nothing happening must report nothing happening, or a dump
// sends the reader after a hang that is not there.
func TestNothingWaitingWhenIdle(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("hi")}}, nil)
	if got := f.eng.Waiting(); len(got) != 0 {
		t.Errorf("Waiting() = %+v on an idle engine", got)
	}
	if got := f.eng.InFlight(); len(got) != 0 {
		t.Errorf("InFlight() = %+v on an idle engine", got)
	}
}
