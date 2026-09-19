package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
		filepath.Join(gitName, "hooks", "keep"),
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
// or skips when this machine cannot give it what its confinement rests on:
// a kernel with Landlock, and the user, network and mount namespaces the
// backend creates around the helper. Where the namespaces are refused —
// unprivileged user namespaces disabled, a seccomp filter, a container
// without the capability, all of which describe some CI runners — the
// backend degrades to the documented fallback and the assertions below would
// be measuring nothing. The skip is decided by the backend's own capability
// probe, never by a CI environment variable, so a machine that *can* enforce
// and does not still fails.
func landlockOrSkip(t *testing.T) sandbox.Backend {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	abi, err := sandbox.LandlockABI()
	if err != nil || abi < 1 {
		t.Skipf("landlock unavailable: abi=%d err=%v", abi, err)
	}
	if err := sandbox.LandlockNamespaces(); err != nil {
		t.Skipf("landlock cannot confine here: no namespaces for the helper (%v)", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.LandlockWithExecutable(exe)
}

// enforcesOrSkip skips when the backend cannot enforce this spec's
// protection on this machine. The namespaces can exist and the read-only
// bind mounts still be refused — a volume a container runtime mounted in is
// locked by the user namespace that owns it — and the backend then says so
// through SpecWarnings and runs the payload anyway. That is the documented
// fallback, not a regression, so the test skips with the backend's own
// reason instead of failing. Backends that report nothing (bwrap) pass
// straight through.
func enforcesOrSkip(t *testing.T, b sandbox.Backend, spec sandbox.Spec) {
	t.Helper()
	if w := sandbox.SpecWarnings(b, spec); len(w) > 0 {
		t.Skipf("%s cannot enforce this workspace's protection: %v", b.Name(), w)
	}
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

// TestLandlockProtectsWithoutBreakingTheWorkspace pins both halves at once.
// Landlock rules are additive, so keeping `.git/hooks` read-only by
// *withholding* write on the workspace root would also stop the agent
// creating a file or a directory there — which is not a usable coding agent,
// and is exactly what the shipped container image (WRIGHT_SANDBOX=landlock,
// no bwrap) would get. The read-only bind mounts in the helper's mount
// namespace carry the protection instead, so the root stays writable.
func TestLandlockProtectsWithoutBreakingTheWorkspace(t *testing.T) {
	b := landlockOrSkip(t)
	ws := workspaceOutsideTmp(t)
	protectedWorkspace(t, ws)
	script := probeScript(map[string]string{
		"HOOK":     filepath.Join(ws, gitName, "hooks", "pre-commit"),
		"GITCFG":   filepath.Join(ws, gitName, "config"),
		"SETTINGS": filepath.Join(ws, ".wright", "settings.local.json"),
		"EXISTING": filepath.Join(ws, "README.md"),
		"GITINDEX": filepath.Join(ws, gitName, "index"),
		"NEWFILE":  filepath.Join(ws, "NEWFILE"),
	})
	script += "(mkdir -p '" + filepath.Join(ws, "newdir") + "' 2>/dev/null && echo SUBDIR=WROTE || echo SUBDIR=BLOCKED)\n"
	script += "(cat '" + filepath.Join(ws, gitName, "hooks", "keep") + "' >/dev/null 2>&1 && echo HOOKREAD=OK || echo HOOKREAD=DENIED)\n"
	spec := sandbox.Spec{
		Argv:      []string{"bash", "-c", script},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
	}
	enforcesOrSkip(t, b, spec)
	out, err := runSpec(t, b, spec)
	if err != nil {
		t.Fatalf("landlock run: %v\n%s", err, out)
	}
	want := []string{
		// The workspace is a workspace: creation and edits work, and so do
		// git's own writes inside .git.
		"NEWFILE=WROTE", "SUBDIR=WROTE", "EXISTING=WROTE", "GITINDEX=WROTE",
		// The paths that decide what happens outside the sandbox do not.
		"HOOK=BLOCKED", "GITCFG=BLOCKED", "SETTINGS=BLOCKED",
		// git still has to be able to *read* its hooks and config.
		"HOOKREAD=OK",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("want %s in output:\n%s", w, out)
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

// TestProtectedPathsThatDoNotExistYet covers the hole a "bind only what
// exists" rule leaves: a payload creates the protected directory itself and
// writes into the one it just made. The spec's roots get their protected
// paths created before the sandbox is built, so there is always something to
// bind read-only.
func TestProtectedPathsThatDoNotExistYet(t *testing.T) {
	backends := map[string]func(*testing.T) sandbox.Backend{
		"bwrap":    bwrapOrSkip,
		"landlock": landlockOrSkip,
	}
	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			ws := workspaceOutsideTmp(t)
			// A repository with no .wright at all, and a .git without hooks.
			if err := os.MkdirAll(filepath.Join(ws, gitName), 0o755); err != nil {
				t.Fatal(err)
			}
			spec := sandbox.Spec{
				Argv: []string{"bash", "-c", "" +
					"(mkdir -p '" + filepath.Join(ws, ".wright") + "' 2>/dev/null && echo MKDIR=OK || echo MKDIR=BLOCKED)\n" +
					probeScript(map[string]string{
						"SETTINGS": filepath.Join(ws, ".wright", "settings.local.json"),
						"HOOK":     filepath.Join(ws, gitName, "hooks", "pre-commit"),
						"GITCFG":   filepath.Join(ws, gitName, "config"),
					})},
				Dir:       ws,
				Env:       sandbox.Env(nil, nil),
				ReadWrite: []string{ws},
				Roots:     []string{ws},
			}
			enforcesOrSkip(t, b, spec)
			out, err := runSpec(t, b, spec)
			if err != nil {
				t.Fatalf("%s run: %v\n%s", name, err, out)
			}
			for _, want := range []string{"SETTINGS=BLOCKED", "HOOK=BLOCKED", "GITCFG=BLOCKED"} {
				if !strings.Contains(out, want) {
					t.Errorf("want %s in output:\n%s", want, out)
				}
			}
			if _, err := os.Stat(filepath.Join(ws, ".wright", "settings.local.json")); err == nil {
				t.Error("settings.local.json was created on the host from inside the sandbox")
			}
		})
	}
}

func TestEnsureProtectedOnlyBuildsInsideExistingStructures(t *testing.T) {
	ws := t.TempDir()
	sandbox.EnsureProtected([]string{ws})
	if info, err := os.Stat(filepath.Join(ws, ".wright")); err != nil || !info.IsDir() {
		t.Errorf(".wright not created: %v", err)
	}
	// A plain directory must not be turned into something git reads as a
	// repository, so .git is never invented.
	if _, err := os.Stat(filepath.Join(ws, gitName)); err == nil {
		t.Error("EnsureProtected created a git directory in a plain directory")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, gitName), 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox.EnsureProtected([]string{repo})
	for _, want := range []string{filepath.Join(gitName, "hooks"), filepath.Join(gitName, "config"), ".wright"} {
		if _, err := os.Stat(filepath.Join(repo, want)); err != nil {
			t.Errorf("%s not created: %v", want, err)
		}
	}
}

// TestHelperRefusesWhenTheNamespacesWereRemoved simulates a caller that
// replaces the command's SysProcAttr after the backend set it — which the
// bash tool did, to set a process group, and which silently cost the
// landlock backend both its network namespace and its read-only bind
// mounts while the status bar still said "landlock". The helper now
// notices it is in the parent's namespaces and refuses to run the payload.
func TestHelperRefusesWhenTheNamespacesWereRemoved(t *testing.T) {
	b := landlockOrSkip(t)
	ws := workspaceOutsideTmp(t)
	protectedWorkspace(t, ws)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd, err := b.Command(ctx, sandbox.Spec{
		Argv:      []string{"bash", "-c", "echo PAYLOAD_RAN"},
		Dir:       ws,
		Env:       sandbox.Env(nil, nil),
		ReadWrite: []string{ws},
		Roots:     []string{ws},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil {
		t.Skip("no namespaces on this machine, so there is nothing to lose")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // the old bash tool
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper ran the payload unconfined: %s", out)
	}
	if strings.Contains(string(out), "PAYLOAD_RAN") {
		t.Errorf("the payload ran anyway: %s", out)
	}
	if !strings.Contains(string(out), "parent network namespace") {
		t.Errorf("the refusal should name what is missing: %s", out)
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
