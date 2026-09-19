package markdown_test

import (
	"errors"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/richardwooding/wright/internal/tui/markdown"
)

const sample = "# Title\n\nSome **bold** text and `code`.\n\n- one\n- two\n\n```go\nfunc main() {}\n```\n"

func TestRenderKeepsContentWithinWidth(t *testing.T) {
	r := markdown.New()
	for _, width := range []int{40, 80, 200} {
		for _, dark := range []bool{true, false} {
			out := r.Render(sample, width, dark)
			if !strings.Contains(out, "Title") || !strings.Contains(out, "bold") {
				t.Fatalf("width %d dark %v: content missing from %q", width, dark, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width %d dark %v: line %q is %d wide", width, dark, line, w)
				}
			}
		}
	}
}

func TestRenderFallsBackWhenBackendFails(t *testing.T) {
	r := markdown.New(markdown.WithBackend(func(string, int, bool) (string, error) {
		return "", errors.New("boom")
	}))
	long := strings.Repeat("word ", 30)
	out := r.Render(long, 40, true)
	if !strings.Contains(out, "word") {
		t.Fatalf("fallback dropped the text: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Errorf("fallback line %q is %d wide", line, w)
		}
	}
}

func TestRenderFallsBackOnEmptyOutput(t *testing.T) {
	r := markdown.New(markdown.WithBackend(func(string, int, bool) (string, error) { return "", nil }))
	if out := r.Render("hello", 20, false); !strings.Contains(out, "hello") {
		t.Fatalf("got %q", out)
	}
}

func TestBackendReceivesWidthAndTheme(t *testing.T) {
	var gotWidth int
	var gotDark bool
	r := markdown.New(markdown.WithBackend(func(md string, width int, isDark bool) (string, error) {
		gotWidth, gotDark = width, isDark
		return md, nil
	}))
	r.Render("x", 33, true)
	if gotWidth != 33 || !gotDark {
		t.Fatalf("backend got width=%d dark=%v", gotWidth, gotDark)
	}
}

func TestPlainWraps(t *testing.T) {
	out := markdown.Plain(strings.Repeat("abcdefghij ", 10), 20)
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 20 {
			t.Errorf("line %q longer than 20", line)
		}
	}
}

func TestStyleHasNoMargin(t *testing.T) {
	for _, dark := range []bool{true, false} {
		st := markdown.Style(dark)
		if st.Document.Margin == nil || *st.Document.Margin != 0 {
			t.Errorf("dark=%v: document margin not zeroed", dark)
		}
	}
}
