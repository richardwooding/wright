package trust_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/trust"
)

func newStore(t *testing.T) *trust.Store {
	t.Helper()
	return trust.Open(filepath.Join(t.TempDir(), "cfg", "trust.json"))
}

func TestProjects(t *testing.T) {
	s := newStore(t)
	if _, ok := s.Project("/w"); ok {
		t.Fatal("empty store has a project")
	}
	if err := s.AcceptProject("/w", "h1"); err != nil {
		t.Fatal(err)
	}
	r, ok := s.Project("/w")
	if !ok || r.Root != "/w" || r.SettingsHash != "h1" || r.Accepted.IsZero() {
		t.Errorf("Project = %+v, %v", r, ok)
	}
	if !s.ProjectTrusted("/w", "h1") || s.ProjectTrusted("/w", "h2") || s.ProjectTrusted("/other", "h1") {
		t.Error("ProjectTrusted mismatch")
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	// Re-accepting with a new hash replaces the record.
	if err := s.AcceptProject("/w", "h2"); err != nil {
		t.Fatal(err)
	}
	if !s.ProjectTrusted("/w", "h2") || s.ProjectTrusted("/w", "h1") {
		t.Error("re-accept did not replace hash")
	}
	if err := s.ForgetProject("/w"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Project("/w"); ok {
		t.Error("ForgetProject left the record")
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path()))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".trust-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestProjectSpellingDoesNotDecideTrust pins that a project accepted through
// one spelling of its path is recognised through the other. On macOS a
// project under /var/folders is accepted as /var/... and looked up as
// /private/var/... — which left every accepted project untrusted, so its
// settings never applied. The two spellings are built here with a symlinked
// directory, so the test means the same thing on Linux.
func TestProjectSpellingDoesNotDecideTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base := t.TempDir()
	linked := filepath.Join(base, "link")
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	if resolved == linked {
		t.Fatalf("the two spellings must differ, got %s for both", linked)
	}
	tests := []struct {
		name         string
		accept, look string
	}{
		{name: "accepted through the link, looked up resolved", accept: linked, look: resolved},
		{name: "accepted resolved, looked up through the link", accept: resolved, look: linked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			if err := s.AcceptProject(tt.accept, "h"); err != nil {
				t.Fatal(err)
			}
			if !s.ProjectTrusted(tt.look, "h") {
				t.Errorf("AcceptProject(%s) then ProjectTrusted(%s) = false", tt.accept, tt.look)
			}
			if s.ProjectTrusted(tt.look, "other") {
				t.Error("a different settings hash must not be trusted")
			}
			// Forgetting through either spelling must remove it too, or a
			// project could be untrusted and still recognised.
			if err := s.ForgetProject(tt.look); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Project(tt.accept); ok {
				t.Errorf("ForgetProject(%s) left the record for %s", tt.look, tt.accept)
			}
		})
	}
}

func TestServers(t *testing.T) {
	s := newStore(t)
	rec := trust.ServerRecord{Name: "github", Transport: "stdio", CommandSHA256: trust.HashStrings("npx"), ArgsSHA256: trust.HashStrings("-y", "@x/server"), ToolsSHA256: trust.HashStrings("a", "b")}
	if err := s.AcceptServer(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptServer(trust.ServerRecord{}); err == nil {
		t.Error("nameless server accepted")
	}
	got, ok := s.Server("github")
	if !ok || !got.Matches(rec) || got.Accepted.IsZero() {
		t.Errorf("Server = %+v, %v", got, ok)
	}
	changed := rec
	changed.ToolsSHA256 = trust.HashStrings("a", "b", "c")
	if got.Matches(changed) {
		t.Error("changed tool list should not match")
	}
	if err := s.AcceptServer(trust.ServerRecord{Name: "api", Transport: "http", URL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	if names := s.Servers(); strings.Join(names, ",") != "api,github" {
		t.Errorf("Servers = %v", names)
	}
	if err := s.ForgetServer("github"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Server("github"); ok {
		t.Error("ForgetServer left the record")
	}
	// Projects and servers share the file without clobbering each other.
	if err := s.AcceptProject("/w", "h"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Server("api"); !ok {
		t.Error("project write lost server record")
	}
}

func TestCorruptFile(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Project("/w"); ok {
		t.Error("corrupt file should trust nothing")
	}
	if err := s.AcceptProject("/w", "h"); err == nil {
		t.Error("writing over a corrupt file should fail rather than discard it")
	}
}

func TestHashes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	empty, err := trust.HashFile(path)
	if err != nil || empty != "" {
		t.Errorf("missing file hash = %q, %v", empty, err)
	}
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := trust.HashFile(path)
	if err != nil || h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashFile = %q, %v", h, err)
	}
	if trust.HashStrings("ab", "c") == trust.HashStrings("a", "bc") {
		t.Error("HashStrings must be length-prefixed")
	}
	if trust.HashStrings() == trust.HashStrings("") {
		t.Error("empty sequence and one empty string should differ")
	}
	first := trust.HashStrings("x", "y")
	if got := trust.HashStrings("x", "y"); got != first || len(got) != 64 {
		t.Errorf("HashStrings not deterministic hex sha256: %q vs %q", first, got)
	}
}
