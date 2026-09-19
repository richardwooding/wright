// Package diffview colours a unified diff for the transcript and the approval
// prompt. The "+" and "-" prefixes are kept in the output so the meaning of a
// line survives a monochrome terminal or a screen reader; colour is an extra
// cue, never the only one.
package diffview

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
)

// Lines colours each line of diff and truncates it to width (0 = no limit).
func Lines(diff string, th theme.Theme, width int) []string {
	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return nil
	}
	raw := strings.Split(diff, "\n")
	out := make([]string, len(raw))
	for i, line := range raw {
		if width > 0 {
			line = ansi.Truncate(line, width, "…")
		}
		out[i] = style(line, th).Render(line)
	}
	return out
}

// Render is Lines joined with newlines.
func Render(diff string, th theme.Theme, width int) string {
	return strings.Join(Lines(diff, th, width), "\n")
}

// Stats counts added and removed lines, ignoring the "+++"/"---" file headers.
func Stats(diff string) (added, removed int) {
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

// IsDiff reports whether text looks like a unified diff.
func IsDiff(text string) bool {
	t := strings.TrimLeft(text, "\n")
	return strings.HasPrefix(t, "diff --git") || strings.HasPrefix(t, "--- ") || strings.HasPrefix(t, "@@ ")
}

// style picks the style for one diff line. File headers and metadata are
// subtle so the hunks stand out.
func style(line string, th theme.Theme) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"),
		strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "):
		return th.Subtle
	case strings.HasPrefix(line, "@@"):
		return th.DiffHunk
	case strings.HasPrefix(line, "+"):
		return th.DiffAdd
	case strings.HasPrefix(line, "-"):
		return th.DiffDel
	}
	return lipgloss.NewStyle()
}
