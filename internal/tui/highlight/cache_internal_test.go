package highlight

import (
	"testing"

	"github.com/richardwooding/wright/internal/theme"
)

// TestLexersAndStylesAreBuiltOnce is white-box on purpose: it pins a
// performance invariant that has no observable behaviour. A resize invalidates
// every block at once, so compiling a lexer or rebuilding the style map per
// card would be felt on a long transcript — and chroma compiles a lexer's
// rules lazily on first use, so the cost is real.
func TestLexersAndStylesAreBuiltOnce(t *testing.T) {
	r := New()
	dark, light := theme.New(true), theme.New(false)
	for range 20 {
		r.Lines("package main\n", "Go", dark)
		r.Lines("program X;\n", "ObjectPascal", dark)
		r.Lines("package main\n", "Go", light)
	}
	if len(r.lexers) != 2 {
		t.Errorf("lexer cache holds %d entries, want 2 (Go, ObjectPascal)", len(r.lexers))
	}
	if len(r.styles) != 2 {
		t.Errorf("style cache holds %d entries, want 2 (one per background)", len(r.styles))
	}
	// A language chroma does not have must also be cached, or every card with
	// an unrecognised extension re-runs the registry lookup.
	for range 5 {
		r.Lines("x\n", "no-such-language", dark)
	}
	if len(r.lexers) != 3 {
		t.Errorf("a missing lexer was not cached: %d entries", len(r.lexers))
	}
}
