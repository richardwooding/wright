package transcript_test

import (
	"encoding/json"
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
	m.Append(&transcript.Approval{Tool: "bash", Summary: "go test", Allowed: true, By: "user", Rule: "bash(go test *)", Scope: "session"})
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
	if s := joined(m.Lines(80)); strings.Contains(s, "+new") {
		t.Fatalf("collapsed card leaked its diff:\n%s", s)
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
	if m.Len() != 8 || len(m.Blocks()) != 8 {
		t.Fatalf("Len = %d", m.Len())
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
