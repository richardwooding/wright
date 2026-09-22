package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/enginetest"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/workspace"
)

// fakeTool stands in for a real wright tool: only its name matters here.
func fakeTool(name string) agentkit.Tool {
	type args struct {
		Path string `json:"path"`
	}
	return agentkit.Func(name, "fake "+name, func(_ context.Context, in args) (string, error) {
		return name + ":" + in.Path, nil
	})
}

func fullToolset() agentkit.Toolset {
	return agentkit.Toolset{
		fakeTool(tools.NameReadFile), fakeTool(tools.NameGlob), fakeTool(tools.NameGrep),
		fakeTool(tools.NameListDir), fakeTool(tools.NameEditFile), fakeTool(tools.NameBash),
	}
}

func names(ts agentkit.Toolset) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Definition().Name)
	}
	slices.Sort(out)
	return out
}

func TestToolsetPerDefinition(t *testing.T) {
	all := fullToolset()
	cases := []struct {
		name string
		def  agents.Definition
		want []string
	}{
		{"read-only default", agents.Definition{Name: "a", ReadOnly: true}, []string{"glob", "grep", "list_dir", "read_file"}},
		{"explicit tools win", agents.Definition{Name: "b", ReadOnly: true, Tools: []string{"bash", "read_file"}}, []string{"bash", "read_file"}},
		{"writer keeps everything but bash", agents.Definition{Name: "c"}, []string{"edit_file", "glob", "grep", "list_dir", "read_file"}},
		{"unknown names are dropped", agents.Definition{Name: "d", Tools: []string{"read_file", "nope"}}, []string{"read_file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := names(agents.Toolset(tc.def, all)); !slices.Equal(got, tc.want) {
				t.Errorf("Toolset = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    agents.Definition
		wantErr error
	}{
		{
			name: "full",
			in:   "---\nname: reviewer\ndescription: Review a diff\ntools:\n  - read_file\n  - grep\nmodel: fast-model\nread-only: false\n---\n\nReview carefully.\n",
			want: agents.Definition{
				Name: "reviewer", Description: "Review a diff", Tools: []string{"read_file", "grep"},
				Model: "fast-model", ReadOnly: false, Instructions: "Review carefully.",
			},
		},
		{
			name: "inline list, quotes and comments",
			in:   "---\nname: \"scout\"\ndescription: 'Find things' # trailing\ntools: read_file, grep\n---\nLook around.",
			want: agents.Definition{Name: "scout", Description: "Find things", Tools: []string{"read_file", "grep"}, ReadOnly: true, Instructions: "Look around."},
		},
		{
			name: "flow sequence",
			in:   "---\nname: flow\ndescription: d\ntools: [read_file, grep]\n---\nbody",
			want: agents.Definition{Name: "flow", Description: "d", Tools: []string{"read_file", "grep"}, ReadOnly: true, Instructions: "body"},
		},
		{
			name: "unreadable read-only stays read-only",
			in:   "---\nname: typo\ndescription: d\nread-only: maybe\n---\nbody",
			want: agents.Definition{Name: "typo", Description: "d", ReadOnly: true, Instructions: "body"},
		},
		{name: "no frontmatter", in: "just a note\n", wantErr: agents.ErrNoFrontmatter},
		{name: "unterminated", in: "---\nname: x\n", wantErr: agents.ErrNoFrontmatter},
		{name: "no name", in: "---\ndescription: d\n---\nbody", wantErr: agents.ErrMissingField},
		{name: "no description", in: "---\nname: x\n---\nbody", wantErr: agents.ErrMissingField},
		{name: "empty body", in: "---\nname: x\ndescription: d\n---\n\n", wantErr: agents.ErrMissingField},
		{name: "bad name", in: "---\nname: my agent!\ndescription: d\n---\nbody", wantErr: agents.ErrInvalidName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agents.Parse([]byte(tc.in))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.want.Name || got.Description != tc.want.Description ||
				got.Model != tc.want.Model || got.ReadOnly != tc.want.ReadOnly ||
				got.Instructions != tc.want.Instructions || !slices.Equal(got.Tools, tc.want.Tools) {
				t.Fatalf("Parse = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func writeAgent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCustomPrefersTheProject(t *testing.T) {
	root := t.TempDir()
	userConfig := t.TempDir()
	writeAgent(t, filepath.Join(userConfig, "agents", "scout.md"), "---\nname: scout\ndescription: user version\n---\nuser body")
	writeAgent(t, filepath.Join(userConfig, "agents", "notes.txt"), "ignored")
	writeAgent(t, filepath.Join(root, ".wright", "agents", "scout.md"), "---\nname: scout\ndescription: project version\n---\nproject body")
	writeAgent(t, filepath.Join(root, ".wright", "agents", "broken.md"), "no frontmatter here")

	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defs, problems, err := agents.LoadCustom(ws, userConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Description != "project version" {
		t.Fatalf("defs = %+v", defs)
	}
	if defs[0].Source == "" || !strings.HasSuffix(defs[0].Source, filepath.Join(".wright", "agents", "scout.md")) {
		t.Errorf("Source = %q", defs[0].Source)
	}
	if len(problems) != 1 || !errors.Is(problems[0], agents.ErrNoFrontmatter) {
		t.Fatalf("problems = %v", problems)
	}
	if ro := agents.ReadOnlyNames(defs); !slices.Equal(ro, []string{"explore", "scout"}) {
		t.Errorf("ReadOnlyNames = %v", ro)
	}
	if docs := agents.Docs(defs); len(docs) != 2 || docs[0].Name != "explore" || !strings.Contains(docs[1].When, "read-only sub-agent") {
		t.Errorf("Docs = %+v", docs)
	}
}

func TestBuildReportsPerAgentProblems(t *testing.T) {
	deps := agents.Deps{Client: &enginetest.Scripted{}, Tools: fullToolset()}
	ts, problems := agents.Build([]agents.Definition{
		{Name: "ok", Description: "fine", Instructions: "do", ReadOnly: true},
		{Name: "", Description: "nameless", Source: "/tmp/x.md"},
	}, deps)
	if len(ts) != 1 || ts[0].Definition().Name != "ok" {
		t.Fatalf("toolset = %v", names(ts))
	}
	if len(problems) != 1 || problems[0].Path != "/tmp/x.md" {
		t.Fatalf("problems = %v", problems)
	}
}

// engineFixture wires an engine whose toolset includes the explore agent,
// exactly as the app does: the child gets the engine's own middleware chain,
// resolved late because the tools are built before the engine exists.
type lateChain struct{ eng *engine.Engine }

func (l *lateChain) middleware() agentkit.Middleware {
	return func(next agentkit.Tool) agentkit.Tool {
		def := next.Definition()
		return agentkit.Raw(def.Name, def.Description, def.Parameters, func(ctx context.Context, args json.RawMessage) (agentkit.Output, error) {
			tool := next
			chain := l.eng.Middleware()
			for _, c := range slices.Backward(chain) {
				tool = c(tool)
			}
			return tool.Call(ctx, args)
		})
	}
}

// newEngine builds an engine whose toolset is the fake tools plus the
// explore agent and any extra custom agents, wired the way the app wires
// them.
func newEngine(t *testing.T, client core.Chatter, custom ...agents.Definition) *engine.Engine {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(filepath.Join(root, ".data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	late := &lateChain{}
	deps := agents.Deps{Client: client, Tools: fullToolset(), Middleware: []agentkit.Middleware{late.middleware()}}
	explore, err := agents.Explore(deps)
	if err != nil {
		t.Fatal(err)
	}
	subs, problems := agents.Build(custom, deps)
	if len(problems) != 0 {
		t.Fatalf("building sub-agents: %v", problems)
	}
	// Calling a sub-agent is allowed the way the app allows it; what the
	// child then does is evaluated on its own.
	pol := policy.New(ws, policy.ModeDefault, policy.Builtin(),
		policy.MustParseRules(agents.Names(custom), policy.Allow, policy.SourceBuiltin))
	eng, err := engine.New(context.Background(), engine.Options{
		Model:         model.Choice{Model: "fake-model", Provider: "fake"},
		Client:        client,
		WS:            ws,
		Cwd:           root,
		Mode:          policy.ModeDefault,
		Policy:        pol,
		Tools:         append(append(fullToolset(), explore), subs...),
		ReadOnlyTools: agents.ReadOnlyNames(custom),
		Describe:      describe(ws),
		Store:         store,
		SessionID:     "s1",
		Sandbox:       "none",
		Headless:      true,
		ContextWindow: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	late.eng = eng
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// describe resolves the fake tools' paths the way the app's adapter does;
// sub-agent tools are unknown to it and are evaluated by name.
func describe(ws *workspace.Workspace) engine.DescribeFunc {
	return func(name string, args []byte) (policy.Request, engine.Preview, bool, error) {
		if name != tools.NameReadFile && name != tools.NameEditFile {
			return policy.Request{}, engine.Preview{}, false, nil
		}
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return policy.Request{}, engine.Preview{}, false, err
		}
		abs, _, err := ws.Resolve(in.Path)
		if err != nil {
			return policy.Request{}, engine.Preview{}, false, err
		}
		req := policy.Request{Tool: name, Args: args, Paths: []string{abs}}
		if name == tools.NameEditFile {
			req.Writes = []string{abs}
		}
		return req, engine.Preview{Title: name}, true, nil
	}
}

func drain(t *testing.T, eng *engine.Engine) []engine.Event {
	t.Helper()
	var out []engine.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-eng.Events():
			out = append(out, ev)
			if ev.Kind == engine.KindRunFinished {
				return out
			}
		case <-deadline:
			t.Fatal("timed out waiting for the run to finish")
		}
	}
}

func TestExploreRunsAtDepthOne(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "explore", `{"input":"where is a.go"}`),
		enginetest.CallResp("c2", "read_file", `{"path":"a.go"}`),
		enginetest.TextResp("a.go is at the root"),
		enginetest.TextResp("done"),
	}}
	eng := newEngine(t, client)
	if err := eng.Submit("find a.go"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, eng)

	var childRead bool
	for _, ev := range evs {
		if ev.Kind == engine.KindToolResult && ev.Depth == 1 && ev.Call != nil && ev.Call.Name == tools.NameReadFile {
			childRead = true
		}
	}
	if !childRead {
		t.Fatal("the child's read_file was not forwarded with Depth 1")
	}
	reqs := client.Requests()
	if len(reqs) != 4 {
		t.Fatalf("want 4 model calls (parent, child, child, parent), got %d", len(reqs))
	}
	if sys := reqs[1].Messages[0].Text(); !strings.Contains(sys, "read-only exploration sub-agent") {
		t.Errorf("the child does not run the explore prompt: %.120s", sys)
	}
	var childTools []string
	for _, tool := range reqs[1].Tools {
		childTools = append(childTools, tool.Name)
	}
	slices.Sort(childTools)
	if !slices.Equal(childTools, []string{"glob", "grep", "list_dir", "read_file"}) {
		t.Errorf("child toolset = %v, want the read-only tools", childTools)
	}
	last := reqs[3].Messages[len(reqs[3].Messages)-1].ToolResults()
	if len(last) != 1 || !strings.Contains(last[0].Text(), "a.go is at the root") {
		t.Fatalf("the child's report did not reach the parent: %+v", last)
	}
}

// TestSubAgentEditIsDeniedByPolicy gives a sub-agent the edit tool on
// purpose: having a tool is not permission to use it. The call is evaluated
// at depth 1 by a child policy engine, and a headless run has nobody to ask,
// so the child is told no — and the main agent hears about it.
func TestSubAgentEditIsDeniedByPolicy(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{
		enginetest.CallResp("c1", "writer", `{"input":"fix a.go"}`),
		enginetest.CallResp("c2", "edit_file", `{"path":"a.go"}`),
		enginetest.TextResp("I was not allowed to edit"),
		enginetest.TextResp("done"),
	}}
	eng := newEngine(t, client, agents.Definition{
		Name: "writer", Description: "edit files", Instructions: "Edit what you are asked to.",
		Tools: []string{tools.NameEditFile}, ReadOnly: false,
	})
	if err := eng.Submit("edit through the sub-agent"); err != nil {
		t.Fatal(err)
	}
	evs := drain(t, eng)
	for _, ev := range evs {
		if ev.Kind == engine.KindApprovalRequest {
			t.Fatal("headless must not prompt")
		}
	}
	var denied bool
	for _, ev := range evs {
		if ev.Kind == engine.KindToolResult && ev.Depth == 1 && ev.Result != nil &&
			strings.Contains(ev.Result.Text(), engine.HeadlessDenialMarker) {
			denied = true
		}
	}
	if !denied {
		t.Fatal("the sub-agent's edit_file was not denied at depth 1")
	}
	reqs := client.Requests()
	if len(reqs) != 4 {
		t.Fatalf("want 4 model calls, got %d", len(reqs))
	}
	res := reqs[2].Messages[len(reqs[2].Messages)-1].ToolResults()
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("the child's edit_file should have failed: %+v", res)
	}
}

// TestOnlyAnExplicitToolsListGivesASubAgentBash guards what keeps the shared
// working directory from mattering in practice. explore gets the read-only
// set, and a custom agent that names no tools gets everything except bash, so
// only an agent that asks for the shell by name can run one. Each sub-agent
// now also has its own working directory (see app), but that guard is the
// reason this was latent rather than live, and it must not disappear quietly.
func TestOnlyAnExplicitToolsListGivesASubAgentBash(t *testing.T) {
	full := agentkit.Toolset{
		fakeTool(tools.NameBash), fakeTool(tools.NameReadFile),
		fakeTool(tools.NameWriteFile), fakeTool(tools.NameGrep),
	}
	has := func(ts agentkit.Toolset, name string) bool {
		_, ok := ts.Lookup(name)
		return ok
	}
	tests := []struct {
		name string
		def  agents.Definition
		want bool
	}{
		{"read-only (explore's shape)", agents.Definition{ReadOnly: true}, false},
		{"no tools listed", agents.Definition{}, false},
		{"lists other tools", agents.Definition{Tools: []string{tools.NameReadFile, tools.NameGrep}}, false},
		{"asks for bash by name", agents.Definition{Tools: []string{tools.NameBash}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := has(agents.Toolset(tt.def, full), tools.NameBash); got != tt.want {
				t.Errorf("bash present = %v, want %v", got, tt.want)
			}
		})
	}
}
