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
			toolview.Plan{Lang: "ObjectPascal", Gutter: toolview.GutterNumbered, OutputIsSource: true},
		},
		{
			"read a file with no language", "read_file", `{"path":"NOTES"}`,
			toolview.Plan{Gutter: toolview.GutterNumbered, OutputIsSource: true},
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
			toolview.Plan{Lang: "ObjectPascal", Trailers: true, OutputIsSource: true},
		},
		{
			"bash building", "bash", `{"command":"go build ./..."}`,
			toolview.Plan{Trailers: true},
		},
		{"grep", "grep", `{"pattern":"func"}`, toolview.Plan{Gutter: toolview.GutterPathLine}},
		{"web_fetch is never highlighted", "web_fetch", `{"url":"https://x.test/a.go"}`, toolview.Plan{}},
		{"an MCP tool", "mcp:srv:thing", `{"path":"x.go"}`, toolview.Plan{}},
		{"malformed arguments", "read_file", `{not json`, toolview.Plan{Gutter: toolview.GutterNumbered, OutputIsSource: true}},
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
			got := toolview.Headline(tt.tool, args(tt.args), tt.output, tt.running, "")
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

// TestAToolsOwnSentenceIsNeverHighlighted is the regression. write_file knows
// its language, for the diff — but its *result* is wright's own sentence, and
// lexing "Wrote 271 bytes (18 lines) to src/Request.php" as PHP paints the
// byte count as a numeric literal. Colouring the harness's words as if the
// program had said them is the one thing highlighting must never do.
func TestAToolsOwnSentenceIsNeverHighlighted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, tool, a, out string }{
		{"write_file", "write_file", `{"path":"src/Request.php"}`, "Wrote 271 bytes (18 lines) to src/Request.php"},
		{"edit_file", "edit_file", `{"path":"src/Request.php"}`, "Edited src/Request.php: replaced 1 occurrence at line 214"},
		{"multi_edit", "multi_edit", `{"edits":[{"path":"a.php"}]}`, "Applied 3 edits (3 replacements) in 1 file: a.php"},
		{"a build log", "bash", `{"command":"composer install"}`, "Generating autoload files\n2 packages installed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, l := range toolview.Full(toolview.Input{
				Plan: toolview.For(tt.tool, args(tt.a)), Output: tt.out, Width: 90,
			}, deps()) {
				if strings.Contains(l, "\x1b[3") { // a foreground colour
					t.Errorf("wright's own words were highlighted as source: %q", l)
				}
			}
		})
	}
}

// TestANewFileShowsWhatWasWritten covers the other half: a write_file has no
// diff (Describe reports "new file, N lines", correctly — a diff against
// nothing is all "+"), so the card had nothing to show but the arguments,
// where the whole file is one escaped JSON string clipped at the width. In a
// project written from scratch that is most of the transcript.
func TestANewFileShowsWhatWasWritten(t *testing.T) {
	t.Parallel()
	php := "<?php\n\nnamespace LLMKit;\n\nfinal class Request\n{\n    public function model(): string\n    {\n        return $this->model;\n    }\n}\n"
	a, err := json.Marshal(map[string]string{"path": "src/Request.php", "content": php})
	if err != nil {
		t.Fatal(err)
	}
	lines := toolview.Full(toolview.Input{
		Plan: toolview.For("write_file", a), Written: toolview.Written("write_file", a),
		Output: "Wrote 271 bytes (11 lines) to src/Request.php", Width: 90,
	}, deps())
	joined := strings.Join(lines, "\n")
	if !strings.Contains(strip(joined), "final class Request") {
		t.Errorf("the written content is not on the card:\n%s", strip(joined))
	}
	if !strings.Contains(joined, "\x1b[") {
		t.Error("the written content was not highlighted, though the path names PHP")
	}
	// One row per line of the file, and no escaped JSON anywhere.
	if strings.Contains(joined, `\n`) {
		t.Errorf("the content was shown escaped:\n%s", joined)
	}
	if got, want := len(lines), strings.Count(strings.TrimRight(php, "\n"), "\n")+1; got != want {
		t.Errorf("got %d rows, want %d (one per line of the file)", got, want)
	}
}

// TestWrittenOnlyForANewFile: an overwrite has a real diff, which is the
// better answer, and no other tool carries content to show.
func TestWrittenOnlyForANewFile(t *testing.T) {
	t.Parallel()
	a, _ := json.Marshal(map[string]string{"path": "x.php", "content": "<?php\necho 1;\n"})
	if got := toolview.Written("write_file", a); got == "" {
		t.Error("write_file carries no content")
	}
	for _, tool := range []string{"edit_file", "multi_edit", "read_file", "bash", "web_fetch"} {
		if got := toolview.Written(tool, a); got != "" {
			t.Errorf("Written(%q) = %q, want empty", tool, got)
		}
	}
	// With a diff present the diff wins: the content is not shown twice.
	lines := toolview.Full(toolview.Input{
		Plan:    toolview.For("write_file", a),
		Written: toolview.Written("write_file", a),
		Diff:    "--- a/x.php\n+++ b/x.php\n@@ -1 +1,2 @@\n echo 1;\n+echo 2;\n",
		Width:   90,
	}, deps())
	if j := strip(strings.Join(lines, "\n")); !strings.Contains(j, "+echo 2;") || strings.Count(j, "echo 1;") != 1 {
		t.Errorf("the diff did not take precedence over the content:\n%s", j)
	}
}

// TestSummaryForStripsARedundantCd is the case that motivated this. In a real
// session 89% of bash calls began `cd <workspace> &&`, 76 of 78 of those
// targeted the directory bash was already in, and the prefix for that project
// was exactly 60 characters — the whole summary budget — so every card showed
// the cd and none of the command.
func TestSummaryForStripsARedundantCd(t *testing.T) {
	t.Parallel()
	const root = "/var/home/richardwooding/Projects/Personal/php-llmkit"
	cmd := func(c string) json.RawMessage {
		b, err := json.Marshal(map[string]string{"command": c})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct{ name, in, want string }{
		// Redundant: the shell is already there.
		{"absolute cd to the root", "cd " + root + " && go test ./...", "go test ./..."},
		{"semicolon separator", "cd " + root + "; go test ./...", "go test ./..."},
		{"trailing slash", "cd " + root + "/ && ls", "ls"},
		{"chained cds collapse", "cd /tmp && cd " + root + " && ls", "ls"},
		{"newline flattened then stripped", "cd " + root + " &&\ngo build ./...", "go build ./..."},
		{"internal spacing survives", "cd " + root + ` && echo "a  b"`, `echo "a  b"`},
		// Real moves: the directory is kept.
		{"below the root", "cd " + root + "/src && phpunit", "src: phpunit"},
		{"relative target", "cd internal/tools && go test", "internal/tools: go test"},
		// Not a cd prefix at all: unchanged.
		{"cd alone is the command", "cd /x", "cd /x"},
		{"quoted target", `cd "my dir" && ls`, `cd "my dir" && ls`},
		{"double dash", "cd -- /x && ls", "cd -- /x && ls"},
		{"command substitution", "cd $(ls | head -1) && ls", "cd $(ls | head -1) && ls"},
		{"variable target", "cd $HOME && ls", "cd $HOME && ls"},
		{"or is not and", "cd /x || ls", "cd /x || ls"},
		{"cd is only a prefix of the word", "cdinstall x && y", "cdinstall x && y"},
		{"cd in the middle", "echo cd /x && ls", "echo cd /x && ls"},
		{"no cd", "go test ./...", "go test ./..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toolview.SummaryFor("bash", cmd(tt.in), root); got != tt.want {
				t.Errorf("SummaryFor(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSummaryForIsBashOnly proves the dispatch: identical arguments on any
// other tool go through the generic Summary untouched, so a tool with a
// "command" key that is not a shell script is never lexed as one.
func TestSummaryForIsBashOnly(t *testing.T) {
	t.Parallel()
	const root = "/ws"
	a := args(`{"command":"cd /ws && go test"}`)
	if got := toolview.SummaryFor("bash", a, root); got != "go test" {
		t.Errorf("bash: %q", got)
	}
	for _, tool := range []string{"mcp:srv:run", "web_fetch", "read_file", ""} {
		if got := toolview.SummaryFor(tool, a, root); got != "cd /ws && go test" {
			t.Errorf("SummaryFor(%q) = %q, want the command untouched", tool, got)
		}
	}
}

// TestSummaryForWithoutAWorkspaceRoot: with no root to compare against, an
// absolute cd is shown rather than guessed at. The comparison fails safe
// because one directory has several spellings — /home is a symlink to
// var/home here, /tmp to /private/tmp on macOS — and a renderer must not
// resolve symlinks on the draw path.
func TestSummaryForWithoutAWorkspaceRoot(t *testing.T) {
	t.Parallel()
	got := toolview.SummaryFor("bash", args(`{"command":"cd /ws && go test"}`), "")
	if got != "/ws: go test" {
		t.Errorf("SummaryFor with no root = %q, want the directory shown", got)
	}
}

// TestSummaryForReclaimsTheBudget is the arithmetic that made this a total
// failure rather than a partial one: the prefix was exactly SummaryMax long,
// so the truncation spent all of it before reaching the command.
func TestSummaryForReclaimsTheBudget(t *testing.T) {
	t.Parallel()
	const root = "/var/home/richardwooding/Projects/Personal/php-llmkit"
	if n := len("cd " + root + " && "); n != toolview.SummaryMax {
		t.Fatalf("the prefix is %d chars and SummaryMax is %d; this test is no longer the real case", n, toolview.SummaryMax)
	}
	long := "vendor/bin/phpstan analyse src --level 8 --no-progress --memory-limit 1G"
	b, err := json.Marshal(map[string]string{"command": "cd " + root + " && " + long})
	if err != nil {
		t.Fatal(err)
	}
	got := toolview.SummaryFor("bash", b, root)
	if !strings.HasPrefix(got, "vendor/bin/phpstan analyse src") {
		t.Errorf("SummaryFor = %q, want it to start with the real command", got)
	}
	if strings.Contains(got, "cd ") || strings.Contains(got, "php-llmkit") {
		t.Errorf("SummaryFor = %q, want no trace of the cd", got)
	}
}
