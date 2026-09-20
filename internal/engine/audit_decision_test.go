package engine_test

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/workspace"
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

// TestSessionIsVisibleBeforeTheFirstRunEnds pins that a session exists as
// soon as it is started. The sidecar and the transcript were both written
// only when a run *ended*, so for the whole of the first run the session the
// status bar was naming did not exist: /sessions listed nothing and /export
// reported "no such session" for the id on screen.
func TestSessionIsVisibleBeforeTheFirstRunEnds(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(filepath.Join(dir, ".data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(context.Background(), engine.Options{
		Model:     model.Choice{Model: "fake-model", Provider: "fake"},
		Client:    &enginetest.Scripted{},
		WS:        ws,
		Cwd:       dir,
		Mode:      policy.ModeDefault,
		Store:     store,
		SessionID: "20260920-100000-abcd",
		Sandbox:   "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	// No run has happened yet.
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "20260920-100000-abcd" {
		t.Fatalf("List before the first run = %+v, want the new session", list)
	}
	if _, ok, err := store.Get(context.Background(), "20260920-100000-abcd"); err != nil || !ok {
		t.Fatalf("Get before the first run: ok=%v err=%v", ok, err)
	}
	// And it can be exported, which is what /export does on the session you
	// are sitting in.
	var buf bytes.Buffer
	if err := store.ExportMarkdown(context.Background(), "20260920-100000-abcd", &buf); err != nil {
		t.Fatalf("export before the first run: %v", err)
	}
	if !strings.Contains(buf.String(), "20260920-100000-abcd") {
		t.Errorf("export = %q, want the session id", buf.String())
	}
}

// TestTranscriptIsVisibleDuringTheRun is the second half of what a user hit:
// a session existed but held no transcript until its run ended, so watching a
// run make a dozen tool calls and then exporting it produced a bare header.
//
// It observes the store from inside the run — a tool that reads the session it
// is running in — because the property is about what is on disk *while* the
// run is still going, which nothing after the run can distinguish.
func TestTranscriptIsVisibleDuringTheRun(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(filepath.Join(dir, ".data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	const id = "20260920-110000-abcd"

	var seen []int // messages in the store at each tool call
	peek := agentkit.Func("peek", "peek", func(ctx context.Context, _ struct{}) (string, error) {
		msgs, err := store.Load(ctx, id)
		if err != nil {
			return "", err
		}
		seen = append(seen, len(msgs))
		return "ok", nil
	})

	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "peek", `{}`),
		enginetest.CallResp("c2", "peek", `{}`),
		enginetest.TextResp("done"),
	}}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Store, o.SessionID, o.Tools = store, id, agentkit.Toolset{peek}
	})
	if err := f.eng.Submit("go"); err != nil {
		t.Fatal(err)
	}
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: true})
		}
	})

	if len(seen) != 2 {
		t.Fatalf("the peek tool ran %d times, want 2", len(seen))
	}
	// The first call is inside step one, which has not completed, so nothing
	// of this run is stored yet. The second is inside step two, by which time
	// step one has been recorded.
	if seen[0] != 0 {
		t.Errorf("store held %d messages inside the first step, want 0", seen[0])
	}
	if seen[1] == 0 {
		t.Error("the store was still empty inside the second step: the first step was not recorded")
	}

	// And the finished run recorded everything exactly once.
	msgs, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range msgs {
		counts[messageKey(m)]++
	}
	for k, n := range counts {
		if n > 1 {
			t.Errorf("message %q stored %d times", k, n)
		}
	}
}

// messageKey identifies a message for duplicate detection. Text alone is not
// enough: an assistant message carrying tool calls has none, and so does the
// tool message answering it.
func messageKey(m core.Message) string {
	key := string(m.Role) + "|" + m.Text()
	for _, c := range m.ToolCalls() {
		key += "|call:" + c.ID
	}
	for _, r := range m.ToolResults() {
		key += "|result:" + r.CallID
	}
	return key
}
