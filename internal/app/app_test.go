package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/richardwooding/llmkit"
	"github.com/richardwooding/llmkit/openaicompat"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/headless"
	"github.com/richardwooding/wright/internal/sandbox"
)

// step is one scripted model turn: a tool call when tool is set, else text.
type step struct {
	tool, args, text string
}

// fake is an in-process OpenAI-compatible endpoint registered with llmkit as
// provider "fake", so "fake/echo" resolves offline through the real client.
var fake struct {
	once   sync.Once
	mu     sync.Mutex
	script []step
	calls  int
}

func registerFake() {
	fake.once.Do(func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fake.mu.Lock()
			n := fake.calls
			fake.calls++
			var st step
			if n < len(fake.script) {
				st = fake.script[n]
			} else {
				st = step{text: "done"}
			}
			fake.mu.Unlock()
			// The engine streams, so answer as server-sent events: one delta
			// chunk, then a finish chunk carrying the usage.
			w.Header().Set("Content-Type", "text/event-stream")
			delta, finish := `{"role":"assistant","content":`+quote(st.text)+`}`, "stop"
			if st.tool != "" {
				delta = `{"role":"assistant","tool_calls":[{"index":0,"id":"c` + string(rune('1'+n)) + `","type":"function","function":{"name":"` + st.tool + `","arguments":` + quote(st.args) + `}}]}`
				finish = "tool_calls"
			}
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":`+delta+`,"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}],"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}}`+"\n\n")
		}))
		llmkit.Register(openaicompat.NewProvider(openaicompat.Config{ID: "fake", BaseURL: srv.URL, KeyOptional: true}))
	})
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// setScript installs the model script for one test.
func setScript(t *testing.T, s []step) {
	t.Helper()
	registerFake()
	fake.mu.Lock()
	fake.script, fake.calls = s, 0
	fake.mu.Unlock()
}

// isolate points every user-level path at a temp directory and clears the
// WRIGHT_* overrides so the real home and environment are never touched.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WRIGHT_CONFIG_DIR", filepath.Join(home, "config"))
	t.Setenv("WRIGHT_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	for _, k := range []string{"WRIGHT_MODEL", "WRIGHT_MODE", "WRIGHT_SANDBOX", "WRIGHT_PLAIN"} {
		t.Setenv(k, "")
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func baseOpts(ws string) app.RunOptions {
	return app.RunOptions{
		Print:   true,
		Output:  "stream-json",
		Model:   "fake/echo",
		Cwd:     ws,
		Sandbox: "none",
		Version: "test",
		Env:     os.Getenv,
	}
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

const editArgs = `{"path":"a.txt","old_string":"hello","new_string":"bye"}`

func TestHeadlessSmoke(t *testing.T) {
	tests := []struct {
		name      string
		script    []step
		mutate    func(*app.RunOptions)
		wantCode  int
		wantFile  string
		wantOut   string
		wantErrIn string
	}{
		{
			name:     "plain prompt",
			script:   []step{{text: "hi there"}},
			wantCode: headless.ExitOK,
			wantFile: "hello world\n",
			wantOut:  "hi there",
		},
		{
			name:      "edit needs approval",
			script:    []step{{tool: "edit_file", args: editArgs}, {text: "could not edit"}},
			wantCode:  headless.ExitApprovalRequired,
			wantFile:  "hello world\n",
			wantErrIn: "--allow",
		},
		{
			name:     "edit allowed by flag rule",
			script:   []step{{tool: "edit_file", args: editArgs}, {text: "edited"}},
			mutate:   func(o *app.RunOptions) { o.Allow = []string{"edit_file(**)"} },
			wantCode: headless.ExitOK,
			wantFile: "bye world\n",
			wantOut:  "edited",
		},
		{
			name:     "stdin becomes the prompt",
			script:   []step{{text: "from stdin"}},
			mutate:   func(o *app.RunOptions) { o.Print = false; o.Stdin = strings.NewReader("summarize this\n") },
			wantCode: headless.ExitOK,
			wantFile: "hello world\n",
			wantOut:  "from stdin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := isolate(t)
			setScript(t, tt.script)
			o := baseOpts(ws)
			o.Prompt = "do the thing"
			if tt.mutate != nil {
				tt.mutate(&o)
			}
			if o.Stdin != nil {
				o.Prompt = ""
			}
			var stdout, stderr bytes.Buffer
			o.Stdout, o.Stderr = &stdout, &stderr
			code, err := app.Run(context.Background(), o, nil)
			if err != nil || code != tt.wantCode {
				t.Fatalf("Run = %d, %v; want %d\nstdout: %s\nstderr: %s", code, err, tt.wantCode, stdout.String(), stderr.String())
			}
			lines := decodeLines(t, stdout.String())
			last := lines[len(lines)-1]
			if last.Type != "result" || last.ExitCode == nil || *last.ExitCode != tt.wantCode || last.SessionID == "" {
				t.Fatalf("result line = %+v", last)
			}
			if tt.wantOut != "" && last.Output != tt.wantOut {
				t.Fatalf("output = %q, want %q", last.Output, tt.wantOut)
			}
			if tt.wantErrIn != "" && !strings.Contains(last.Error+stdout.String(), tt.wantErrIn) {
				t.Fatalf("expected %q in the report, got %s", tt.wantErrIn, stdout.String())
			}
			got, _ := os.ReadFile(filepath.Join(ws, "a.txt"))
			if string(got) != tt.wantFile {
				t.Fatalf("a.txt = %q, want %q", got, tt.wantFile)
			}
			if lines[0].Type == "" {
				t.Fatal("first line has no type")
			}
		})
	}
}

func TestStreamJSONShapes(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{tool: "read_file", args: `{"path":"a.txt"}`}, {text: "read it"}})
	o := baseOpts(ws)
	o.Prompt = "read a.txt"
	var stdout, stderr bytes.Buffer
	o.Stdout, o.Stderr = &stdout, &stderr
	if code, err := app.Run(context.Background(), o, nil); err != nil || code != 0 {
		t.Fatalf("Run = %d, %v: %s", code, err, stderr.String())
	}
	seen := map[string]bool{}
	for _, l := range decodeLines(t, stdout.String()) {
		seen[l.Type] = true
		if l.Type == "tool_result" && (l.Result == nil || !strings.Contains(l.Result.Text, "hello world") || l.Result.IsError) {
			t.Errorf("tool_result = %+v", l)
		}
	}
	for _, k := range []string{"run_started", "tool_call", "tool_result", "usage", "result"} {
		if !seen[k] {
			t.Errorf("missing %s line:\n%s", k, stdout.String())
		}
	}
}

func TestBypassRefusedWithoutSandbox(t *testing.T) {
	if sandbox.InContainer() {
		t.Skip("a container counts as a sandbox boundary")
	}
	ws := isolate(t)
	setScript(t, nil)
	o := baseOpts(ws)
	o.Bypass = true
	if _, err := app.Build(context.Background(), o); !errors.Is(err, app.ErrBypassUnsandboxed) {
		t.Fatalf("Build error = %v, want ErrBypassUnsandboxed", err)
	}
	o.AllowUnsandboxedBypass = true
	b, err := app.Build(context.Background(), o)
	if err != nil {
		t.Fatalf("Build with --allow-unsandboxed-bypass: %v", err)
	}
	defer func() { _ = b.Close() }()
	if !b.Engine.Status().Bypass || !strings.Contains(strings.Join(b.Warnings, "\n"), "bypass") {
		t.Fatalf("bypass not reflected: status=%+v warnings=%v", b.Engine.Status(), b.Warnings)
	}
}

func TestModeBypassNeedsFlag(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	o := baseOpts(ws)
	o.Mode = "bypass"
	if _, err := app.Build(context.Background(), o); !errors.Is(err, app.ErrBypassNeedsFlag) {
		t.Fatalf("Build error = %v, want ErrBypassNeedsFlag", err)
	}
}

func TestUntrustedProjectSettingsAreInert(t *testing.T) {
	ws := isolate(t)
	setScript(t, []step{{tool: "edit_file", args: editArgs}, {text: "no"}})
	if err := os.MkdirAll(filepath.Join(ws, ".wright"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".wright", "settings.json"), []byte(`{"permissions":{"allow":["edit_file(**)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(ws)
	o.Prompt = "edit"
	var stdout, stderr bytes.Buffer
	o.Stdout, o.Stderr = &stdout, &stderr
	code, err := app.Run(context.Background(), o, nil)
	if err != nil || code != headless.ExitApprovalRequired {
		t.Fatalf("untrusted project allow rule must not apply: code=%d err=%v\n%s", code, err, stdout.String())
	}
	if !strings.Contains(stderr.String(), "not trusted") {
		t.Fatalf("expected a trust warning on stderr, got %q", stderr.String())
	}
	eff, err := app.LoadEffective(ws, os.Getenv)
	if err != nil || eff.Trusted || len(eff.Settings.Permissions.Allow) != len(config.Defaults().Permissions.Allow) {
		t.Fatalf("LoadEffective = %+v, %v", eff, err)
	}
}

func TestRunWithoutInteractive(t *testing.T) {
	ws := isolate(t)
	o := baseOpts(ws)
	o.Print, o.IsTerminal = false, true
	if _, err := app.Run(context.Background(), o, nil); !errors.Is(err, app.ErrNoInteractive) {
		t.Fatalf("err = %v, want ErrNoInteractive", err)
	}
}

func TestInitProject(t *testing.T) {
	root := isolate(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	written, err := app.InitProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 3 {
		t.Fatalf("written = %v", written)
	}
	for _, rel := range []string{"AGENTS.md", ".wright/settings.json", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s not created: %v", rel, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, ".wright", "settings.json"))
	if _, err := config.Decode(data); err != nil {
		t.Fatalf("settings skeleton does not decode: %v", err)
	}
	// The file init wrote is the user's own: it must not trigger the trust warning.
	eff, err := app.LoadEffective(root, os.Getenv)
	if err != nil || !eff.Trusted {
		t.Fatalf("freshly written settings should be trusted: %+v, %v", eff, err)
	}
	again, err := app.InitProject(root)
	if err != nil || len(again) != 0 {
		t.Fatalf("second init should be a no-op, got %v, %v", again, err)
	}
}

func TestBuildDetectsBwrap(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	if b, _ := sandbox.Detect(context.Background(), sandbox.NameBwrap); b.Name() != sandbox.NameBwrap {
		t.Skip("bwrap present but unusable here")
	}
	ws := isolate(t)
	setScript(t, nil)
	o := baseOpts(ws)
	o.Sandbox = sandbox.NameBwrap
	b, err := app.Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if b.Sandbox.Name() != sandbox.NameBwrap || b.Engine.Status().Sandbox != sandbox.NameBwrap {
		t.Fatalf("sandbox = %s, status = %+v", b.Sandbox.Name(), b.Engine.Status())
	}
}
