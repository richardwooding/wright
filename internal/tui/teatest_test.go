package tui_test

import (
	"bytes"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/tui"
)

// TestEndToEnd drives a real Program: the event pump reads the channel, the
// approval overlay appears, "y" answers it and ctrl+c twice quits.
func TestEndToEnd(t *testing.T) {
	events := make(chan engine.Event, 32)
	ctl := &fakeController{events: events}
	m := tui.New(ctl, tui.Options{Events: events, WorkspaceRoot: "/ws/wright"})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))

	tm.Type("hello")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	// The overlay covers the transcript, so the model's text is only visible
	// once the approval is answered.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("[y] allow once"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte("hello from model")) && bytes.Contains(b, []byte("✓ ok")) && bytes.Contains(b, []byte("steps 2"))
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	final, ok := tm.FinalModel(t).(tui.Model)
	if !ok {
		t.Fatalf("final model is %T", tm.FinalModel(t))
	}
	if got := final.ExitSummary(); !bytes.Contains([]byte(got), []byte("steps 2")) {
		t.Errorf("exit summary %q", got)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.submits) != 1 || ctl.submits[0] != "hello" || len(ctl.replies) != 1 || !ctl.replies[0].d.Allow {
		t.Errorf("controller saw submits=%v replies=%+v", ctl.submits, ctl.replies)
	}
}
