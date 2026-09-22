package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/workspace"
)

type echoArgs struct {
	Path string `json:"path"`
}

// echoTool stands in for read_file/edit_file: the describer decides which.
func echoTool(name string) agentkit.Tool {
	return agentkit.Func(name, "echo", func(_ context.Context, in echoArgs) (string, error) {
		return "content of " + in.Path, nil
	})
}

func describer(ws *workspace.Workspace) engine.DescribeFunc {
	return func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
		if name != "read_file" && name != "edit_file" {
			return policy.Request{}, engine.Preview{}, false, nil // sub-agents are evaluated by name
		}
		var in echoArgs
		if err := json.Unmarshal(args, &in); err != nil {
			return policy.Request{}, engine.Preview{}, false, err
		}
		abs, _, err := ws.Resolve(in.Path)
		if err != nil {
			return policy.Request{}, engine.Preview{}, false, err
		}
		req := policy.Request{Tool: name, Args: args, Paths: []string{abs}}
		if name == "edit_file" {
			req.Writes = []string{abs}
		}
		return req, engine.Preview{Title: name + " " + in.Path}, true, nil
	}
}

type fixture struct {
	eng    *engine.Engine
	client *enginetest.Scripted
	events <-chan engine.Event
}

func newFixture(t *testing.T, client *enginetest.Scripted, mutate func(*engine.Options)) fixture {
	t.Helper()
	root := t.TempDir()
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(filepath.Join(root, ".data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	o := engine.Options{
		Model:         model.Choice{Model: "fake-model", Provider: "fake"},
		Client:        client,
		WS:            ws,
		Cwd:           root,
		Mode:          policy.ModeDefault,
		Tools:         agentkit.Toolset{echoTool("read_file"), echoTool("edit_file")},
		ReadOnlyTools: []string{"read_file"},
		Describe:      describer(ws),
		Store:         store,
		SessionID:     "s1",
		Sandbox:       "none",
		ContextWindow: 1000,
	}
	if mutate != nil {
		mutate(&o)
	}
	eng, err := engine.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return fixture{eng: eng, client: client, events: eng.Events()}
}

// collect drains events until a RunFinished arrives or the deadline passes.
func (f fixture) collect(t *testing.T, onEvent func(engine.Event)) []engine.Event {
	t.Helper()
	var out []engine.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-f.events:
			out = append(out, ev)
			if onEvent != nil {
				onEvent(ev)
			}
			if ev.Kind == engine.KindRunFinished {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out; events so far: %s", kinds(out))
		}
	}
}

func kinds(evs []engine.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind.String())
		b.WriteByte(' ')
	}
	return b.String()
}

func hasKind(evs []engine.Event, k engine.Kind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func TestPlainTextRun(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("hello there")}}, nil)
	if err := f.eng.Submit("hi"); err != nil {
		t.Fatal(err)
	}
	evs := f.collect(t, nil)
	for _, k := range []engine.Kind{engine.KindRunStarted, engine.KindText, engine.KindUsage, engine.KindRunFinished} {
		if !hasKind(evs, k) {
			t.Fatalf("missing %s in %s", k, kinds(evs))
		}
	}
	last := evs[len(evs)-1]
	if last.Finish == nil || last.Finish.Output != "hello there" || last.Finish.StopReason != "completed" {
		t.Fatalf("finish = %+v", last.Finish)
	}
	st := f.eng.Status()
	if st.Running || st.Usage.InputTokens != 100 || st.ContextUsed != 100 || st.ContextWindow != 1000 {
		t.Fatalf("status = %+v", st)
	}
	reqs := f.client.Requests()
	if len(reqs) != 1 || reqs[0].Cache == nil || !reqs[0].Cache.System {
		t.Fatalf("expected one cached request, got %+v", reqs)
	}
	sys := reqs[0].Messages[0].Text()
	if !strings.Contains(sys, "# Boundaries and honesty") {
		t.Fatalf("system prompt lacks the ethics section: %.200s", sys)
	}
}

func TestReadIsAllowedSilently(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "read_file", `{"path":"a.go"}`), enginetest.TextResp("ok")}}, nil)
	_ = f.eng.Submit("read a.go")
	evs := f.collect(t, nil)
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatalf("read inside the workspace must not prompt: %s", kinds(evs))
	}
	var result *engine.Event
	for i := range evs {
		if evs[i].Kind == engine.KindToolResult {
			result = &evs[i]
		}
	}
	if result == nil || result.Result == nil {
		t.Fatalf("no tool result in %s", kinds(evs))
	}
	if txt := result.Result.Text(); !strings.HasPrefix(txt, `<untrusted source="read_file"`) || !strings.Contains(txt, "content of a.go") {
		t.Fatalf("tool result not fenced as untrusted: %q", txt)
	}
}

func TestEditAsksAndUserAllows(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`), enginetest.TextResp("edited")}}, nil)
	_ = f.eng.Submit("edit a.go")
	var approvalID string
	evs := f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			approvalID = ev.Approval.ID
			if ev.Approval.Severity != engine.SeverityCaution || ev.Approval.Preview.Title != "edit_file a.go" {
				t.Errorf("approval = %+v", ev.Approval)
			}
			f.eng.Reply(approvalID, engine.Decision{Allow: true})
		}
	})
	if !hasKind(evs, engine.KindApprovalDecided) {
		t.Fatalf("no decision event: %s", kinds(evs))
	}
	reqs := f.client.Requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 model calls, got %d", len(reqs))
	}
	if res := reqs[1].Messages[len(reqs[1].Messages)-1].ToolResults(); len(res) != 1 || res[0].IsError || !strings.Contains(res[0].Text(), "content of a.go") {
		t.Fatalf("model did not see the tool result: %+v", res)
	}
}

func TestEditDeniedFeedsModel(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`), enginetest.TextResp("understood")}}, nil)
	_ = f.eng.Submit("edit a.go")
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: false, Reason: "not today"})
		}
	})
	reqs := f.client.Requests()
	res := reqs[1].Messages[len(reqs[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "not today") {
		t.Fatalf("denial did not reach the model: %+v", res)
	}
}

func TestHeadlessDeniesWithHint(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`), enginetest.TextResp("ok")}}, func(o *engine.Options) { o.Headless = true })
	_ = f.eng.Submit("edit a.go")
	evs := f.collect(t, nil)
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatal("headless must not prompt")
	}
	res := f.client.Requests()[1].Messages[len(f.client.Requests()[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "--allow") {
		t.Fatalf("headless denial lacks the --allow hint: %+v", res)
	}
}

func TestPlanModeHidesWriteTools(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("planned")}}, func(o *engine.Options) { o.Mode = policy.ModePlan })
	_ = f.eng.Submit("plan")
	f.collect(t, nil)
	tools := f.client.Requests()[0].Tools
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("plan mode tools = %+v", tools)
	}
}

func TestSecretFileIsHardDenied(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "read_file", `{"path":".env"}`), enginetest.TextResp("ok")}}, nil)
	_ = f.eng.Submit("read .env")
	evs := f.collect(t, nil)
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatal("secret files must be denied, not asked")
	}
	res := f.client.Requests()[1].Messages[len(f.client.Requests()[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "denied by policy") {
		t.Fatalf("denial = %+v", res)
	}
}

func TestSteeringQueuesWhileRunning(t *testing.T) {
	block := make(chan struct{})
	client := &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("first"), enginetest.TextResp("second")}, Block: block}
	f := newFixture(t, client, nil)
	_ = f.eng.Submit("one")
	for len(client.Requests()) == 0 { // wait until the first model call is in flight
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.eng.Submit("two"); err != nil {
		t.Fatal(err)
	}
	close(block)
	evs := f.collect(t, nil)
	if !hasKind(evs, engine.KindQueued) {
		t.Fatalf("no queued event: %s", kinds(evs))
	}
	reqs := client.Requests()
	if len(reqs) != 2 {
		t.Fatalf("want the queued message to trigger a second model call, got %d", len(reqs))
	}
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; last.Role != core.RoleUser || last.Text() != "two" {
		t.Fatalf("second request should end with the steered message, got %+v", last)
	}
}

func TestCancelStopsRun(t *testing.T) {
	block := make(chan struct{})
	f := newFixture(t, &enginetest.Scripted{Block: block}, nil)
	_ = f.eng.Submit("slow")
	time.Sleep(50 * time.Millisecond)
	f.eng.Cancel()
	evs := f.collect(t, nil)
	fin := evs[len(evs)-1].Finish
	if fin == nil || !errors.Is(fin.Err, context.Canceled) || fin.StopReason != "canceled" {
		t.Fatalf("finish = %+v", fin)
	}
	close(block)
}

// lateApprover hands sub-agent calls to an engine that does not exist yet
// when the sub-agent is built, the way the app wires its own children.
type lateApprover struct{ eng *engine.Engine }

func (l *lateApprover) Approve(ctx context.Context, c agentkit.Call) (agentkit.Decision, error) {
	return l.eng.Approve(ctx, c)
}

// TestSubAgentDoesNotInheritBypass pins the child clamp: the parent runs in
// bypass mode, where an edit is allowed without a prompt, but the same edit
// attempted by a depth-1 sub-agent is evaluated by a child policy engine
// (mode clamped to default), so it needs approval — and headless has none.
func TestSubAgentDoesNotInheritBypass(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "child", `{"input":"edit a.go"}`),
		enginetest.CallResp("c2", "edit_file", `{"path":"a.go"}`),
		enginetest.TextResp("could not edit"),
		enginetest.TextResp("done"),
	}}
	late := &lateApprover{}
	child, err := agentkit.NewFromClient(client,
		agentkit.WithName("child"),
		agentkit.WithTools(echoTool("edit_file")),
		agentkit.WithMiddleware(agentkit.ApproveWith(late)),
	)
	if err != nil {
		t.Fatal(err)
	}
	sub := agentkit.AsTool(child, "child", "delegate", agentkit.WithForwardEvents())
	f := newFixture(t, client, func(o *engine.Options) {
		o.Mode, o.Bypass, o.Headless = policy.ModeBypass, true, true
		o.Tools = append(o.Tools, sub)
	})
	late.eng = f.eng

	_ = f.eng.Submit("delegate the edit")
	evs := f.collect(t, nil)

	var denied, deep bool
	for _, ev := range evs {
		if ev.Depth == 1 {
			deep = true
		}
		if ev.Kind == engine.KindToolResult && ev.Depth == 1 && ev.Result != nil &&
			strings.Contains(ev.Result.Text(), engine.HeadlessDenialMarker) {
			denied = true
		}
	}
	if !deep {
		t.Fatalf("no depth-1 events were forwarded: %s", kinds(evs))
	}
	if !denied {
		t.Fatalf("the sub-agent's edit was not stopped by the child policy: %s", kinds(evs))
	}
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatal("headless must not prompt")
	}
}

func TestSetModeRejectsBypass(t *testing.T) {
	f := newFixture(t, &enginetest.Scripted{}, nil)
	if err := f.eng.SetMode(policy.ModeBypass); err == nil {
		t.Fatal("bypass must not be reachable through SetMode")
	}
	if err := f.eng.SetMode(policy.ModeAutoEdit); err != nil || f.eng.Mode() != policy.ModeAutoEdit {
		t.Fatalf("SetMode auto-edit: %v", err)
	}
}

// TestAutoAllowedCallCarriesItsPreview is the regression this exists for. The
// engine computes a preview — for an edit, a real unified diff — for every
// call, at every depth, and before this it was discarded on the Allow branch.
// A user in auto-edit mode, where edits never prompt, therefore never saw a
// diff of what the agent changed.
func TestAutoAllowedCallCarriesItsPreview(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`),
		enginetest.TextResp("done"),
	}}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Mode = policy.ModeAutoEdit // edits are allowed outright: no prompt
		inner := o.Describe          // the fixture's real describer, for this workspace
		o.Describe = func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
			req, pv, ok, err := inner(name, args)
			pv.Diff = "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
			return req, pv, ok, err
		}
	})
	_ = f.eng.Submit("edit a.go")
	var result *engine.Event
	for _, ev := range f.collect(t, nil) {
		if ev.Kind == engine.KindApprovalRequest {
			t.Fatalf("auto-edit mode raised an approval prompt; this test is not exercising the Allow branch")
		}
		if ev.Kind == engine.KindToolResult && ev.Call != nil && ev.Call.Name == "edit_file" {
			result = &ev
		}
	}
	if result == nil {
		t.Fatal("no edit_file result event")
	}
	if result.Preview == nil {
		t.Fatal("the result carried no preview: the diff was computed and thrown away")
	}
	if !strings.Contains(result.Preview.Diff, "+new") {
		t.Errorf("Preview.Diff = %q, want the unified diff", result.Preview.Diff)
	}
}

// TestThePromptFollowsTheWorkingDirectory is the bug that made the model
// prefix almost every bash command with `cd <workspace> &&`. It was told —
// twice — that the working directory persists, and then handed a `cwd:` line
// snapshotted once at session build that never moved. Told a fact and given a
// value it could not rely on, re-establishing the directory every call was
// the rational thing to do.
func TestThePromptFollowsTheWorkingDirectory(t *testing.T) {
	here := "/somewhere/start"
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.TextResp("one"), enginetest.TextResp("two"),
	}}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Cwd = here
		o.CwdNow = func() string { return here }
	})
	_ = f.eng.Submit("first")
	f.collect(t, nil)
	here = "/somewhere/start/sub" // as a `cd` would move it
	_ = f.eng.Submit("second")
	f.collect(t, nil)

	reqs := client.Requests()
	if len(reqs) < 2 {
		t.Fatalf("got %d requests, want at least 2", len(reqs))
	}
	first, last := environmentOf(t, reqs[0]), environmentOf(t, reqs[len(reqs)-1])
	if !strings.Contains(first, "/somewhere/start") || strings.Contains(first, "/sub") {
		t.Errorf("first run's cwd = %q, want the starting directory", first)
	}
	if !strings.Contains(last, "/somewhere/start/sub") {
		t.Errorf("second run's cwd = %q, want the directory the shell moved to", last)
	}
}

// TestPromptCwdFallsBackToTheStaticValue: every caller that tracks nothing —
// which is every test and every embedder — keeps the old behaviour.
func TestPromptCwdFallsBackToTheStaticValue(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("hi")}}
	f := newFixture(t, client, func(o *engine.Options) {
		o.Cwd = "/only/static"
		o.CwdNow = nil
	})
	_ = f.eng.Submit("go")
	f.collect(t, nil)
	if got := environmentOf(t, client.Requests()[0]); !strings.Contains(got, "/only/static") {
		t.Errorf("cwd = %q, want the static Options.Cwd", got)
	}
}

// environmentOf returns the cwd line of the <environment> block in whichever
// part of a request carries it.
func environmentOf(t *testing.T, r *core.Request) string {
	t.Helper()
	var b strings.Builder
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if txt, ok := p.(core.TextPart); ok {
				b.WriteString(txt.Text)
				b.WriteString("\n")
			}
		}
	}
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(line, "cwd: ") {
			return line
		}
	}
	t.Fatalf("no cwd line in the request:\n%s", b.String())
	return ""
}
