package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/git"
)

// hostileRepo builds a repository whose own config names a program for git to
// run, under every key that fires on a command this package issues. The
// program appends to a sentinel file, so "did anything run?" is a file
// existence check rather than a guess about which key git honours when.
//
// It returns the repository directory and the sentinel path.
func hostileRepo(t *testing.T) (dir, sentinel string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the payload is a shell script")
	}
	dir = repo(t)
	base := filepath.Dir(dir)
	sentinel = filepath.Join(base, "sentinel")
	payload := filepath.Join(base, "payload.sh")
	script := "#!/bin/sh\necho \"ran:$1\" >> " + strconv.Quote(sentinel) + "\nexit 0\n"
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
	// The keys git names itself.
	gitc("config", "core.fsmonitor", payload+" fsmonitor")
	gitc("config", "diff.external", payload+" diff-external")
	// The keys whose middle segment the *repository* chooses, reached from
	// its own .gitattributes. No fixed list can name these.
	gitc("config", "filter.hostile.clean", payload+" filter-clean")
	gitc("config", "filter.hostile.smudge", payload+" filter-smudge")
	gitc("config", "diff.hostile.textconv", payload+" textconv")
	attrs := filepath.Join(dir, ".gitattributes")
	if err := os.WriteFile(attrs, []byte("*.txt diff=hostile filter=hostile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, sentinel
}

func ran(t *testing.T, sentinel string) string {
	t.Helper()
	b, err := os.ReadFile(sentinel)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// A repository carries its own .git/config, and several of its keys name a
// program git will run during an ordinary read. wright reads repositories on
// the host — the status bar polls every five seconds, and the workspace is
// located before the user has been asked to trust anything — so every
// host-side call has to disarm them. This test runs git rather than asserting
// about it, because which key fires on which subcommand is a property of the
// git binary, not of this package.
func TestHostGitDoesNotRunWhatTheRepositoryNames(t *testing.T) {
	dir, sentinel := hostileRepo(t)
	ctx := context.Background()

	if s := git.Status(ctx, dir); !s.Repo {
		t.Fatal("Status did not recognise the repository")
	}
	if _, err := git.Diff(ctx, dir, false); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	git.IsTracked(ctx, dir, "tracked.txt")
	git.WhoAmI(ctx, dir)
	// The one that runs before the trust prompt: workspace.Open locates the
	// repository through this before any settings have been read.
	if top := git.Toplevel(ctx, dir); top == "" {
		t.Fatal("Toplevel found no repository")
	}

	if got := ran(t, sentinel); got != "" {
		t.Errorf("the repository's own config executed during a read:\n%s", got)
	}
}

// Closing the hole by breaking the command is not closing it. diff.external
// cannot be blanked in the environment — git tries to run the empty string
// and dies — so Diff refuses it on the command line instead, and this pins
// that the diff still arrives.
func TestDiffStillWorksAgainstAHostileConfig(t *testing.T) {
	dir, _ := hostileRepo(t)
	out, err := git.Diff(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(out, "+two") || !strings.Contains(out, "-one") {
		t.Errorf("diff did not contain the change: %q", out)
	}
}

// The sandbox environment has to carry the same protection, including the
// keys named after drivers this repository invented, which no written-down
// list could contain.
func TestSandboxEnvBlanksTheRepositoryOwnDriverKeys(t *testing.T) {
	dir, _ := hostileRepo(t)
	env := git.SandboxEnv(context.Background(), dir)
	n, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil {
		t.Fatalf("GIT_CONFIG_COUNT = %q", env["GIT_CONFIG_COUNT"])
	}
	keys := map[string]bool{}
	for i := range n {
		keys[env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]] = true
		if v := env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]; v != "" {
			t.Errorf("key %d is neutralised to %q, want empty", i, v)
		}
	}
	for _, want := range []string{
		"filter.hostile.clean", "filter.hostile.smudge", "diff.hostile.textconv",
	} {
		if !keys[want] {
			t.Errorf("%s is not neutralised; the repository chose that name, so only enumeration finds it", want)
		}
	}
}

// The two keys that must stay out of the block, because blanking them breaks
// git rather than disarming it: diff.external ends every diff with "external
// diff died", core.sshCommand ends every push with "cannot run". Both are
// measured behaviour of the git binary, so pin them here — if a future git
// treats an empty value as unset, this test says so and the list can grow.
func TestKeysThatCannotBeBlankedAreNotBlanked(t *testing.T) {
	env := git.SandboxEnv(context.Background(), repo(t))
	n, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	for i := range n {
		switch k := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]; k {
		case "diff.external", "core.sshCommand":
			t.Errorf("%s is blanked; an empty value makes git run \"\" and fail, see harden.go", k)
		}
	}
}
