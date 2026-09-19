package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/richardwooding/wright/internal/workspace"
)

// Errors reported by the custom-agent loader.
var (
	ErrNoFrontmatter = errors.New("agents: missing frontmatter")
	ErrMissingField  = errors.New("agents: missing required field")
	ErrInvalidName   = errors.New("agents: invalid name")
)

// maxAgentNameLen bounds a name so it stays a usable tool name.
const maxAgentNameLen = 48

// Definition is one custom sub-agent: the frontmatter plus the Markdown body
// as its instructions.
type Definition struct {
	Name        string
	Description string
	// Instructions is the body of the file, used as the agent's system prompt.
	Instructions string
	// Tools names the tools the agent may use. Empty means the default for
	// its read-only flag.
	Tools []string
	// Model is the model name to run the agent with; empty means the fast
	// model the app supplies.
	Model string
	// ReadOnly defaults to true: an agent that does not say otherwise gets
	// the read-only toolset.
	ReadOnly bool
	// Source is the file the definition came from ("" for built-ins).
	Source string
}

// Problem is a definition file that could not be used.
type Problem struct {
	Path string
	Err  error
}

// Error implements error.
func (p Problem) Error() string { return p.Path + ": " + p.Err.Error() }

// Unwrap returns the underlying error.
func (p Problem) Unwrap() error { return p.Err }

// LoadCustom reads <workspace>/.wright/agents/*.md and
// <userConfigDir>/agents/*.md. The project directory wins a name collision,
// since the more specific definition should be the one that runs. Missing
// directories are normal; a file that cannot be parsed is a Problem.
func LoadCustom(ws *workspace.Workspace, userConfigDir string) ([]Definition, []Problem, error) {
	if ws == nil {
		return nil, nil, errors.New("agents: workspace is required")
	}
	var (
		defs     []Definition
		problems []Problem
	)
	dirs := []string{}
	if userConfigDir != "" {
		dirs = append(dirs, filepath.Join(userConfigDir, "agents"))
	}
	dirs = append(dirs, filepath.Join(ws.Root(), ".wright", "agents"))
	for _, dir := range dirs {
		found, probs := loadDir(dir)
		problems = append(problems, probs...)
		for _, d := range found {
			if i := slices.IndexFunc(defs, func(e Definition) bool { return e.Name == d.Name }); i >= 0 {
				defs[i] = d // a later directory (the project) wins
				continue
			}
			defs = append(defs, d)
		}
	}
	return defs, problems, nil
}

// loadDir parses every *.md in dir, sorted so the result is stable.
func loadDir(dir string) ([]Definition, []Problem) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // a directory nobody created is not a problem
	}
	var (
		defs     []Definition
		problems []Problem
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path) //nolint:gosec // the path comes from a directory wright owns
		if err != nil {
			problems = append(problems, Problem{Path: path, Err: err})
			continue
		}
		def, err := Parse(data)
		if err != nil {
			problems = append(problems, Problem{Path: path, Err: err})
			continue
		}
		def.Source = path
		defs = append(defs, def)
	}
	return defs, problems
}

// Parse reads one agent definition: frontmatter between "---" lines, then the
// Markdown body as the agent's instructions. The frontmatter parser is the
// same small YAML subset the skills loader uses — "key: value" with optional
// quotes, "#" comments, and lists written either inline ("a, b") or as "- a"
// lines — because a full YAML dependency for six keys is not worth it.
//
// Recognised keys: name, description, tools, model, read-only (default true).
func Parse(data []byte) (Definition, error) {
	front, body, err := split(string(data))
	if err != nil {
		return Definition{}, err
	}
	fields := parseFields(front)
	def := Definition{
		Name:         fields.scalar("name"),
		Description:  fields.scalar("description"),
		Instructions: body,
		Tools:        fields.list("tools"),
		Model:        fields.scalar("model"),
		ReadOnly:     true,
	}
	if v, ok := fields.lookup("read-only"); ok {
		def.ReadOnly = boolValue(v, true)
	}
	if def.Name == "" {
		return Definition{}, fmt.Errorf("%w: name", ErrMissingField)
	}
	if err := validateName(def.Name); err != nil {
		return Definition{}, err
	}
	if def.Description == "" {
		return Definition{}, fmt.Errorf("%w: description", ErrMissingField)
	}
	if strings.TrimSpace(def.Instructions) == "" {
		return Definition{}, fmt.Errorf("%w: instructions (the body of the file)", ErrMissingField)
	}
	return def, nil
}

// validateName keeps a name usable as a tool name: letters, digits, hyphen
// and underscore, which is what every provider accepts.
func validateName(name string) error {
	if len(name) > maxAgentNameLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidName, name, maxAgentNameLen)
	}
	for _, r := range name {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("%w: %q may only contain letters, digits, - and _", ErrInvalidName, name)
		}
	}
	return nil
}

// split separates the frontmatter lines from the trimmed body.
func split(text string) (front []string, body string, err error) {
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return nil, "", ErrNoFrontmatter
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimRight(lines[i], " \t"); t == "---" || t == "..." {
			return lines[1:i], strings.TrimSpace(strings.Join(lines[i+1:], "\n")), nil
		}
	}
	return nil, "", fmt.Errorf("%w: no closing ---", ErrNoFrontmatter)
}

// fields holds the parsed frontmatter: scalars by key, plus list items.
type fields struct {
	scalars map[string]string
	lists   map[string][]string
}

func (f fields) lookup(key string) (string, bool) {
	v, ok := f.scalars[key]
	return v, ok
}

func (f fields) scalar(key string) string { return f.scalars[key] }

// list returns a key's items, whether they were written inline
// ("tools: read_file, grep"), as a YAML flow sequence ("[a, b]") or as
// "- name" lines beneath the key.
func (f fields) list(key string) []string {
	if items, ok := f.lists[key]; ok && len(items) > 0 {
		return items
	}
	v := strings.Trim(f.scalars[key], "[]")
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if item := unquote(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// parseFields reads the frontmatter lines.
func parseFields(lines []string) fields {
	f := fields{scalars: map[string]string{}, lists: map[string][]string{}}
	key := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if item, ok := strings.CutPrefix(trimmed, "- "); ok && key != "" {
			f.lists[key] = append(f.lists[key], unquote(item))
			continue
		}
		k, rest, ok := strings.Cut(trimmed, ":")
		if !ok || strings.ContainsAny(strings.TrimSpace(k), " \t") {
			continue
		}
		key = strings.TrimSpace(k)
		f.scalars[key] = unquote(strings.TrimSpace(rest))
	}
	return f
}

// unquote takes the contents of a quoted value, or strips a trailing " #"
// comment from an unquoted one. A quoted value keeps its "#" characters,
// which is the point of quoting it.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if i := strings.IndexByte(v[1:], v[0]); i >= 0 {
			return v[1 : 1+i]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// boolValue reads a YAML-ish boolean, falling back to def for anything it
// does not recognise: a typo in "read-only" leaves the agent read-only
// rather than quietly widening what it may do.
func boolValue(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	default:
		return def
	}
}
