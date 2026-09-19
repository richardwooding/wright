package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

const (
	// maxGrepMatches caps matches (content mode) or files (files/count mode).
	maxGrepMatches = 200
	// maxGrepContext bounds the context lines per match.
	maxGrepContext = 10

	grepModeFiles   = "files"
	grepModeContent = "content"
	grepModeCount   = "count"
)

type grepArgs struct {
	Pattern         string `json:"pattern" jsonschema:"regular expression (RE2 syntax)"`
	Path            string `json:"path,omitempty" jsonschema:"directory or file to search (default: workspace root)"`
	Glob            string `json:"glob,omitempty" jsonschema:"only search files matching this glob, e.g. *.go or **/*_test.go"`
	Mode            string `json:"mode,omitempty" jsonschema:"files (default): matching file names; content: matching lines; count: matches per file"`
	Context         int    `json:"context,omitempty" jsonschema:"lines of context around each match in content mode (max 10)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum matches or files to return (default and max 200)"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" jsonschema:"ignore case"`
}

// grepLine is one line of output, a match or a context line, from either
// backend; formatting is shared.
type grepLine struct {
	path    string // absolute
	line    int
	text    string
	context bool
}

func (d *Deps) grep() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameGrep,
			"Search file contents with a regular expression (ripgrep when available). Ignored, hidden and binary files are skipped; at most 200 results.",
			d.runGrep),
		describe: d.describeGrep,
	}
}

func (d *Deps) describeGrep(args json.RawMessage) (policy.Request, Preview, error) {
	var a grepArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, err := d.grepBase(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameGrep, Args: args, Paths: []string{abs}}
	return req, Preview{Title: NameGrep + " " + a.Pattern, Body: abs}, nil
}

// grepBase accepts a directory or a single file.
func (d *Deps) grepBase(p string) (string, error) {
	if p == "" {
		return d.WS.Root(), nil
	}
	abs, _, err := d.resolve(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// normalize applies defaults and bounds.
func (a *grepArgs) normalize() error {
	if a.Pattern == "" {
		return errors.New("pattern is required")
	}
	switch a.Mode {
	case "":
		a.Mode = grepModeFiles
	case grepModeFiles, grepModeContent, grepModeCount:
	default:
		return fmt.Errorf("mode must be files, content or count, not %q", a.Mode)
	}
	if a.Limit < 1 || a.Limit > maxGrepMatches {
		a.Limit = maxGrepMatches
	}
	a.Context = max(0, min(a.Context, maxGrepContext))
	if a.Mode != grepModeContent {
		a.Context = 0
	}
	if a.Glob != "" && !doublestar.ValidatePattern(a.Glob) {
		return fmt.Errorf("invalid glob %q", a.Glob)
	}
	return nil
}

func (d *Deps) runGrep(ctx context.Context, a grepArgs) (agentkit.Output, error) {
	if err := a.normalize(); err != nil {
		return agentkit.Output{}, err
	}
	re, err := compilePattern(a)
	if err != nil {
		return agentkit.Output{}, err
	}
	base, err := d.grepBase(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	var lines []grepLine
	if rg, ok := d.ripgrep(); ok {
		lines, err = d.grepRipgrep(ctx, rg, base, a)
	} else {
		lines, err = d.grepGo(ctx, re, base, a)
	}
	if err != nil {
		return agentkit.Output{}, err
	}
	// Both backends may surface paths the workspace hides (rg does not read
	// .wrightignore), so the filter is applied here, once.
	lines = d.filterVisible(lines)
	return agentkit.Text(d.redact(ctx, NameGrep, formatGrep(d, lines, a))), nil
}

// compilePattern validates the expression with Go's RE2 engine, which also
// serves as the fallback matcher.
func compilePattern(a grepArgs) (*regexp.Regexp, error) {
	p := a.Pattern
	if a.CaseInsensitive {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return re, nil
}

// ripgrep locates rg unless disabled.
func (d *Deps) ripgrep() (string, bool) {
	if d.NoRipgrep {
		return "", false
	}
	p, err := exec.LookPath("rg")
	return p, err == nil
}

func (d *Deps) filterVisible(lines []grepLine) []grepLine {
	out := lines[:0]
	seen := map[string]bool{}
	for _, l := range lines {
		ok, cached := seen[l.path]
		if !cached {
			ok = d.visible(l.path)
			seen[l.path] = ok
		}
		if ok {
			out = append(out, l)
		}
	}
	return out
}

// rgMessage is the subset of `rg --json` lines we read.
type rgMessage struct {
	Type string `json:"type"`
	Data struct {
		Path       struct{ Text string } `json:"path"`
		Lines      struct{ Text string } `json:"lines"`
		LineNumber int                   `json:"line_number"`
	} `json:"data"`
}

// grepRipgrep runs rg and stops reading once enough matches arrived; the
// process is then cancelled rather than drained.
func (d *Deps) grepRipgrep(ctx context.Context, rg, base string, a grepArgs) ([]grepLine, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"--json", "--no-messages", "--hidden", "--no-require-git", "--glob", "!.git"}
	if a.CaseInsensitive {
		args = append(args, "-i")
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	if a.Context > 0 {
		args = append(args, "-C", strconv.Itoa(a.Context))
	}
	if a.Mode != grepModeContent {
		args = append(args, "--max-count", strconv.Itoa(a.Limit))
	}
	args = append(args, "-e", a.Pattern, "--", base)
	cmd := exec.CommandContext(ctx, rg, args...) //nolint:gosec // rg path from LookPath, arguments are flags and a validated base
	cmd.Dir = base
	if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
		cmd.Dir = filepath.Dir(base)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lines := readRipgrep(out, a)
	cancel()
	_ = cmd.Wait() // exit status 1 means "no matches"; 2 with --no-messages means unreadable files
	return lines, nil
}

// readRipgrep decodes rg's JSON stream until the limit is reached.
func readRipgrep(r io.Reader, a grepArgs) []grepLine {
	var lines []grepLine
	files := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	matches := 0
	for sc.Scan() {
		var m rgMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Type != "match" && m.Type != "context" {
			continue
		}
		path := m.Data.Path.Text
		if m.Type == "match" {
			matches++
			files[path] = true
		}
		lines = append(lines, grepLine{
			path: path, line: m.Data.LineNumber,
			text: strings.TrimRight(m.Data.Lines.Text, "\r\n"), context: m.Type == "context",
		})
		if (a.Mode == grepModeContent && matches > a.Limit) || (a.Mode != grepModeContent && len(files) > a.Limit) {
			break
		}
	}
	return lines
}

// grepGo is the pure-Go fallback: walk visible files, skip binaries, match
// each line and pull context from a small ring of previous lines.
func (d *Deps) grepGo(ctx context.Context, re *regexp.Regexp, base string, a grepArgs) ([]grepLine, error) {
	var lines []grepLine
	files := 0
	matches := 0
	done := errors.New("limit reached")
	visit := func(abs string, _ fs.DirEntry) error {
		if a.Glob != "" && !globMatches(a.Glob, base, abs) {
			return nil
		}
		got := grepFile(abs, re, a.Context)
		if len(got) == 0 {
			return nil
		}
		files++
		lines = append(lines, got...)
		for _, l := range got {
			if !l.context {
				matches++
			}
		}
		if (a.Mode == grepModeContent && matches > a.Limit) || (a.Mode != grepModeContent && files > a.Limit) {
			return done
		}
		return nil
	}
	var err error
	if fi, statErr := os.Stat(base); statErr == nil && !fi.IsDir() {
		err = visit(base, nil)
	} else {
		err = d.walk(ctx, base, visit)
	}
	if err != nil && !errors.Is(err, done) {
		return nil, err
	}
	return lines, nil
}

// globMatches tests glob against the path relative to base and its name,
// mirroring rg's -g semantics closely enough.
func globMatches(glob, base, abs string) bool {
	rel, _ := filepath.Rel(base, abs)
	rel = filepath.ToSlash(rel)
	if ok, _ := doublestar.Match(glob, rel); ok {
		return true
	}
	ok, _ := doublestar.Match(glob, filepath.Base(abs))
	return ok
}

// grepFile returns the matching (and context) lines of one text file.
func grepFile(abs string, re *regexp.Regexp, contextN int) []grepLine {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	var out []grepLine
	var prev []string // ring of the last contextN lines
	after := 0        // context lines still owed after the last match
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for ln := 1; sc.Scan(); ln++ {
		text := sc.Text()
		switch {
		case re.MatchString(text):
			for i, p := range prev {
				out = append(out, grepLine{path: abs, line: ln - len(prev) + i, text: p, context: true})
			}
			prev = prev[:0]
			out = append(out, grepLine{path: abs, line: ln, text: text})
			after = contextN
		case after > 0:
			out = append(out, grepLine{path: abs, line: ln, text: text, context: true})
			after--
		default:
			if contextN > 0 {
				prev = append(prev, text)
				if len(prev) > contextN {
					prev = prev[1:]
				}
			}
		}
	}
	return out
}

// formatGrep renders lines in the requested mode.
func formatGrep(d *Deps, lines []grepLine, a grepArgs) string {
	if len(lines) == 0 {
		return "no matches for " + a.Pattern
	}
	switch a.Mode {
	case grepModeContent:
		return formatContent(d, lines, a.Limit)
	case grepModeCount:
		return formatCount(d, lines, a.Limit)
	default:
		return formatFiles(d, lines, a.Limit)
	}
}

func formatFiles(d *Deps, lines []grepLine, limit int) string {
	seen := map[string]bool{}
	var files []string
	for _, l := range lines {
		if !l.context && !seen[l.path] {
			seen[l.path] = true
			files = append(files, d.rel(l.path))
		}
	}
	sort.Strings(files)
	return truncateList(files, limit, "files")
}

func formatCount(d *Deps, lines []grepLine, limit int) string {
	counts := map[string]int{}
	var order []string
	for _, l := range lines {
		if l.context {
			continue
		}
		if counts[l.path] == 0 {
			order = append(order, l.path)
		}
		counts[l.path]++
	}
	sort.Strings(order)
	rows := make([]string, len(order))
	for i, p := range order {
		rows[i] = fmt.Sprintf("%s:%d", d.rel(p), counts[p])
	}
	return truncateList(rows, limit, "files")
}

func formatContent(d *Deps, lines []grepLine, limit int) string {
	var b strings.Builder
	matches := 0
	lastPath, lastLine := "", 0
	for _, l := range lines {
		if !l.context {
			matches++
			if matches > limit {
				fmt.Fprintf(&b, "[truncated: showing %d matches; narrow the pattern or path]\n", limit)
				break
			}
		}
		if lastPath != "" && (l.path != lastPath || l.line > lastLine+1) {
			b.WriteString("--\n")
		}
		sep := ":"
		if l.context {
			sep = "-"
		}
		fmt.Fprintf(&b, "%s%s%d%s%s\n", d.rel(l.path), sep, l.line, sep, l.text)
		lastPath, lastLine = l.path, l.line
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateList joins rows, noting when the cap cut them.
func truncateList(rows []string, limit int, what string) string {
	if len(rows) > limit {
		rows = append(rows[:limit], fmt.Sprintf("[truncated: showing %d %s; narrow the pattern or path]", limit, what))
	}
	return strings.Join(rows, "\n")
}
