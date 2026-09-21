// Package highlight colours source text for a tool card, one output line per
// input line.
//
// It uses chroma's lexers but not its formatters or styles, deliberately.
// chroma's terminal formatter writes raw SGR sequences, which bypass the
// colour-profile downsampling every other part of the UI goes through — on a
// 16-colour terminal the card would be the one thing that lies about what it
// can show — and its token values span newlines, so splitting its output does
// not yield self-contained rows. The transcript's row budget requires that one
// slice element is exactly one terminal row, so the tokens are split into
// lines here and painted with the theme's own six colours.
//
// Highlighting is decoration and nothing more. wright's rule is glyph + word,
// never colour alone, so every line still reads correctly with the colour
// stripped, and a language this package does not recognise is shown plain
// rather than guessed at.
package highlight

import (
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/richardwooding/wright/internal/theme"
)

// Backend tokenises src as lang. It is the seam tests use to count calls and
// to exercise the plain fallback.
type Backend func(src, lang string) ([]chroma.Token, bool)

// Renderer highlights source text, caching the compiled lexer for each
// language and the token styles for each background.
//
// One Renderer is built per process and carried on the transcript's render
// context, exactly as markdown.Renderer is. It must never be a package global:
// the render context is the only channel into a block renderer, and a global
// would outlive the theme change that has to invalidate it.
type Renderer struct {
	mu      sync.Mutex
	backend Backend
	lexers  map[string]chroma.Lexer
	styles  map[bool]map[chroma.TokenType]lipgloss.Style
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithBackend replaces the tokeniser.
func WithBackend(b Backend) Option {
	return func(r *Renderer) { r.backend = b }
}

// New builds a Renderer.
func New(opts ...Option) *Renderer {
	r := &Renderer{
		lexers: map[string]chroma.Lexer{},
		styles: map[bool]map[chroma.TokenType]lipgloss.Style{},
	}
	for _, o := range opts {
		o(r)
	}
	if r.backend == nil {
		r.backend = r.tokenise
	}
	return r
}

// Lines is deliberately given only the text that will be shown, never the whole
// of a long output: tokenising scales with the input, and on this machine a
// 1198-line Go file costs ~40 ms against ~0.9 ms for the 40-line window a
// collapsed card displays. A resize re-renders every card at once, so the
// difference is the difference between a redraw and a stall. The cost of
// slicing first is that a window starting inside a multi-line construct — a Go
// raw string, a block comment — may colour oddly until the construct closes;
// that is the same imprecision every editor's viewport highlighting has, and
// it is worth 44x.
// Lines returns src highlighted as lang, one element per line of src. ok is
// false when lang is unknown, when tokenising fails, or when the result would
// not be line-for-line with the input — in every one of those the caller shows
// the plain text, because a wrong lexer looks worse than none.
func (r *Renderer) Lines(src, lang string, th theme.Theme) (lines []string, ok bool) {
	if lang == "" || src == "" {
		return nil, false
	}
	tokens, ok := r.backend(src, lang)
	if !ok {
		return nil, false
	}
	split := chroma.SplitTokensIntoLines(tokens)
	styles := r.styleSet(th)
	out := make([]string, 0, len(split))
	for _, line := range split {
		var b strings.Builder
		for _, tok := range line {
			// SplitAfterN keeps the separator, so the last token of a line
			// carries the newline that must not reach the row.
			v := strings.TrimSuffix(tok.Value, "\n")
			if v == "" {
				continue
			}
			b.WriteString(styles[styleKey(tok.Type)].Render(v))
		}
		out = append(out, b.String())
	}
	want := strings.Split(src, "\n")
	// chroma strips an empty trailing line, which nearly every file has.
	if len(out) == len(want)-1 && want[len(want)-1] == "" {
		out = append(out, "")
	}
	if len(out) != len(want) {
		// The row budget rests on one element being one row. A mismatch here
		// means the tokeniser and the text disagree; plain is the safe answer.
		return nil, false
	}
	return out, true
}

// tokenise is the default backend: a cached, coalesced chroma lexer per
// language. chroma compiles a lexer's rules lazily on first use, so caching
// the lexer is what keeps a resize — which re-renders every card at once —
// from recompiling the same grammar per card.
func (r *Renderer) tokenise(src, lang string) ([]chroma.Token, bool) {
	r.mu.Lock()
	lex, ok := r.lexers[lang]
	if !ok {
		if l := lexers.Get(lang); l != nil {
			lex = chroma.Coalesce(l)
		}
		r.lexers[lang] = lex // nil is cached too: a miss must not re-look-up
	}
	r.mu.Unlock()
	if lex == nil {
		return nil, false
	}
	it, err := lex.Tokenise(nil, src)
	if err != nil {
		return nil, false
	}
	return it.Tokens(), true
}

// styleSet returns the token styles for a background, building them once.
func (r *Renderer) styleSet(th theme.Theme) map[chroma.TokenType]lipgloss.Style {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.styles[th.IsDark]; ok {
		return s
	}
	s := styleSet(th)
	r.styles[th.IsDark] = s
	return s
}

// styleSet maps chroma's token categories onto the theme's own colours.
//
// Deliberately six colours and not a syntax palette of its own: a stock chroma
// style brings two hundred colours and a background, in a card that is already
// painting DiffAdd/DiffDel/Subtle from the same six. Two colour systems in one
// block is worse than one.
//
// Every style keeps tabs as tabs. lipgloss expands them to spaces by default,
// which would make a highlighted card disagree with the file it is showing —
// silently, and semantically in a Makefile, where a tab is the rule syntax.
// Unhighlighted output already passes tabs through, so this keeps the two
// paths honest with each other.
func styleSet(th theme.Theme) map[chroma.TokenType]lipgloss.Style {
	keepTabs := func(s lipgloss.Style) lipgloss.Style { return s.TabWidth(lipgloss.NoTabConversion) }
	return map[chroma.TokenType]lipgloss.Style{
		chroma.Comment:        keepTabs(th.Subtle),
		chroma.CommentPreproc: keepTabs(th.Subtle),
		chroma.Keyword:        keepTabs(th.Accented),
		chroma.NameFunction:   keepTabs(th.Good),
		chroma.NameClass:      keepTabs(th.Good),
		chroma.NameBuiltin:    keepTabs(th.Good),
		chroma.LiteralString:  keepTabs(th.Warm),
		chroma.LiteralNumber:  keepTabs(th.Warm),
		chroma.Error:          keepTabs(th.Hot),
		// chroma.None is the unstyled majority: whitespace, punctuation,
		// ordinary identifiers. It still goes through a style, so it still
		// needs the tab setting.
		chroma.None: keepTabs(lipgloss.NewStyle()),
	}
}

// styleKey resolves a token type to the nearest type styleSet knows, using
// chroma's own category fallback: an unmapped KeywordDeclaration becomes
// Keyword, and anything with no mapping renders in the terminal's own colour.
func styleKey(t chroma.TokenType) chroma.TokenType {
	for _, c := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		switch c {
		case chroma.Comment, chroma.CommentPreproc, chroma.Keyword, chroma.NameFunction,
			chroma.NameClass, chroma.NameBuiltin, chroma.LiteralString, chroma.LiteralNumber, chroma.Error:
			return c
		}
	}
	return chroma.None
}
