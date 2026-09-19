package audit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/audit"
)

// anchoredLog writes n notice events through an anchored log and returns the
// log path and its anchor store.
func anchoredLog(t *testing.T, n int) (string, *audit.Anchors) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "log", "s.jsonl")
	anchors := audit.OpenAnchors(filepath.Join(dir, "anchors"))
	l, err := audit.OpenAnchored(path, nil, anchors)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := l.Write(audit.Event{Session: "s", Kind: audit.KindNotice, Text: strings.Repeat("a", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path, anchors
}

// rewrite replaces the log with the lines mutate returns.
func rewrite(t *testing.T, path string, mutate func([]string) []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := mutate(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyAnchoredDetectsTruncationAndForgery(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]string) []string
		want   error
		wantN  int
	}{
		{
			name:   "untouched",
			mutate: func(l []string) []string { return l },
			wantN:  3,
		},
		{
			// The chain is backwards, so dropping the tail leaves a log
			// that verifies perfectly on its own. Only the recorded head
			// knows how long it was.
			name:   "tail removed",
			mutate: func(l []string) []string { return l[:2] },
			want:   audit.ErrTruncated,
			wantN:  2,
		},
		{
			name:   "emptied",
			mutate: func([]string) []string { return nil },
			want:   audit.ErrTruncated,
			wantN:  0,
		},
		{
			name:   "line appended",
			mutate: func(l []string) []string { return append(l, l[len(l)-1]) },
			want:   audit.ErrTampered, // the copy does not chain
			wantN:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, anchors := anchoredLog(t, 3)
			rewrite(t, path, tt.mutate)
			n, err := audit.VerifyAnchored(path, anchors)
			if !errors.Is(err, tt.want) {
				t.Fatalf("VerifyAnchored = %d, %v; want %v", n, err, tt.want)
			}
			if n != tt.wantN {
				t.Errorf("events = %d, want %d", n, tt.wantN)
			}
			// The unanchored chain walk cannot tell a truncation apart from
			// a short log: that is exactly why the anchor exists.
			if tt.want == audit.ErrTruncated {
				if _, err := audit.Verify(path); err != nil {
					t.Errorf("Verify alone should still be happy: %v", err)
				}
			}
		})
	}
}

func TestVerifyAnchoredDetectsARecomputedChain(t *testing.T) {
	path, anchors := anchoredLog(t, 3)
	// The whole point: whoever can write the log can rewrite a line and
	// recompute every Prev after it, so the chain alone proves nothing.
	reforge(t, path, func(l []string) []string {
		l[2] = strings.Replace(l[2], `"aaa"`, `"bbb"`, 1)
		return l
	})
	if _, err := audit.Verify(path); err != nil {
		t.Fatalf("the re-forged chain should verify on its own: %v", err)
	}
	n, err := audit.VerifyAnchored(path, anchors)
	if !errors.Is(err, audit.ErrForged) {
		t.Fatalf("VerifyAnchored = %d, %v; want ErrForged", n, err)
	}
}

func TestVerifyAnchoredWithoutAnchor(t *testing.T) {
	path, _ := anchoredLog(t, 2)
	empty := audit.OpenAnchors(t.TempDir())
	n, err := audit.VerifyAnchored(path, empty)
	if !errors.Is(err, audit.ErrNoAnchor) || n != 2 {
		t.Fatalf("VerifyAnchored = %d, %v; want ErrNoAnchor with 2 events", n, err)
	}
	// Deleting the anchor is itself an attack step, so it must not read as
	// success: the caller has to see that nothing vouched for the log.
	if _, err := audit.VerifyAnchored(path, nil); !errors.Is(err, audit.ErrNoAnchor) {
		t.Errorf("a nil anchor store must report ErrNoAnchor, got %v", err)
	}
}

func TestAnchorFileIsPrivateAndOutsideTheLog(t *testing.T) {
	path, anchors := anchoredLog(t, 1)
	if strings.HasPrefix(anchors.Dir(), filepath.Dir(path)) {
		t.Errorf("anchors live at %s, inside the log directory %s", anchors.Dir(), filepath.Dir(path))
	}
	entries, err := os.ReadDir(anchors.Dir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("anchor directory = %v, %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("anchor mode = %v, want 0600", info.Mode().Perm())
	}
	head, ok := anchors.Head(path)
	if !ok || head.Seq != 1 || head.Hash == "" {
		t.Errorf("Head = %+v, %v", head, ok)
	}
	if err := anchors.Forget(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := anchors.Head(path); ok {
		t.Error("Forget left the anchor behind")
	}
}

// reforge rewrites the log and repairs the chain the way an attacker with
// write access would, so only the anchor can still tell.
func reforge(t *testing.T, path string, mutate func([]string) []string) {
	t.Helper()
	rewrite(t, path, mutate)
	// Re-chain by replaying the file through a fresh log.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []audit.Event
	for ev, err := range audit.Read(path) {
		if err != nil {
			t.Fatalf("reading %q: %v", raw, err)
		}
		events = append(events, ev)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	l, err := audit.Open(path, nil) // no anchors: this is the attacker's log
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		ev.Seq, ev.Prev = 0, ""
		if err := l.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}
