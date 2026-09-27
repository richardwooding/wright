package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/workspace"
)

// Open locates the repository with `git rev-parse --show-toplevel`, the
// earliest git wright runs — before the trust prompt, because the workspace
// has to be found before its settings can be read.
//
// Measured against git 2.55 that subcommand fires none of the keys a
// repository can name, because it reads no index. So this test passes today
// either way, and that is the point of keeping it: which key fires on which
// subcommand is a property of the git binary, not a promise it makes. If a
// future git refreshes the index here, this says so while the call is still
// routed through internal/git's hardening.
func TestOpenDoesNotRunWhatTheRepositoryNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the payload is a shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "repo")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(base, "sentinel")
	payload := filepath.Join(base, "payload.sh")
	script := "#!/bin/sh\necho ran >> " + strconv.Quote(sentinel) + "\nexit 0\n"
	if err := os.WriteFile(payload, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitc("init", "-q", "-b", "main")
	gitc("config", "core.fsmonitor", payload)

	ws, err := workspace.Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ws.Root() != dir {
		t.Errorf("Root() = %q, want %q", ws.Root(), dir)
	}
	if b, err := os.ReadFile(sentinel); err == nil {
		t.Errorf("the repository's own config executed while the workspace was being located, before any trust prompt: %q",
			strings.TrimSpace(string(b)))
	}
}
