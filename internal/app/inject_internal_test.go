package app

import (
	"errors"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/diag"
)

// TestPostNeverWaitsForDelivery is the property the hand-off exists for. A
// diag handler must not block — the package says so about Source, and the
// same goes for Input — while delivery legitimately can: Engine.Submit takes
// the engine's lock and emits on a channel that fills when nothing drains
// it. So delivery is made to block here, and post must still return.
func TestPostNeverWaitsForDelivery(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	inj := newInjectorWith(func(diag.Prompt) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	defer func() { close(release); _ = inj.Close() }()

	// The first prompt is taken by the goroutine and wedges there.
	if err := inj.post(diag.Prompt{Text: "one"}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery goroutine never started")
	}

	// Everything after that is answered by the buffer, and then refused —
	// promptly, either way.
	var refused bool
	for i := range injectBuffer + 4 {
		done := make(chan error, 1)
		go func() { done <- inj.post(diag.Prompt{Text: "more"}) }()
		select {
		case err := <-done:
			switch {
			case err == nil:
			case errors.Is(err, diag.ErrBusy):
				refused = true
			default:
				t.Fatalf("post %d: unexpected error %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("post %d blocked; the handler would have hung with it", i)
		}
	}
	if !refused {
		t.Error("the buffer never filled, so the refusal path was never taken")
	}
}

// Close must stop the goroutine and refuse anything later, so a prompt
// accepted during shutdown is never submitted to a session that has gone.
func TestInjectorCloseStopsAcceptingAndJoins(t *testing.T) {
	var delivered int
	inj := newInjectorWith(func(diag.Prompt) { delivered++ })
	if err := inj.post(diag.Prompt{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- inj.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the goroutine")
	}
	if err := inj.post(diag.Prompt{Text: "late"}); !errors.Is(err, diag.ErrClosed) {
		t.Errorf("post after Close = %v, want ErrClosed", err)
	}
	if err := inj.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// The marker is what tells the model and a later reader that the turn was
// not typed at the terminal, and a sender must not be able to forge it.
func TestPromptMarking(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	got := markPrompt("do the thing", at)
	if !looksMarked(got) {
		t.Errorf("markPrompt produced something looksMarked does not recognise: %q", got)
	}
	if !contains(got, "do the thing") || !contains(got, "2026-09-20T12:00:00Z") {
		t.Errorf("marked prompt = %q", got)
	}
	if looksMarked("an ordinary prompt") {
		t.Error("looksMarked matched text with no marker")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
