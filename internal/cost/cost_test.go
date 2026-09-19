package cost_test

import (
	"math"
	"testing"

	"github.com/richardwooding/llmkit/catalog"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/cost"
)

// known is a catalog row every test can rely on regardless of seed churn.
const known = "claude-sonnet-4-5"

type add struct {
	model string
	u     core.Usage
}

func TestPrice(t *testing.T) {
	info := catalog.Lookup(known)
	if !info.Known {
		t.Fatalf("catalog lost %s", known)
	}
	u := core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CachedInputTokens: 500_000}
	tests := []struct {
		name      string
		model     string
		usage     core.Usage
		wantUSD   float64
		wantKnown bool
	}{
		{"known model matches catalog cost", known, u, info.Cost(u), true},
		{"cached tokens are a subset of input", known, u, 0.5*info.Pricing.Input + 0.5*info.Pricing.CacheRead + info.Pricing.Output, true},
		{"unknown model is unknown, not free", "qwen2.5-coder:7b", u, 0, false},
		{"zero usage costs zero", known, core.Usage{}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, ok := cost.Price(tt.model, tt.usage)
			if ok != tt.wantKnown {
				t.Fatalf("known = %v, want %v", ok, tt.wantKnown)
			}
			if math.Abs(usd-tt.wantUSD) > 1e-9 {
				t.Errorf("usd = %v, want %v", usd, tt.wantUSD)
			}
		})
	}
}

func TestMeter(t *testing.T) {
	info := catalog.Lookup(known)
	tests := []struct {
		name      string
		adds      []add
		wantIn    int
		wantOut   int
		wantUSD   float64
		wantKnown bool
		wantLines int
	}{
		{name: "empty meter", wantKnown: true},
		{
			name: "same model accumulates",
			adds: []add{
				{known, core.Usage{InputTokens: 1000, OutputTokens: 10}},
				{known, core.Usage{InputTokens: 2000, OutputTokens: 20}},
			},
			wantIn: 3000, wantOut: 30,
			wantUSD:   info.Cost(core.Usage{InputTokens: 3000, OutputTokens: 30}),
			wantKnown: true, wantLines: 1,
		},
		{
			name: "unknown model poisons known",
			adds: []add{
				{known, core.Usage{InputTokens: 1000}},
				{"llama3:8b", core.Usage{InputTokens: 500}},
			},
			wantIn:  1500,
			wantUSD: info.Cost(core.Usage{InputTokens: 1000}), wantKnown: false, wantLines: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m cost.Meter
			for _, a := range tt.adds {
				m.Add(a.model, a.u)
			}
			u, usd, ok := m.Total()
			if u.InputTokens != tt.wantIn || u.OutputTokens != tt.wantOut {
				t.Errorf("usage = %+v, want in=%d out=%d", u, tt.wantIn, tt.wantOut)
			}
			if math.Abs(usd-tt.wantUSD) > 1e-9 || ok != tt.wantKnown {
				t.Errorf("total = %v/%v, want %v/%v", usd, ok, tt.wantUSD, tt.wantKnown)
			}
			if lines := m.ByModel(); len(lines) != tt.wantLines {
				t.Errorf("ByModel = %d lines, want %d", len(lines), tt.wantLines)
			}
		})
	}
}

func TestByModelSorted(t *testing.T) {
	var m cost.Meter
	m.Add("zeta:1b", core.Usage{InputTokens: 1})
	m.Add(known, core.Usage{InputTokens: 1})
	lines := m.ByModel()
	if len(lines) != 2 || lines[0].Model != known || lines[1].Model != "zeta:1b" {
		t.Fatalf("ByModel = %+v", lines)
	}
	if !lines[0].Known || lines[1].Known {
		t.Errorf("known flags = %v %v", lines[0].Known, lines[1].Known)
	}
}

func TestMeterConcurrent(t *testing.T) {
	var m cost.Meter
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 100 {
				m.Add(known, core.Usage{InputTokens: 1})
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	if u, _, _ := m.Total(); u.InputTokens != 800 {
		t.Errorf("InputTokens = %d, want 800", u.InputTokens)
	}
}

func TestFormatUSD(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0.38, "$0.38"},
		{0.384, "$0.38"},
		{12.5, "$12.50"},
		{0.004, "<$0.01"},
		{0.001, "<$0.01"},
		{0, "$0.00"},
		{cost.Unknown, "—"},
		{-1, "—"},
	}
	for _, tt := range tests {
		if got := cost.FormatUSD(tt.in); got != tt.want {
			t.Errorf("FormatUSD(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{812, "812"},
		{1000, "1.0k"},
		{41_234, "41.2k"},
		{999_499, "999.5k"},
		{1_300_000, "1.3M"},
		{12_345_678, "12.3M"},
	}
	for _, tt := range tests {
		if got := cost.FormatTokens(tt.in); got != tt.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
