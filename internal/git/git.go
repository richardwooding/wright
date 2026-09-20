// Package git shells out to the git binary for the few read-only facts wright
// needs: a status summary for the status bar, diffs for approval previews and
// whether a path is tracked (which decides if truncating it is destructive).
// Every call is bounded by a short timeout and never blocks the UI.
package git

import (
	"context"
	"os"
	"os/exec"
	"slices"
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

// run executes git in dir with the package timeout layered on ctx.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// Status summarises the repository containing dir. It never returns an
// error: outside a repository (or without git) Repo is false.
func Status(ctx context.Context, dir string) Summary {
	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Summary{}
	}
	s := Summary{Repo: true, Root: strings.TrimSpace(root)}
	out, err := run(ctx, dir, "status", "--porcelain=v2", "-b", "--untracked-files=normal")
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
func Diff(ctx context.Context, dir string, staged bool) (string, error) {
	args := []string{"diff", "--no-color"}
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

// neutralised are configuration keys that name a program for git to run. A
// repository carries its own .git/config, so cloning a hostile one and
// running an ordinary `git diff` or `git status` is enough to execute it —
// and those commands are allowed by default because they are how an agent
// reads a repository. Nothing static can see this: the command line is
// innocent. Overriding the keys in the environment is what closes it.
var neutralised = []string{
	"diff.external",   // runs per changed file on git diff
	"core.fsmonitor",  // runs on git status
	"core.sshCommand", // runs on any remote operation
	"credential.helper",
	"sequence.editor",
	"core.editor",
	"core.askpass", // runs when no helper produced a credential
	// The proxy keys are not programs: they decide who *receives* an
	// authenticated request. They were harmless while no credential could be
	// produced inside the sandbox; once one can, a cloned repository's own
	// http.proxy is a way to be handed the request that carries it. Empty
	// means "no proxy", so blanking them changes nothing for anyone who is
	// not behind one — and someone who is cannot reach the network from the
	// sandbox without --allow-network anyway.
	"http.proxy",
	"core.gitproxy",
}

// credentialHelperKey is credential.helper's position in neutralised, found
// rather than written down: reordering the list above must not silently
// blank a different key than the one a caller meant to replace.
func credentialHelperKey() int { return slices.Index(neutralised, "credential.helper") }

// CredentialHelperKeys re-points git's credential helper at one program
// wright chose, replacing the blank that Env installs.
//
// Only the *value* changes: the key name and GIT_CONFIG_COUNT are the ones
// Env already emitted, so the two maps overlay without disturbing the
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

// Env returns the environment entries wright adds to a sandboxed git: the
// commit identity resolved on the host, empty stand-ins for the config files
// the sandbox hides, and overrides that disarm the configuration keys a
// repository could use to name a program. Entries injected this way take
// precedence over every config file, including the repository's own.
func (id Identity) Env() map[string]string {
	env := map[string]string{
		// The global and system files are unreadable inside the sandbox;
		// pointing git at an empty one turns a warning (or a fatal error
		// under landlock) into ordinary "no global config".
		"GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_CONFIG_SYSTEM": os.DevNull,
		"GIT_CONFIG_COUNT":  strconv.Itoa(len(neutralised)),
		// With credential.helper blank, a push that needs a credential would
		// otherwise block on git's terminal prompt inside a sandbox where
		// nobody can answer it. Failing immediately is the honest outcome.
		"GIT_TERMINAL_PROMPT": "0",
	}
	for i, key := range neutralised {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = key
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = ""
	}
	// With no identity configured wright carries none: git keeps its own
	// behaviour rather than committing under a name wright invented.
	if id.Name != "" && id.Email != "" {
		env["GIT_AUTHOR_NAME"] = id.Name
		env["GIT_AUTHOR_EMAIL"] = id.Email
		env["GIT_COMMITTER_NAME"] = id.Name
		env["GIT_COMMITTER_EMAIL"] = id.Email
	}
	return env
}
