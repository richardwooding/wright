package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/sandbox"
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

// recordingBackend keeps the Spec of every call and runs nothing: a test
// that asserts on what an approved `brew install` would be given must not
// install anything on the machine it runs on.
type recordingBackend struct{ specs []sandbox.Spec }

func (b *recordingBackend) Name() string                    { return "recording" }
func (b *recordingBackend) Available(context.Context) error { return nil }
func (b *recordingBackend) Command(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error) {
	b.specs = append(b.specs, spec)
	return exec.CommandContext(ctx, "true"), nil
}

func (b *recordingBackend) last(t *testing.T) sandbox.Spec {
	t.Helper()
	if len(b.specs) == 0 {
		t.Fatal("no sandboxed command was built")
	}
	return b.specs[len(b.specs)-1]
}

// TestBashGrantAppliesToOneCallOnly pins the per-call widening: the grant the
// engine puts on the context reaches this call's Spec and nothing else. The
// base spec is shared by every call, so appending to its slice in place would
// hand the next command the last one's writable prefixes.
func TestBashGrantAppliesToOneCallOnly(t *testing.T) {
	rec := &recordingBackend{}
	prefix := t.TempDir()
	f := newFixture(t, func(d *tools.Deps) { d.Sandbox = rec })
	granted := sandbox.WithGrant(context.Background(), sandbox.Grant{Network: true, Writable: []string{prefix}})
	if _, err := f.call(granted, tools.NameBash, `{"command":"echo install"}`); err != nil {
		t.Fatal(err)
	}
	spec := rec.last(t)
	if !spec.Network {
		t.Error("granted network did not reach the spec")
	}
	if !slices.Contains(spec.ReadWrite, prefix) {
		t.Errorf("granted prefix missing from ReadWrite: %v", spec.ReadWrite)
	}
	if _, err := f.call(context.Background(), tools.NameBash, `{"command":"echo plain"}`); err != nil {
		t.Fatal(err)
	}
	spec = rec.last(t)
	if spec.Network {
		t.Error("the next call kept the granted network")
	}
	if slices.Contains(spec.ReadWrite, prefix) {
		t.Errorf("the next call kept the granted prefix: %v", spec.ReadWrite)
	}
}

// TestBashNetworkArgumentDoesNotSelfGrant pins that the model's own argument
// reaches the sandbox only because the engine rewrote it to the verdict: the
// tool has no opinion, so a grant-free call with network:true still runs with
// whatever the base spec says. (The engine-side pinning is what makes that
// argument trustworthy; see TestApprovalGrantsWhatTheCommandNeeds.)
func TestBashGrantIsTheOnlyWidening(t *testing.T) {
	rec := &recordingBackend{}
	f := newFixture(t, func(d *tools.Deps) { d.Sandbox = rec })
	if _, err := f.text(tools.NameBash, `{"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	spec := rec.last(t)
	if spec.Network {
		t.Error("a call with no grant must not have network")
	}
	if len(spec.ReadWrite) != len(f.deps.SandboxSpec.ReadWrite) {
		t.Errorf("ReadWrite widened without a grant: %v", spec.ReadWrite)
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
	_, after, ok := strings.Cut(note, marker)
	if !ok {
		t.Fatalf("note names no spill file: %q", note)
	}
	return after
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
	return slices.Contains(paths, want)
}

// TestBashIsNotALoginShell pins that the tool runs `bash -c`, not `bash -lc`:
// a login shell sources /etc/profile, /etc/profile.d/* and — on the none
// backend, where $HOME is the user's own — ~/.bash_profile, any of which can
// put back what the sandbox's filtered environment left out.
func TestBashIsNotALoginShell(t *testing.T) {
	home := t.TempDir()
	profile := "export WRIGHT_LOGIN_SHELL=sourced\n"
	for _, name := range []string{".bash_profile", ".profile", ".bash_login"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(profile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t, func(d *tools.Deps) {
		// os/exec keeps the last duplicate, so this replaces the fixture's HOME.
		d.SandboxSpec.Env = append(d.SandboxSpec.Env, "HOME="+home)
	})
	got, err := f.text(tools.NameBash, `{"command":"echo login=[$WRIGHT_LOGIN_SHELL]"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "login=[]") {
		t.Errorf("the shell sourced a profile:\n%s", got)
	}
}

// hasEnv reports whether a spec's environment carries name=value.
func hasEnv(spec sandbox.Spec, name string) (string, bool) {
	for _, e := range spec.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// TestGitHubEnvReachesOnlyNetworkedCalls is the control this whole feature
// rests on. A command with no network cannot use a credential, so it is not
// given one — which is what keeps the token out of the environment of an
// auto-allowed read, the kind of call that runs with no prompt at all and
// where a prompt-injected `base64 <<<"$GH_TOKEN"` would defeat the redactor.
func TestGitHubEnvReachesOnlyNetworkedCalls(t *testing.T) {
	const token = "gho_0123456789abcdef0123456789abcdef0123"
	const helper = `!'/usr/bin/gh' auth git-credential`
	ghEnv := map[string]string{"GH_TOKEN": token, "GIT_CONFIG_VALUE_3": helper}

	tests := []struct {
		name    string
		enabled bool
		// how this call comes to have (or not have) the network
		baseNetwork  bool // --allow-network / sandbox.allowNetwork
		grantNetwork bool // the engine's per-call grant
		argNetwork   bool // the model's own network argument
		want         bool
	}{
		{name: "off, no network", want: false},
		{name: "off, networked by grant", grantNetwork: true, want: false},
		{name: "on, no network", enabled: true, want: false},
		{name: "on, networked by grant", enabled: true, grantNetwork: true, want: true},
		{name: "on, networked by the session", enabled: true, baseNetwork: true, want: true},
		{name: "on, networked by the argument", enabled: true, argNetwork: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingBackend{}
			f := newFixture(t, func(d *tools.Deps) {
				d.Sandbox = rec
				d.SandboxSpec.Network = tt.baseNetwork
				if tt.enabled {
					d.GitHub = tools.NewGitHubAuth()
					d.GitHub.Enable(ghEnv, "gh auth token")
				}
			})
			ctx := context.Background()
			if tt.grantNetwork {
				ctx = sandbox.WithGrant(ctx, sandbox.Grant{Network: true})
			}
			args := `{"command":"gh pr list"}`
			if tt.argNetwork {
				args = `{"command":"gh pr list","network":true}`
			}
			if _, err := f.call(ctx, tools.NameBash, args); err != nil {
				t.Fatal(err)
			}
			spec := rec.last(t)
			got, ok := hasEnv(spec, "GH_TOKEN")
			if ok != tt.want {
				t.Fatalf("GH_TOKEN present = %v, want %v (network=%v)", ok, tt.want, spec.Network)
			}
			if !tt.want {
				return
			}
			if got != token {
				t.Errorf("GH_TOKEN = %q, want the resolved token", got)
			}
			// The token is useless to `git push` without the helper that
			// reads it; they travel together or not at all.
			if v, ok := hasEnv(spec, "GIT_CONFIG_VALUE_3"); !ok || v != helper {
				t.Errorf("credential helper = %q (present=%v), want %q", v, ok, helper)
			}
		})
	}
}

// The base spec is shared by every bash call *and* by the MCP stdio servers,
// so appending to its environment in place would hand the next command — and
// every MCP server — the last one's credential.
func TestGitHubEnvDoesNotLeakIntoTheBaseSpec(t *testing.T) {
	rec := &recordingBackend{}
	var base []string
	f := newFixture(t, func(d *tools.Deps) {
		d.Sandbox = rec
		d.GitHub = tools.NewGitHubAuth()
		d.GitHub.Enable(map[string]string{
			"GH_TOKEN": "gho_secret_value_here_0123456789",
			// The real shape: the base environment already carries this key
			// blanked, so the contribution *replaces* an entry rather than
			// only appending. That is what makes the aliasing observable —
			// a replacement shifts the shared backing array.
			"GIT_CONFIG_VALUE_3": `!'/usr/bin/gh' auth git-credential`,
		}, "GH_TOKEN")
		d.SandboxSpec.Env = append(slices.Clone(d.SandboxSpec.Env), "GIT_CONFIG_VALUE_3=", "PATH=/usr/bin")
		base = slices.Clone(d.SandboxSpec.Env)
	})
	ctx := sandbox.WithGrant(context.Background(), sandbox.Grant{Network: true})
	if _, err := f.call(ctx, tools.NameBash, `{"command":"gh pr list"}`); err != nil {
		t.Fatal(err)
	}
	if _, ok := hasEnv(rec.last(t), "GH_TOKEN"); !ok {
		t.Fatal("the networked call did not get the token")
	}
	if !slices.Equal(f.deps.SandboxSpec.Env, base) {
		t.Errorf("the base spec's env was mutated:\n got %v\nwant %v", f.deps.SandboxSpec.Env, base)
	}
	// The next call has no network, so no token — proving the first call did
	// not leave it behind.
	if _, err := f.call(context.Background(), tools.NameBash, `{"command":"echo plain"}`); err != nil {
		t.Fatal(err)
	}
	if v, ok := hasEnv(rec.last(t), "GH_TOKEN"); ok {
		t.Errorf("a later call inherited the credential: %q", v)
	}
}

// /github off must take effect on the next call.
func TestGitHubAuthCanBeTurnedOffMidSession(t *testing.T) {
	rec := &recordingBackend{}
	gh := tools.NewGitHubAuth()
	f := newFixture(t, func(d *tools.Deps) { d.Sandbox = rec; d.GitHub = gh })
	ctx := sandbox.WithGrant(context.Background(), sandbox.Grant{Network: true})

	gh.Enable(map[string]string{"GH_TOKEN": "gho_0123456789abcdef0123456789abcdef"}, "gh auth token")
	if !gh.On() || gh.Source() != "gh auth token" {
		t.Fatalf("On=%v Source=%q", gh.On(), gh.Source())
	}
	if _, err := f.call(ctx, tools.NameBash, `{"command":"gh pr list"}`); err != nil {
		t.Fatal(err)
	}
	if _, ok := hasEnv(rec.last(t), "GH_TOKEN"); !ok {
		t.Fatal("enabled but the call has no token")
	}

	gh.Disable()
	if gh.On() {
		t.Error("On() after Disable")
	}
	if _, err := f.call(ctx, tools.NameBash, `{"command":"gh pr list"}`); err != nil {
		t.Fatal(err)
	}
	if v, ok := hasEnv(rec.last(t), "GH_TOKEN"); ok {
		t.Errorf("still carrying a credential after Disable: %q", v)
	}
}
