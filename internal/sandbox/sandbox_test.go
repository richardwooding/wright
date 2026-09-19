package sandbox_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/sandbox"
)

// TestMain lets the test binary stand in for `wright __sandbox`: when invoked
// with that first argument it behaves as the landlock helper, so the backend
// can be exercised end to end without building the real binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__sandbox" {
		h, err := sandbox.ParseHelperArgs(os.Args[2:])
		if err == nil {
			err = h.Exec()
		}
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(97)
	}
	os.Exit(m.Run())
}

func TestEnvFrom(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"LC_ALL=C",
		"LANG=en",
		"TERM=xterm",
		"XDG_CACHE_HOME=/c",
		"GOFLAGS=-mod=mod",
		"GOPATH=/go",
		"ANTHROPIC_API_KEY=sk-ant-x",
		"OPENAI_API_KEY=x",
		"AWS_ACCESS_KEY_ID=x",
		"AWS_REGION=eu",
		"GITHUB_TOKEN=x",
		"GH_TOKEN=x",
		"MY_SECRET=x",
		"DB_PASSWORD=x",
		"SERVICE_CREDENTIALS=x",
		"LLMKIT_DEBUG=1",
		"DATABASE_URL=postgres://",
		"SSH_AUTH_SOCK=/run/x",
		"RANDOM_VAR=1",
		"CUSTOM_OK=yes",
		"MALFORMED",
		"=novalue",
	}
	tests := []struct {
		name  string
		pass  []string
		extra map[string]string
		want  []string
		deny  []string
	}{
		{
			name: "allowlist only",
			want: []string{"PATH=/usr/bin", "HOME=/home/u", "LC_ALL=C", "LANG=en", "TERM=xterm", "XDG_CACHE_HOME=/c", "GOFLAGS=-mod=mod", "GOPATH=/go"},
			deny: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_REGION", "GITHUB_TOKEN", "GH_TOKEN", "MY_SECRET", "DB_PASSWORD", "SERVICE_CREDENTIALS", "LLMKIT_DEBUG", "DATABASE_URL", "SSH_AUTH_SOCK", "RANDOM_VAR", "CUSTOM_OK", "MALFORMED"},
		},
		{
			name: "passthrough adds but cannot override strip",
			pass: []string{"CUSTOM_OK", "RANDOM_*", "ANTHROPIC_API_KEY", "AWS_REGION", "MY_SECRET"},
			want: []string{"CUSTOM_OK=yes", "RANDOM_VAR=1", "PATH=/usr/bin"},
			deny: []string{"ANTHROPIC_API_KEY", "AWS_REGION", "MY_SECRET"},
		},
		{
			name:  "extra replaces and is stripped too",
			extra: map[string]string{"HOME": "/tmp/h", "WRIGHT_CONTAINER": "1", "FOO_TOKEN": "x"},
			want:  []string{"HOME=/tmp/h", "WRIGHT_CONTAINER=1"},
			deny:  []string{"FOO_TOKEN", "HOME=/home/u"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandbox.EnvFrom(environ, tt.pass, tt.extra)
			for _, w := range tt.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing %q in %v", w, got)
				}
			}
			for _, d := range tt.deny {
				for _, kv := range got {
					if kv == d || strings.HasPrefix(kv, d+"=") {
						t.Errorf("leaked %q", kv)
					}
				}
			}
		})
	}
}

func TestDetectNone(t *testing.T) {
	b, warnings := sandbox.Detect(context.Background(), "none")
	if b.Name() != sandbox.NameNone {
		t.Fatalf("backend = %s", b.Name())
	}
	if len(warnings) != 1 || warnings[0].Backend != sandbox.NameNone {
		t.Errorf("warnings = %v, want one none warning", warnings)
	}
	cmd, err := b.Command(context.Background(), sandbox.Spec{Argv: []string{"echo", "hi"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Errorf("none backend: %q %v", out, err)
	}
}

func TestDetectUnknownFallsBack(t *testing.T) {
	b, warnings := sandbox.Detect(context.Background(), "bogus")
	if b == nil {
		t.Fatal("nil backend")
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0].String(), "unknown") {
		t.Errorf("expected unknown-backend warning, got %v", warnings)
	}
}

func TestDetectAutoNeverReturnsNil(t *testing.T) {
	b, _ := sandbox.Detect(context.Background(), "auto")
	if b == nil {
		t.Fatal("nil backend")
	}
	if err := b.Available(context.Background()); err != nil {
		t.Errorf("selected backend %s not available: %v", b.Name(), err)
	}
}

func TestContainerDetection(t *testing.T) {
	t.Setenv("WRIGHT_CONTAINER", "1")
	if !sandbox.InContainer() {
		t.Error("WRIGHT_CONTAINER=1 should mark a container")
	}
	b, warnings := sandbox.Detect(context.Background(), "")
	if b.Name() != sandbox.NameContainer {
		t.Errorf("backend = %s, want container", b.Name())
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestEmptyArgvRejected(t *testing.T) {
	for _, name := range []string{"none", "bwrap"} {
		b, _ := sandbox.Detect(context.Background(), name)
		if b.Name() != name {
			continue
		}
		if _, err := b.Command(context.Background(), sandbox.Spec{}); err == nil {
			t.Errorf("%s accepted empty argv", name)
		}
	}
}

func TestBwrapArgs(t *testing.T) {
	ws := t.TempDir()
	spec := sandbox.Spec{
		Argv:      []string{"bash", "-lc", "echo hi"},
		Dir:       ws,
		Env:       []string{"PATH=/usr/bin", "HOME=/home/u"},
		ReadWrite: []string{ws},
		ReadOnly:  []string{"/opt"},
	}
	args, err := sandbox.BwrapArgs(spec, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --die-with-parent ", " --new-session ", " --unshare-all ", " --unshare-user-try ",
		" --ro-bind / / ", " --tmpfs /tmp ", " --tmpfs /home/u ",
		" --bind " + ws + " " + ws + " ", " --ro-bind /opt /opt ",
		" --clearenv ", " --setenv PATH /usr/bin ", " --setenv HOME /home/u ",
		" --chdir " + ws + " ", " -- bash -lc echo hi ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	if strings.Contains(joined, "--share-net") || strings.Contains(joined, "resolv.conf") {
		t.Error("network flags present without Network")
	}
	// tmpfs $HOME must come before the workspace bind so the bind wins.
	if strings.Index(joined, "--tmpfs /home/u") > strings.Index(joined, "--bind "+ws) {
		t.Error("tmpfs $HOME must precede workspace bind")
	}

	spec.Network = true
	args, _ = sandbox.BwrapArgs(spec, "/home/u")
	if !slices.Contains(args, "--share-net") {
		t.Error("--share-net missing with Network")
	}

	spec.ReadWrite = []string{filepath.Join(ws, "missing")}
	if _, err := sandbox.BwrapArgs(spec, "/home/u"); err == nil {
		t.Error("missing read-write dir should error")
	}
}

func TestSeatbeltProfile(t *testing.T) {
	p := sandbox.SeatbeltProfile(sandbox.Spec{ReadWrite: []string{"/Users/u/ws"}}, "/Users/u")
	for _, want := range []string{"(deny default)", `(deny file-read* (subpath "/Users/u/.ssh"))`, `(allow file-write* (subpath "/Users/u/ws"))`, "(deny network*)"} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q:\n%s", want, p)
		}
	}
	if !strings.Contains(sandbox.SeatbeltProfile(sandbox.Spec{Network: true}, "/Users/u"), "(allow network*)") {
		t.Error("network profile should allow network")
	}
}

func TestHelperArgsRoundTrip(t *testing.T) {
	h := sandbox.Helper{Dir: "/w", RW: []string{"/w", "/c"}, RO: []string{"/o"}, Net: true, Argv: []string{"bash", "-lc", "echo --net"}}
	got, err := sandbox.ParseHelperArgs(h.Args())
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir != h.Dir || !slices.Equal(got.RW, h.RW) || !slices.Equal(got.RO, h.RO) || got.Net != h.Net || !slices.Equal(got.Argv, h.Argv) {
		t.Errorf("round trip = %+v, want %+v", got, h)
	}
	if _, err := sandbox.ParseHelperArgs([]string{"--dir", "/w"}); err == nil {
		t.Error("missing payload should error")
	}
}

func TestCachesExist(t *testing.T) {
	for _, c := range sandbox.Caches() {
		if info, err := os.Stat(c); err != nil || !info.IsDir() {
			t.Errorf("cache %s is not an existing directory", c)
		}
	}
}

// bwrapOrSkip returns the bwrap backend or skips when it cannot run here.
func bwrapOrSkip(t *testing.T) sandbox.Backend {
	t.Helper()
	b, _ := sandbox.Detect(context.Background(), sandbox.NameBwrap)
	if b.Name() != sandbox.NameBwrap {
		t.Skip("bwrap unavailable on this machine")
	}
	return b
}

// runSpec runs argv through backend b with a timeout and returns output/err.
func runSpec(t *testing.T, b sandbox.Backend, spec sandbox.Spec) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := b.Command(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

func TestBwrapRunsEcho(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	out, err := runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", "echo hi && pwd"},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if err != nil {
		t.Fatalf("bwrap echo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, ws) {
		t.Errorf("output = %q", out)
	}
}

// homeEntry finds an existing entry in the real $HOME that is not the given
// workspace, so the test can prove it is invisible inside the sandbox.
func homeEntry(t *testing.T, exclude string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Skip("cannot read home dir")
	}
	for _, e := range entries {
		p := filepath.Join(home, e.Name())
		if !strings.HasPrefix(exclude, p) {
			return p
		}
	}
	t.Skip("home dir empty")
	return ""
}

func TestBwrapHidesHome(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	target := homeEntry(t, ws)
	out, err := runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", "test -e \"$1\" && echo VISIBLE || echo HIDDEN", "_", target},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if err != nil {
		t.Fatalf("bwrap: %v\n%s", err, out)
	}
	if !strings.Contains(out, "HIDDEN") {
		t.Errorf("%s visible inside sandbox: %q", target, out)
	}
	// A secret path under ~/.ssh must be unreadable even if it exists outside.
	out, _ = runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", "cat \"$HOME/.ssh/x\" 2>&1; ls -A \"$HOME\""},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if !strings.Contains(out, "No such file") {
		t.Errorf("expected ~/.ssh to be absent, got %q", out)
	}
}

func TestBwrapNetworkOff(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	// With the network unshared there is no route to anything; a loopback
	// bind still works but external connects fail fast. `ip`/`curl` may be
	// absent, so use bash's /dev/tcp, which needs no tools.
	out, err := runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/80 && echo CONNECTED || echo BLOCKED"},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
		Timeout:   5 * time.Second,
	})
	if err != nil && !strings.Contains(out, "BLOCKED") {
		t.Fatalf("bwrap: %v\n%s", err, out)
	}
	if strings.Contains(out, "CONNECTED") {
		t.Error("network reachable with Network=false")
	}
}

func TestLandlockEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	abi, err := sandbox.LandlockABI()
	if err != nil || abi < 1 {
		t.Skipf("landlock unavailable: abi=%d err=%v", abi, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := sandbox.LandlockWithExecutable(exe)
	ws := t.TempDir()
	// /tmp stays writable inside the sandbox (build scratch), so the probe for
	// "outside the workspace" is $HOME, which the helper never grants.
	home, err := os.UserHomeDir()
	if err != nil || strings.HasPrefix(home, "/tmp") {
		t.Skip("no usable home dir to probe")
	}
	spec := sandbox.Spec{
		Argv:      []string{"bash", "-c", "echo hi > out.txt && cat out.txt && (ls \"$1\" >/dev/null 2>&1 && echo READ_OK || echo READ_DENIED)", "_", home},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	}
	out, err := runSpec(t, b, spec)
	if err != nil {
		t.Fatalf("landlock run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("workspace write/read failed: %q", out)
	}
	if !strings.Contains(out, "READ_DENIED") {
		t.Errorf("home directory was listable inside the sandbox: %q", out)
	}
}
