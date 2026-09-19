package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/snapshot"
)

func newStore(t *testing.T) (*snapshot.Store, string) {
	t.Helper()
	base := t.TempDir()
	s, err := snapshot.Open(filepath.Join(base, "snap"))
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return s, ws
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSaveRestore(t *testing.T) {
	s, ws := newStore(t)
	file := filepath.Join(ws, "a.txt")
	write(t, file, "v1", 0o640)

	e1, err := s.Save("run1", file)
	if err != nil {
		t.Fatal(err)
	}
	if e1.Seq != 1 || e1.Absent || e1.Blob == "" || e1.Mode != 0o640 || e1.Size != 2 || e1.Time.IsZero() {
		t.Errorf("entry = %+v", e1)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), "blobs", e1.Blob))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob mode = %o, want 0600", perm)
	}

	write(t, file, "v2", 0o640)
	e2, err := s.Save("run1", file)
	if err != nil {
		t.Fatal(err)
	}
	if e2.Seq != 2 || e2.Blob == e1.Blob {
		t.Errorf("second entry = %+v", e2)
	}
	write(t, file, "v3", 0o640)

	entries, err := s.Entries("run1")
	if err != nil || len(entries) != 2 {
		t.Fatalf("Entries = %v, %v", entries, err)
	}
	if err := s.Restore(entries[1]); err != nil {
		t.Fatal(err)
	}
	if got := read(t, file); got != "v2" {
		t.Errorf("after restoring seq 2: %q", got)
	}
	if err := s.Restore(entries[0]); err != nil {
		t.Fatal(err)
	}
	if got := read(t, file); got != "v1" {
		t.Errorf("after restoring seq 1: %q", got)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o640 {
		t.Errorf("mode not restored: %o", info.Mode().Perm())
	}
}

func TestAbsentAndRestoreRun(t *testing.T) {
	s, ws := newStore(t)
	created := filepath.Join(ws, "new", "file.go")
	existing := filepath.Join(ws, "old.go")
	write(t, existing, "orig", 0o644)

	e, err := s.Save("r", created)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Absent || e.Blob != "" {
		t.Errorf("absent entry = %+v", e)
	}
	if _, err := s.Save("r", existing); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, created, "made by tool", 0o644)
	write(t, existing, "edited", 0o644)
	// Same file touched again later in the run.
	if _, err := s.Save("r", existing); err != nil {
		t.Fatal(err)
	}
	write(t, existing, "edited twice", 0o644)

	done, err := s.RestoreRun("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 3 || done[0].Seq != 3 || done[2].Seq != 1 {
		t.Errorf("RestoreRun order = %+v", done)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Error("created file should be removed on undo")
	}
	if got := read(t, existing); got != "orig" {
		t.Errorf("existing = %q, want orig", got)
	}
	// Restoring an absent entry twice is fine.
	if err := s.Restore(e); err != nil {
		t.Errorf("second absent restore: %v", err)
	}
}

func TestSaveValidation(t *testing.T) {
	s, ws := newStore(t)
	if _, err := s.Save("", filepath.Join(ws, "x")); err == nil {
		t.Error("empty run id accepted")
	}
	if _, err := s.Save("../x", filepath.Join(ws, "x")); err == nil {
		t.Error("run id with separator accepted")
	}
	if _, err := s.Save("r", "relative.txt"); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := s.Save("r", ws); err == nil {
		t.Error("directory accepted")
	}
}

func TestRunsNewestFirst(t *testing.T) {
	s, ws := newStore(t)
	f := filepath.Join(ws, "f")
	write(t, f, "x", 0o644)
	for i, id := range []string{"20260101-a", "20260102-b", "20260103-c"} {
		if _, err := s.Save(id, f); err != nil {
			t.Fatal(err)
		}
		// Ensure distinct mtimes without sleeping in the common case.
		past := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(filepath.Join(s.Dir(), "runs", id+".jsonl"), past, past); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Runs()
	if strings.Join(got, ",") != "20260103-c,20260102-b,20260101-a" {
		t.Errorf("Runs = %v", got)
	}
	if s.Runs()[0] != "20260103-c" {
		t.Error("Runs not stable")
	}
	empty, _ := snapshot.Open(t.TempDir())
	if len(empty.Runs()) != 0 {
		t.Error("empty store has runs")
	}
}

func TestSeqContinuesAcrossReopen(t *testing.T) {
	s, ws := newStore(t)
	f := filepath.Join(ws, "f")
	write(t, f, "x", 0o644)
	if _, err := s.Save("r", f); err != nil {
		t.Fatal(err)
	}
	again, err := snapshot.Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := again.Save("r", f)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 2 {
		t.Errorf("Seq after reopen = %d, want 2", e.Seq)
	}
}

func TestPrune(t *testing.T) {
	s, ws := newStore(t)
	shared := filepath.Join(ws, "shared")
	write(t, shared, strings.Repeat("s", 100), 0o644)
	only := filepath.Join(ws, "only")
	write(t, only, strings.Repeat("o", 100), 0o644)

	// Old run references shared+only; new run references shared only.
	for _, f := range []string{shared, only} {
		if _, err := s.Save("old", f); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(s.Dir(), "runs", "old.jsonl"), past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("new", shared); err != nil {
		t.Fatal(err)
	}
	if s.Size() != 200 {
		t.Fatalf("Size = %d, want 200 (content-addressed)", s.Size())
	}
	if err := s.Prune(1000); err != nil {
		t.Fatal(err)
	}
	if len(s.Runs()) != 2 {
		t.Error("prune within budget removed runs")
	}
	if err := s.Prune(150); err != nil {
		t.Fatal(err)
	}
	if got := s.Runs(); len(got) != 1 || got[0] != "new" {
		t.Errorf("Runs after prune = %v", got)
	}
	if s.Size() != 100 {
		t.Errorf("Size after prune = %d, want 100 (shared blob kept)", s.Size())
	}
	if entries, _ := s.Entries("new"); len(entries) != 1 {
		t.Error("surviving run lost entries")
	}
	if err := s.Restore(mustEntry(t, s, "new")); err != nil {
		t.Errorf("restore after prune: %v", err)
	}
	if err := s.Prune(0); err != nil {
		t.Fatal(err)
	}
	if s.Size() != 0 || len(s.Runs()) != 0 {
		t.Error("prune to zero left data")
	}
}

func mustEntry(t *testing.T, s *snapshot.Store, run string) snapshot.Entry {
	t.Helper()
	entries, err := s.Entries(run)
	if err != nil || len(entries) == 0 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	return entries[0]
}
