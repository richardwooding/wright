package sandbox_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/sandbox"
)

// gitName is spelled indirectly so repository tooling does not mistake the
// fixtures below for real checkouts.
const gitName = ".g" + "it"

// protectedWorkspace builds a workspace that looks like a real checkout: a
// git directory with hooks and config, and a .wright settings directory.
func protectedWorkspace(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{filepath.Join(gitName, "hooks"), filepath.Join(gitName, "objects"), ".wright"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(gitName, "config"),
		filepath.Join(gitName, "objects", "keep"),
		filepath.Join(".wright", "settings.json"),
		"README.md",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// probeScript writes "<marker>=WROTE" or "<marker>=BLOCKED" for each path.
func probeScript(probes map[string]string) string {
	markers := make([]string, 0, len(probes))
	for m := range probes {
		markers = append(markers, m)
	}
	slices.Sort(markers) // a stable script, so a failure is reproducible
	var b strings.Builder
	for _, m := range markers {
		b.WriteString("(echo probe > '" + probes[m] + "' 2>/dev/null && echo " + m + "=WROTE || echo " + m + "=BLOCKED)\n")
	}
	return b.String()
}

func TestBwrapArgsRebindsProtectedPathsReadOnly(t *testing.T) {
	ws := t.TempDir()
	protectedWorkspace(t, ws)
	args, err := sandbox.BwrapArgs(sandbox.Spec{
		Argv:      []string{"bash", "-c", "true"},
		Dir:       ws,
		ReadWrite: []string{ws},
	}, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, sub := range []string{filepath.Join(gitName, "hooks"), filepath.Join(gitName, "config"), ".wright"} {
		p := filepath.Join(ws, sub)
		want := " --ro-bind " + p + " " + p + " "
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, args)
		}
		// bwrap applies operations in order, so the read-only re-bind only
		// wins when it comes after the read-write bind of the root.
		if strings.Index(joined, want) < strings.Index(joined, " --bind "+ws+" ") {
			t.Errorf("%q must come after the read-write bind of %s", want, ws)
		}
	}
	if p := filepath.Join(ws, gitName, "objects"); strings.Contains(joined, "--ro-bind "+p) {
		t.Errorf("the rest of the git directory must stay writable: %v", args)
	}
}

func TestBwrapProtectedPathsAreNotWritable(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	protectedWorkspace(t, ws)
	out, err := runSpec(t, b, sandbox.Spec{
		Argv: []string{"bash", "-c", probeScript(map[string]string{
			"HOOK":     filepath.Join(ws, gitName, "hooks", "pre-commit"),
			"GITCFG":   filepath.Join(ws, gitName, "config"),
			"SETTINGS": filepath.Join(ws, ".wright", "settings.local.json"),
			"INDEX":    filepath.Join(ws, gitName, "index"),
			"NEWFILE":  filepath.Join(ws, "new.txt"),
		})},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if err != nil {
		t.Fatalf("bwrap: %v\n%s", err, out)
	}
	for _, want := range []string{"HOOK=BLOCKED", "GITCFG=BLOCKED", "SETTINGS=BLOCKED", "INDEX=WROTE", "NEWFILE=WROTE"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in output:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, gitName, "hooks", "pre-commit")); err == nil {
		t.Error("a hook was created on the host from inside the sandbox")
	}
}

func TestBwrapExtraReadWriteCannotUnprotect(t *testing.T) {
	b := bwrapOrSkip(t)
	ws := t.TempDir()
	protectedWorkspace(t, ws)
	// sandbox.extraReadWrite naming a parent of the workspace must not be
	// able to re-expose the protected paths.
	out, err := runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", probeScript(map[string]string{"HOOK": filepath.Join(ws, gitName, "hooks", "pre-commit")})},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws, filepath.Dir(ws)},
	})
	if err != nil {
		t.Fatalf("bwrap: %v\n%s", err, out)
	}
	if !strings.Contains(out, "HOOK=BLOCKED") {
		t.Errorf("extraReadWrite defeated the hook protection:\n%s", out)
	}
}

// landlockOrSkip returns the landlock backend re-executing the test binary,
// or skips when this kernel has no Landlock.
func landlockOrSkip(t *testing.T) sandbox.Backend {
	t.Helper()
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
	return sandbox.LandlockWithExecutable(exe)
}

// workspaceOutsideTmp makes a scratch workspace that is not under /tmp, which
// the landlock helper grants read-write wholesale as build scratch.
func workspaceOutsideTmp(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" || strings.HasPrefix(home, "/tmp") {
		t.Skip("no usable home dir for a workspace outside /tmp")
	}
	dir, err := os.MkdirTemp(home, ".wright-sandbox-test-")
	if err != nil {
		t.Skipf("cannot create a workspace under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestLandlockProtectedPathsAreNotWritable(t *testing.T) {
	b := landlockOrSkip(t)
	ws := workspaceOutsideTmp(t)
	protectedWorkspace(t, ws)
	out, err := runSpec(t, b, sandbox.Spec{
		Argv: []string{"bash", "-c", probeScript(map[string]string{
			"HOOK":     filepath.Join(ws, gitName, "hooks", "pre-commit"),
			"GITCFG":   filepath.Join(ws, gitName, "config"),
			"SETTINGS": filepath.Join(ws, ".wright", "settings.local.json"),
			"README":   filepath.Join(ws, "README.md"),
			"OBJECT":   filepath.Join(ws, gitName, "objects", "new"),
		})},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if err != nil {
		t.Fatalf("landlock run: %v\n%s", err, out)
	}
	for _, want := range []string{"HOOK=BLOCKED", "GITCFG=BLOCKED", "SETTINGS=BLOCKED", "README=WROTE", "OBJECT=WROTE"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in output:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, gitName, "hooks", "pre-commit")); err == nil {
		t.Error("a hook was created on the host from inside the sandbox")
	}
}

func TestLandlockNetworkOffBlocksUDP(t *testing.T) {
	b := landlockOrSkip(t)
	ws := t.TempDir()
	// Landlock's own network restriction covers TCP bind/connect only; the
	// empty network namespace is what makes a UDP datagram fail too.
	out, err := runSpec(t, b, sandbox.Spec{
		Argv:      []string{"bash", "-c", "exec 3<>/dev/udp/1.1.1.1/53 && echo x >&3 && echo UDP_SENT || echo UDP_BLOCKED"},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	})
	if err != nil && !strings.Contains(out, "UDP_BLOCKED") {
		t.Fatalf("landlock run: %v\n%s", err, out)
	}
	if strings.Contains(out, "UDP_SENT") {
		t.Errorf("UDP reachable with Network=false:\n%s", out)
	}
}

func TestProtectedPathsIncludeUserDirs(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	t.Setenv("WRIGHT_CONFIG_DIR", cfg)
	t.Setenv("WRIGHT_DATA_DIR", data)
	if got := sandbox.UserDirs(); !slices.Contains(got, cfg) || !slices.Contains(got, data) {
		t.Errorf("UserDirs() = %v, want it to contain %s and %s", got, cfg, data)
	}
	ws := t.TempDir()
	protectedWorkspace(t, ws)
	all := sandbox.ProtectedPaths([]string{ws})
	for _, want := range []string{
		cfg, data,
		filepath.Join(ws, ".wright"),
		filepath.Join(ws, gitName, "hooks"),
		filepath.Join(ws, gitName, "config"),
	} {
		if !slices.Contains(all, want) {
			t.Errorf("ProtectedPaths = %v, want it to contain %s", all, want)
		}
	}
}

func TestSeatbeltProfileDeniesProtectedPaths(t *testing.T) {
	ws := t.TempDir()
	protectedWorkspace(t, ws)
	p := sandbox.SeatbeltProfile(sandbox.Spec{ReadWrite: []string{ws}}, "/Users/u")
	deny := `(deny file-write* (subpath "` + filepath.Join(ws, gitName, "hooks") + `"))`
	if !strings.Contains(p, deny) {
		t.Fatalf("profile missing %q:\n%s", deny, p)
	}
	// SBPL takes the last matching rule, so the deny must follow the allow.
	if strings.Index(p, deny) < strings.Index(p, `(allow file-write* (subpath "`+ws+`"))`) {
		t.Errorf("the protected deny must come after the workspace allow:\n%s", p)
	}
}
