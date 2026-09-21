// Package transcript is the block list behind the TUI's transcript viewport:
// user messages, assistant markdown, tool cards, approval records, notices
// and errors. Each block renders to lines for a width and the result is
// cached per block, so a streamed delta only re-renders the live block and a
// resize is the only thing that re-renders everything.
package transcript

import (
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/highlight"
	"github.com/richardwooding/wright/internal/tui/markdown"
)

// Block is one transcript entry. Implementations live in blocks.go.
type Block interface {
	render(ctx renderContext) []string
}

// renderContext is what a block needs to draw itself.
type renderContext struct {
	width         int
	th            theme.Theme
	md            *markdown.Renderer
	hl            *highlight.Renderer
	showReasoning bool
}

// entry pairs a block with its cached lines.
type entry struct {
	block Block
	lines []string
	valid bool
}

// Model owns the blocks and their render cache.
type Model struct {
	th            theme.Theme
	hl            *highlight.Renderer
	md            *markdown.Renderer
	width         int
	showReasoning bool
	entries       []*entry
}

// New builds an empty transcript. md may be nil, in which case assistant text
// is rendered as plain wrapped text.
func New(th theme.Theme, md *markdown.Renderer, opts ...Option) *Model {
	m := &Model{th: th, md: md}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Option configures a Model.
type Option func(*Model)

// WithHighlighter supplies the syntax highlighter tool cards use. Without one
// every card is shown plain, which is what the tests that predate it expect
// and a perfectly good way to run.
func WithHighlighter(h *highlight.Renderer) Option {
	return func(m *Model) { m.hl = h }
}

// Append adds a block and returns it for later mutation by the caller.
func (m *Model) Append(b Block) Block {
	m.entries = append(m.entries, &entry{block: b})
	return b
}

// Invalidate drops the cached lines of one block (the live assistant block
// after a text delta, a tool card after progress or its result).
func (m *Model) Invalidate(b Block) {
	for _, e := range m.entries {
		if e.block == b {
			e.valid = false
			return
		}
	}
}

// InvalidateAll drops every cache entry; used on resize and theme change.
func (m *Model) InvalidateAll() {
	for _, e := range m.entries {
		e.valid = false
	}
}

// SetTheme swaps the theme (after tea.BackgroundColorMsg) and re-renders.
func (m *Model) SetTheme(th theme.Theme) {
	m.th = th
	m.InvalidateAll()
}

// SetShowReasoning toggles the collapsed reasoning sections.
func (m *Model) SetShowReasoning(show bool) {
	if m.showReasoning == show {
		return
	}
	m.showReasoning = show
	for _, e := range m.entries {
		if _, ok := e.block.(*Assistant); ok {
			e.valid = false
		}
	}
}

// ShowReasoning reports whether reasoning sections are expanded.
func (m *Model) ShowReasoning() bool { return m.showReasoning }

// SetToolsExpanded expands or collapses every tool card.
func (m *Model) SetToolsExpanded(expanded bool) {
	for _, e := range m.entries {
		if c, ok := e.block.(*ToolCard); ok && c.Expanded != expanded {
			c.Expanded = expanded
			e.valid = false
		}
	}
}

// Clear removes every block.
func (m *Model) Clear() { m.entries = nil }

// Len returns the number of blocks.
func (m *Model) Len() int { return len(m.entries) }

// Blocks returns the blocks in order (the slice is a copy, the blocks are not).
func (m *Model) Blocks() []Block {
	out := make([]Block, len(m.entries))
	for i, e := range m.entries {
		out[i] = e.block
	}
	return out
}

// Lines renders the transcript for width. Changing the width invalidates
// every cached block; otherwise only invalid entries are re-rendered.
func (m *Model) Lines(width int) []string {
	if width < 1 {
		width = 1
	}
	if width != m.width {
		m.width = width
		m.InvalidateAll()
	}
	ctx := renderContext{width: width, th: m.th, md: m.md, hl: m.hl, showReasoning: m.showReasoning}
	var out []string
	for i, e := range m.entries {
		if !e.valid {
			e.lines = clampWidth(e.block.render(ctx), width)
			e.valid = true
		}
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, e.lines...)
	}
	return out
}
