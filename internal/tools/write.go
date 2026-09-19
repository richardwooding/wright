package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aymanbagabas/go-udiff"
	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

type writeArgs struct {
	Path    string `json:"path" jsonschema:"file to create or overwrite, absolute or workspace-relative"`
	Content string `json:"content" jsonschema:"the complete new content of the file"`
}

func (d *Deps) writeFile() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameWriteFile,
			"Create a file or replace its whole content. Parent directories are created inside the workspace. A snapshot is taken first so the change can be undone.",
			d.runWriteFile),
		describe: d.describeWrite,
	}
}

func (d *Deps) describeWrite(args json.RawMessage) (policy.Request, Preview, error) {
	var a writeArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, _, err := d.resolve(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameWriteFile, Args: args, Paths: []string{abs}, Writes: []string{abs}}
	rel := d.rel(abs)
	old, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		body := fmt.Sprintf("new file, %d lines", countLines(a.Content))
		return req, Preview{Title: NameWriteFile + " " + rel, Body: body}, nil
	}
	if err != nil {
		return req, Preview{}, err
	}
	return req, Preview{Title: NameWriteFile + " " + rel, Diff: unified(rel, string(old), a.Content)}, nil
}

func (d *Deps) runWriteFile(ctx context.Context, a writeArgs) (agentkit.Output, error) {
	abs, inside, err := d.resolve(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := d.refuseWrite(abs, inside); err != nil {
		return agentkit.Output{}, err
	}
	mode, err := d.prepareTarget(abs, inside)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := d.snapshot(ctx, abs); err != nil {
		return agentkit.Output{}, err
	}
	if err := os.WriteFile(abs, []byte(a.Content), mode); err != nil {
		return agentkit.Output{}, err
	}
	return agentkit.Text(fmt.Sprintf("Wrote %d bytes (%d lines) to %s", len(a.Content), countLines(a.Content), d.rel(abs))), nil
}

// prepareTarget checks the write target and returns the mode to use:
// the existing file's mode, or 0644 for a new file. Missing parents are
// created only inside the workspace.
func (d *Deps) prepareTarget(abs string, inside bool) (os.FileMode, error) {
	info, err := os.Stat(abs)
	switch {
	case err == nil && info.IsDir():
		return 0, fmt.Errorf("%s is a directory", d.rel(abs))
	case err == nil:
		return info.Mode().Perm(), nil
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	parent := filepath.Dir(abs)
	if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
		if !inside {
			return 0, fmt.Errorf("parent directory %s does not exist (only created inside the workspace)", parent)
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return 0, err
		}
	}
	return 0o644, nil
}

// countLines counts newline-terminated lines plus a trailing partial line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// unified renders a git-style unified diff with three lines of context.
func unified(rel, before, after string) string {
	return udiff.Unified("a/"+rel, "b/"+rel, before, after)
}
