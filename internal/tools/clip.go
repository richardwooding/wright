package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Clip bounds s to about max bytes, keeping the head and the tail with a
// note in between, so the model sees both how a command started and how it
// ended. When spillDir is set the full text is saved as out-<id>.txt and the
// note names the file. spilled reports whether that file was written.
func Clip(s string, maxBytes int, spillDir, id string) (out string, spilled bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	note := fmt.Sprintf("[output truncated: %d bytes", len(s))
	if spillDir != "" && id != "" {
		path := filepath.Join(spillDir, "out-"+sanitizeID(id)+".txt")
		if err := os.MkdirAll(spillDir, 0o700); err == nil {
			if err := os.WriteFile(path, []byte(s), 0o600); err == nil {
				note += "; full output saved to " + path
				spilled = true
			}
		}
	}
	note += "]"
	keep := maxBytes / 3
	head := cutAfterLine(s[:keep])
	tail := cutBeforeLine(s[len(s)-keep:])
	return head + "\n" + note + "\n" + tail, spilled
}

// cutAfterLine drops a trailing partial line so the head ends cleanly.
func cutAfterLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// cutBeforeLine drops a leading partial line so the tail starts cleanly.
func cutBeforeLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

// sanitizeID keeps a call ID safe to use as a file name.
func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}
