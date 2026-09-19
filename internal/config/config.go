// Package config loads wright's layered settings: embedded builtin defaults <
// user config < project settings < project-local settings < environment
// overrides. Later layers win for scalars; permission rule lists accumulate
// (append + dedupe) so a project can only ever add rules, never silently drop
// a user's deny.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

//go:embed defaults.json
var defaultsJSON []byte

// Settings is the full settings document. Every field is optional in JSON;
// unset scalars are zero and are overridden by any non-zero value in a later
// layer. Booleans that default to true are pointers so a later layer can turn
// them off explicitly.
type Settings struct {
	Model        Model                `json:"model,omitzero"`
	Permissions  Permissions          `json:"permissions,omitzero"`
	Sandbox      Sandbox              `json:"sandbox,omitzero"`
	MCPServers   map[string]MCPServer `json:"mcpServers,omitempty"`
	Skills       Skills               `json:"skills,omitzero"`
	Git          Git                  `json:"git,omitzero"`
	Redaction    *bool                `json:"redaction,omitempty"`
	Instructions Instructions         `json:"instructions,omitzero"`
	Updates      Updates              `json:"updates,omitzero"`
}

// Model selects the primary and fast models and generation limits.
type Model struct {
	Default         string `json:"default,omitempty"`
	Fast            string `json:"fast,omitempty"`
	Reasoning       string `json:"reasoning,omitempty"`
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
	ContextWindow   int    `json:"contextWindow,omitempty"`
}

// Permissions holds the permission mode and the three rule lists.
type Permissions struct {
	Mode           string   `json:"mode,omitempty"`
	Allow          []string `json:"allow,omitempty"`
	Ask            []string `json:"ask,omitempty"`
	Deny           []string `json:"deny,omitempty"`
	AdditionalDirs []string `json:"additionalDirectories,omitempty"`
}

// Sandbox configures the OS sandbox backend and its extra mounts.
type Sandbox struct {
	Backend      string   `json:"backend,omitempty"`
	AllowNetwork bool     `json:"allowNetwork,omitempty"`
	ExtraRO      []string `json:"extraReadOnly,omitempty"`
	ExtraRW      []string `json:"extraReadWrite,omitempty"`
	PassEnv      []string `json:"passEnv,omitempty"`
}

// MCPServer describes one MCP server: a stdio command or an HTTP URL.
type MCPServer struct {
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
}

// Skills configures where skills are discovered.
type Skills struct {
	ExtraDirs        []string `json:"extraDirs,omitempty"`
	LoadClaudeSkills bool     `json:"loadClaudeSkills,omitempty"`
}

// Git configures commit attribution and branch protection.
type Git struct {
	Attribution       *bool    `json:"attribution,omitempty"`
	Trailer           string   `json:"trailer,omitempty"`
	ProtectedBranches []string `json:"protectedBranches,omitempty"`
}

// Instructions lists project instruction files and the CLAUDE.md fallback
// policy ("ask", "always" or "never").
type Instructions struct {
	Files    []string `json:"files,omitempty"`
	Fallback string   `json:"fallback,omitempty"`
}

// Updates controls the (opt-in, default off) release check.
type Updates struct {
	Check bool `json:"check,omitempty"`
}

// RedactionEnabled reports the effective redaction switch (default on).
func (s Settings) RedactionEnabled() bool { return s.Redaction == nil || *s.Redaction }

// AttributionEnabled reports whether commits get the trailer (default on).
func (s Settings) AttributionEnabled() bool { return s.Git.Attribution == nil || *s.Git.Attribution }

// Layer names a settings source in precedence order.
type Layer struct {
	Name    string // "builtin", "user", "project", "project.local", "env"
	Path    string // file path, empty for builtin/env
	Present bool
}

// Layered is the result of Load: the merged Settings plus provenance.
type Layered struct {
	Settings Settings
	Paths    Paths
	Layers   []Layer
	// Project and ProjectLocal are the raw project layers, kept separately so
	// the trust prompt can show exactly what a project asks for before its
	// allow/additionalDirectories/passEnv/mcpServers take effect.
	Project      Settings
	ProjectLocal Settings
}

// Defaults returns the embedded builtin settings.
func Defaults() Settings {
	var s Settings
	// The embedded file is validated by tests; a decode failure here is a
	// build defect, not a runtime condition.
	if err := json.Unmarshal(defaultsJSON, &s); err != nil {
		panic("config: embedded defaults.json invalid: " + err.Error())
	}
	return s
}

// Load builds the layered settings for a workspace rooted at cwd's project
// directory. Missing files are not errors; malformed JSON is.
func Load(cwd string) (*Layered, error) {
	paths := DefaultPaths(cwd)
	l := &Layered{Settings: Defaults(), Paths: paths}
	l.Layers = append(l.Layers, Layer{Name: "builtin"})

	steps := []struct {
		name string
		path string
		keep *Settings
	}{
		{"user", paths.UserConfigFile(), nil},
		{"project", paths.ProjectSettingsFile(), &l.Project},
		{"project.local", paths.ProjectLocalFile(), &l.ProjectLocal},
	}
	for _, st := range steps {
		s, present, err := readFile(st.path)
		if err != nil {
			return nil, err
		}
		l.Layers = append(l.Layers, Layer{Name: st.name, Path: st.path, Present: present})
		if !present {
			continue
		}
		if st.keep != nil {
			*st.keep = s
		}
		Merge(&l.Settings, s)
	}
	applyEnv(&l.Settings)
	l.Layers = append(l.Layers, Layer{Name: "env"})
	return l, nil
}

// applyEnv applies the WRIGHT_* overrides, which always win over files.
func applyEnv(s *Settings) {
	if v := os.Getenv("WRIGHT_MODEL"); v != "" {
		s.Model.Default = v
	}
	if v := os.Getenv("WRIGHT_MODE"); v != "" {
		s.Permissions.Mode = v
	}
	if v := os.Getenv("WRIGHT_SANDBOX"); v != "" {
		s.Sandbox.Backend = v
	}
}

// readFile decodes one settings file; present=false when it does not exist.
func readFile(path string) (Settings, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("config: read %s: %w", path, err)
	}
	s, err := Decode(data)
	if err != nil {
		return Settings{}, true, fmt.Errorf("config: %s: %w", path, err)
	}
	return s, true, nil
}

// Decode parses a settings document. Unknown fields are rejected so typos in
// a permission key fail loudly instead of silently granting nothing.
func Decode(data []byte) (Settings, error) {
	var s Settings
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// Merge overlays src onto dst: non-zero scalars replace, rule lists and other
// slices append-dedupe, maps merge by key.
func Merge(dst *Settings, src Settings) {
	mergeModel(&dst.Model, src.Model)
	mergePermissions(&dst.Permissions, src.Permissions)
	mergeSandbox(&dst.Sandbox, src.Sandbox)
	if src.MCPServers != nil {
		if dst.MCPServers == nil {
			dst.MCPServers = map[string]MCPServer{}
		}
		maps.Copy(dst.MCPServers, src.MCPServers)
	}
	dst.Skills.ExtraDirs = appendDedupe(dst.Skills.ExtraDirs, src.Skills.ExtraDirs)
	dst.Skills.LoadClaudeSkills = dst.Skills.LoadClaudeSkills || src.Skills.LoadClaudeSkills
	if src.Git.Attribution != nil {
		dst.Git.Attribution = src.Git.Attribution
	}
	setIf(&dst.Git.Trailer, src.Git.Trailer)
	dst.Git.ProtectedBranches = appendDedupe(dst.Git.ProtectedBranches, src.Git.ProtectedBranches)
	if src.Redaction != nil {
		dst.Redaction = src.Redaction
	}
	dst.Instructions.Files = appendDedupe(dst.Instructions.Files, src.Instructions.Files)
	setIf(&dst.Instructions.Fallback, src.Instructions.Fallback)
	dst.Updates.Check = dst.Updates.Check || src.Updates.Check
}

func mergeModel(dst *Model, src Model) {
	setIf(&dst.Default, src.Default)
	setIf(&dst.Fast, src.Fast)
	setIf(&dst.Reasoning, src.Reasoning)
	if src.MaxOutputTokens != 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if src.ContextWindow != 0 {
		dst.ContextWindow = src.ContextWindow
	}
}

func mergePermissions(dst *Permissions, src Permissions) {
	setIf(&dst.Mode, src.Mode)
	dst.Allow = appendDedupe(dst.Allow, src.Allow)
	dst.Ask = appendDedupe(dst.Ask, src.Ask)
	dst.Deny = appendDedupe(dst.Deny, src.Deny)
	dst.AdditionalDirs = appendDedupe(dst.AdditionalDirs, src.AdditionalDirs)
}

func mergeSandbox(dst *Sandbox, src Sandbox) {
	setIf(&dst.Backend, src.Backend)
	dst.AllowNetwork = dst.AllowNetwork || src.AllowNetwork
	dst.ExtraRO = appendDedupe(dst.ExtraRO, src.ExtraRO)
	dst.ExtraRW = appendDedupe(dst.ExtraRW, src.ExtraRW)
	dst.PassEnv = appendDedupe(dst.PassEnv, src.PassEnv)
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// appendDedupe appends the items of add not already in base, preserving order.
func appendDedupe(base, add []string) []string {
	out := slices.Clone(base)
	for _, a := range add {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// Paths are the resolved filesystem locations wright reads and writes.
type Paths struct {
	UserConfig string // $WRIGHT_CONFIG_DIR or $XDG_CONFIG_HOME/wright
	UserData   string // $WRIGHT_DATA_DIR or $XDG_DATA_HOME/wright
	UserCache  string // $XDG_CACHE_HOME/wright
	ProjectDir string // <workspace>/.wright
}

// UserConfigFile is the user-level settings file.
func (p Paths) UserConfigFile() string { return filepath.Join(p.UserConfig, "config.json") }

// TrustFile is where accepted project/MCP hashes are stored.
func (p Paths) TrustFile() string { return filepath.Join(p.UserConfig, "trust.json") }

// ProjectSettingsFile is the shared (committed) project settings file.
func (p Paths) ProjectSettingsFile() string { return filepath.Join(p.ProjectDir, "settings.json") }

// ProjectLocalFile is the per-developer (gitignored) project settings file.
func (p Paths) ProjectLocalFile() string { return filepath.Join(p.ProjectDir, "settings.local.json") }

// DefaultPaths resolves Paths from the environment for a workspace at root.
func DefaultPaths(root string) Paths {
	home, _ := os.UserHomeDir()
	return Paths{
		UserConfig: firstDir(os.Getenv("WRIGHT_CONFIG_DIR"), xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"))),
		UserData:   firstDir(os.Getenv("WRIGHT_DATA_DIR"), xdg("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))),
		UserCache:  xdg("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
		ProjectDir: filepath.Join(root, ".wright"),
	}
}

// firstDir returns override when set, else base/wright.
func firstDir(override, base string) string {
	if override != "" {
		return override
	}
	return filepath.Join(base, "wright")
}

// xdg returns the XDG variable's value or the fallback.
func xdg(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// SaveProjectLocal loads .wright/settings.local.json (or starts empty),
// applies mutate and writes it back atomically with mode 0600.
func (l *Layered) SaveProjectLocal(mutate func(*Settings)) error {
	return saveFile(l.Paths.ProjectLocalFile(), mutate)
}

// SaveUser does the same for the user config file.
func (l *Layered) SaveUser(mutate func(*Settings)) error {
	return saveFile(l.Paths.UserConfigFile(), mutate)
}

func saveFile(path string, mutate func(*Settings)) error {
	current, _, err := readFile(path)
	if err != nil {
		return err
	}
	mutate(&current)
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(data, '\n'), 0o600)
}

// WriteAtomic writes data to path via a same-directory temp file and rename,
// so readers never observe a torn file and the mode is set before publish.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
