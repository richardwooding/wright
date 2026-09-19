package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/prompt"
	"github.com/richardwooding/wright/internal/trust"
	"github.com/richardwooding/wright/internal/workspace"
)

// agentsSkeleton is the AGENTS.md `wright init` writes. It is deliberately a
// prompt to the human, not a description of the project: the model reads
// whatever ends up here, so guessed content would mislead it.
const agentsSkeleton = `# AGENTS.md

Instructions for AI coding agents working in this repository. Keep it short
and factual; every line here is sent to the model on every turn.

## Build and test

<!-- e.g. go build ./... && go test -race ./... -->

## Conventions

<!-- formatting, naming, commit style, what not to touch -->

## Layout

<!-- the two or three directories that matter and what lives in them -->
`

// settingsSkeleton is the project settings file. Rules are empty on purpose:
// the builtin defaults already allow reads and builds, and a project should
// add allow rules only where a reviewer can see them in a diff.
const settingsSkeleton = `{
  "permissions": {
    "allow": [],
    "ask": [],
    "deny": []
  }
}
`

// InitProject creates AGENTS.md and .wright/settings.json at the workspace
// root containing dir when they are missing, and adds
// .wright/settings.local.json to .gitignore when the root is a git checkout.
// A settings file it just wrote is recorded as trusted: the user made it, so
// asking them to accept it on the next run would be noise. It returns the
// paths it wrote, relative to the root.
func InitProject(dir string) ([]string, error) {
	ws, err := workspace.Open(dir, nil)
	if err != nil {
		return nil, err
	}
	root := ws.Root()
	var written []string
	ok, err := writeIfMissing(filepath.Join(root, prompt.AgentsFile), agentsSkeleton)
	if err != nil {
		return written, err
	}
	if ok {
		written = append(written, prompt.AgentsFile)
	}
	paths := config.DefaultPaths(root)
	if ok, err = writeIfMissing(paths.ProjectSettingsFile(), settingsSkeleton); err != nil {
		return written, err
	}
	if ok {
		written = append(written, filepath.Join(".wright", "settings.json"))
		if err := trustFile(paths, root); err != nil {
			return written, err
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		ok, err := ensureIgnored(filepath.Join(root, ".gitignore"), ".wright/settings.local.json")
		if err != nil {
			return written, err
		}
		if ok {
			written = append(written, ".gitignore (+.wright/settings.local.json)")
		}
	}
	if _, err := config.Decode([]byte(settingsSkeleton)); err != nil {
		return written, fmt.Errorf("settings skeleton invalid: %w", err) // a build defect, caught by tests
	}
	return written, nil
}

// trustFile accepts the project settings file as it stands on disk.
func trustFile(paths config.Paths, root string) error {
	hash, err := trust.HashFile(paths.ProjectSettingsFile())
	if err != nil {
		return err
	}
	return trust.Open(paths.TrustFile()).AcceptProject(root, hash)
}

// writeIfMissing creates path with content; false when it already exists.
func writeIfMissing(path, content string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}

// ensureIgnored appends line to the ignore file unless it is already listed.
func ensureIgnored(path, line string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for l := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(l) == line {
			return false, nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	sep := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		sep = "\n"
	}
	if _, err := f.WriteString(sep + line + "\n"); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}
