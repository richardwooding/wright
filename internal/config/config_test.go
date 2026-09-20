package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
)

func TestDefaults(t *testing.T) {
	d := config.Defaults()
	if d.Permissions.Mode != "default" {
		t.Errorf("mode = %q, want default", d.Permissions.Mode)
	}
	if d.Sandbox.Backend != "auto" {
		t.Errorf("sandbox backend = %q, want auto", d.Sandbox.Backend)
	}
	if !d.RedactionEnabled() {
		t.Error("redaction should default on")
	}
	if d.Updates.Check {
		t.Error("update check must default off (no phone-home)")
	}
	if want := "Co-Authored-By: wright <wright@richardwooding.github.io>"; d.Git.Trailer != want {
		t.Errorf("trailer = %q", d.Git.Trailer)
	}
	if !slices.Contains(d.Permissions.Allow, "bash(go test *)") {
		t.Error("builtin allow list missing bash(go test *)")
	}
	if !slices.Contains(d.Permissions.Deny, "bash(sudo *)") {
		t.Error("builtin deny list missing bash(sudo *)")
	}
	if !slices.Contains(d.Permissions.Ask, "web_fetch") {
		t.Error("builtin ask list missing web_fetch")
	}
}

func TestMerge(t *testing.T) {
	f := false
	tests := []struct {
		name string
		dst  config.Settings
		src  config.Settings
		want func(t *testing.T, got config.Settings)
	}{
		{
			name: "scalar overrides when non-zero",
			dst:  config.Settings{Model: config.Model{Default: "a", Fast: "f"}},
			src:  config.Settings{Model: config.Model{Default: "b"}},
			want: func(t *testing.T, got config.Settings) {
				if got.Model.Default != "b" || got.Model.Fast != "f" {
					t.Errorf("model = %+v", got.Model)
				}
			},
		},
		{
			name: "rule lists append and dedupe",
			dst:  config.Settings{Permissions: config.Permissions{Allow: []string{"a", "b"}, Deny: []string{"x"}}},
			src:  config.Settings{Permissions: config.Permissions{Allow: []string{"b", "c"}, Deny: []string{"x", "y"}}},
			want: func(t *testing.T, got config.Settings) {
				if !slices.Equal(got.Permissions.Allow, []string{"a", "b", "c"}) {
					t.Errorf("allow = %v", got.Permissions.Allow)
				}
				if !slices.Equal(got.Permissions.Deny, []string{"x", "y"}) {
					t.Errorf("deny = %v", got.Permissions.Deny)
				}
			},
		},
		{
			name: "pointer bools can turn defaults off",
			dst:  config.Defaults(),
			src:  config.Settings{Redaction: &f, Git: config.Git{Attribution: &f}},
			want: func(t *testing.T, got config.Settings) {
				if got.RedactionEnabled() || got.AttributionEnabled() {
					t.Error("expected redaction and attribution off")
				}
			},
		},
		{
			name: "mcp servers merge by key",
			dst:  config.Settings{MCPServers: map[string]config.MCPServer{"a": {Command: "a"}}},
			src:  config.Settings{MCPServers: map[string]config.MCPServer{"b": {URL: "http://b"}}},
			want: func(t *testing.T, got config.Settings) {
				if len(got.MCPServers) != 2 || got.MCPServers["b"].URL != "http://b" {
					t.Errorf("mcp = %+v", got.MCPServers)
				}
			},
		},
		{
			name: "empty src is a no-op",
			dst:  config.Settings{Sandbox: config.Sandbox{Backend: "bwrap", AllowNetwork: true}},
			src:  config.Settings{},
			want: func(t *testing.T, got config.Settings) {
				if got.Sandbox.Backend != "bwrap" || !got.Sandbox.AllowNetwork {
					t.Errorf("sandbox = %+v", got.Sandbox)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := tt.dst
			config.Merge(&dst, tt.src)
			tt.want(t, dst)
		})
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	if _, err := config.Decode([]byte(`{"permisions": {}}`)); err == nil {
		t.Fatal("expected error for misspelled key")
	}
	if _, err := config.Decode([]byte(`{"permissions": {"allow": ["x"]}}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// setupLayers points every layer at a temp dir and returns the workspace root.
func setupLayers(t *testing.T) (root, userDir string) {
	t.Helper()
	base := t.TempDir()
	userDir = filepath.Join(base, "cfg")
	root = filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(root, ".wright"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WRIGHT_CONFIG_DIR", userDir)
	t.Setenv("WRIGHT_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	t.Setenv("WRIGHT_MODEL", "")
	t.Setenv("WRIGHT_MODE", "")
	t.Setenv("WRIGHT_SANDBOX", "")
	return root, userDir
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

func TestLoadLayering(t *testing.T) {
	root, userDir := setupLayers(t)
	write(t, filepath.Join(userDir, "config.json"), `{"model":{"default":"user-model"},"permissions":{"allow":["bash(make *)"]}}`)
	write(t, filepath.Join(root, ".wright", "settings.json"), `{"model":{"default":"project-model"},"permissions":{"deny":["bash(rm *)"]}}`)
	write(t, filepath.Join(root, ".wright", "settings.local.json"), `{"permissions":{"mode":"auto-edit","allow":["bash(make *)","bash(npm test *)"]}}`)

	l, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	s := l.Settings
	if s.Model.Default != "project-model" {
		t.Errorf("model = %q, want project layer to win over user", s.Model.Default)
	}
	if s.Permissions.Mode != "auto-edit" {
		t.Errorf("mode = %q", s.Permissions.Mode)
	}
	if n := countOf(s.Permissions.Allow, "bash(make *)"); n != 1 {
		t.Errorf("bash(make *) appears %d times, want 1 (dedupe)", n)
	}
	if !slices.Contains(s.Permissions.Allow, "bash(npm test *)") || !slices.Contains(s.Permissions.Deny, "bash(rm *)") {
		t.Errorf("rules not merged: allow=%v deny=%v", s.Permissions.Allow, s.Permissions.Deny)
	}
	// Builtins survive underneath.
	if !slices.Contains(s.Permissions.Allow, "bash(go test *)") {
		t.Error("builtin allow lost during merge")
	}
	if l.Project.Model.Default != "project-model" || l.ProjectLocal.Permissions.Mode != "auto-edit" {
		t.Error("raw project layers not captured")
	}
	names := make([]string, 0, len(l.Layers))
	for _, layer := range l.Layers {
		names = append(names, layer.Name)
	}
	if !slices.Equal(names, []string{"builtin", "user", "project", "project.local", "env"}) {
		t.Errorf("layers = %v", names)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func TestLoadEnvOverrides(t *testing.T) {
	root, _ := setupLayers(t)
	t.Setenv("WRIGHT_MODEL", "env-model")
	t.Setenv("WRIGHT_MODE", "plan")
	t.Setenv("WRIGHT_SANDBOX", "none")
	l, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if l.Settings.Model.Default != "env-model" || l.Settings.Permissions.Mode != "plan" || l.Settings.Sandbox.Backend != "none" {
		t.Errorf("env overrides not applied: %+v", l.Settings)
	}
}

// TestLoadMalformedFile pins who a broken settings file may stop. The user's
// own config is theirs to fix, so it is fatal. A project file arrived with
// the repository: making it fatal would let any checkout stop wright from
// starting in that directory, which is the denial of service the trust gate
// closes for the file's *contents*. It is recorded and dropped instead.
func TestLoadMalformedFile(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		body  string
		layer string
		fatal bool
		want  string
	}{
		{name: "project settings", file: ".wright/settings.json", body: "{not json", layer: "project", want: "invalid JSON at line 1"},
		{name: "project local", file: ".wright/settings.local.json", body: "", layer: "project.local", want: "the file is empty"},
		{name: "truncated project file", file: ".wright/settings.json", body: "{\n  \"model\": {\n", layer: "project", want: "ends in the middle of a value at line 3"},
		{name: "wrong type", file: ".wright/settings.json", body: `{"model": {"default": 7}}`, layer: "project", want: "line 1"},
		{name: "user config", file: "config.json", body: "{not json", fatal: true, want: "invalid JSON at line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, home := setupLayers(t)
			path := filepath.Join(root, filepath.FromSlash(tt.file))
			if tt.fatal {
				path = filepath.Join(home, filepath.FromSlash(tt.file))
			}
			write(t, path, tt.body)

			l, err := config.Load(root)
			if tt.fatal {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("user config error = %v, want one mentioning %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("a broken project layer must not stop the load: %v", err)
			}
			var got error
			for _, layer := range l.Layers {
				if layer.Name == tt.layer {
					got = layer.Err
				}
			}
			if got == nil {
				t.Fatalf("layer %s carries no error; layers = %+v", tt.layer, l.Layers)
			}
			if !strings.Contains(got.Error(), tt.want) {
				t.Errorf("error = %v, want one mentioning %q", got, tt.want)
			}
			if !strings.Contains(got.Error(), filepath.Base(tt.file)) {
				t.Errorf("error = %v, want it to name the file", got)
			}
			if len(l.Project.Permissions.Allow) != 0 || len(l.ProjectLocal.Permissions.Allow) != 0 {
				t.Error("a layer that failed to parse must contribute nothing")
			}
		})
	}
}

func TestSaveProjectLocalAtomic0600(t *testing.T) {
	root, _ := setupLayers(t)
	l, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SaveProjectLocal(func(s *config.Settings) {
		s.Permissions.Allow = append(s.Permissions.Allow, "bash(go test *)")
	}); err != nil {
		t.Fatal(err)
	}
	path := l.Paths.ProjectLocalFile()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	// A second save must read the existing file back rather than clobber it.
	if err := l.SaveProjectLocal(func(s *config.Settings) { s.Model.Default = "m" }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got config.Settings
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model.Default != "m" || !slices.Contains(got.Permissions.Allow, "bash(go test *)") {
		t.Errorf("saved = %s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".settings.local.json.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSaveUserCreatesDir(t *testing.T) {
	root, userDir := setupLayers(t)
	l, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SaveUser(func(s *config.Settings) { s.Model.Fast = "fast" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"permissions":{"allow":["bash(go test *)"]}}`))
	f.Add([]byte(`{"redaction":false,"updates":{"check":true}}`))
	f.Add([]byte(`{"mcpServers":{"a":{"command":"x","args":["1"]}}}`))
	f.Add([]byte(`[1,2,3]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := config.Decode(data)
		if err != nil {
			return
		}
		// Anything that decodes must merge onto the defaults without panicking
		// and re-encode to valid JSON.
		d := config.Defaults()
		config.Merge(&d, s)
		if _, err := json.Marshal(d); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	})
}

// TestMergeCarriesEveryField builds a Settings in which every leaf is non-zero,
// merges it onto an empty one, and reports any leaf that did not survive.
//
// It exists because Merge is a hand-written field list: a field added to
// Settings is silently ignored by every layer above the embedded defaults until
// someone remembers to add a line here too. That is not a visible failure — the
// setting simply never takes effect — so it needs a test that cannot be
// forgotten rather than one row per field.
func TestMergeCarriesEveryField(t *testing.T) {
	var src config.Settings
	fill(reflect.ValueOf(&src).Elem())

	var dst config.Settings
	config.Merge(&dst, src)

	for _, p := range zeroLeaves(reflect.ValueOf(dst), "") {
		t.Errorf("config.Merge did not carry %s", p)
	}
}

// fill sets every leaf of v to a non-zero value.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e)
		v.Set(reflect.Append(v, e))
	case reflect.Map:
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k)
		fill(e)
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Float64:
		v.SetFloat(1)
	}
}

// zeroLeaves returns the dotted path of every leaf of v that is still zero.
func zeroLeaves(v reflect.Value, path string) []string {
	switch v.Kind() {
	case reflect.Struct:
		var out []string
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				out = append(out, zeroLeaves(v.Field(i), path+"."+f.Name)...)
			}
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return []string{path}
		}
		return zeroLeaves(v.Elem(), path)
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			return []string{path}
		}
		return nil
	default:
		if v.IsZero() {
			return []string{path}
		}
		return nil
	}
}
