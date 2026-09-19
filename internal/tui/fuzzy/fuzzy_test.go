package fuzzy_test

import (
	"slices"
	"testing"

	"github.com/richardwooding/wright/internal/tui/fuzzy"
)

func TestFilter(t *testing.T) {
	items := []string{"internal/tui/model.go", "internal/theme/theme.go", "README.md", "cmd/wright/main.go"}
	tests := []struct {
		query string
		want  []string
	}{
		{"", items},
		{"theme", []string{"internal/theme/theme.go"}},
		{"THEME", []string{"internal/theme/theme.go"}},
		{"internal", []string{"internal/tui/model.go", "internal/theme/theme.go"}},
		{"ntl", []string{"internal/tui/model.go", "internal/theme/theme.go"}}, // subsequence
		{"zzz", nil},
		{"README", []string{"README.md"}},
	}
	for _, tt := range tests {
		got := fuzzy.Filter(items, tt.query)
		if !slices.Equal(got, tt.want) {
			t.Errorf("Filter(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestScoreOrdersPrefixOverSubstringOverSubsequence(t *testing.T) {
	prefix, _ := fuzzy.Score("model.go", "mod")
	substr, _ := fuzzy.Score("tui/model.go", "mod")
	subseq, _ := fuzzy.Score("m/o/d.go", "mod")
	if prefix <= substr || substr <= subseq {
		t.Fatalf("prefix %d, substring %d, subsequence %d", prefix, substr, subseq)
	}
	if _, ok := fuzzy.Score("abc", "x"); ok {
		t.Fatal("no match reported as match")
	}
}
