package headless_test

import (
	"bytes"
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
	"github.com/richardwooding/wright/internal/headless"
	"github.com/richardwooding/wright/internal/model"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/workspace"
)

type echoArgs struct {
	Path string `json:"path"`
}

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

func newEngine(t *testing.T, client core.Chatter, mutate func(*engine.Options)) *engine.Engine {
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
		Headless:      true,
	}
	if mutate != nil {
		mutate(&o)
	}
	eng, err := engine.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func decodeLines(t *testing.T, out string) []headless.Line {
	t.Helper()
	var lines []headless.Line
	for raw := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var l headless.Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line %q does not decode: %v", raw, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		client   *enginetest.Scripted
		prompt   string
		mutate   func(*engine.Options)
		wantCode int
		wantStop string
	}{
		{
			name:     "plain text run",
			client:   &enginetest.Scripted{Responses: []*core.Response{enginetest.TextResp("hello there")}},
			prompt:   "hi",
			wantCode: headless.ExitOK,
			wantStop: "completed",
		},
		{
			name:     "empty prompt is a usage error",
			client:   &enginetest.Scripted{},
			prompt:   "   ",
			wantCode: headless.ExitUsage,
		},
		{
			name:     "edit needing approval",
			client:   &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "edit_file", `{"path":"a.go"}`), enginetest.TextResp("understood")}},
			prompt:   "edit a.go",
			wantCode: headless.ExitApprovalRequired,
			wantStop: "completed",
		},
		{
			name:     "provider error",
			client:   &enginetest.Scripted{Err: errors.New("provider exploded")},
			prompt:   "hi",
			wantCode: headless.ExitError,
			wantStop: "error",
		},
		{
			name: "step budget",
			client: &enginetest.Scripted{Responses: []*core.Response{
				enginetest.CallResp("c1", "read_file", `{"path":"a.go"}`),
				enginetest.CallResp("c2", "read_file", `{"path":"b.go"}`),
				enginetest.TextResp("done"),
			}},
			prompt:   "read things",
			mutate:   func(o *engine.Options) { o.MaxSteps = 1 },
			wantCode: headless.ExitBudget,
			wantStop: "max_steps",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := newEngine(t, tt.client, tt.mutate)
			var out, errw bytes.Buffer
			code := headless.Run(context.Background(), eng, tt.prompt, nil, headless.FormatJSON, &out, &errw, false)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d (out=%s err=%s)", code, tt.wantCode, out.String(), errw.String())
			}
			lines := decodeLines(t, out.String())
			if len(lines) != 1 || lines[0].Type != "result" {
				t.Fatalf("json format must print exactly one result line, got %+v", lines)
			}
			res := lines[0]
			if res.ExitCode != tt.wantCode || res.StopReason != tt.wantStop {
				t.Fatalf("result = %+v", res)
			}
			if tt.wantCode == headless.ExitApprovalRequired && !strings.Contains(res.Error, engine.HeadlessDenialMarker) {
				t.Fatalf("result error should explain the denial: %+v", res)
			}
		})
	}
}

func TestStreamJSONLines(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "read_file", `{"path":"a.go"}`), enginetest.TextResp("all good")}}
	eng := newEngine(t, client, nil)
	var out, errw bytes.Buffer
	if code := headless.Run(context.Background(), eng, "read a.go", nil, headless.FormatStreamJSON, &out, &errw, false); code != headless.ExitOK {
		t.Fatalf("exit = %d: %s", code, errw.String())
	}
	lines := decodeLines(t, out.String())
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l.Type] = true
		switch l.Type {
		case "tool_call":
			if l.Tool == nil || l.Tool.Name != "read_file" || l.Tool.ID != "c1" {
				t.Errorf("tool_call line = %+v", l)
			}
		case "tool_result":
			if l.Result == nil || !strings.Contains(l.Result.Text, "content of a.go") || l.Result.IsError {
				t.Errorf("tool_result line = %+v", l)
			}
		case "usage":
			if l.Usage == nil || l.Usage.InputTokens == 0 || l.ContextPct == 0 {
				t.Errorf("usage line = %+v", l)
			}
		}
	}
	for _, k := range []string{"run_started", "tool_call", "tool_result", "text", "usage", "result"} {
		if !seen[k] {
			t.Errorf("missing %s line in %s", k, out.String())
		}
	}
	last := lines[len(lines)-1]
	if last.Type != "result" || last.Output != "all good" || last.ToolCalls != 1 || last.SessionID != "s1" || last.Usage == nil {
		t.Fatalf("result line = %+v", last)
	}
	if last.Time.IsZero() == false && lines[0].Time.IsZero() {
		t.Fatal("event lines should carry timestamps")
	}
}

func TestTextFormat(t *testing.T) {
	client := &enginetest.Scripted{Responses: []*core.Response{enginetest.CallResp("c1", "read_file", `{"path":"a.go"}`), enginetest.TextResp("final answer")}}
	eng := newEngine(t, client, nil)
	var out, errw bytes.Buffer
	if code := headless.Run(context.Background(), eng, "go", nil, headless.FormatText, &out, &errw, true); code != headless.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if out.String() != "final answer\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if !strings.Contains(errw.String(), "[tool] read_file") || !strings.Contains(errw.String(), "→ ok") {
		t.Fatalf("verbose stderr lacks tool one-liners: %q", errw.String())
	}
}

func TestInterrupt(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	eng := newEngine(t, &enginetest.Scripted{Block: block}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	var out, errw bytes.Buffer
	code := headless.Run(ctx, eng, "slow", nil, headless.FormatJSON, &out, &errw, false)
	if code != headless.ExitInterrupted {
		t.Fatalf("exit = %d, out=%s", code, out.String())
	}
	if res := decodeLines(t, out.String())[0]; res.StopReason != "canceled" {
		t.Fatalf("result = %+v", res)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]headless.Format{"": headless.FormatText, "text": headless.FormatText, "JSON": headless.FormatJSON, "stream-json": headless.FormatStreamJSON} {
		got, err := headless.ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := headless.ParseFormat("yaml"); err == nil {
		t.Error("yaml should be rejected")
	}
}
