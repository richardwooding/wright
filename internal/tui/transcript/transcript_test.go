package transcript_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/markdown"
	"github.com/richardwooding/wright/internal/tui/transcript"
)

const diff = "--- a/x.go\n+++ b/x.go\n@@ -1 +1,2 @@\n-old\n+new\n+more\n"

func joined(lines []string) string { return ansi.Strip(strings.Join(lines, "\n")) }

func sample(md *markdown.Renderer) *transcript.Model {
	m := transcript.New(theme.New(true), md)
	m.Append(&transcript.User{Text: "hello there"})
	m.Append(&transcript.Assistant{Text: "# Heading\n\nSome **answer** text.", Reasoning: "thinking hard\nabout it"})
	m.Append(&transcript.ToolCard{
		ID: "c1", Name: "edit_file", Args: json.RawMessage(`{"path":"internal/x.go","content":"x"}`),
		Diff: diff, Status: transcript.StatusOK, Duration: 340 * time.Millisecond,
	})
	m.Append(&transcript.ToolCard{ID: "c2", Name: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`), Status: transcript.StatusDenied, Depth: 1})
	// Highlighted content, so the width clamp and the one-row-per-element
	// rule are checked against styled lines too — a clamped line that lost
	// its reset sequence would bleed colour into the rest of the row.
	m.Append(&transcript.ToolCard{
		ID: "c3", Name: "bash", Args: json.RawMessage(`{"command":"cat src/Demo.pas"}`),
		Output: "program Demo;\nbegin\n  WriteLn('a very long string literal that will certainly be clamped at a narrow width');\nend.\n[exit code 0, 3ms]",
		Status: transcript.StatusOK,
	})
	m.Append(&transcript.ToolCard{
		ID: "c4", Name: "read_file", Args: json.RawMessage(`{"path":"internal/x.go"}`),
		Output: "     1\tpackage main\n     2\tfunc main() { println(\"a long line that needs clamping at forty columns or fewer\") }",
		Status: transcript.StatusOK,
	})
	m.Append(&transcript.Approval{Tool: "bash", Summary: "go test", Allowed: true, By: "user", Rules: []string{"bash(go test *) (session)"}})
	m.Append(&transcript.Notice{Text: "compacted 40k → 12k tokens", Level: transcript.LevelInfo})
	m.Append(&transcript.Notice{Text: "sandbox off", Level: transcript.LevelError})
	m.Append(&transcript.Error{Text: "provider unreachable"})
	return m
}

func TestLinesContainEveryBlock(t *testing.T) {
	m := sample(markdown.New())
	out := joined(m.Lines(80))
	for _, want := range []string{
		"› hello there", "Heading", "answer", "reasoning (2 lines",
		"▸ edit_file", "internal/x.go", "+2", "−1", "340ms", "✓ ok",
		"  ▸ bash", "go test ./...", "✗ denied",
		"? approval", "✓ allowed", "by user", "bash(go test *)", "session",
		"ℹ compacted", "⛔ sandbox off", "✗ provider unreachable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestExpandedCardShowsArgsDiffAndOutput(t *testing.T) {
	m := transcript.New(theme.New(false), nil)
	card := &transcript.ToolCard{Name: "edit_file", Args: json.RawMessage(`{"path":"a.go"}`), Diff: diff, Status: transcript.StatusOK}
	m.Append(card)
	long := strings.Repeat("line\n", 60)
	out := &transcript.ToolCard{Name: "bash", Args: json.RawMessage(`{"command":"ls"}`), Output: long, Status: transcript.StatusError, Expanded: true}
	m.Append(out)
	// A collapsed edit card now shows a *short* diff — that is the point of
	// the card, and the question "what did it just change?" otherwise costs a
	// keystroke and a scroll. It still shows no arguments and no output.
	if s := joined(m.Lines(80)); !strings.Contains(s, "+new") {
		t.Fatalf("collapsed edit card did not show its short diff:\n%s", s)
	}
	if s := joined(m.Lines(80)); strings.Contains(s, `"path": "a.go"`) || strings.Contains(s, "line\nline") {
		t.Fatalf("collapsed card leaked its arguments or output:\n%s", s)
	}
	m.SetToolsExpanded(true)
	s := joined(m.Lines(80))
	for _, want := range []string{"▾ edit_file", "args:", `"path": "a.go"`, "+new", "-old", "… 20 earlier lines", "✗ error"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Count(s, "line\n") > 41 {
		t.Errorf("output tail not limited to 40 lines")
	}
}

// TestCollapsedDiffCollapsesWhenLarge is the other half of the rule the user
// chose: a small change is worth a few rows inline, a large one is a count and
// a keystroke. 48 edits in one session is an ordinary day.
func TestCollapsedDiffCollapsesWhenLarge(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- a/x.go\n+++ b/x.go\n@@ -1,40 +1,40 @@\n")
	for i := range 40 {
		fmt.Fprintf(&b, "-old %d\n+new %d\n", i, i)
	}
	m := transcript.New(theme.New(false), nil)
	m.Append(&transcript.ToolCard{Name: "edit_file", Args: json.RawMessage(`{"path":"a.go"}`), Diff: b.String(), Status: transcript.StatusOK})
	s := joined(m.Lines(80))
	if strings.Contains(s, "+new 0") {
		t.Errorf("an 80-line diff was shown inline:\n%s", s)
	}
	for _, want := range []string{"80 changed lines", "ctrl+o", "+40", "−40"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

// TestCollapsedCardIsOneLineForEverythingElse pins the invariant the row
// budget rests on: only a small edit earns extra rows.
func TestCollapsedCardIsOneLineForEverythingElse(t *testing.T) {
	long := strings.Repeat("line\n", 60)
	for _, c := range []*transcript.ToolCard{
		{Name: "bash", Args: json.RawMessage(`{"command":"ls"}`), Output: long, Status: transcript.StatusOK},
		{Name: "read_file", Args: json.RawMessage(`{"path":"a.go"}`), Output: long, Status: transcript.StatusOK},
		{Name: "web_fetch", Args: json.RawMessage(`{"url":"https://x.test"}`), Output: long, Status: transcript.StatusOK},
		{Name: "grep", Args: json.RawMessage(`{"pattern":"x"}`), Output: long, Status: transcript.StatusOK},
		{Name: "bash", Args: json.RawMessage(`{"command":"sleep 9"}`), Status: transcript.StatusRunning, Progress: []string{"a", "b"}},
	} {
		m := transcript.New(theme.New(false), nil)
		m.Append(c)
		if got := len(m.Lines(80)); got != 1 {
			t.Errorf("%s collapsed to %d lines, want 1:\n%s", c.Name, got, joined(m.Lines(80)))
		}
	}
}

// TestCollapsedErrorCardShowsWhyWithoutExpanding is the deliberate second
// exception to one-line-collapsed. A refusal or a failed build that has to be
// expanded before it says anything is the worst thing the old card did.
func TestCollapsedErrorCardShowsWhyWithoutExpanding(t *testing.T) {
	m := transcript.New(theme.New(false), nil)
	m.Append(&transcript.ToolCard{
		Name: "bash", Args: json.RawMessage(`{"command":"go build ./..."}`),
		Output: "internal/x.go:12:3: undefined: foo\n[exit code 2, 1.8s]",
		Status: transcript.StatusError,
	})
	s := joined(m.Lines(80))
	for _, want := range []string{"✗ error", "undefined: foo", "[exit code 2, 1.8s]"} {
		if !strings.Contains(s, want) {
			t.Errorf("a collapsed failure did not say %q:\n%s", want, s)
		}
	}
}

// TestOneSliceElementPerRow is the invariant the row budget rests on: the
// viewport counts elements, so an element containing a newline is a row the
// layout does not know about.
func TestOneSliceElementPerRow(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		m := sample(markdown.New())
		m.SetToolsExpanded(true)
		m.SetShowReasoning(true)
		for i, l := range m.Lines(width) {
			if strings.Contains(l, "\n") {
				t.Errorf("width %d, line %d contains a newline: %q", width, i, l)
			}
		}
	}
}

func TestNoLineWiderThanWidth(t *testing.T) {
	for _, width := range []int{40, 60, 120, 200} {
		m := sample(markdown.New())
		m.SetToolsExpanded(true)
		m.SetShowReasoning(true)
		for _, line := range m.Lines(width) {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("width %d: %d-wide line %q", width, w, ansi.Strip(line))
			}
		}
	}
}

func TestCacheOnlyRerendersInvalidatedBlocks(t *testing.T) {
	var calls atomic.Int32
	md := markdown.New(markdown.WithBackend(func(s string, _ int, _ bool) (string, error) {
		calls.Add(1)
		return s, nil
	}))
	m := transcript.New(theme.New(true), md)
	done := &transcript.Assistant{Text: "finished block"}
	live := &transcript.Assistant{Text: "partial", Live: true}
	m.Append(done)
	m.Append(live)
	m.Lines(80)
	if got := calls.Load(); got != 2 {
		t.Fatalf("first render made %d markdown calls, want 2", got)
	}
	m.Lines(80)
	if got := calls.Load(); got != 2 {
		t.Fatalf("unchanged transcript re-rendered: %d calls", got)
	}
	live.Text += " more"
	m.Invalidate(live)
	m.Lines(80)
	if got := calls.Load(); got != 3 {
		t.Fatalf("delta re-rendered %d blocks, want only the live one (3 calls total)", got)
	}
	if !strings.Contains(joined(m.Lines(80)), "partial more") {
		t.Fatal("live block text not updated")
	}
	// A resize invalidates everything.
	m.Lines(60)
	if got := calls.Load(); got != 5 {
		t.Fatalf("resize made %d calls in total, want 5", got)
	}
	m.SetTheme(theme.New(false))
	m.Lines(60)
	if got := calls.Load(); got != 7 {
		t.Fatalf("theme change made %d calls in total, want 7", got)
	}
}

func TestReasoningToggle(t *testing.T) {
	m := transcript.New(theme.New(true), nil)
	m.Append(&transcript.Assistant{Text: "answer", Reasoning: "secret plan"})
	if s := joined(m.Lines(80)); strings.Contains(s, "secret plan") {
		t.Fatalf("reasoning shown while collapsed:\n%s", s)
	}
	m.SetShowReasoning(true)
	if s := joined(m.Lines(80)); !strings.Contains(s, "secret plan") || !strings.Contains(s, "▾ reasoning") {
		t.Fatalf("reasoning not shown after toggle:\n%s", s)
	}
}

func TestLiveEmptyAssistantShowsEllipsis(t *testing.T) {
	m := transcript.New(theme.New(true), nil)
	m.Append(&transcript.Assistant{Live: true})
	if s := joined(m.Lines(40)); !strings.Contains(s, "…") {
		t.Fatalf("got %q", s)
	}
}

func TestClearAndBlocks(t *testing.T) {
	m := sample(nil)
	// The count is whatever sample() appends; what matters is that Len and
	// Blocks agree with each other and that Clear empties both.
	if m.Len() == 0 || m.Len() != len(m.Blocks()) {
		t.Fatalf("Len = %d, Blocks = %d", m.Len(), len(m.Blocks()))
	}
	m.Clear()
	if m.Len() != 0 || len(m.Lines(80)) != 0 {
		t.Fatal("Clear left blocks behind")
	}
}

func TestSummaryPrefersPathThenCommand(t *testing.T) {
	tests := []struct {
		args string
		want string
	}{
		{`{"path":"a/b.go","command":"x"}`, "a/b.go"},
		{`{"command":"go build\n./..."}`, "go build ./..."},
		{`{"pattern":"TODO"}`, "TODO"},
		{`{"n":1}`, ""},
		{`not json`, ""},
	}
	for _, tt := range tests {
		c := &transcript.ToolCard{Args: json.RawMessage(tt.args)}
		if got := c.Summary(); got != tt.want {
			t.Errorf("Summary(%s) = %q, want %q", tt.args, got, tt.want)
		}
	}
}
