package prompt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/workspace"
)

// MaxInstructionBytes caps each instruction file so one stray generated
// document cannot crowd the model's context; the tail is replaced by a note.
const MaxInstructionBytes = 64 * 1024

// Instruction file names wright understands. AGENTS.md is the cross-tool
// standard; CLAUDE.md is only used as a fallback with the user's consent.
const (
	AgentsFile = "AGENTS.md"
	ClaudeFile = "CLAUDE.md"
)

// Instruction is one project or user instruction file, ready for the prompt.
type Instruction struct {
	// Source is a display path: workspace-relative inside the workspace,
	// absolute for the user-global file.
	Source string
	Body   string
}

// LoadInstructions gathers instruction files in precedence order, lowest
// first: $userConfigDir/AGENTS.md, then every AGENTS.md from the workspace
// root down to cwd, then root-relative files such as .wright/instructions.md.
// When the walk finds no AGENTS.md but the root has a CLAUDE.md, the
// cfg.Fallback policy decides: "never" skips it, "always" includes it, and
// anything else asks confirmClaudeMD once (nil means no). Files hidden by
// .wrightignore are skipped everywhere.
func LoadInstructions(ws *workspace.Workspace, cwd string, cfg config.Instructions, userConfigDir string, confirmClaudeMD func(path string) bool) ([]Instruction, error) {
	names := cfg.Files
	if len(names) == 0 {
		names = []string{AgentsFile, filepath.Join(".wright", "instructions.md")}
	}
	var out []Instruction
	if userConfigDir != "" {
		p := filepath.Join(userConfigDir, AgentsFile)
		if err := appendFile(&out, ws, p, p); err != nil {
			return nil, err
		}
	}
	sawAgents, err := loadWalk(&out, ws, walkDirs(ws.Root(), cwd), names)
	if err != nil {
		return nil, err
	}
	if err := loadRootRelative(&out, ws, names); err != nil {
		return nil, err
	}
	if !sawAgents {
		if err := claudeFallback(&out, ws, cfg.Fallback, confirmClaudeMD); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadWalk collects the bare file names (no path separator) from each
// directory in dirs and reports whether any of them was an AGENTS.md, which
// decides the CLAUDE.md fallback.
func loadWalk(out *[]Instruction, ws *workspace.Workspace, dirs, names []string) (bool, error) {
	sawAgents := false
	for _, name := range names {
		if hasSeparator(name) {
			continue
		}
		for _, dir := range dirs {
			p := filepath.Join(dir, name)
			n := len(*out)
			if err := appendFile(out, ws, p, ws.Rel(p)); err != nil {
				return false, err
			}
			if len(*out) > n && name == AgentsFile {
				sawAgents = true
			}
		}
	}
	return sawAgents, nil
}

// loadRootRelative collects names that carry a path (".wright/instructions.md")
// from the workspace root only.
func loadRootRelative(out *[]Instruction, ws *workspace.Workspace, names []string) error {
	for _, name := range names {
		if !hasSeparator(name) {
			continue
		}
		p := filepath.Join(ws.Root(), filepath.FromSlash(name))
		if err := appendFile(out, ws, p, ws.Rel(p)); err != nil {
			return err
		}
	}
	return nil
}

func hasSeparator(name string) bool {
	return strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/')
}

// walkDirs lists root and every directory between it and cwd, root first.
// A cwd outside root contributes nothing beyond root.
func walkDirs(root, cwd string) []string {
	dirs := []string{root}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return dirs
	}
	cur := root
	for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		dirs = append(dirs, cur)
	}
	return dirs
}

func claudeFallback(out *[]Instruction, ws *workspace.Workspace, policy string, confirm func(string) bool) error {
	p := filepath.Join(ws.Root(), ClaudeFile)
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	switch policy {
	case "never":
		return nil
	case "always":
	default:
		if confirm == nil || !confirm(p) {
			return nil
		}
	}
	return appendFile(out, ws, p, ws.Rel(p))
}

// appendFile reads path (if it exists, is a regular file and is not hidden)
// and appends it as an Instruction labelled source.
func appendFile(out *[]Instruction, ws *workspace.Workspace, path, source string) error {
	if ws.Hidden(path) {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("prompt: %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("prompt: %s: %w", path, err)
	}
	body := strings.TrimSpace(string(data))
	if len(body) > MaxInstructionBytes {
		body = body[:MaxInstructionBytes] + fmt.Sprintf("\n\n[wright: %s truncated at %d KiB]", source, MaxInstructionBytes/1024)
	}
	if body == "" {
		return nil
	}
	*out = append(*out, Instruction{Source: source, Body: body})
	return nil
}
