package tools_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

// searchTree lays out a small workspace with ignored, hidden and binary files.
func searchTree(t *testing.T, f *fixture) {
	t.Helper()
	f.write("main.go", "package main\n\nfunc main() { Hello() }\n")
	f.write("lib/hello.go", "package lib\n\n// Hello greets.\nfunc Hello() {}\n")
	f.write("lib/hello_test.go", "package lib\n\nfunc TestHello(t *T) { Hello() }\n")
	f.write("README.md", "# hello\n\nSay HELLO.\n")
	f.write("build/out.go", "package build // Hello ignored\n")
	f.write("secrets/key.go", "package secrets // Hello hidden\n")
	f.write("bin.dat", "Hello\x00binary")
	f.write(".gitignore", "build/\n")
	f.write(".wrightignore", "secrets/\n")
	// Make main.go the newest so mtime ordering is observable.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.root, "main.go"), future, future); err != nil {
		t.Fatal(err)
	}
}

func TestGlob(t *testing.T) {
	f := newFixture(t, nil)
	searchTree(t, f)
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		absent  []string
		first   string
		wantErr string
	}{
		{name: "all go", args: map[string]any{"pattern": "**/*.go"}, want: []string{"main.go", "lib/hello.go", "lib/hello_test.go"}, absent: []string{"build/out.go", "secrets/key.go"}, first: "main.go"},
		{name: "top level only", args: map[string]any{"pattern": "*.go"}, want: []string{"main.go"}, absent: []string{"lib/hello.go"}},
		{name: "subdir path", args: map[string]any{"pattern": "*_test.go", "path": "lib"}, want: []string{"lib/hello_test.go"}, absent: []string{"lib/hello.go\n"}},
		{name: "no match", args: map[string]any{"pattern": "**/*.rs"}, want: []string{"no files match"}},
		{name: "bad pattern", args: map[string]any{"pattern": "[["}, wantErr: "invalid pattern"},
		{name: "missing pattern", args: map[string]any{}, wantErr: "required"},
		{name: "path not dir", args: map[string]any{"pattern": "*", "path": "main.go"}, wantErr: "not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameGlob, jsonArgs(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
			if tt.first != "" && !strings.HasPrefix(got, tt.first) {
				t.Errorf("first line should be %q:\n%s", tt.first, got)
			}
		})
	}
}

func TestGrep(t *testing.T) {
	backends := []struct {
		name string
		norg bool
	}{{"go", true}, {"ripgrep", false}}
	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			if !be.norg {
				if _, err := exec.LookPath("rg"); err != nil {
					t.Skip("rg not installed")
				}
			}
			f := newFixture(t, func(d *tools.Deps) {
				d.NoRipgrep = be.norg
				d.Redactor = redact.New()
			})
			searchTree(t, f)
			f.write("tok.txt", "ghp_"+strings.Repeat("B", 36)+"\n")
			runGrepTable(t, f)
		})
	}
}

func runGrepTable(t *testing.T, f *fixture) {
	t.Helper()
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		absent  []string
		wantErr string
	}{
		{name: "files", args: map[string]any{"pattern": "Hello"}, want: []string{"main.go", "lib/hello.go", "lib/hello_test.go"}, absent: []string{"build/", "secrets/", "bin.dat", "README"}},
		{name: "case insensitive", args: map[string]any{"pattern": "hello", "case_insensitive": true}, want: []string{"README.md", "main.go"}},
		{name: "glob filter", args: map[string]any{"pattern": "Hello", "glob": "*_test.go"}, want: []string{"lib/hello_test.go"}, absent: []string{"lib/hello.go\n", "main.go"}},
		{name: "path scope", args: map[string]any{"pattern": "Hello", "path": "lib"}, want: []string{"lib/hello.go"}, absent: []string{"main.go"}},
		{name: "single file", args: map[string]any{"pattern": "Hello", "path": "main.go", "mode": "content"}, want: []string{"main.go:3:func main() { Hello() }"}},
		{name: "content with context", args: map[string]any{"pattern": "^func Hello", "mode": "content", "context": 1}, want: []string{"lib/hello.go-3-// Hello greets.", "lib/hello.go:4:func Hello() {}"}},
		{name: "count", args: map[string]any{"pattern": "Hello", "mode": "count", "path": "lib"}, want: []string{"lib/hello.go:2", "lib/hello_test.go:1"}},
		{name: "limit", args: map[string]any{"pattern": "Hello", "mode": "content", "limit": 1}, want: []string{"[truncated: showing 1 matches"}},
		{name: "redacted", args: map[string]any{"pattern": "ghp_", "mode": "content"}, want: []string{"[redacted: github"}, absent: []string{"BBBBBBBB"}},
		{name: "no match", args: map[string]any{"pattern": "zzzz"}, want: []string{"no matches for zzzz"}},
		{name: "bad regexp", args: map[string]any{"pattern": "("}, wantErr: "invalid pattern"},
		{name: "bad mode", args: map[string]any{"pattern": "x", "mode": "lines"}, wantErr: "mode must be"},
		{name: "empty pattern", args: map[string]any{}, wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameGrep, jsonArgs(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
}

func TestListDir(t *testing.T) {
	f := newFixture(t, nil)
	searchTree(t, f)
	tests := []struct {
		name   string
		args   map[string]any
		want   []string
		absent []string
	}{
		{name: "depth 1", args: map[string]any{}, want: []string{"  lib/\n", "\n  main.go"}, absent: []string{"hello.go", "build/", "secrets/"}},
		{name: "depth 2", args: map[string]any{"depth": 2}, want: []string{"  lib/\n    hello.go\n    hello_test.go\n"}},
		{name: "subdir", args: map[string]any{"path": "lib"}, want: []string{"lib/\n  hello.go\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameListDir, jsonArgs(tt.args))
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
	// Directories come before files regardless of name.
	got, _ := f.text(tools.NameListDir, `{}`)
	if strings.Index(got, "lib/") > strings.Index(got, "README.md") {
		t.Errorf("dirs should precede files:\n%s", got)
	}
	if _, err := f.text(tools.NameListDir, `{"path":"missing"}`); err == nil {
		t.Error("expected error for a missing directory")
	}
}
