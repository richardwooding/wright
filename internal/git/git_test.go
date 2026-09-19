package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/git"
)

// repo creates a temp repository with one committed file (tracked.txt).
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitc("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitc("add", "tracked.txt")
	gitc("commit", "-q", "-m", "init")
	return dir
}

func TestStatusOutsideRepo(t *testing.T) {
	s := git.Status(context.Background(), t.TempDir())
	if s.Repo {
		t.Error("Repo should be false outside a repository")
	}
	if s.String() != "" {
		t.Errorf("String = %q, want empty", s.String())
	}
}

func TestStatus(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()

	clean := git.Status(ctx, dir)
	if !clean.Repo || clean.Branch != "main" || clean.Root != dir {
		t.Fatalf("clean = %+v", clean)
	}
	if clean.Modified+clean.Staged+clean.Untracked != 0 {
		t.Errorf("clean repo reports changes: %+v", clean)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "staged.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	s := git.Status(ctx, dir)
	if s.Modified != 1 || s.Staged != 1 || s.Untracked != 1 {
		t.Errorf("status = %+v, want modified=1 staged=1 untracked=1", s)
	}
	if got := s.String(); got != "main +1 ~1 ?1" {
		t.Errorf("String = %q", got)
	}
}

func TestDiff(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := git.Diff(ctx, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "+changed") || !strings.Contains(d, "-one") {
		t.Errorf("diff = %q", d)
	}
	staged, err := git.Diff(ctx, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if staged != "" {
		t.Errorf("staged diff should be empty, got %q", staged)
	}
}

func TestIsTracked(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	if !git.IsTracked(ctx, dir, "tracked.txt") {
		t.Error("tracked.txt should be tracked")
	}
	if !git.IsTracked(ctx, dir, filepath.Join(dir, "tracked.txt")) {
		t.Error("absolute path should be tracked")
	}
	if git.IsTracked(ctx, dir, "missing.txt") {
		t.Error("missing.txt should not be tracked")
	}
	if git.IsTracked(ctx, t.TempDir(), "x") {
		t.Error("outside a repo nothing is tracked")
	}
}

func TestSummaryString(t *testing.T) {
	tests := []struct {
		name string
		s    git.Summary
		want string
	}{
		{"not repo", git.Summary{}, ""},
		{"clean", git.Summary{Repo: true, Branch: "main"}, "main"},
		{"all", git.Summary{Repo: true, Branch: "dev", Ahead: 1, Behind: 2, Modified: 3, Staged: 4, Untracked: 5}, "dev +4 ~3 ?5 ↑1 ↓2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.String(); got != tt.want {
				t.Errorf("String = %q, want %q", got, tt.want)
			}
		})
	}
}

// A sandboxed git cannot read the user's global config, so wright resolves
// the identity on the host and carries it in. Without this a commit is
// attributed to user@hostname, which the user never chose.
func TestWhoAmIAndEnv(t *testing.T) {
	dir := repo(t)
	for _, args := range [][]string{{"config", "user.name", "Ada Lovelace"}, {"config", "user.email", "ada@example.test"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	id := git.WhoAmI(context.Background(), dir)
	if id.Name != "Ada Lovelace" || id.Email != "ada@example.test" {
		t.Fatalf("WhoAmI = %+v", id)
	}
	env := id.Env()
	for k, want := range map[string]string{
		"GIT_AUTHOR_NAME":     "Ada Lovelace",
		"GIT_AUTHOR_EMAIL":    "ada@example.test",
		"GIT_COMMITTER_NAME":  "Ada Lovelace",
		"GIT_COMMITTER_EMAIL": "ada@example.test",
	} {
		if env[k] != want {
			t.Errorf("Env()[%s] = %q, want %q", k, env[k], want)
		}
	}
	if env["GIT_CONFIG_GLOBAL"] != os.DevNull {
		t.Errorf("GIT_CONFIG_GLOBAL = %q, want %q", env["GIT_CONFIG_GLOBAL"], os.DevNull)
	}
	// With no identity configured wright must not invent one.
	if got := (git.Identity{Name: "Ada"}).Env(); got != nil {
		t.Errorf("a half-configured identity produced %v, want nil", got)
	}
}
