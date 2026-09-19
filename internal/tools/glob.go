package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

// maxGlobResults caps a glob listing.
const maxGlobResults = 500

type globArgs struct {
	Pattern string `json:"pattern" jsonschema:"doublestar pattern relative to path, e.g. **/*.go or src/*.ts"`
	Path    string `json:"path,omitempty" jsonschema:"directory to search (default: workspace root)"`
}

// globHit is one matching file with its modification time for sorting.
type globHit struct {
	rel string
	mod time.Time
}

func (d *Deps) glob() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameGlob,
			"Find files matching a doublestar pattern. Ignored and hidden paths are skipped; results are newest first, at most 500.",
			d.runGlob),
		describe: d.describeGlob,
	}
}

// base resolves an optional directory argument, defaulting to the root.
func (d *Deps) base(p string) (string, error) {
	if p == "" {
		return d.WS.Root(), nil
	}
	abs, _, err := d.resolve(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", d.rel(abs))
	}
	return abs, nil
}

func (d *Deps) describeGlob(args json.RawMessage) (policy.Request, Preview, error) {
	var a globArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, err := d.base(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameGlob, Args: args, Paths: []string{abs}}
	return req, Preview{Title: NameGlob + " " + a.Pattern, Body: abs}, nil
}

func (d *Deps) runGlob(ctx context.Context, a globArgs) (agentkit.Output, error) {
	if a.Pattern == "" {
		return agentkit.Output{}, fmt.Errorf("pattern is required")
	}
	if !doublestar.ValidatePattern(a.Pattern) {
		return agentkit.Output{}, fmt.Errorf("invalid pattern %q", a.Pattern)
	}
	base, err := d.base(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	var hits []globHit
	err = d.walk(ctx, base, func(abs string, de fs.DirEntry) error {
		rel, _ := filepath.Rel(base, abs)
		rel = filepath.ToSlash(rel)
		if ok, _ := doublestar.Match(a.Pattern, rel); !ok {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil //nolint:nilerr // a file that vanished mid-walk is simply not listed
		}
		hits = append(hits, globHit{rel: d.rel(abs), mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return agentkit.Output{}, err
	}
	if len(hits) == 0 {
		return agentkit.Text("no files match " + a.Pattern), nil
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
	var b strings.Builder
	for i, h := range hits {
		if i == maxGlobResults {
			fmt.Fprintf(&b, "[truncated: showing %d of %d matches; narrow the pattern]\n", maxGlobResults, len(hits))
			break
		}
		b.WriteString(h.rel)
		b.WriteByte('\n')
	}
	return agentkit.Text(strings.TrimRight(b.String(), "\n")), nil
}

// walk visits every visible regular file under base, skipping ignored and
// hidden directories whole so node_modules never costs a stat per file.
func (d *Deps) walk(ctx context.Context, base string, visit func(abs string, de fs.DirEntry) error) error {
	return filepath.WalkDir(base, func(abs string, de fs.DirEntry, err error) error {
		if err != nil {
			if abs == base {
				return err
			}
			return nil // unreadable entry: skip, do not abort the listing
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if abs == base {
			return nil
		}
		if !d.visible(abs) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if de.IsDir() || !de.Type().IsRegular() {
			return nil
		}
		return visit(abs, de)
	})
}
