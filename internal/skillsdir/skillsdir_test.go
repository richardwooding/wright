package skillsdir_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/skillsdir"
	"github.com/richardwooding/wright/internal/workspace"
)

// skill writes a SKILL.md for name under dir/name.
func skill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	write(t, filepath.Join(dir, name, "SKILL.md"),
		"---\nname: "+name+"\ndescription: "+description+"\n---\n\n"+body+"\n")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openWS(t *testing.T, root string) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestLoadMergesInPrecedenceOrder(t *testing.T) {
	root := t.TempDir()
	userConfig := t.TempDir()
	extra := t.TempDir()
	skill(t, filepath.Join(userConfig, "skills"), "shared", "user copy", "user body")
	skill(t, filepath.Join(userConfig, "skills"), "only-user", "just the user", "body")
	skill(t, filepath.Join(root, ".agents", "skills"), "shared", "agents copy", "agents body")
	skill(t, filepath.Join(root, ".wright", "skills"), "shared", "project copy", "project body")
	skill(t, extra, "shared", "extra copy", "extra body")
	skill(t, filepath.Join(root, ".claude", "skills"), "claude-only", "opt-in", "body")

	ws := openWS(t, root)
	cfg := config.Skills{ExtraDirs: []string{extra}}
	set, problems, err := skillsdir.Load(ws, userConfig, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	rows := skillsdir.Describe(set)
	byName := map[string]skillsdir.Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if _, ok := byName["claude-only"]; ok {
		t.Error(".claude/skills must stay out unless loadClaudeSkills is set")
	}
	if _, ok := byName["only-user"]; !ok {
		t.Error("user skills must be loaded")
	}
	shared, ok := byName["shared"]
	if !ok {
		t.Fatalf("shared skill missing from %+v", rows)
	}
	if shared.Description != "extra copy" {
		t.Errorf("extraDirs must win: description = %q", shared.Description)
	}
	if want := filepath.Join(extra, "shared"); shared.Source != want {
		t.Errorf("Source = %q, want %q", shared.Source, want)
	}

	cfg.LoadClaudeSkills = true
	set, _, err = skillsdir.Load(ws, userConfig, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Lookup("claude-only"); !ok {
		t.Error("loadClaudeSkills must add .claude/skills")
	}
}

func TestLoadReportsInvalidSkills(t *testing.T) {
	root := t.TempDir()
	// No frontmatter at all: unparseable, so it is skipped with a problem.
	write(t, filepath.Join(root, ".wright", "skills", "broken", "SKILL.md"), "just prose, no frontmatter\n")
	// Parseable but the name does not match the directory: loaded, reported.
	write(t, filepath.Join(root, ".wright", "skills", "mismatch", "SKILL.md"),
		"---\nname: other-name\ndescription: wrong directory\n---\n\nbody\n")
	skill(t, filepath.Join(root, ".wright", "skills"), "good", "fine", "body")

	set, problems, err := skillsdir.Load(openWS(t, root), "", config.Skills{})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 {
		t.Fatalf("want 2 problems, got %d: %v", len(problems), problems)
	}
	for _, p := range problems {
		if !filepath.IsAbs(p.Path) || !strings.HasSuffix(p.Path, "SKILL.md") {
			t.Errorf("problem path %q should be the absolute SKILL.md", p.Path)
		}
		if p.Err == nil || p.Error() == "" {
			t.Errorf("problem %+v has no error", p)
		}
	}
	if _, ok := set.Lookup("good"); !ok {
		t.Error("a valid skill must survive its broken neighbours")
	}
	if _, ok := set.Lookup("broken"); ok {
		t.Error("an unparseable skill must not be loaded")
	}
	if _, ok := set.Lookup("other-name"); !ok {
		t.Error("a skill that only violates the spec is still loaded (and reported)")
	}
}

func TestLoadWithoutAnyDirectories(t *testing.T) {
	root := t.TempDir()
	set, problems, err := skillsdir.Load(openWS(t, root), filepath.Join(root, "nope"), config.Skills{})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 0 || len(problems) != 0 {
		t.Fatalf("empty search path: set=%d problems=%v", set.Len(), problems)
	}
	if rows := skillsdir.Describe(set); len(rows) != 0 {
		t.Fatalf("Describe = %+v", rows)
	}
}

func TestDirsOrder(t *testing.T) {
	root := t.TempDir()
	ws := openWS(t, root)
	dirs := skillsdir.Dirs(ws, "/cfg", config.Skills{ExtraDirs: []string{"rel/skills"}, LoadClaudeSkills: true})
	want := []string{
		filepath.Join("/cfg", "skills"),
		filepath.Join(ws.Root(), ".agents", "skills"),
		filepath.Join(ws.Root(), ".wright", "skills"),
		ws.Expand("rel/skills"),
		filepath.Join(ws.Root(), ".claude", "skills"),
	}
	if len(dirs) != len(want) {
		t.Fatalf("Dirs = %v, want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("Dirs[%d] = %q, want %q", i, dirs[i], want[i])
		}
	}
}
