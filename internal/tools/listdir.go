package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

const (
	// maxListEntries caps a tree listing.
	maxListEntries = 500
	// maxListDepth bounds how deep list_dir will go.
	maxListDepth = 8
)

type listArgs struct {
	Path  string `json:"path,omitempty" jsonschema:"directory to list (default: workspace root)"`
	Depth int    `json:"depth,omitempty" jsonschema:"levels to descend (default 1, max 8)"`
}

func (d *Deps) listDir() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameListDir,
			"List a directory as a tree, directories first, skipping ignored and hidden paths. At most 500 entries.",
			d.runListDir),
		describe: d.describeList,
	}
}

func (d *Deps) describeList(args json.RawMessage) (policy.Request, Preview, error) {
	var a listArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, err := d.base(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameListDir, Args: args, Paths: []string{abs}}
	return req, Preview{Title: NameListDir + " " + d.rel(abs), Body: abs}, nil
}

func (d *Deps) runListDir(ctx context.Context, a listArgs) (agentkit.Output, error) {
	base, err := d.base(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	depth := a.Depth
	if depth < 1 {
		depth = 1
	}
	depth = min(depth, maxListDepth)
	t := &treeWriter{d: d, limit: maxListEntries}
	t.b.WriteString(d.rel(base) + "/\n")
	if err := t.list(ctx, base, 1, depth); err != nil {
		return agentkit.Output{}, err
	}
	if t.truncated {
		fmt.Fprintf(&t.b, "[truncated at %d entries; list a subdirectory or reduce depth]\n", maxListEntries)
	}
	return agentkit.Text(strings.TrimRight(t.b.String(), "\n")), nil
}

// treeWriter renders an indented listing with a global entry cap.
type treeWriter struct {
	d         *Deps
	b         strings.Builder
	count     int
	limit     int
	truncated bool
}

func (t *treeWriter) list(ctx context.Context, dir string, level, maxLevel int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if level == 1 {
			return err
		}
		return nil // an unreadable subdirectory is shown but not expanded
	}
	visible := entries[:0]
	for _, e := range entries {
		if t.d.visible(filepath.Join(dir, e.Name())) {
			visible = append(visible, e)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		if visible[i].IsDir() != visible[j].IsDir() {
			return visible[i].IsDir()
		}
		return visible[i].Name() < visible[j].Name()
	})
	indent := strings.Repeat("  ", level)
	for _, e := range visible {
		if t.count == t.limit {
			t.truncated = true
			return nil
		}
		t.count++
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		t.b.WriteString(indent + name + "\n")
		if e.IsDir() && level < maxLevel {
			if err := t.list(ctx, filepath.Join(dir, e.Name()), level+1, maxLevel); err != nil {
				return err
			}
		}
	}
	return nil
}
