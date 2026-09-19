// Package markdown renders assistant text with glamour. A glamour renderer is
// expensive to build (it compiles a goldmark pipeline and chroma styles), so
// one is cached per (width, dark) pair; the cache is small because widths
// only change on resize. When rendering fails the text is shown as plain,
// word-wrapped text rather than dropped — the model's words must never
// disappear because of a formatting bug.
package markdown

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
)

// Backend renders markdown to styled terminal text at the given width.
type Backend func(md string, width int, isDark bool) (string, error)

// Renderer renders markdown, caching one glamour renderer per (width, dark).
// The zero value is not usable; call New.
type Renderer struct {
	mu      sync.Mutex
	backend Backend
	cache   map[cacheKey]*glamour.TermRenderer
}

type cacheKey struct {
	width  int
	isDark bool
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithBackend replaces the glamour backend. It exists so tests can exercise
// the fallback path and so a headless caller can plug in a no-op.
func WithBackend(b Backend) Option {
	return func(r *Renderer) { r.backend = b }
}

// New builds a Renderer.
func New(opts ...Option) *Renderer {
	r := &Renderer{cache: map[cacheKey]*glamour.TermRenderer{}}
	for _, o := range opts {
		o(r)
	}
	if r.backend == nil {
		r.backend = r.glamour
	}
	return r
}

// Render returns md rendered for a terminal of the given width. Output lines
// never exceed width; on any failure the plain-text fallback is used.
func (r *Renderer) Render(md string, width int, isDark bool) string {
	if width < 1 {
		width = 1
	}
	out, err := r.backend(md, width, isDark)
	if err != nil || out == "" && strings.TrimSpace(md) != "" {
		return Plain(md, width)
	}
	return strings.TrimRight(out, "\n")
}

// Plain is the fallback: hard-wrapped text with no styling.
func Plain(md string, width int) string {
	if width < 1 {
		width = 1
	}
	return strings.TrimRight(xansi.Wrap(md, width, ""), "\n")
}

// Style returns the glamour style for a background, with the document margin
// removed so the transcript controls its own gutters.
func Style(isDark bool) ansi.StyleConfig {
	cfg := styles.LightStyleConfig
	if isDark {
		cfg = styles.DarkStyleConfig
	}
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.BlockPrefix = ""
	cfg.Document.BlockSuffix = ""
	return cfg
}

// glamour is the default backend: a cached TermRenderer per (width, dark).
func (r *Renderer) glamour(md string, width int, isDark bool) (string, error) {
	tr, err := r.renderer(width, isDark)
	if err != nil {
		return "", err
	}
	// glamour renderers are not safe for concurrent use; Render is only
	// called from the Bubble Tea goroutine but the lock keeps that a
	// guarantee rather than a convention.
	r.mu.Lock()
	defer r.mu.Unlock()
	return tr.Render(md)
}

func (r *Renderer) renderer(width int, isDark bool) (*glamour.TermRenderer, error) {
	key := cacheKey{width, isDark}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tr, ok := r.cache[key]; ok {
		return tr, nil
	}
	tr, err := glamour.NewTermRenderer(glamour.WithStyles(Style(isDark)), glamour.WithWordWrap(width))
	if err != nil {
		return nil, err
	}
	r.cache[key] = tr
	return tr, nil
}
