package toolview_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/highlight"
	"github.com/richardwooding/wright/internal/tui/toolview"
)

func args(s string) json.RawMessage { return json.RawMessage(s) }

// TestForDecidesFromTheRequest is the heart of "the card may be affected by
// what they request": every decision here is made from the tool and its
// arguments, with the output nowhere in sight.
func TestForDecidesFromTheRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, tool, args string
		want             toolview.Plan
	}{
		{
			"read a pascal unit", "read_file", `{"path":"src/LLMKit.Core.pas"}`,
			toolview.Plan{Lang: "ObjectPascal", Gutter: toolview.GutterNumbered},
		},
		{
			"read a file with no language", "read_file", `{"path":"NOTES"}`,
			toolview.Plan{Gutter: toolview.GutterNumbered},
		},
		{
			"edit go", "edit_file", `{"path":"internal/x.go"}`,
			toolview.Plan{Lang: "Go", Diffable: true},
		},
		{
			"write go", "write_file", `{"path":"x.go"}`,
			toolview.Plan{Lang: "Go", Diffable: true},
		},
		{
			"multi_edit, all one language", "multi_edit", `{"edits":[{"path":"a.go"},{"path":"b.go"}]}`,
			toolview.Plan{Lang: "Go", Diffable: true},
		},
		{
			"multi_edit, mixed languages", "multi_edit", `{"edits":[{"path":"a.go"},{"path":"b.py"}]}`,
			toolview.Plan{Diffable: true},
		},
		{
			"multi_edit, one file unknown", "multi_edit", `{"edits":[{"path":"a.go"},{"path":"NOTES"}]}`,
			toolview.Plan{Diffable: true},
		},
		{
			"bash reading a file", "bash", `{"command":"cat src/x.pas"}`,
			toolview.Plan{Lang: "ObjectPascal", Trailers: true},
		},
		{
			"bash building", "bash", `{"command":"go build ./..."}`,
			toolview.Plan{Trailers: true},
		},
		{"grep", "grep", `{"pattern":"func"}`, toolview.Plan{Gutter: toolview.GutterPathLine}},
		{"web_fetch is never highlighted", "web_fetch", `{"url":"https://x.test/a.go"}`, toolview.Plan{}},
		{"an MCP tool", "mcp:srv:thing", `{"path":"x.go"}`, toolview.Plan{}},
		{"malformed arguments", "read_file", `{not json`, toolview.Plan{Gutter: toolview.GutterNumbered}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toolview.For(tt.tool, args(tt.args)); got != tt.want {
				t.Errorf("For(%q, %s) = %+v, want %+v", tt.tool, tt.args, got, tt.want)
			}
		})
	}
}

// TestBashLanguageGate is where a wrong answer is easiest and most visible.
// The rule: only a pager, only one recognisable operand, and only when the
// output is the file rather than something computed from it.
func TestBashLanguageGate(t *testing.T) {
	t.Parallel()
	tests := []struct{ cmd, want string }{
		// The output is the file.
		{"cat src/x.pas", "ObjectPascal"},
		{"head -20 y.go", "Go"},
		{"sed -n '1,50p' x.pas", "ObjectPascal"},
		{"tail -n 5 conf.yaml", "YAML"},
		{"FOO=1 cat x.go", "Go"},
		{"nl x.py", "Python"},
		// The output is something else.
		{"cat x.go | wc -l", ""},  // a number
		{"cat x.go > y.go", ""},   // nothing
		{"ls && cat x.go", ""},    // two commands
		{"cat a.go b.py", ""},     // two languages
		{"go build ./...", ""},    // a build log
		{"rm x.go", ""},           // not a pager
		{"grep -rn foo x.go", ""}, // path:line:text, not the file
		{"cat", ""},               // standard input
		{"cat NOTES", ""},         // no language
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(map[string]string{"command": tt.cmd})
			if got := toolview.For("bash", body).Lang; got != tt.want {
				t.Errorf("bash %q -> %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestSummaryIsUnchanged pins the contract /ps and the approval block rely on:
// Summary describes the *request*, and moving it between packages must not
// have altered a character of what it produces.
func TestSummaryIsUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct{ args, want string }{
		{`{"path":"a.go"}`, "a.go"},
		{`{"command":"go test ./..."}`, "go test ./..."},
		{`{"path":"a.go","command":"x"}`, "a.go"}, // path wins
		{`{"pattern":"func"}`, "func"},
		{`{"url":"https://x.test"}`, "https://x.test"},
		{`{"command":"line one\nline two"}`, "line one line two"}, // flattened
		{`{"other":"x"}`, ""},
		{`{"path":""}`, ""},
		{`not json`, ""},
		{strings.Replace(`{"path":"PAD"}`, "PAD", strings.Repeat("x", 100), 1), strings.Repeat("x", 59) + "…"},
	}
	for _, tt := range tests {
		if got := toolview.Summary(args(tt.args)); got != tt.want {
			t.Errorf("Summary(%s) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// TestHeadlinePrefersWhatTheToolSaid covers the human summary the user asked
// for: the tools already write one, and the card used to ignore it.
func TestHeadlinePrefersWhatTheToolSaid(t *testing.T) {
	t.Parallel()
	const said = "Edited src/X.pas: replaced 1 occurrence at line 214"
	tests := []struct {
		name, tool, args, output string
		running                  bool
		want                     string
	}{
		{"an edit says what it did", "edit_file", `{"path":"src/X.pas"}`, said, false, said},
		{"a write says what it did", "write_file", `{"path":"y.pas"}`, "Wrote 8210 bytes (212 lines) to y.pas", false, "Wrote 8210 bytes (212 lines) to y.pas"},
		{"still running: no result yet", "edit_file", `{"path":"src/X.pas"}`, "", true, "src/X.pas"},
		{"running with partial output", "edit_file", `{"path":"src/X.pas"}`, said, true, "src/X.pas"},
		// These return content. Its first line describes nothing.
		{"read_file", "read_file", `{"path":"a.go"}`, "     1\tpackage main", false, "a.go"},
		{"bash", "bash", `{"command":"ls"}`, "a.go\nb.go", false, "ls"},
		{"grep", "grep", `{"pattern":"func"}`, "a.go:1:func x()", false, "func"},
		{"an edit that said nothing", "edit_file", `{"path":"a.go"}`, "", false, "a.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toolview.Headline(tt.tool, args(tt.args), tt.output, tt.running)
			if got != tt.want {
				t.Errorf("Headline = %q, want %q", got, tt.want)
			}
		})
	}
}

func deps() toolview.Deps {
	return toolview.Deps{Theme: theme.New(true), Highlighter: highlight.New()}
}

// TestTrailersAreWrightsOwnWords asserts the "[exit code …]" and "[note: …]"
// lines are lifted out of the output. They are the harness talking, not the
// program, and a 900-line result must not scroll the exit code away.
func TestTrailersAreWrightsOwnWords(t *testing.T) {
	t.Parallel()
	out := strings.Repeat("noise\n", 200) + "[note: output truncated]\n[exit code 2, 1.8s]"
	lines := toolview.Full(toolview.Input{
		Plan: toolview.For("bash", args(`{"command":"make"}`)), Output: out, Width: 80,
	}, deps())
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"[note: output truncated]", "[exit code 2, 1.8s]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("trailer %q was scrolled away by the output tail", want)
		}
	}
	// And they are the last two rows, not buried in the middle of the tail.
	if n := len(lines); n < 2 || !strings.Contains(lines[n-1], "[exit code 2") || !strings.Contains(lines[n-2], "[note:") {
		t.Errorf("trailers are not the final rows:\n%s", joined)
	}
	// A tool that writes no trailers must not have its output rewritten.
	plain := toolview.Full(toolview.Input{
		Plan: toolview.For("web_fetch", args(`{"url":"https://x.test"}`)), Output: "[exit code 0] is my content", Width: 80,
	}, deps())
	if len(plain) != 1 || !strings.Contains(plain[0], "[exit code 0] is my content") {
		t.Errorf("a non-bash tool's output was treated as a trailer: %q", plain)
	}
}

// TestReadFileKeepsItsOwnLineNumbers is the "do not add a second gutter" rule.
func TestReadFileKeepsItsOwnLineNumbers(t *testing.T) {
	t.Parallel()
	out := "     1\tpackage main\n     2\t\n     3\tfunc main() {}\n"
	lines := toolview.Full(toolview.Input{
		Plan: toolview.For("read_file", args(`{"path":"a.go"}`)), Output: out, Width: 80,
	}, deps())
	if len(lines) != 3 {
		t.Fatalf("got %d rows, want 3: %q", len(lines), lines)
	}
	for i, want := range []string{"     1\tpackage main", "     2\t", "     3\tfunc main() {}"} {
		if got := strip(lines[i]); got != want {
			t.Errorf("row %d = %q, want %q (the tool's own gutter, tab intact)", i, got, want)
		}
	}
}

// TestRunningCardsAreNotHighlighted guards the re-render hazard: a running
// card's body is rebuilt on every progress event.
func TestRunningCardsAreNotHighlighted(t *testing.T) {
	t.Parallel()
	in := toolview.Input{
		Plan:   toolview.For("bash", args(`{"command":"cat x.go"}`)),
		Output: "package main\nfunc main() {}\n", Width: 80, Running: true,
	}
	for _, l := range toolview.Full(in, deps()) {
		if strings.Contains(l, "\x1b[") {
			t.Errorf("a running card was highlighted: %q", l)
		}
	}
	in.Running = false
	if !strings.Contains(strings.Join(toolview.Full(in, deps()), ""), "\x1b[") {
		t.Error("a finished card was not highlighted")
	}
}

// TestHighlightingFollowsTheRequestNotTheContent is the decision the user
// took, stated as one test: byte-identical Go source is coloured when the
// request named a .go file and left plain when it did not.
func TestHighlightingFollowsTheRequestNotTheContent(t *testing.T) {
	t.Parallel()
	const src = "package main\n\nfunc main() {}\n"
	coloured := toolview.Full(toolview.Input{
		Plan: toolview.For("read_file", args(`{"path":"a.go"}`)), Output: src, Width: 80,
	}, deps())
	plain := toolview.Full(toolview.Input{
		Plan: toolview.For("web_fetch", args(`{"url":"https://x.test/page"}`)), Output: src, Width: 80,
	}, deps())
	if !strings.Contains(strings.Join(coloured, ""), "\x1b[") {
		t.Error("a .go request was not highlighted")
	}
	if strings.Contains(strings.Join(plain, ""), "\x1b[") {
		t.Error("identical content was highlighted for a request that named no language")
	}
	// Either way the text itself is untouched.
	if strip(strings.Join(coloured, "\n")) != strip(strings.Join(plain, "\n")) {
		t.Error("highlighting changed the text")
	}
}
