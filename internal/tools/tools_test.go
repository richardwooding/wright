package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/snapshot"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/workspace"
)

// fixture is a temporary workspace with a toolset over it.
type fixture struct {
	t    testing.TB
	ws   *workspace.Workspace
	root string
	deps tools.Deps
	ts   agentkit.Toolset
}

// newFixture builds a workspace in a temp dir. mutate customizes Deps
// before the toolset is built.
func newFixture(t testing.TB, mutate func(*tools.Deps)) *fixture {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := sandbox.Detect(context.Background(), sandbox.NameNone)
	env := sandbox.EnvFrom(os.Environ(), nil, map[string]string{
		"HOME": t.TempDir(), "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	})
	deps := tools.Deps{
		WS:          ws,
		Sandbox:     backend,
		SandboxSpec: sandbox.Spec{ReadWrite: ws.Roots, Env: env},
		SpillDir:    t.TempDir(),
		RunID:       func(context.Context) string { return "run-1" },
		// The test servers live on 127.0.0.1, which web_fetch refuses by
		// default; TestWebFetchRefusesLocalHosts covers the refusal.
		AllowLocalFetch: true,
	}
	if mutate != nil {
		mutate(&deps)
	}
	f := &fixture{t: t, ws: ws, root: ws.Root(), deps: deps}
	f.ts = tools.New(deps)
	return f
}

func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	abs := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return abs
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// call invokes a tool with JSON arguments.
func (f *fixture) call(ctx context.Context, name, args string) (agentkit.Output, error) {
	f.t.Helper()
	tool, ok := f.ts.Lookup(name)
	if !ok {
		f.t.Fatalf("tool %s not registered", name)
	}
	return tool.Call(ctx, json.RawMessage(args))
}

func (f *fixture) text(name, args string) (string, error) {
	f.t.Helper()
	out, err := f.call(context.Background(), name, args)
	return out.Text(), err
}

func (f *fixture) describe(name, args string) (tools.Preview, error) {
	f.t.Helper()
	d, ok := tools.Lookup(f.ts, name)
	if !ok {
		f.t.Fatalf("tool %s has no Describer", name)
	}
	_, p, err := d.Describe(json.RawMessage(args))
	return p, err
}

// jsonArgs marshals a map for readability in tables.
func jsonArgs(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestReadFile(t *testing.T) {
	f := newFixture(t, func(d *tools.Deps) { d.Redactor = redact.New() })
	f.write("a.txt", "one\ntwo\nthree\nfour\n")
	f.write("long.txt", strings.Repeat("x", 3000)+"\n")
	f.write("empty.txt", "")
	f.write("bin.dat", "abc\x00def")
	f.write(".env", "SECRET=1\n")
	f.write("secrets/hidden.txt", "hidden\n")
	f.write(".wrightignore", "secrets/\n")
	f.write("token.txt", "key ghp_"+strings.Repeat("A", 36)+"\n")
	os.Mkdir(filepath.Join(f.root, "dir"), 0o755) //nolint:errcheck // fixture

	tests := []struct {
		name    string
		args    map[string]any
		want    []string // substrings that must appear
		absent  []string
		wantErr string
	}{
		{name: "numbered", args: map[string]any{"path": "a.txt"}, want: []string{"     1\tone", "     4\tfour"}},
		{name: "absolute path", args: map[string]any{"path": filepath.Join(f.root, "a.txt")}, want: []string{"     1\tone"}},
		{name: "offset and limit", args: map[string]any{"path": "a.txt", "offset": 2, "limit": 2}, want: []string{"     2\ttwo", "     3\tthree", "continue with offset=4"}, absent: []string{"four"}},
		{name: "limit reaching end has no note", args: map[string]any{"path": "a.txt", "offset": 3, "limit": 2}, want: []string{"     4\tfour"}, absent: []string{"truncated"}},
		{name: "long line clipped", args: map[string]any{"path": "long.txt"}, want: []string{"[line truncated]"}},
		{name: "empty", args: map[string]any{"path": "empty.txt"}, want: []string{"(empty file)"}},
		{name: "redacted", args: map[string]any{"path": "token.txt"}, want: []string{"[redacted: github"}, absent: []string{"AAAAAAAA"}},
		{name: "offset past end", args: map[string]any{"path": "a.txt", "offset": 10}, wantErr: "past the end"},
		{name: "missing", args: map[string]any{"path": "nope.txt"}, wantErr: "no such file"},
		{name: "directory", args: map[string]any{"path": "dir"}, wantErr: "is a directory"},
		{name: "binary", args: map[string]any{"path": "bin.dat"}, wantErr: "binary (7 bytes, application/octet-stream)"},
		{name: "secret file refused", args: map[string]any{"path": ".env"}, wantErr: "credential file"},
		{name: "hidden file refused", args: map[string]any{"path": "secrets/hidden.txt"}, wantErr: "hidden by .wrightignore"},
		{name: "no path", args: map[string]any{}, wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameReadFile, jsonArgs(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("output should not contain %q:\n%s", a, got)
				}
			}
		})
	}
}

func TestReadFileImage(t *testing.T) {
	f := newFixture(t, nil)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	f.write("pic.png", png)
	f.write("big.png", strings.Repeat("\x00", 5<<20+1))
	out, err := f.call(context.Background(), tools.NameReadFile, `{"path":"pic.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	var img *core.ImagePart
	for _, p := range out.Content {
		if ip, ok := p.(core.ImagePart); ok {
			img = &ip
		}
	}
	if img == nil {
		t.Fatalf("no image part in %#v", out.Content)
	}
	if !strings.Contains(out.Text(), "image/png") {
		t.Errorf("caption = %q", out.Text())
	}
	if _, err := f.call(context.Background(), tools.NameReadFile, `{"path":"big.png"}`); err == nil || !strings.Contains(err.Error(), "images over") {
		t.Errorf("big image err = %v", err)
	}
}

func TestWriteFile(t *testing.T) {
	snapDir := t.TempDir()
	snap, err := snapshot.Open(snapDir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(d *tools.Deps) { d.Snap = snap })
	f.write("exists.txt", "old\n")
	if err := os.Chmod(filepath.Join(f.root, "exists.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr string
	}{
		{name: "new file with parents", args: map[string]any{"path": "a/b/new.txt", "content": "hi\nthere"}, want: "Wrote 8 bytes (2 lines) to a/b/new.txt"},
		{name: "overwrite", args: map[string]any{"path": "exists.txt", "content": "new\n"}, want: "Wrote 4 bytes (1 lines) to exists.txt"},
		{name: "protected .git", args: map[string]any{"path": ".git/config", "content": "x"}, wantErr: "protected path"},
		{name: "secret name", args: map[string]any{"path": "conf/.env", "content": "x"}, wantErr: "credential file"},
		{name: "outside missing parent", args: map[string]any{"path": filepath.Join(outside, "no", "such", "f.txt"), "content": "x"}, wantErr: "outside the workspace"},
		{name: "outside existing dir", args: map[string]any{"path": filepath.Join(outside, "f.txt"), "content": "x"}, wantErr: "outside the workspace"},
		{name: "directory target", args: map[string]any{"path": "a", "content": "x"}, wantErr: "is a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameWriteFile, jsonArgs(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	if got := f.read("a/b/new.txt"); got != "hi\nthere" {
		t.Errorf("content = %q", got)
	}
	info, err := os.Stat(filepath.Join(f.root, "exists.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode not preserved: %v", info.Mode())
	}
	entries, err := snap.Entries("run-1")
	if err != nil {
		t.Fatal(err)
	}
	var absent, present bool
	for _, e := range entries {
		switch filepath.Base(e.Path) {
		case "new.txt":
			absent = e.Absent
		case "exists.txt":
			present = !e.Absent
		}
	}
	if !absent || !present {
		t.Errorf("snapshots = %+v", entries)
	}
}

func TestEditFile(t *testing.T) {
	f := newFixture(t, nil)
	const src = "package main\n\nfunc a() {}\n\nfunc b() {}\n\nfunc b() {}\n"
	tests := []struct {
		name    string
		args    map[string]any
		want    string
		after   string
		wantErr string
	}{
		{
			name:  "single",
			args:  map[string]any{"path": "m.go", "old_string": "func a() {}", "new_string": "func a() int { return 1 }"},
			want:  "Edited m.go: replaced 1 occurrence at line 3",
			after: strings.Replace(src, "func a() {}", "func a() int { return 1 }", 1),
		},
		{
			name:  "replace all",
			args:  map[string]any{"path": "m.go", "old_string": "func b() {}", "new_string": "func c() {}", "replace_all": true},
			want:  "Edited m.go: replaced 2 occurrences at lines 5, 7",
			after: strings.ReplaceAll(src, "func b() {}", "func c() {}"),
		},
		{
			name:  "multiline",
			args:  map[string]any{"path": "m.go", "old_string": "func a() {}\n\nfunc b() {}", "new_string": "func ab() {}"},
			want:  "Edited m.go: replaced 1 occurrence at line 3",
			after: "package main\n\nfunc ab() {}\n\nfunc b() {}\n",
		},
		{name: "ambiguous", args: map[string]any{"path": "m.go", "old_string": "func b() {}", "new_string": "x"}, wantErr: "occurs 2 times in m.go (lines 5, 7)"},
		{name: "not found with hint", args: map[string]any{"path": "m.go", "old_string": "func a() { panic() }", "new_string": "x"}, wantErr: "not found in m.go; closest line 3: func a() {}"},
		{name: "not found no hint", args: map[string]any{"path": "m.go", "old_string": "zzz", "new_string": "x"}, wantErr: "not found in m.go"},
		{name: "identical", args: map[string]any{"path": "m.go", "old_string": "func a() {}", "new_string": "func a() {}"}, wantErr: "identical"},
		{name: "empty old", args: map[string]any{"path": "m.go", "old_string": "", "new_string": "x"}, wantErr: "must not be empty"},
		{name: "missing file", args: map[string]any{"path": "none.go", "old_string": "a", "new_string": "b"}, wantErr: "no such file"},
		{name: "secret file", args: map[string]any{"path": "id_rsa", "old_string": "a", "new_string": "b"}, wantErr: "credential file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.write("m.go", src)
			got, err := f.text(tools.NameEditFile, jsonArgs(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if f.read("m.go") != src {
					t.Error("file changed on error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if after := f.read("m.go"); after != tt.after {
				t.Errorf("content =\n%s\nwant\n%s", after, tt.after)
			}
		})
	}
}

func TestDescribePreviews(t *testing.T) {
	f := newFixture(t, nil)
	f.write("x.txt", "alpha\nbeta\ngamma\n")

	t.Run("write diff", func(t *testing.T) {
		p, err := f.describe(tools.NameWriteFile, `{"path":"x.txt","content":"alpha\nBETA\ngamma\n"}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{"--- a/x.txt", "+++ b/x.txt", "-beta", "+BETA"} {
			if !strings.Contains(p.Diff, w) {
				t.Errorf("diff missing %q:\n%s", w, p.Diff)
			}
		}
	})
	t.Run("write new file", func(t *testing.T) {
		p, err := f.describe(tools.NameWriteFile, `{"path":"new.txt","content":"a\nb\nc"}`)
		if err != nil {
			t.Fatal(err)
		}
		if p.Body != "new file, 3 lines" || p.Diff != "" {
			t.Errorf("preview = %+v", p)
		}
		if _, err := os.Stat(filepath.Join(f.root, "new.txt")); err == nil {
			t.Error("describe must not write")
		}
	})
	t.Run("edit diff", func(t *testing.T) {
		p, err := f.describe(tools.NameEditFile, `{"path":"x.txt","old_string":"beta","new_string":"delta"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Diff, "-beta") || !strings.Contains(p.Diff, "+delta") {
			t.Errorf("diff:\n%s", p.Diff)
		}
		if f.read("x.txt") != "alpha\nbeta\ngamma\n" {
			t.Error("describe must not edit")
		}
	})
	t.Run("edit error surfaces", func(t *testing.T) {
		if _, err := f.describe(tools.NameEditFile, `{"path":"x.txt","old_string":"nope","new_string":"x"}`); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("read paths absolute", func(t *testing.T) {
		d, _ := tools.Lookup(f.ts, tools.NameReadFile)
		req, _, err := d.Describe(json.RawMessage(`{"path":"x.txt"}`))
		if err != nil {
			t.Fatal(err)
		}
		if req.Tool != tools.NameReadFile || len(req.Paths) != 1 || req.Paths[0] != filepath.Join(f.root, "x.txt") {
			t.Errorf("request = %+v", req)
		}
	})
	t.Run("symlink out resolves", func(t *testing.T) {
		// t.TempDir is itself under a symlink on macOS (/var → /private/var),
		// so the expectation is the resolved directory, not the one handed out.
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(f.root, "link")); err != nil {
			t.Skip("symlinks unavailable")
		}
		if real, err := filepath.EvalSymlinks(outside); err == nil {
			outside = real
		}
		d, _ := tools.Lookup(f.ts, tools.NameWriteFile)
		req, _, err := d.Describe(json.RawMessage(`{"path":"link/f.txt","content":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(req.Writes[0], outside) {
			t.Errorf("write path %q not resolved through the symlink", req.Writes[0])
		}
	})
	t.Run("bash analysis", func(t *testing.T) {
		d, _ := tools.Lookup(f.ts, tools.NameBash)
		req, p, err := d.Describe(json.RawMessage(`{"command":"go test ./... > out.log","description":"run tests"}`))
		if err != nil {
			t.Fatal(err)
		}
		if req.Shell == nil || len(req.Shell.Commands) == 0 {
			t.Fatalf("no analysis: %+v", req)
		}
		if p.Title != "run tests" || !strings.Contains(p.Body, "$ go test") || !strings.Contains(p.Body, req.Shell.Summary()) {
			t.Errorf("preview = %+v", p)
		}
		if len(req.Writes) != 1 || filepath.Base(req.Writes[0]) != "out.log" {
			t.Errorf("writes = %v", req.Writes)
		}
	})
	t.Run("bash network flag", func(t *testing.T) {
		d, _ := tools.Lookup(f.ts, tools.NameBash)
		req, _, err := d.Describe(json.RawMessage(`{"command":"go mod download","network":true}`))
		if err != nil {
			t.Fatal(err)
		}
		if !req.Network {
			t.Error("Network not propagated")
		}
	})
	t.Run("bash empty", func(t *testing.T) {
		if _, err := f.describe(tools.NameBash, `{"command":"  "}`); err == nil {
			t.Error("expected error")
		}
	})
}

func FuzzEditFile(f *testing.F) {
	const src = "alpha\nbeta\ngamma\nbeta\n"
	f.Add("beta", "BETA", false)
	f.Add("beta", "BETA", true)
	f.Add("alpha\nbeta", "x", false)
	f.Add("", "x", false)
	f.Add("zzz", "", false)
	f.Add("a", "a", true)
	fx := newFixture(f, nil)
	f.Fuzz(func(t *testing.T, old, repl string, all bool) {
		if !utf8.ValidString(old) || !utf8.ValidString(repl) {
			t.Skip("tool arguments arrive as JSON, which is always valid UTF-8")
		}
		fx.t = t // helpers must report against the fuzz iteration, not the F
		fx.write("f.txt", src)
		args := jsonArgs(map[string]any{"path": "f.txt", "old_string": old, "new_string": repl, "replace_all": all})
		_, err := fx.text(tools.NameEditFile, args)
		got := fx.read("f.txt")
		if err != nil {
			if got != src {
				t.Fatalf("file changed after error %v", err)
			}
			return
		}
		want := strings.Replace(src, old, repl, 1)
		if all {
			want = strings.ReplaceAll(src, old, repl)
		}
		if got != want {
			t.Fatalf("content %q, want %q", got, want)
		}
	})
}

// TestAChildToolsetIsolatesOnlyTheDirectory pins the shape app relies on for
// sub-agents: a second toolset built from the same Deps with its own
// CwdState. A child's `cd` must not move the parent — the parent's relative
// paths are resolved against that directory when its *next* command is
// described, so a leaked `cd` changes which files a permission decision is
// about — while everything else in Deps stays shared, or /jobs would stop
// seeing a child's work and an undo would stop covering its edits.
func TestAChildToolsetIsolatesOnlyTheDirectory(t *testing.T) {
	parentCwd := tools.NewCwd("")
	f := newFixture(t, func(d *tools.Deps) { d.Cwd = parentCwd })
	parentCwd.Set(f.root)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Exactly what app does when it builds the sub-agent toolset.
	child := f.deps
	child.Cwd = tools.NewCwd(f.deps.WS.Root())
	childSet := tools.New(child)

	run := func(ts agentkit.Toolset, cmd string) {
		t.Helper()
		tl, ok := ts.Lookup(tools.NameBash)
		if !ok {
			t.Fatal("no bash tool")
		}
		if _, err := tl.Call(context.Background(), json.RawMessage(cmd)); err != nil {
			t.Fatal(err)
		}
	}

	run(childSet, `{"command":"cd sub"}`)
	if got := child.Cwd.Get(); got != filepath.Join(f.root, "sub") {
		t.Errorf("the child did not move: %q", got)
	}
	if got := parentCwd.Get(); got != f.root {
		t.Errorf("the child's cd moved the parent to %q; it must stay where it was", got)
	}
	// And the reverse: the parent moving does not drag the child.
	if _, err := f.text(tools.NameBash, `{"command":"cd sub"}`); err != nil {
		t.Fatal(err)
	}
	if got := child.Cwd.Get(); got != filepath.Join(f.root, "sub") {
		t.Errorf("the child's directory followed the parent: %q", got)
	}

	// Everything else is the same state, by identity.
	if child.Jobs != f.deps.Jobs || child.Todos != f.deps.Todos || child.WS != f.deps.WS || child.Snap != f.deps.Snap {
		t.Error("a child toolset must share jobs, todos, snapshots and the workspace; only the directory is isolated")
	}
}
