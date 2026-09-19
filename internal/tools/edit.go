package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

type editArgs struct {
	Path       string `json:"path" jsonschema:"file to edit, absolute or workspace-relative"`
	OldString  string `json:"old_string" jsonschema:"exact text to replace; must occur once unless replace_all"`
	NewString  string `json:"new_string" jsonschema:"replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match"`
}

// edit is a computed, not yet applied, edit.
type edit struct {
	abs    string
	mode   os.FileMode
	before string
	after  string
	count  int
	lines  []int // 1-based line numbers of each replacement in after
}

func (d *Deps) editFile() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameEditFile,
			"Replace an exact string in a file. old_string must match exactly (whitespace included) and, unless replace_all is set, exactly once. A snapshot is taken first so the change can be undone.",
			d.runEditFile),
		describe: d.describeEdit,
	}
}

func (d *Deps) describeEdit(args json.RawMessage) (policy.Request, Preview, error) {
	var a editArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, _, err := d.resolve(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameEditFile, Args: args, Paths: []string{abs}, Writes: []string{abs}}
	e, err := d.computeEdit(abs, a)
	if err != nil {
		return req, Preview{}, err
	}
	rel := d.rel(abs)
	return req, Preview{Title: NameEditFile + " " + rel, Diff: unified(rel, e.before, e.after)}, nil
}

func (d *Deps) runEditFile(ctx context.Context, a editArgs) (agentkit.Output, error) {
	abs, inside, err := d.resolve(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := d.refuseWrite(abs, inside); err != nil {
		return agentkit.Output{}, err
	}
	e, err := d.computeEdit(abs, a)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := d.snapshot(ctx, abs); err != nil {
		return agentkit.Output{}, err
	}
	if err := os.WriteFile(abs, []byte(e.after), e.mode); err != nil {
		return agentkit.Output{}, err
	}
	return agentkit.Text(fmt.Sprintf("Edited %s: replaced %d occurrence%s at line%s %s",
		d.rel(abs), e.count, plural(e.count), plural(len(e.lines)), joinInts(e.lines))), nil
}

// computeEdit validates the edit against the file and produces the result
// without touching the disk, so Describe and Call share one code path.
func (d *Deps) computeEdit(abs string, a editArgs) (edit, error) {
	if a.OldString == "" {
		return edit{}, errors.New("old_string must not be empty; use write_file to create content")
	}
	if a.OldString == a.NewString {
		return edit{}, errors.New("old_string and new_string are identical")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return edit{}, err
	}
	if info.IsDir() {
		return edit{}, fmt.Errorf("%s is a directory", d.rel(abs))
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return edit{}, err
	}
	before := string(raw)
	count := strings.Count(before, a.OldString)
	switch {
	case count == 0:
		return edit{}, fmt.Errorf("old_string not found in %s%s", d.rel(abs), closestLine(before, a.OldString))
	case count > 1 && !a.ReplaceAll:
		return edit{}, fmt.Errorf("old_string occurs %d times in %s (lines %s); add context to make it unique or set replace_all",
			count, d.rel(abs), joinInts(occurrenceLines(before, a.OldString)))
	}
	after, lines := replace(before, a.OldString, a.NewString, a.ReplaceAll)
	return edit{abs: abs, mode: info.Mode().Perm(), before: before, after: after, count: len(lines), lines: lines}, nil
}

// replace performs the substitution and records the line each replacement
// starts on in the result.
func replace(s, old, repl string, all bool) (string, []int) {
	var b strings.Builder
	var lines []int
	rest := s
	for {
		i := strings.Index(rest, old)
		if i < 0 {
			break
		}
		b.WriteString(rest[:i])
		lines = append(lines, strings.Count(b.String(), "\n")+1)
		b.WriteString(repl)
		rest = rest[i+len(old):]
		if !all {
			break
		}
	}
	b.WriteString(rest)
	return b.String(), lines
}

// occurrenceLines lists the 1-based lines where old starts in s.
func occurrenceLines(s, old string) []int {
	var out []int
	off := 0
	for {
		i := strings.Index(s[off:], old)
		if i < 0 {
			return out
		}
		out = append(out, strings.Count(s[:off+i], "\n")+1)
		off += i + len(old)
	}
}

// closestLine names the line sharing the longest prefix with the first
// line of old (whitespace-trimmed), to hint at what the model misremembered.
func closestLine(s, old string) string {
	want, _, _ := strings.Cut(old, "\n")
	want = strings.TrimSpace(want)
	if want == "" {
		return ""
	}
	best, bestN, bestLine := 0, 0, ""
	for n, line := range strings.Split(s, "\n") {
		if k := commonPrefix(strings.TrimSpace(line), want); k > best {
			best, bestN, bestLine = k, n+1, strings.TrimSpace(line)
		}
	}
	if best == 0 {
		return ""
	}
	if len(bestLine) > 120 {
		bestLine = bestLine[:120] + "…"
	}
	return fmt.Sprintf("; closest line %d: %s", bestN, bestLine)
}

// commonPrefix returns the length of the shared prefix of a and b.
func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
