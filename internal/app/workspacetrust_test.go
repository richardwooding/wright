package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/headless"
	"github.com/richardwooding/wright/internal/trust"
)

// interactiveOpts is a run shaped as if a terminal were attached: the only
// shape in which the workspace-trust question is ever asked.
func interactiveOpts(ws string) app.RunOptions {
	o := baseOpts(ws)
	o.Print, o.IsTerminal, o.Output = false, true, ""
	return o
}

// noUI stands in for the TUI. It records that Build got far enough to hand
// over a session, which is what "the declined run started nothing" means.
func noUI(started *bool) app.Interactive {
	return func(context.Context, *engine.Engine, app.InteractiveDeps) (string, error) {
		*started = true
		return "", nil
	}
}

// sessionCount counts the sessions recorded for a workspace.
func sessionCount(t *testing.T, ws string) int {
	t.Helper()
	store, _, err := app.OpenStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(metas)
}

// workspaceTrusted reports what trust.json says about a workspace.
func workspaceTrusted(ws string) bool {
	return trust.Open(config.DefaultPaths(ws).TrustFile()).WorkspaceTrusted(ws)
}

// TestWorkspaceTrustDeclinedExitsWithoutStarting pins the user's side of the
// bargain: the question is real. Declining ends the process with its own
// exit code, no UI runs and no session is left behind.
func TestWorkspaceTrustDeclinedExitsWithoutStarting(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{text: "hi"}})
	var asked []string
	o := interactiveOpts(ws)
	o.Confirm = func(q string) bool {
		asked = append(asked, q)
		return false
	}
	var stdout, stderr bytes.Buffer
	o.Stdout, o.Stderr = &stdout, &stderr
	started := false
	code, err := app.Run(context.Background(), o, noUI(&started))
	if err != nil {
		t.Fatalf("declining is not a failure: %v", err)
	}
	if code != app.ExitTrustDeclined {
		t.Errorf("exit code = %d, want %d", code, app.ExitTrustDeclined)
	}
	if started {
		t.Error("the UI was started for a run the user declined")
	}
	if n := sessionCount(t, ws); n != 0 {
		t.Errorf("a declined run created %d session(s)", n)
	}
	if len(asked) != 1 {
		t.Fatalf("expected exactly one question, got %d: %v", len(asked), asked)
	}
	// The question has to say what trust grants and what it does not, or
	// the consent is to something the user never read.
	for _, want := range []string{"edit files", "shell commands", "outside this directory", "exits without starting"} {
		if !strings.Contains(asked[0], want) {
			t.Errorf("the prompt does not mention %q:\n%s", want, asked[0])
		}
	}
	note := stderr.String()
	if !strings.Contains(note, "not trusted") || !strings.Contains(note, "--trust") {
		t.Errorf("the exit message must say what happened and how to trust it: %q", note)
	}
	// Nothing may be recorded for a directory the user refused.
	if workspaceTrusted(ws) {
		t.Error("declining recorded the workspace as trusted")
	}
}

// TestWorkspaceTrustAcceptedIsRecordedAndApplied covers the other
// direction: accepting records the directory, does not ask again, and hands
// the policy engine the baseline so an ordinary edit stops prompting.
func TestWorkspaceTrustAcceptedIsRecordedAndApplied(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{text: "hi"}})
	o := interactiveOpts(ws)
	asked := 0
	o.Confirm = func(string) bool { asked++; return true }
	started := false
	code, err := app.Run(context.Background(), o, noUI(&started))
	if err != nil || code != headless.ExitOK {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if !started || asked != 1 {
		t.Fatalf("started=%v asked=%d", started, asked)
	}
	if !workspaceTrusted(ws) {
		t.Fatal("accepting did not record the workspace")
	}
	// A second run in the same directory must not ask again.
	second := interactiveOpts(ws)
	second.Confirm = func(q string) bool {
		t.Errorf("asked again in an accepted directory: %s", q)
		return false
	}
	if code, err = app.Run(context.Background(), second, noUI(&started)); err != nil || code != headless.ExitOK {
		t.Fatalf("second Run = %d, %v", code, err)
	}
	// And the engine it builds really carries the baseline: an ordinary
	// edit inside the workspace is approved with nobody there to ask.
	b, err := app.Build(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if !b.WorkspaceTrusted {
		t.Error("Built.WorkspaceTrusted = false in an accepted directory")
	}
	go func() {
		//nolint:revive // drained so an unexpected approval request cannot block the test
		for range b.Engine.Events() {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	call := agentkit.Call{Call: core.ToolCall{ID: "c1", Name: "edit_file", Arguments: json.RawMessage(editArgs)}}
	d, err := b.Engine.Approve(ctx, call)
	if err != nil {
		t.Fatalf("Approve: %v (an edit inside a trusted workspace must not wait for a prompt)", err)
	}
	if !d.Allow {
		t.Errorf("edit inside a trusted workspace = %+v, want allow", d)
	}
}

// TestTrustFlagAnswersTheQuestion pins the scripted equivalent: --trust
// accepts the directory without a prompt, and records it.
func TestTrustFlagAnswersTheQuestion(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{text: "hi"}})
	o := interactiveOpts(ws)
	o.TrustWorkspace = true
	o.Confirm = func(q string) bool {
		t.Errorf("--trust must answer the question, not ask it: %s", q)
		return false
	}
	started := false
	code, err := app.Run(context.Background(), o, noUI(&started))
	if err != nil || code != headless.ExitOK || !started {
		t.Fatalf("Run = %d, %v, started=%v", code, err, started)
	}
	if !workspaceTrusted(ws) {
		t.Error("--trust did not record the workspace")
	}
}

// TestFirstRunWithSettingsAsksOnce pins the fusion: a directory that is both
// unaccepted and ships settings asks one question naming both, and one
// answer records both.
func TestFirstRunWithSettingsAsksOnce(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{text: "hi"}})
	writeFile(t, filepath.Join(ws, ".wright", "settings.json"), `{"permissions":{"allow":["bash(go test *)"]}}`)
	o := interactiveOpts(ws)
	var asked []string
	o.Confirm = func(q string) bool {
		asked = append(asked, q)
		return true
	}
	started := false
	if code, err := app.Run(context.Background(), o, noUI(&started)); err != nil || code != headless.ExitOK {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if len(asked) != 1 {
		t.Fatalf("a first run must ask once, not %d times: %v", len(asked), asked)
	}
	for _, want := range []string{"Trust this directory", "settings", "bash(go test *)"} {
		if !strings.Contains(asked[0], want) {
			t.Errorf("the fused question does not mention %q:\n%s", want, asked[0])
		}
	}
	if !workspaceTrusted(ws) {
		t.Error("the fused answer did not record the workspace")
	}
	eff, err := app.LoadEffective(ws, os.Getenv)
	if err != nil || !eff.Trusted {
		t.Errorf("the fused answer did not accept the settings: %v %v", eff.Trusted, err)
	}
}

// TestWorkspaceTrustDoesNotVouchForSettings is the other half of the
// independence the trust store keeps: a directory accepted on its own (the
// --trust flag, or `wright trust accept`) leaves a settings file that
// arrived with the repository inert, and still asks about it.
func TestWorkspaceTrustDoesNotVouchForSettings(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{text: "hi"}})
	writeFile(t, filepath.Join(ws, ".wright", "settings.json"), localOnlySettings)
	if err := trust.Open(config.DefaultPaths(ws).TrustFile()).AcceptWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	eff, err := app.LoadEffective(ws, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Trusted {
		t.Fatal("trusting the directory must not trust its settings")
	}
	o := interactiveOpts(ws)
	asked := 0
	o.Confirm = func(q string) bool {
		asked++
		if !strings.Contains(q, "Trust this project's settings?") {
			t.Errorf("the remaining question should be the settings one: %s", q)
		}
		return false
	}
	started := false
	if code, err := app.Run(context.Background(), o, noUI(&started)); err != nil || code != headless.ExitOK {
		t.Fatalf("Run = %d, %v (declining the settings is not declining the directory)", code, err)
	}
	if asked != 1 {
		t.Errorf("settings questions asked = %d, want 1", asked)
	}
}

// TestHeadlessIsUnaffectedByWorkspaceTrust is the invariant that keeps -p
// safe to script: a headless run never prompts, never exits over trust and
// never takes the baseline, however the directory was accepted in a
// terminal. Without it, one interactive "yes" would silently widen every
// later unattended run in the same checkout.
func TestHeadlessIsUnaffectedByWorkspaceTrust(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{tool: "edit_file", args: editArgs}, {text: "no"}})
	if err := trust.Open(config.DefaultPaths(ws).TrustFile()).AcceptWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(ws)
	o.Prompt = "edit"
	o.Confirm = func(q string) bool {
		t.Errorf("a headless run must never ask: %s", q)
		return true
	}
	var stdout, stderr bytes.Buffer
	o.Stdout, o.Stderr = &stdout, &stderr
	code, err := app.Run(context.Background(), o, nil)
	if err != nil || code != headless.ExitApprovalRequired {
		t.Fatalf("a trusted workspace must not grant a headless edit: code=%d err=%v\n%s", code, err, stdout.String())
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(got) != "hello world\n" {
		t.Errorf("the file was edited without an approval: %q", got)
	}
	b, err := app.Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if b.WorkspaceTrusted {
		t.Error("a headless build took the workspace baseline")
	}
}
