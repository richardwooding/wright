package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

func TestBashEcho(t *testing.T) {
	f := newFixture(t, func(d *tools.Deps) { d.Redactor = redact.New() })
	got, err := f.text(tools.NameBash, `{"command":"echo hello; echo err >&2; echo ghp_`+strings.Repeat("C", 36)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"hello\n", "err\n", "[redacted: github", "[exit code 0, "} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "CCCCCCCC") || strings.Contains(got, "__WRIGHT_CWD") {
		t.Errorf("leaked token or marker:\n%s", got)
	}
	out, err := f.call(context.Background(), tools.NameBash, `{"command":"exit 3"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text(), "[exit code 3, ") || out.IsError {
		t.Errorf("exit 3 result = %+v", out)
	}
	if _, err := f.text(tools.NameBash, `{"command":""}`); err == nil {
		t.Error("empty command should fail")
	}
}

func TestBashSequential(t *testing.T) {
	f := newFixture(t, nil)
	tool, _ := f.ts.Lookup(tools.NameBash)
	if s, ok := tool.(agentkit.Sequential); !ok || !s.Sequential() {
		t.Error("bash must be Sequential")
	}
	tool, _ = f.ts.Lookup(tools.NameReadFile)
	if s, ok := tool.(agentkit.Sequential); ok && s.Sequential() {
		t.Error("read_file must not be Sequential")
	}
}

func TestBashCwdTracking(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.Mkdir(filepath.Join(f.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.text(tools.NameBash, `{"command":"cd sub"}`); err != nil {
		t.Fatal(err)
	}
	got, err := f.text(tools.NameBash, `{"command":"pwd"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, filepath.Join(f.root, "sub")+"\n") {
		t.Errorf("cwd not tracked:\n%s", got)
	}
	if f.deps.Cwd != nil {
		t.Fatal("fixture should not have set Cwd")
	}
	// Leaving the workspace is refused: the next call stays in sub.
	got, err = f.text(tools.NameBash, `{"command":"cd /"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "outside the workspace") {
		t.Errorf("expected a note:\n%s", got)
	}
	got, _ = f.text(tools.NameBash, `{"command":"pwd"}`)
	if !strings.HasPrefix(got, filepath.Join(f.root, "sub")+"\n") {
		t.Errorf("cwd escaped:\n%s", got)
	}
}

func TestBashSharedCwd(t *testing.T) {
	cwd := tools.NewCwd("")
	f := newFixture(t, func(d *tools.Deps) { d.Cwd = cwd })
	cwd.Set(f.root)
	if _, err := f.text(tools.NameBash, `{"command":"mkdir -p d && cd d"}`); err != nil {
		t.Fatal(err)
	}
	if cwd.Get() != filepath.Join(f.root, "d") {
		t.Errorf("shared cwd = %q", cwd.Get())
	}
}

func TestBashTimeoutKillsGroup(t *testing.T) {
	f := newFixture(t, nil)
	start := time.Now()
	out, err := f.call(context.Background(), tools.NameBash, `{"command":"echo before; sleep 30; echo after","timeout":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s: process group not killed", took)
	}
	if !out.IsError || !strings.Contains(out.Text(), "timed out after 1s") || !strings.Contains(out.Text(), "before") {
		t.Errorf("result = %+v", out)
	}
}

func TestBashCancel(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := f.call(ctx, tools.NameBash, `{"command":"sleep 30"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s after cancel", took)
	}
}

func TestBashTruncationSpill(t *testing.T) {
	f := newFixture(t, nil)
	got, err := f.text(tools.NameBash, `{"command":"i=0; while [ $i -lt 4000 ]; do echo line-$i-0123456789; i=$((i+1)); done"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "line-0-") || !strings.Contains(got, "line-3999-") {
		t.Errorf("head or tail missing:\n%.300s", got)
	}
	i := strings.Index(got, "[output truncated: ")
	if i < 0 {
		t.Fatalf("no truncation note:\n%.300s", got)
	}
	note := got[i : strings.Index(got[i:], "]")+i]
	if !strings.Contains(note, "full output saved to ") {
		t.Fatalf("note = %q", note)
	}
	path := note[strings.Index(note, "saved to ")+len("saved to "):]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 4000 {
		t.Errorf("spill has %d lines", strings.Count(string(data), "\n"))
	}
	if len(got) > 40*1024 {
		t.Errorf("result too long: %d bytes", len(got))
	}
}

func TestBashCommitTrailer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const trailer = "Co-Authored-By: wright <wright@example.com>"
	tests := []struct {
		name    string
		command string
		want    string // expected commit body suffix
		noted   bool
	}{
		{name: "double quoted", command: `git add . && git commit -q -m "add file"`, want: "add file\n\n" + trailer, noted: true},
		{name: "single quoted", command: `git add . && git commit -q -m 'add file'`, want: "add file\n\n" + trailer, noted: true},
		{name: "message equals", command: `git add . && git commit -q --message="add file"`, want: "add file\n\n" + trailer, noted: true},
		{name: "attached", command: `git add . && git commit -q -m"add file"`, want: "add file\n\n" + trailer, noted: true},
		{name: "already present", command: `git add . && git commit -q -m "add file` + "\n\n" + trailer + `"`, want: "add file\n\n" + trailer, noted: false},
		{name: "attribution off", command: `git add . && git commit -q -m "add file"`, want: "add file", noted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, func(d *tools.Deps) {
				d.Attribution = tt.name != "attribution off"
				d.Trailer = trailer
			})
			f.write("f.txt", "x\n")
			if _, err := f.text(tools.NameBash, `{"command":"git init -q && git checkout -q -b main"}`); err != nil {
				t.Fatal(err)
			}
			got, err := f.text(tools.NameBash, jsonArgs(map[string]any{"command": tt.command}))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, "[exit code 0, ") {
				t.Fatalf("commit failed:\n%s", got)
			}
			if noted := strings.Contains(got, "[note: appended the attribution trailer"); noted != tt.noted {
				t.Errorf("noted = %v, want %v:\n%s", noted, tt.noted, got)
			}
			body, err := f.text(tools.NameBash, `{"command":"git log -1 --format=%B"}`)
			if err != nil {
				t.Fatal(err)
			}
			body, _, _ = strings.Cut(body, "\n[exit code")
			if strings.TrimSpace(body) != tt.want {
				t.Errorf("commit body = %q, want %q", strings.TrimSpace(body), tt.want)
			}
		})
	}
}

func TestNetworkContext(t *testing.T) {
	if _, ok := tools.NetworkFrom(context.Background()); ok {
		t.Error("unset context should report ok=false")
	}
	ctx := tools.WithNetwork(context.Background(), true)
	if allow, ok := tools.NetworkFrom(ctx); !ok || !allow {
		t.Error("override lost")
	}
}

// TestBashSpillIsRedacted pins M3: the spill file is written by Clip, so
// redaction has to happen before it, not after. Otherwise the full secret
// lands in $XDG_CACHE_HOME/wright/spill and the model is handed the path.
func TestBashSpillIsRedacted(t *testing.T) {
	f := newFixture(t, func(d *tools.Deps) { d.Redactor = redact.New() })
	token := "ghp_" + strings.Repeat("D", 36)
	got, err := f.text(tools.NameBash, `{"command":"echo `+token+`; i=0; while [ $i -lt 4000 ]; do echo line-$i-0123456789; i=$((i+1)); done"}`)
	if err != nil {
		t.Fatal(err)
	}
	path := spillPath(t, got)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Errorf("spill file %s holds the unredacted secret", path)
	}
	if !strings.Contains(string(data), "[redacted: github") {
		t.Errorf("spill file %s has no redaction marker", path)
	}
	if strings.Contains(got, token) {
		t.Errorf("result holds the unredacted secret:\n%.200s", got)
	}
}

// spillPath pulls the spill file path out of a truncation note.
func spillPath(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "[output truncated: ")
	if i < 0 {
		t.Fatalf("no truncation note in:\n%.300s", out)
	}
	note := out[i : strings.Index(out[i:], "]")+i]
	const marker = "full output saved to "
	j := strings.Index(note, marker)
	if j < 0 {
		t.Fatalf("note names no spill file: %q", note)
	}
	return note[j+len(marker):]
}

// TestBashDescribeResolvesAgainstCwd pins M6: the analyzer must resolve a
// relative word against the working directory the command will actually run
// in (spec.Dir, which `cd` moves), not against the workspace root. Otherwise
// the verdict and the approval preview name a different file from the one
// the command opens.
func TestBashDescribeResolvesAgainstCwd(t *testing.T) {
	f := newFixture(t, nil)
	for _, dir := range []string{"sub", "sub/deep"} {
		if err := os.MkdirAll(filepath.Join(f.root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, ok := tools.Lookup(f.ts, tools.NameBash)
	if !ok {
		t.Fatal("bash has no Describer")
	}
	req, _, err := d.Describe(json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.root, "notes.txt"); !contains(req.Paths, want) {
		t.Errorf("at the root, read paths = %v, want %s", req.Paths, want)
	}
	if _, err := f.text(tools.NameBash, `{"command":"cd sub"}`); err != nil {
		t.Fatal(err)
	}
	req, _, err = d.Describe(json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.root, "sub", "notes.txt"); !contains(req.Paths, want) {
		t.Errorf("after cd sub, read paths = %v, want %s", req.Paths, want)
	}
	// Writes follow the same path resolution.
	req, _, err = d.Describe(json.RawMessage(`{"command":"echo hi > deep/out.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.root, "sub", "deep", "out.txt"); !contains(req.Writes, want) {
		t.Errorf("after cd sub, write paths = %v, want %s", req.Writes, want)
	}
	// Absolute paths are untouched by the rebase.
	abs := filepath.Join(f.root, "top.txt")
	req, _, err = d.Describe(json.RawMessage(`{"command":"cat ` + filepath.ToSlash(abs) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(req.Paths, abs) {
		t.Errorf("absolute path rewritten: %v, want %s", req.Paths, abs)
	}
}

func contains(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
