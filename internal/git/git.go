// Package git shells out to the git binary for the few read-only facts wright
// needs: a status summary for the status bar, diffs for approval previews and
// whether a path is tracked (which decides if truncating it is destructive).
// Every call is bounded by a short timeout and never blocks the UI.
package git

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds every git invocation; a hung git must never freeze the TUI.
const Timeout = 2 * time.Second

// Summary is the compact repository state shown in the status bar.
type Summary struct {
	Repo      bool
	Branch    string
	Ahead     int
	Behind    int
	Modified  int // unstaged changes (including unmerged)
	Staged    int
	Untracked int
	Root      string
}

// String renders the status-bar form: "main +3 ~2 ?1 ↑1 ↓2" (zero counters omitted).
func (s Summary) String() string {
	if !s.Repo {
		return ""
	}
	parts := []string{s.Branch}
	add := func(n int, glyph string) {
		if n > 0 {
			parts = append(parts, glyph+strconv.Itoa(n))
		}
	}
	add(s.Staged, "+")
	add(s.Modified, "~")
	add(s.Untracked, "?")
	add(s.Ahead, "↑")
	add(s.Behind, "↓")
	return strings.Join(parts, " ")
}

// runWith executes git in dir with env and the package timeout layered on ctx.
func runWith(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	return string(out), err
}

// run executes git in dir with the repository's program-naming config keys
// neutralised. Nothing in this package may call exec directly: the whole
// point is that a host-side git never runs what the repository chose. See
// harden.go.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runWith(ctx, dir, hostEnv(ctx, dir), args...)
}

// Status summarises the repository containing dir. It never returns an
// error: outside a repository (or without git) Repo is false.
func Status(ctx context.Context, dir string) Summary {
	// Resolved once and reused: hostEnv shells out to enumerate the
	// repository's own keys, and this runs every five seconds.
	env := hostEnv(ctx, dir)
	root, err := runWith(ctx, dir, env, "rev-parse", "--show-toplevel")
	if err != nil {
		return Summary{}
	}
	s := Summary{Repo: true, Root: strings.TrimSpace(root)}
	out, err := runWith(ctx, dir, env, "status", "--porcelain=v2", "-b", "--untracked-files=normal")
	if err != nil {
		return s
	}
	parseStatus(&s, out)
	return s
}

// parseStatus fills s from `git status --porcelain=v2 -b` output.
func parseStatus(s *Summary, out string) {
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			s.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.ab "):
			s.Ahead, s.Behind = parseAheadBehind(strings.TrimPrefix(line, "# branch.ab "))
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			countXY(s, line[2:])
		case strings.HasPrefix(line, "u "):
			s.Modified++
		case strings.HasPrefix(line, "? "):
			s.Untracked++
		}
	}
	if s.Branch == "(detached)" {
		s.Branch = "detached"
	}
}

// parseAheadBehind parses "+N -M".
func parseAheadBehind(ab string) (ahead, behind int) {
	for field := range strings.FieldsSeq(ab) {
		n, err := strconv.Atoi(field[1:])
		if err != nil || field == "" {
			continue
		}
		switch field[0] {
		case '+':
			ahead = n
		case '-':
			behind = n
		}
	}
	return ahead, behind
}

// countXY interprets the two-letter XY field: X is the index state, Y the
// worktree state; '.' means unchanged.
func countXY(s *Summary, rest string) {
	if len(rest) < 2 {
		return
	}
	if rest[0] != '.' {
		s.Staged++
	}
	if rest[1] != '.' {
		s.Modified++
	}
}

// Diff returns the unified diff of the worktree (or the index when staged).
//
// --no-ext-diff and --no-textconv are the hardening: both keys name a program
// the repository chose, and neither can be blanked in the environment without
// breaking diff outright (see harden.go). Refusing them on the command line
// costs nothing and is what keeps an approval preview from running the code
// it is previewing.
func Diff(ctx context.Context, dir string, staged bool) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	if staged {
		args = append(args, "--cached")
	}
	out, err := run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// IsTracked reports whether path (absolute or dir-relative) is in the index.
func IsTracked(ctx context.Context, dir, path string) bool {
	_, err := run(ctx, dir, "ls-files", "--error-unmatch", "--", path)
	return err == nil
}

// Identity is the commit identity git would use in a directory, resolved on
// the host so conditional includes (includeIf) apply. Either field may be
// empty when the user has not configured one.
type Identity struct{ Name, Email string }

// WhoAmI resolves the commit identity for dir. It runs on the host before the
// sandbox is built: inside the sandbox the user's global config is either
// masked (bwrap replaces $HOME with a tmpfs) or unreadable (landlock grants
// no rule for it), so git would otherwise invent user@hostname and commit
// under an address the user never chose.
func WhoAmI(ctx context.Context, dir string) Identity {
	var id Identity
	if out, err := run(ctx, dir, "config", "--get", "user.name"); err == nil {
		id.Name = strings.TrimSpace(out)
	}
	if out, err := run(ctx, dir, "config", "--get", "user.email"); err == nil {
		id.Email = strings.TrimSpace(out)
	}
	return id
}

// CredentialHelperKeys re-points git's credential helper at one program
// wright chose, replacing the blank that SandboxEnv installs.
//
// Only the *value* changes: the key name and GIT_CONFIG_COUNT are the ones
// SandboxEnv already emitted, so the two maps overlay without disturbing the
// block's bookkeeping. Every other neutralised key stays blank — in
// particular core.sshCommand, which is the one a reader will worry about.
//
// helper is a git credential.helper value; a leading "!" makes git run it as
// a shell command rather than looking for git-credential-<name>. Pass an
// absolute, quoted path: without one git resolves the program through the
// sandbox's PATH, and a script can put its own directory first.
func CredentialHelperKeys(helper string) map[string]string {
	i := strconv.Itoa(credentialHelperKey())
	return map[string]string{
		"GIT_CONFIG_KEY_" + i:   "credential.helper",
		"GIT_CONFIG_VALUE_" + i: helper,
	}
}

// Toplevel returns the repository toplevel containing dir, or "" when dir is
// not in a repository (or git is missing).
//
// It exists so that finding the workspace goes through this package's
// hardening rather than running git separately. `rev-parse --show-toplevel`
// is not known to run any of the keys harden.go disarms — measured against
// git 2.55 it fires none of them, because it reads no index — but it is the
// earliest git wright runs, before the user has been asked to trust
// anything, and one hardened path is easier to keep true than two.
func Toplevel(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
