package highlight_test

import (
	"os"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/highlight"
)

func BenchmarkLinesLargeFile(b *testing.B) {
	src, err := os.ReadFile("../../policy/engine.go")
	if err != nil {
		b.Skip(err)
	}
	b.Logf("%d lines", strings.Count(string(src), "\n"))
	r := highlight.New()
	th := theme.New(true)
	b.ResetTimer()
	for b.Loop() {
		r.Lines(string(src), "Go", th)
	}
}

func BenchmarkLines40(b *testing.B) {
	src, _ := os.ReadFile("../../policy/engine.go")
	lines := strings.Split(string(src), "\n")
	small := strings.Join(lines[:40], "\n")
	r := highlight.New()
	th := theme.New(true)
	b.ResetTimer()
	for b.Loop() {
		r.Lines(small, "Go", th)
	}
}
