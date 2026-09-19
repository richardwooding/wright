package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/theme"
)

// statusBar renders the one-line footer:
//
//	wright · <model> · <mode> · ctx 23% · 41.2k tok · $0.38 · bwrap ⊘net · main +3 · sess <id>
//
// "sandbox off" and "⛔ bypass" are hot red and bold: the two states a user
// must never miss.
func (m Model) statusBar() string {
	s := m.status
	th := m.th
	segs := []string{th.Title.Render("wright")}
	if s.Model != "" {
		segs = append(segs, s.Model)
	} else {
		segs = append(segs, th.Subtle.Render("no model"))
	}
	segs = append(segs, m.modeSegment())
	if s.ContextWindow > 0 {
		segs = append(segs, fmt.Sprintf("ctx %d%%", int(s.ContextPct()*100+0.5)))
	}
	segs = append(segs, formatTokens(s.Usage.TotalTokens)+" tok", formatCost(s.Cost, s.CostKnown), m.sandboxSegment())
	if g := m.git.String(); g != "" {
		segs = append(segs, g)
	}
	if s.SessionID != "" {
		segs = append(segs, "sess "+s.SessionID)
	}
	if m.running {
		segs = append(segs, th.Warm.Render(m.spin.View()+" running"))
	}
	if m.hint != "" {
		segs = append(segs, th.Warm.Render(m.hint))
	}
	// The style pads one column each side, so the text gets width-2.
	return th.StatusBar.Render(truncate(strings.Join(segs, th.Subtle.Render(" · ")), max(m.width-2, 1)))
}

func (m Model) modeSegment() string {
	if m.status.Bypass || m.status.Mode == policy.ModeBypass {
		return m.th.HotBold.Render(theme.GlyphStop + " bypass")
	}
	return m.status.Mode.String()
}

func (m Model) sandboxSegment() string {
	switch m.status.Sandbox {
	case "", "none":
		return m.th.HotBold.Render("sandbox off")
	}
	if m.status.SandboxNet {
		return m.status.Sandbox + " net"
	}
	return m.status.Sandbox + " ⊘net"
}

// formatTokens renders 41234 as "41.2k" and 1234567 as "1.2M".
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

// formatCost renders USD, or "—" when the model is not in the catalog so an
// unknown price is never shown as free.
func formatCost(usd float64, known bool) string {
	if !known {
		return "—"
	}
	if usd < 0.01 && usd > 0 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

func itoa(n int) string { return strconv.Itoa(n) }

// truncate clips styled text to w cells.
func truncate(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }
