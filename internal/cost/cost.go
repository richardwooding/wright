// Package cost turns token usage into money for the status bar and /cost.
// Prices come from the llmkit catalog; a model the catalog does not know
// still has its tokens counted but its dollars are reported as unknown
// rather than as zero, so a local or unlisted model never looks free.
package cost

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/richardwooding/llmkit/catalog"
	"github.com/richardwooding/llmkit/core"
)

// Unknown is the USD value FormatUSD renders as "—". Meter.Total reports a
// separate known flag; Unknown exists for callers that carry a single float.
var Unknown = math.NaN()

// Line is the per-model breakdown returned by Meter.ByModel.
type Line struct {
	Model string
	Usage core.Usage
	USD   float64
	Known bool
}

// Meter accumulates usage per model. It is safe for concurrent use because
// agentkit hooks fire from tool-pool goroutines when tools run in parallel.
type Meter struct {
	mu    sync.Mutex
	lines map[string]*Line
}

// Add records u against model.
func (m *Meter) Add(model string, u core.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lines == nil {
		m.lines = map[string]*Line{}
	}
	l, ok := m.lines[model]
	if !ok {
		l = &Line{Model: model}
		m.lines[model] = l
	}
	l.Usage = l.Usage.Add(u)
	l.USD, l.Known = Price(model, l.Usage)
}

// Total sums every model. known is false when any recorded model is missing
// from the catalog, because a partial dollar figure would understate the bill.
func (m *Meter) Total() (usage core.Usage, usd float64, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	known = true
	for _, l := range m.lines {
		usage = usage.Add(l.Usage)
		usd += l.USD
		known = known && l.Known
	}
	return usage, usd, known
}

// ByModel returns one Line per model, sorted by model name for stable output.
func (m *Meter) ByModel() []Line {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Line, 0, len(m.lines))
	for _, l := range m.lines {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Price returns the list price of u on model. It relies on the core.Usage
// convention that cached and cache-write tokens are subsets of InputTokens,
// which catalog.Model.Cost already honors. known is false for models the
// catalog has no row for; their price is 0.
func Price(model string, u core.Usage) (usd float64, known bool) {
	info := catalog.Lookup(model)
	if !info.Known {
		return 0, false
	}
	return info.Cost(u), true
}

// FormatUSD renders a dollar amount for the status bar: "$0.38", "<$0.01"
// for a positive amount under a cent, "$0.00" for exactly zero and "—" for
// Unknown (NaN or negative).
func FormatUSD(usd float64) string {
	switch {
	case math.IsNaN(usd) || usd < 0:
		return "—"
	case usd == 0:
		return "$0.00"
	case usd < 0.005:
		return "<$0.01"
	default:
		return fmt.Sprintf("$%.2f", usd)
	}
}

// FormatTokens renders a token count compactly: "812", "41.2k", "1.3M".
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}
