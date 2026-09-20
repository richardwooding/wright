package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/policy"
)

// appendRule is what the policy engine's persist hook calls, and the hook is
// reachable only from inside a built session, so it is exercised here
// directly against the same file-writing path a grant takes.
func TestAcceptingTheSameGrantTwiceSavesItOnce(t *testing.T) {
	t.Setenv("WRIGHT_CONFIG_DIR", t.TempDir())
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".wright"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := config.Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	save := func(text string, d policy.Decision) {
		t.Helper()
		r, err := policy.ParseRule(text, policy.SourceProjectLocal)
		if err != nil {
			t.Fatal(err)
		}
		r.Decision = d
		if err := l.SaveProjectLocal(func(s *config.Settings) { appendRule(&s.Permissions, r) }); err != nil {
			t.Fatal(err)
		}
	}
	// A user accepts the same offer again — the rule may not have matched
	// the next call for some other reason — and one real settings.local.json
	// ended up with three copies of bash(fpc *).
	save("bash(fpc *)", policy.Allow)
	save("bash(fpc *)", policy.Allow)
	save("bash(./bin/t *)", policy.Allow)
	save("bash(fpc *)", policy.Ask)

	raw, err := os.ReadFile(l.Paths.ProjectLocalFile())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"bash(fpc *)"`); n != 2 {
		t.Errorf("bash(fpc *) appears %d times, want 2 (once in allow, once in ask):\n%s", n, raw)
	}
	l2, err := config.Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	s := l2.ProjectLocal
	if got := s.Permissions.Allow; len(got) != 2 {
		t.Errorf("allow = %v, want the two distinct rules", got)
	}
	// The decision lists are separate: deduping must not collapse across them.
	if got := s.Permissions.Ask; len(got) != 1 || got[0] != "bash(fpc *)" {
		t.Errorf("ask = %v, want the same text recorded under its own decision", got)
	}
}

// A rule accepted "for every project" lands in the user's own config, which
// applies to every workspace and is not gated by any project's trust.
func TestAUserScopedGrantIsWrittenToTheUserConfig(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("WRIGHT_CONFIG_DIR", cfg)
	ws := t.TempDir()
	l, err := config.Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.ParseRule("bash(gh pr *) +net", policy.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	r.Decision = policy.Allow
	save := func() {
		t.Helper()
		if err := l.SaveUser(func(s *config.Settings) { appendRule(&s.Permissions, r) }); err != nil {
			t.Fatal(err)
		}
	}
	save()
	save() // accepting twice must not write it twice

	raw, err := os.ReadFile(l.Paths.UserConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), `"bash(gh pr *) +net"`); n != 1 {
		t.Errorf("the rule appears %d times, want 1:\n%s", n, raw)
	}
	// And nothing was written into the workspace.
	if _, err := os.Stat(filepath.Join(ws, ".wright", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("a user-scoped grant touched the project (%v)", err)
	}
}

// A file written before addOnce existed can already hold duplicates, and
// nothing would ever have removed them. Every grant rewrites the whole file,
// so that is the moment to clear them up.
func TestSavingARuleCleansDuplicatesAlreadyInTheFile(t *testing.T) {
	p := &config.Permissions{
		Allow: []string{"bash(fpc *)", "bash(mkdir *)", "bash(fpc *)", "bash(cat *)", "bash(fpc *)"},
		Ask:   []string{"bash(git push *)", "bash(git push *)"},
	}
	r, err := policy.ParseRule("bash(gh pr *) +net", policy.SourceProjectLocal)
	if err != nil {
		t.Fatal(err)
	}
	r.Decision = policy.Allow
	appendRule(p, r)

	want := []string{"bash(fpc *)", "bash(mkdir *)", "bash(cat *)", "bash(gh pr *) +net"}
	if !slices.Equal(p.Allow, want) {
		t.Errorf("allow = %v, want %v (order preserved, duplicates gone)", p.Allow, want)
	}
	if !slices.Equal(p.Ask, []string{"bash(git push *)"}) {
		t.Errorf("ask = %v", p.Ask)
	}
}
