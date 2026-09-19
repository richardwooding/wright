// Package fuzzy is the small matcher behind the composer's completion popup
// and the picker overlay: case-insensitive substring first, then in-order
// subsequence, so "intx" still finds internal/tui/x.go without a dependency.
package fuzzy

import (
	"sort"
	"strings"
)

// Score ranks how well item matches query; ok is false for no match. Higher
// is better: exact prefix beats substring beats subsequence, and shorter
// items win ties so the most specific completion floats up.
func Score(item, query string) (score int, ok bool) {
	if query == "" {
		return 0, true
	}
	li, lq := strings.ToLower(item), strings.ToLower(query)
	switch {
	case strings.HasPrefix(li, lq):
		return 3000 - len(item), true
	case strings.Contains(li, lq):
		return 2000 - len(item), true
	case subsequence(li, lq):
		return 1000 - len(item), true
	}
	return 0, false
}

// Filter returns the items matching query, best first, keeping input order
// for equal scores.
func Filter(items []string, query string) []string {
	type scored struct {
		item  string
		score int
		idx   int
	}
	var hits []scored
	for i, it := range items {
		if s, ok := Score(it, query); ok {
			hits = append(hits, scored{it, s, i})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].idx < hits[b].idx
	})
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.item
	}
	return out
}

// subsequence reports whether every rune of q appears in s in order.
func subsequence(s, q string) bool {
	rs := []rune(s)
	i := 0
	for _, r := range q {
		for i < len(rs) && rs[i] != r {
			i++
		}
		if i == len(rs) {
			return false
		}
		i++
	}
	return true
}
