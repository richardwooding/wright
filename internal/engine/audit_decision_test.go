package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
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
