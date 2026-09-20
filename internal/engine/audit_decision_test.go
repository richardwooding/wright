package engine_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/tools"
)

// auditFixture is newFixture plus an audit log, and returns the log's path so
// a test can read back what the engine actually recorded.
func auditFixture(t *testing.T, client *enginetest.Scripted, mutate func(*engine.Options)) (fixture, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	lg, err := audit.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Audit = lg
		if mutate != nil {
			mutate(o)
		}
	})
	return f, path
}

func auditedDecisions(t *testing.T, path string) []audit.Decision {
	t.Helper()
	var out []audit.Decision
	for ev, err := range audit.Read(path) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == audit.KindDecision && ev.Decision != nil {
			out = append(out, *ev.Decision)
		}
	}
	return out
}

// TestAuditRecordsTheUsersAnswer pins that the log says what happened. The
// verdict on both user branches is still Ask — it is what raised the prompt —
// so recording the verdict left an allow and a deny reading identically
// ("ask", by "user") and counted a denial as merely asked.
func TestAuditRecordsTheUsersAnswer(t *testing.T) {
	tests := []struct {
		name        string
		allow       bool
		wantOutcome string
	}{
		{name: "allowed", allow: true, wantOutcome: "allow"},
		{name: "denied", wantOutcome: "deny"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &enginetest.Scripted{Responses: []*core.Response{
				enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`),
				enginetest.TextResp("done"),
			}}
			f, path := auditFixture(t, client, nil)
			if err := f.eng.Submit("edit a.go"); err != nil {
				t.Fatal(err)
			}
			f.collect(t, func(ev engine.Event) {
				if ev.Kind == engine.KindApprovalRequest {
					f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: tt.allow, Reason: "because"})
				}
			})
			decs := auditedDecisions(t, path)
			if len(decs) != 1 {
				t.Fatalf("decisions recorded = %d, want 1: %+v", len(decs), decs)
			}
			if decs[0].By != "user" || decs[0].Outcome != tt.wantOutcome {
				t.Errorf("recorded %q by %q, want %q by user", decs[0].Outcome, decs[0].By, tt.wantOutcome)
			}
		})
	}
}

// TestAuditRecordsTheGrant pins the field that explains a confusing session:
// what the approval handed the call. A user reading the log could see that a
// command was allowed but not that it ran *without* the network.
func TestAuditRecordsTheGrant(t *testing.T) {
	prefix := fakeBrewPrefix(t)
	tests := []struct {
		name        string
		script      string
		wantNet     bool
		wantWritten bool
	}{
		{name: "install", script: "brew install fpc", wantNet: true, wantWritten: true},
		{name: "ordinary edit", script: "echo hi > a.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingBackend{}
			client := &enginetest.Scripted{Responses: []*core.Response{
				enginetest.CallResp("c1", tools.NameBash, jsonCmd(tt.script)),
				enginetest.TextResp("done"),
			}}
			var ts agentkit.Toolset
			f, path := auditFixture(t, client, func(o *engine.Options) {
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
			if err := f.eng.Submit("do it"); err != nil {
				t.Fatal(err)
			}
			f.collect(t, func(ev engine.Event) {
				if ev.Kind == engine.KindApprovalRequest {
					f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: true})
				}
			})
			decs := auditedDecisions(t, path)
			if len(decs) != 1 {
				t.Fatalf("decisions recorded = %d, want 1: %+v", len(decs), decs)
			}
			if decs[0].GrantedNetwork != tt.wantNet {
				t.Errorf("granted_network = %v, want %v", decs[0].GrantedNetwork, tt.wantNet)
			}
			if named := slices.Contains(decs[0].GrantedWritable, prefix); named != tt.wantWritten {
				t.Errorf("granted_writable names %q = %v, want %v (%v)", prefix, named, tt.wantWritten, decs[0].GrantedWritable)
			}
		})
	}
}

// TestAuditRecordsAHeadlessDenialAsADenial pins the same property for the
// third branch. Headless never prompts and always denies, so recording the
// Ask verdict left a CI run's log — the log most likely to be read by someone
// who was not there — claiming nothing was denied.
func TestAuditRecordsAHeadlessDenialAsADenial(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`),
		enginetest.TextResp("done"),
	}}
	f, path := auditFixture(t, client, func(o *engine.Options) { o.Headless = true })
	if err := f.eng.Submit("edit a.go"); err != nil {
		t.Fatal(err)
	}
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			t.Error("headless raised an approval prompt")
		}
	})
	decs := auditedDecisions(t, path)
	if len(decs) != 1 {
		t.Fatalf("decisions recorded = %d, want 1: %+v", len(decs), decs)
	}
	if decs[0].Outcome != "deny" || decs[0].By != "headless" {
		t.Errorf("recorded %q by %q, want deny by headless", decs[0].Outcome, decs[0].By)
	}
	// The verdict that would have prompted is still legible: the reason names
	// the flag that would let the call through.
	if !strings.Contains(decs[0].Reason, "--allow") {
		t.Errorf("reason does not name the rule that would allow it: %q", decs[0].Reason)
	}

	// A denial must land in Denied, not in Asked: no prompt was shown.
	sum, err := audit.Summarize(audit.Read(path))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Denied != 1 || sum.Asked != 0 {
		t.Errorf("summary denied=%d asked=%d, want 1 and 0", sum.Denied, sum.Asked)
	}
}
