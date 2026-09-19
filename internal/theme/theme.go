// Package theme is the single source of colour for wright's terminal UI. The
// palette is the gloam token set used across the author's tools, expressed as
// light/dark pairs so the same code reads well on either background.
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Glyphs used next to status words. Every status is always rendered as
// glyph + word so meaning never depends on colour alone.
const (
	GlyphOK        = "✓"
	GlyphFail      = "✗"
	GlyphAsk       = "?"
	GlyphRunning   = "●"
	GlyphWarn      = "⚠"
	GlyphStop      = "⛔"
	GlyphCollapsed = "▸"
	GlyphExpanded  = "▾"
)

// Palette holds the resolved colours for one background (light or dark).
type Palette struct {
	Accent    color.Color // brand purple: titles, active elements
	DimAccent color.Color // muted purple: borders, inactive accents
	Subtle    color.Color // grey: secondary text
	Hot       color.Color // red: errors, denials, sandbox off
	Warm      color.Color // amber: warnings, asks
	Good      color.Color // green: success, allows
}

// Theme is a Palette plus the derived styles the UI composes from.
type Theme struct {
	Palette
	IsDark bool

	Title      lipgloss.Style
	Subtle     lipgloss.Style
	Hot        lipgloss.Style
	Warm       lipgloss.Style
	Good       lipgloss.Style
	Card       lipgloss.Style // bordered block for tool calls
	CardBorder lipgloss.Style // border-only variant for nested cards
	DiffAdd    lipgloss.Style
	DiffDel    lipgloss.Style
	DiffHunk   lipgloss.Style // "@@ … @@" hunk headers
	StatusBar  lipgloss.Style

	Bold       lipgloss.Style
	Accented   lipgloss.Style // accent foreground without bold
	HotBold    lipgloss.Style // sandbox off, bypass: must be impossible to miss
	Key        lipgloss.Style // "[y]" key hints in prompts
	Selected   lipgloss.Style // focused list row
	Overlay    lipgloss.Style // bordered, centred prompt box
	UserPrompt lipgloss.Style // the "›" gutter of a user message
	Badge      lipgloss.Style // inline warning badges (injection, redacted)
}

// NewPalette resolves the gloam tokens for the given background.
func NewPalette(isDark bool) Palette {
	pick := lipgloss.LightDark(isDark)
	return Palette{
		Accent:    pick(lipgloss.Color("#7c3aed"), lipgloss.Color("#a78bfa")),
		DimAccent: pick(lipgloss.Color("#c4b5fd"), lipgloss.Color("#4c1d95")),
		Subtle:    pick(lipgloss.Color("#6b7280"), lipgloss.Color("#9ca3af")),
		Hot:       pick(lipgloss.Color("#dc2626"), lipgloss.Color("#f87171")),
		Warm:      pick(lipgloss.Color("#d97706"), lipgloss.Color("#fbbf24")),
		Good:      pick(lipgloss.Color("#059669"), lipgloss.Color("#34d399")),
	}
}

// New builds the theme for a dark or light terminal background.
func New(isDark bool) Theme {
	p := NewPalette(isDark)
	return Theme{
		Palette:    p,
		IsDark:     isDark,
		Title:      lipgloss.NewStyle().Bold(true).Foreground(p.Accent),
		Subtle:     lipgloss.NewStyle().Foreground(p.Subtle),
		Hot:        lipgloss.NewStyle().Foreground(p.Hot),
		Warm:       lipgloss.NewStyle().Foreground(p.Warm),
		Good:       lipgloss.NewStyle().Foreground(p.Good),
		Card:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.DimAccent).Padding(0, 1),
		CardBorder: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.DimAccent),
		DiffAdd:    lipgloss.NewStyle().Foreground(p.Good),
		DiffDel:    lipgloss.NewStyle().Foreground(p.Hot),
		DiffHunk:   lipgloss.NewStyle().Foreground(p.Accent),
		StatusBar:  lipgloss.NewStyle().Foreground(p.Subtle).Padding(0, 1),
		Bold:       lipgloss.NewStyle().Bold(true),
		Accented:   lipgloss.NewStyle().Foreground(p.Accent),
		HotBold:    lipgloss.NewStyle().Bold(true).Foreground(p.Hot),
		Key:        lipgloss.NewStyle().Bold(true).Foreground(p.Accent),
		Selected:   lipgloss.NewStyle().Bold(true).Foreground(p.Accent).Reverse(true),
		Overlay:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.Accent).Padding(0, 1),
		UserPrompt: lipgloss.NewStyle().Bold(true).Foreground(p.Accent),
		Badge:      lipgloss.NewStyle().Bold(true).Foreground(p.Warm),
	}
}
