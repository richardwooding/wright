package diffview_test

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/diffview"
)

const sample = `diff --git a/x.go b/x.go
index 1234..5678 100644
--- a/x.go
+++ b/x.go
@@ -1,3 +1,4 @@
 package x
-old line
+new line
+another
`

func TestLinesKeepPrefixes(t *testing.T) {
	th := theme.New(true)
	lines := diffview.Lines(sample, th, 0)
	raw := strings.Split(strings.TrimRight(sample, "\n"), "\n")
	if len(lines) != len(raw) {
		t.Fatalf("got %d lines, want %d", len(lines), len(raw))
	}
	for i, line := range lines {
		if got := ansi.Strip(line); got != raw[i] {
			t.Errorf("line %d: stripped %q, want %q", i, got, raw[i])
		}
	}
}

func TestLinesTruncateToWidth(t *testing.T) {
	th := theme.New(false)
	long := "+" + strings.Repeat("x", 100) + "\n-" + strings.Repeat("y", 100)
	for _, w := range []int{10, 40} {
		for _, line := range diffview.Lines(long, th, w) {
			if got := lipgloss.Width(line); got > w {
				t.Errorf("width %d: line is %d wide", w, got)
			}
			s := ansi.Strip(line)
			if s[0] != '+' && s[0] != '-' {
				t.Errorf("width %d: prefix lost in %q", w, s)
			}
		}
	}
}

func TestStats(t *testing.T) {
	add, del := diffview.Stats(sample)
	if add != 2 || del != 1 {
		t.Fatalf("Stats = +%d -%d, want +2 -1", add, del)
	}
}

func TestIsDiff(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{sample, true},
		{"--- a\n+++ b\n", true},
		{"@@ -1 +1 @@\n", true},
		{"hello world", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := diffview.IsDiff(tt.in); got != tt.want {
			t.Errorf("IsDiff(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestEmptyDiff(t *testing.T) {
	if got := diffview.Lines("", theme.New(true), 80); got != nil {
		t.Fatalf("empty diff produced %v", got)
	}
	if got := diffview.Render("", theme.New(true), 80); got != "" {
		t.Fatalf("empty render produced %q", got)
	}
}
