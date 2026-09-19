package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/workspace"
)

// scripted returns canned responses in order and records every request.
type scripted struct {
	mu        sync.Mutex
	responses []*core.Response
	seen      []*core.Request
	block     chan struct{} // when set, the first call blocks until closed
}

func (s *scripted) Chat(ctx context.Context, req *core.Request) (*core.Response, error) {
	s.mu.Lock()
	cp := *req
	s.seen = append(s.seen, &cp)
	n := len(s.seen)
	block := s.block
	s.mu.Unlock()
	if block != nil && n == 1 {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if n > len(s.responses) {
		return textResp("done"), nil
	}
	return s.responses[n-1], nil
}

func (s *scripted) requests() []*core.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*core.Request(nil), s.seen...)
}

func textResp(t string) *core.Response {
	return &core.Response{Message: core.Assistant(core.Text(t)), FinishReason: core.FinishStop, Usage: core.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}}
}

func callResp(id, name, args string) *core.Response {
	return &core.Response{Message: core.Assistant(core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}), FinishReason: core.FinishToolCalls, Usage: core.Usage{InputTokens: 120, OutputTokens: 5, TotalTokens: 125}}
}

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
	client *scripted
	events <-chan engine.Event
}

func newFixture(t *testing.T, client *scripted, mutate func(*engine.Options)) fixture {
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
	f := newFixture(t, &scripted{responses: []*core.Response{textResp("hello there")}}, nil)
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
	reqs := f.client.requests()
	if len(reqs) != 1 || reqs[0].Cache == nil || !reqs[0].Cache.System {
		t.Fatalf("expected one cached request, got %+v", reqs)
	}
	sys := reqs[0].Messages[0].Text()
	if !strings.Contains(sys, "# Boundaries and honesty") {
		t.Fatalf("system prompt lacks the ethics section: %.200s", sys)
	}
}

func TestReadIsAllowedSilently(t *testing.T) {
	f := newFixture(t, &scripted{responses: []*core.Response{callResp("c1", "read_file", `{"path":"a.go"}`), textResp("ok")}}, nil)
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
	f := newFixture(t, &scripted{responses: []*core.Response{callResp("c1", "edit_file", `{"path":"a.go"}`), textResp("edited")}}, nil)
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
	reqs := f.client.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 model calls, got %d", len(reqs))
	}
	if res := reqs[1].Messages[len(reqs[1].Messages)-1].ToolResults(); len(res) != 1 || res[0].IsError || !strings.Contains(res[0].Text(), "content of a.go") {
		t.Fatalf("model did not see the tool result: %+v", res)
	}
}

func TestEditDeniedFeedsModel(t *testing.T) {
	f := newFixture(t, &scripted{responses: []*core.Response{callResp("c1", "edit_file", `{"path":"a.go"}`), textResp("understood")}}, nil)
	_ = f.eng.Submit("edit a.go")
	f.collect(t, func(ev engine.Event) {
		if ev.Kind == engine.KindApprovalRequest {
			f.eng.Reply(ev.Approval.ID, engine.Decision{Allow: false, Reason: "not today"})
		}
	})
	reqs := f.client.requests()
	res := reqs[1].Messages[len(reqs[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "not today") {
		t.Fatalf("denial did not reach the model: %+v", res)
	}
}

func TestHeadlessDeniesWithHint(t *testing.T) {
	f := newFixture(t, &scripted{responses: []*core.Response{callResp("c1", "edit_file", `{"path":"a.go"}`), textResp("ok")}}, func(o *engine.Options) { o.Headless = true })
	_ = f.eng.Submit("edit a.go")
	evs := f.collect(t, nil)
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatal("headless must not prompt")
	}
	res := f.client.requests()[1].Messages[len(f.client.requests()[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "--allow") {
		t.Fatalf("headless denial lacks the --allow hint: %+v", res)
	}
}

func TestPlanModeHidesWriteTools(t *testing.T) {
	f := newFixture(t, &scripted{responses: []*core.Response{textResp("planned")}}, func(o *engine.Options) { o.Mode = policy.ModePlan })
	_ = f.eng.Submit("plan")
	f.collect(t, nil)
	tools := f.client.requests()[0].Tools
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("plan mode tools = %+v", tools)
	}
}

func TestSecretFileIsHardDenied(t *testing.T) {
	f := newFixture(t, &scripted{responses: []*core.Response{callResp("c1", "read_file", `{"path":".env"}`), textResp("ok")}}, nil)
	_ = f.eng.Submit("read .env")
	evs := f.collect(t, nil)
	if hasKind(evs, engine.KindApprovalRequest) {
		t.Fatal("secret files must be denied, not asked")
	}
	res := f.client.requests()[1].Messages[len(f.client.requests()[1].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text(), "denied by policy") {
		t.Fatalf("denial = %+v", res)
	}
}

func TestSteeringQueuesWhileRunning(t *testing.T) {
	block := make(chan struct{})
	client := &scripted{responses: []*core.Response{textResp("first"), textResp("second")}, block: block}
	f := newFixture(t, client, nil)
	_ = f.eng.Submit("one")
	for len(client.requests()) == 0 { // wait until the first model call is in flight
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
	reqs := client.requests()
	if len(reqs) != 2 {
		t.Fatalf("want the queued message to trigger a second model call, got %d", len(reqs))
	}
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; last.Role != core.RoleUser || last.Text() != "two" {
		t.Fatalf("second request should end with the steered message, got %+v", last)
	}
}

func TestCancelStopsRun(t *testing.T) {
	block := make(chan struct{})
	f := newFixture(t, &scripted{block: block}, nil)
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

func TestSetModeRejectsBypass(t *testing.T) {
	f := newFixture(t, &scripted{}, nil)
	if err := f.eng.SetMode(policy.ModeBypass); err == nil {
		t.Fatal("bypass must not be reachable through SetMode")
	}
	if err := f.eng.SetMode(policy.ModeAutoEdit); err != nil || f.eng.Mode() != policy.ModeAutoEdit {
		t.Fatalf("SetMode auto-edit: %v", err)
	}
}
