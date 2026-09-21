package highlight_test

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/highlight"
)

const goSrc = `package main

// greet says hello.
func greet(name string) string {
	return "hello " + name
}
`

// TestLinesAreOnePerInputLine is the row-budget invariant: the transcript
// assumes one slice element is exactly one terminal row, so a renderer that
// added or dropped a line would silently overflow every card.
func TestLinesAreOnePerInputLine(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, src, lang string }{
		{"go", goSrc, "Go"},
		{"go without a trailing newline", strings.TrimRight(goSrc, "\n"), "Go"},
		{"pascal", "program X;\nbegin\n  WriteLn('hi');\nend.\n", "ObjectPascal"},
		{"json", "{\n  \"a\": 1\n}\n", "JSON"},
		{"bash", "#!/bin/sh\nset -e\necho hi\n", "Bash"},
		{"one line", "package main", "Go"},
		{"blank lines inside", "package main\n\n\nvar x = 1\n", "Go"},
		{"only newlines", "\n\n\n", "Go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lines, ok := highlight.New().Lines(tt.src, tt.lang, theme.New(true))
			if !ok {
				t.Fatalf("Lines(%q) not ok", tt.lang)
			}
			if want := len(strings.Split(tt.src, "\n")); len(lines) != want {
				t.Errorf("got %d lines, want %d", len(lines), want)
			}
			for i, l := range lines {
				if strings.Contains(l, "\n") {
					t.Errorf("line %d contains a newline: %q", i, l)
				}
			}
		})
	}
}

// TestLinesPreserveEveryCharacter asserts highlighting is decoration: with the
// styling stripped the text is byte-identical to what the tool produced.
func TestLinesPreserveEveryCharacter(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ src, lang string }{
		{goSrc, "Go"},
		{"program X;\nbegin\n  WriteLn('hi');\nend.\n", "ObjectPascal"},
		{"{\n  \"k\": \"v\"\n}\n", "JSON"},
	} {
		lines, ok := highlight.New().Lines(tt.src, tt.lang, theme.New(false))
		if !ok {
			t.Fatalf("Lines(%q) not ok", tt.lang)
		}
		if got := xansi.Strip(strings.Join(lines, "\n")); got != tt.src {
			t.Errorf("highlighting changed the text:\n got %q\nwant %q", got, tt.src)
		}
	}
}

// TestLinesRefusesWhatItCannotLex pins the "plain is the safe answer" rule.
func TestLinesRefusesWhatItCannotLex(t *testing.T) {
	t.Parallel()
	r := highlight.New()
	for _, tt := range []struct{ name, src, lang string }{
		{"no language", goSrc, ""},
		{"unknown language", goSrc, "no-such-language-exists"},
		{"empty source", "", "Go"},
	} {
		if _, ok := r.Lines(tt.src, tt.lang, theme.New(true)); ok {
			t.Errorf("%s: Lines returned ok; plain was the right answer", tt.name)
		}
	}
}

// TestLinesFallsBackWhenTheTokeniserDisagrees covers the guard rather than
// trusting it: a backend whose tokens do not reconstruct the input must
// produce plain text, not a card with the wrong number of rows.
func TestLinesFallsBackWhenTheTokeniserDisagrees(t *testing.T) {
	t.Parallel()
	r := highlight.New(highlight.WithBackend(func(_, _ string) ([]chroma.Token, bool) {
		return nil, true // no tokens at all: zero lines for a three-line input
	}))
	if _, ok := r.Lines("a\nb\nc\n", "Go", theme.New(true)); ok {
		t.Error("Lines accepted a token stream that was not line-for-line with the input")
	}
}

// TestRepeatedRendersAgree is the observable half of caching: the lexer and
// the style set are reused across calls, so a card re-rendered after a resize
// must look exactly as it did before.
func TestRepeatedRendersAgree(t *testing.T) {
	t.Parallel()
	r := highlight.New()
	th := theme.New(true)
	first, ok := r.Lines(goSrc, "Go", th)
	if !ok {
		t.Fatal("Lines not ok")
	}
	for range 4 {
		again, ok := r.Lines(goSrc, "Go", th)
		if !ok || strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatal("a repeated render disagreed with the first")
		}
	}
}

// TestStylesFollowTheTheme asserts the colours come from wright's palette and
// change with the terminal background, rather than from a style of chroma's
// own that would not.
func TestStylesFollowTheTheme(t *testing.T) {
	t.Parallel()
	r := highlight.New()
	dark, ok1 := r.Lines(goSrc, "Go", theme.New(true))
	light, ok2 := r.Lines(goSrc, "Go", theme.New(false))
	if !ok1 || !ok2 {
		t.Fatal("Lines not ok")
	}
	if strings.Join(dark, "\n") == strings.Join(light, "\n") {
		t.Error("dark and light highlighting are identical; the theme is not reaching the styles")
	}
	// Something must actually be styled, or the whole thing is a no-op.
	if !strings.Contains(strings.Join(dark, "\n"), "\x1b[") {
		t.Error("nothing was styled")
	}
}

func TestLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct{ path, want string }{
		{"x.go", "Go"},
		{"internal/tui/apply.go", "Go"},
		{"src/LLMKit.Core.pas", "ObjectPascal"},
		{"SRC/UNIT1.PAS", "ObjectPascal"}, // DOS-descended trees spell it upper
		{"a.json", "JSON"},
		{"run.sh", "Bash"},
		{"x.py", "Python"},
		// No confident answer: plain, never a guess.
		{"notes", ""},
		{"x.txt", ""},
		{"x.unknownext", ""},
		{"", ""},
		{".", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := highlight.Language(tt.path); got != tt.want {
			t.Errorf("Language(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestTheLexersWeRelyOnStillExist fails loudly if a chroma upgrade renames a
// language out from under us, rather than silently falling back to plain.
func TestTheLexersWeRelyOnStillExist(t *testing.T) {
	t.Parallel()
	r := highlight.New()
	for _, lang := range []string{"Go", "ObjectPascal", "JSON", "Bash", "YAML", "Python", "diff"} {
		if _, ok := r.Lines("x\n", lang, theme.New(true)); !ok {
			t.Errorf("chroma no longer has a lexer named %q", lang)
		}
	}
}
