package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/tools"
)

// recordingBackend keeps every Spec and runs nothing. These tests assert what
// an approved `brew install` would be *given*; actually running one would
// install software on the machine running the tests and write into the
// user's real Homebrew prefix.
type recordingBackend struct{ specs []sandbox.Spec }

func (b *recordingBackend) Name() string                    { return "recording" }
func (b *recordingBackend) Available(context.Context) error { return nil }

func (b *recordingBackend) Command(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error) {
	b.specs = append(b.specs, spec)
	return exec.CommandContext(ctx, "true"), nil
}

// bashFixture is the engine over the real bash tool and a recording sandbox,
// which is the only level at which "approving this call makes it work" can be
// checked: the classifier, the verdict, the approval and the Spec all take
// part.
func bashFixture(t *testing.T, script string) (fixture, *recordingBackend) {
	t.Helper()
	rec := &recordingBackend{}
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", tools.NameBash, jsonCmd(script)),
		enginetest.TextResp("done"),
	}}
	var ts agentkit.Toolset
	f := newFixture(t, client, func(o *engine.Options) {
		ts = tools.New(tools.Deps{
			WS:          o.WS,
			Sandbox:     rec,
			SandboxSpec: sandbox.Spec{ReadWrite: o.WS.Roots, Env: sandbox.EnvFrom(nil, nil, nil)},
		})
		o.Tools = ts
		o.Describe = func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
			d, ok := tools.Lookup(ts, name)
			if !ok {
				return policy.Request{}, engine.Preview{}, false, nil
			}
			req, pv, err := d.Describe(args)
			if err != nil {
				return policy.Request{}, engine.Preview{}, false, err
			}
			return req, engine.Preview{Title: pv.Title, Diff: pv.Diff, Body: pv.Body}, true, nil
		}
	})
	return f, rec
}

func jsonCmd(script string) string {
	b, _ := json.Marshal(map[string]any{"command": script})
	return string(b)
}

// fakeBrewPrefix points ToolPrefixes at a directory of this test's own, so
// nothing here depends on (or names) the user's real Homebrew installation.
func fakeBrewPrefix(t *testing.T) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "homebrew")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_PREFIX", prefix)
	return prefix
}

// TestApprovalGrantsWhatTheCommandNeeds is the acceptance check: approving an
// install gets network *and* a writable Homebrew prefix, approving a network
// query gets network and no extra writes, and an ordinary build gets neither.
// Splitting "allow" from "allow with network" left the user approving a call
// that then failed with "Could not connect".
func TestApprovalGrantsWhatTheCommandNeeds(t *testing.T) {
	tests := []struct {
		name        string
		script      string
		wantPrompt  bool
		wantNet     bool
		wantWritten bool
	}{
		{name: "install", script: "brew install fpc", wantPrompt: true, wantNet: true, wantWritten: true},
		{name: "network query", script: "brew info fpc", wantPrompt: true, wantNet: true},
		// Neither of these prompts at all — a local query is a safe read and
		// `go test` rides a builtin allow rule — and neither is widened.
		{name: "local query", script: "brew list"},
		{name: "ordinary build", script: "go test ./..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix := fakeBrewPrefix(t)
			f, rec := bashFixture(t, tt.script)
			if err := f.eng.Submit("do it"); err != nil {
				t.Fatal(err)
			}
			var grants engine.CallGrant
			prompted := false
			f.collect(t, func(ev engine.Event) {
				if ev.Kind != engine.KindApprovalRequest {
					return
				}
				prompted = true
				grants = ev.Approval.Grants
				f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: true})
			})
			if prompted != tt.wantPrompt {
				t.Fatalf("prompted = %v, want %v", prompted, tt.wantPrompt)
			}
			if len(rec.specs) != 1 {
				t.Fatalf("sandboxed commands = %d, want 1", len(rec.specs))
			}
			spec := rec.specs[0]
			if spec.Network != tt.wantNet {
				t.Errorf("Spec.Network = %v, want %v", spec.Network, tt.wantNet)
			}
			if grants.Network != tt.wantNet {
				t.Errorf("Approval.Grants.Network = %v, want %v (the prompt must say what it grants)", grants.Network, tt.wantNet)
			}
			written := slices.Contains(spec.ReadWrite, prefix)
			if written != tt.wantWritten {
				t.Errorf("Spec.ReadWrite contains %q = %v, want %v (%v)", prefix, written, tt.wantWritten, spec.ReadWrite)
			}
			if named := slices.Contains(grants.Writable, prefix); named != tt.wantWritten {
				t.Errorf("Approval.Grants.Writable names the prefix = %v, want %v (%v)", named, tt.wantWritten, grants.Writable)
			}
		})
	}
}

// TestDenialGrantsNothing pins that the grant follows the approval: a call
// the user declined never reaches the sandbox, and the recorded grant does
// not wait around for the next call.
func TestDenialGrantsNothing(t *testing.T) {
	fakeBrewPrefix(t)
	f, rec := bashFixture(t, "brew install fpc")
	if err := f.eng.Submit("do it"); err != nil {
		t.Fatal(err)
	}
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: false, Reason: "no"})
		}
	})
	if len(rec.specs) != 0 {
		t.Fatalf("a denied call reached the sandbox: %+v", rec.specs)
	}
}

// TestModelCannotSelfGrantNetwork pins the invariant the whole design rests
// on: the model asking for network in its own arguments is a request the
// user is prompted about, never a grant. Denying it leaves the call unrun;
// the argument the tool sees is always the engine's, rewritten to the
// verdict.
func TestModelCannotSelfGrantNetwork(t *testing.T) {
	rec := &recordingBackend{}
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", tools.NameBash, `{"command":"echo hi","network":true}`),
		enginetest.TextResp("done"),
	}}
	var ts agentkit.Toolset
	f := newFixture(t, client, func(o *engine.Options) {
		ts = tools.New(tools.Deps{
			WS: o.WS, Sandbox: rec,
			SandboxSpec: sandbox.Spec{ReadWrite: o.WS.Roots, Env: sandbox.EnvFrom(nil, nil, nil)},
		})
		o.Tools = ts
		o.Describe = func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
			d, ok := tools.Lookup(ts, name)
			if !ok {
				return policy.Request{}, engine.Preview{}, false, nil
			}
			req, pv, err := d.Describe(args)
			if err != nil {
				return policy.Request{}, engine.Preview{}, false, err
			}
			return req, engine.Preview{Title: pv.Title, Diff: pv.Diff, Body: pv.Body}, true, nil
		}
	})
	if err := f.eng.Submit("echo"); err != nil {
		t.Fatal(err)
	}
	prompted := false
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			prompted = true
			f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: false, Reason: "no network"})
		}
	})
	if !prompted {
		t.Fatal("a bash call asking for network must prompt")
	}
	if len(rec.specs) != 0 {
		t.Fatalf("the model's network request ran without approval: %+v", rec.specs)
	}
}
