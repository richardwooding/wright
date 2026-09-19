// Package skillsdir finds Agent Skills on disk and merges them into one set
// for the agent. It decides *where* wright looks and in what order; parsing,
// validation and the tools themselves belong to agentkit's skills package.
//
// The search path, lowest precedence first:
//
//	$WRIGHT_CONFIG_DIR/skills      the user's own skills
//	<workspace>/.agents/skills     the portable project location
//	<workspace>/.wright/skills     wright's project location
//	skills.extraDirs               from settings
//	<workspace>/.claude/skills     only with skills.loadClaudeSkills
//
// Later directories win a name collision, so a project can override a user
// skill and an explicitly configured directory can override both. Missing
// directories are not an error: most projects have none.
//
// A skill is instructions plus files, never a licence to run anything: the
// scripts a skill ships are executed by the model through bash like any other
// command, so the permission engine and the sandbox still decide.
package skillsdir

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/richardwooding/agentkit/skills"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/workspace"
)

// Problem is a skill that could not be loaded, or was loaded despite
// violating the specification. Path is the SKILL.md it came from.
type Problem struct {
	Path string
	Err  error
}

// Error implements error.
func (p Problem) Error() string { return p.Path + ": " + p.Err.Error() }

// Unwrap returns the underlying error.
func (p Problem) Unwrap() error { return p.Err }

// Row describes one loaded skill for `/skills` and `wright skills`.
type Row struct {
	Name        string
	Description string
	// Source is the directory the skill was loaded from.
	Source string
}

// Dirs returns the skill directories in precedence order, lowest first,
// whether or not they exist. userConfigDir is Paths.UserConfig.
func Dirs(ws *workspace.Workspace, userConfigDir string, cfg config.Skills) []string {
	var dirs []string
	if userConfigDir != "" {
		dirs = append(dirs, filepath.Join(userConfigDir, "skills"))
	}
	root := ws.Root()
	dirs = append(dirs, filepath.Join(root, ".agents", "skills"), filepath.Join(root, ".wright", "skills"))
	for _, d := range cfg.ExtraDirs {
		dirs = append(dirs, ws.Expand(d))
	}
	if cfg.LoadClaudeSkills {
		dirs = append(dirs, filepath.Join(root, ".claude", "skills"))
	}
	return dirs
}

// Load scans the search path and merges what it finds into one set. The
// error is returned only for a failure that makes the result meaningless;
// an unreadable directory or an invalid SKILL.md is a Problem, so one bad
// skill never costs the user the rest of them.
func Load(ws *workspace.Workspace, userConfigDir string, cfg config.Skills) (*skills.Set, []Problem, error) {
	if ws == nil {
		return nil, nil, fmt.Errorf("skillsdir: workspace is required")
	}
	var (
		sets     []*skills.Set
		problems []Problem
	)
	for _, dir := range Dirs(ws, userConfigDir, cfg) {
		set, probs := loadDir(dir)
		problems = append(problems, probs...)
		if set != nil {
			sets = append(sets, set)
		}
	}
	return skills.Merge(sets...), problems, nil
}

// loadDir loads one directory, rewriting each skill's Dir to its absolute
// path so the model (and `wright skills`) can see where a skill lives.
func loadDir(dir string) (*skills.Set, []Problem) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil // a directory nobody created is not a problem
	}
	set, err := skills.LoadAll(os.DirFS(dir))
	if err != nil {
		return nil, []Problem{{Path: dir, Err: err}}
	}
	problems := make([]Problem, 0, len(set.Problems))
	for _, p := range set.Problems {
		problems = append(problems, Problem{Path: filepath.Join(dir, p.Path), Err: p.Err})
	}
	for _, s := range set.All() {
		s.Dir = filepath.Join(dir, s.Dir)
	}
	return set, problems
}

// Describe lists the merged set for display, in load order.
func Describe(set *skills.Set) []Row {
	all := set.All()
	rows := make([]Row, 0, len(all))
	for _, s := range all {
		rows = append(rows, Row{Name: s.Name, Description: s.Description, Source: s.Dir})
	}
	return rows
}
