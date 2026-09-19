// Package redact replaces secret-looking strings in tool output with a
// visible marker before they reach the model or the transcript. Only
// high-confidence patterns are on by default (vendor-prefixed tokens, key
// blocks, auth headers); a generic "assignment of a high-entropy value"
// pattern is opt-in because it misfires on fixtures and hashes.
//
// The marker keeps the last four characters ("[redacted: github…3f9a]") so
// a user can tell which credential was hit without the value ever being
// shown.
package redact

import (
	"bufio"
	"bytes"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Pattern is one redaction rule. Group selects the capture group holding
// the secret (0 = whole match); Keep is how many trailing characters of the
// secret survive in the marker (0 = none).
type Pattern struct {
	Name  string
	Re    *regexp.Regexp
	Group int
	Keep  int
}

// Hit reports how many times a pattern fired in one Redact call.
type Hit struct {
	Name  string
	Count int
}

// Redactor applies an ordered pattern list.
type Redactor struct {
	patterns []Pattern
}

// Option configures New.
type Option func(*config)

type config struct {
	disabled map[string]bool
	extra    []Pattern
	generic  bool
}

// WithDisabled turns off the named default patterns.
func WithDisabled(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.disabled[n] = true
		}
	}
}

// WithExtra appends caller-supplied patterns after the defaults.
func WithExtra(patterns ...Pattern) Option {
	return func(c *config) { c.extra = append(c.extra, patterns...) }
}

// WithGeneric enables the opt-in generic pattern: KEY=value assignments
// whose value has Shannon entropy above 3.5 bits per character.
func WithGeneric() Option {
	return func(c *config) { c.generic = true }
}

// New builds a Redactor with the default patterns and options applied.
func New(opts ...Option) *Redactor {
	c := config{disabled: map[string]bool{}}
	for _, o := range opts {
		o(&c)
	}
	r := &Redactor{}
	for _, p := range Defaults() {
		if !c.disabled[p.Name] {
			r.patterns = append(r.patterns, p)
		}
	}
	r.patterns = append(r.patterns, c.extra...)
	if c.generic {
		r.patterns = append(r.patterns, genericPattern)
	}
	return r
}

// Patterns returns the active pattern names in order.
func (r *Redactor) Patterns() []string {
	out := make([]string, 0, len(r.patterns))
	for _, p := range r.patterns {
		out = append(out, p.Name)
	}
	return out
}

// Redact replaces every match in s and reports the hits by pattern.
func (r *Redactor) Redact(s string) (string, []Hit) {
	counts := map[string]int{}
	for _, p := range r.patterns {
		s = p.apply(s, counts)
	}
	if len(counts) == 0 {
		return s, nil
	}
	hits := make([]Hit, 0, len(counts))
	for name, n := range counts {
		hits = append(hits, Hit{Name: name, Count: n})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
	return s, hits
}

// apply replaces one pattern's matches in s.
func (p Pattern) apply(s string, counts map[string]int) string {
	return p.Re.ReplaceAllStringFunc(s, func(m string) string {
		idx := p.Re.FindStringSubmatchIndex(m)
		start, end := 0, len(m)
		if p.Group > 0 && 2*p.Group+1 < len(idx) && idx[2*p.Group] >= 0 {
			start, end = idx[2*p.Group], idx[2*p.Group+1]
		}
		secret := m[start:end]
		if p.Group > 0 && genericPattern.Name == p.Name && !highEntropy(secret) {
			return m
		}
		counts[p.Name]++
		return m[:start] + Marker(p.Name, secret, p.Keep) + m[end:]
	})
}

// Marker renders the replacement text for a secret. No default pattern can
// match inside a marker (their token character classes exclude "[" and the
// vendor prefixes never occur in a name), which is what makes Redact
// idempotent; FuzzRedact checks that property.
func Marker(name, secret string, keep int) string {
	if keep <= 0 || len(secret) <= keep {
		return "[redacted: " + name + "]"
	}
	return "[redacted: " + name + "…" + secret[len(secret)-keep:] + "]"
}

// maxHeldBytes bounds the private-key lookbehind: past this, the block is
// flushed redacted rather than buffered forever on a missing END marker.
const maxHeldBytes = 64 * 1024

// keyBegin and keyEnd are the markers of the one default pattern that spans
// lines. The writer watches for them so a streamed key is held back instead
// of being forwarded a line at a time, which no line-wise pattern can catch.
var (
	keyBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	keyEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// Writer returns a line-buffered writer that redacts each line before
// forwarding it to w. Partial lines are held until a newline or Close.
//
// Line buffering alone cannot see a multi-line secret, so the writer keeps a
// bounded lookbehind: once a "-----BEGIN … PRIVATE KEY" marker appears,
// output is held back until the matching END arrives (then the whole block
// is redacted), until maxHeldBytes accumulate, or until Close — an
// unterminated block is redacted from the marker on, because a stream that
// starts a key and stops is a key. A block that overruns the cap is redacted
// at the cap and the rest is dropped until its END marker, since those lines
// are key material no line-wise pattern would catch. Everything else still
// streams live.
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	return &writer{r: r, w: w, guard: r.has(NamePrivateKey)}
}

type writer struct {
	mu   sync.Mutex
	r    *Redactor
	w    io.Writer
	buf  bytes.Buffer // incomplete trailing line
	held bytes.Buffer // a private-key block being accumulated
	// holding accumulates a key block; suppressing drops the remainder of
	// one that overran the cap and has already been marked.
	holding, suppressing bool
	guard                bool // the private-key pattern is active
}

func (lw *writer) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.buf.Write(p)
	for {
		line, err := lw.buf.ReadString('\n')
		if err != nil {
			// No complete line: keep the remainder for the next write.
			lw.buf.Reset()
			lw.buf.WriteString(line)
			break
		}
		if err := lw.line(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// line forwards one complete line, or feeds the held key block.
func (lw *writer) line(s string) error {
	if lw.suppressing {
		lw.suppressing = !keyEnd.MatchString(s)
		return nil
	}
	if lw.holding {
		lw.held.WriteString(s)
		switch {
		case keyEnd.MatchString(s):
			return lw.flushHeld(true)
		case lw.held.Len() >= maxHeldBytes:
			err := lw.flushHeld(false)
			lw.suppressing = true
			return err
		}
		return nil
	}
	if lw.guard && keyBegin.MatchString(s) {
		lw.holding = true
		lw.held.WriteString(s)
		if keyEnd.MatchString(s) {
			return lw.flushHeld(true)
		}
		return nil
	}
	red, _ := lw.r.Redact(s)
	_, err := io.WriteString(lw.w, red)
	return err
}

// flushHeld writes the held block. A terminated block is redacted by the
// ordinary patterns; anything still showing a BEGIN marker afterwards (an
// unterminated block, or one the pattern could not match) is cut at the
// marker, because the rest of it is key material.
func (lw *writer) flushHeld(terminated bool) error {
	s := lw.held.String()
	lw.held.Reset()
	lw.holding = false
	out, _ := lw.r.Redact(s)
	if !terminated || keyBegin.MatchString(out) {
		out = cutAtKey(lw.r, s)
	}
	_, err := io.WriteString(lw.w, out)
	return err
}

// cutAtKey replaces everything from the BEGIN marker on with the marker,
// keeping (and redacting) whatever preceded it on that line.
func cutAtKey(r *Redactor, s string) string {
	loc := keyBegin.FindStringIndex(s)
	if loc == nil {
		out, _ := r.Redact(s)
		return out
	}
	head, _ := r.Redact(s[:loc[0]])
	tail := ""
	if strings.HasSuffix(s, "\n") {
		tail = "\n"
	}
	return head + Marker(NamePrivateKey, "", 0) + tail
}

// Close flushes a trailing partial line and any held key block.
func (lw *writer) Close() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.buf.Len() > 0 {
		line := lw.buf.String()
		lw.buf.Reset()
		if err := lw.line(line); err != nil {
			return err
		}
	}
	if lw.holding {
		return lw.flushHeld(false)
	}
	return nil
}

// has reports whether the named pattern is active.
func (r *Redactor) has(name string) bool {
	for _, p := range r.patterns {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Scan is a convenience for callers that already have a bufio.Scanner: it
// redacts each token as it goes.
func (r *Redactor) Scan(sc *bufio.Scanner, fn func(line string)) {
	for sc.Scan() {
		red, _ := r.Redact(sc.Text())
		fn(red)
	}
}

// highEntropy reports whether s looks random enough to be a secret rather
// than a word: Shannon entropy above 3.5 bits per character.
func highEntropy(s string) bool {
	if len(s) < 16 {
		return false
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	var h float64
	n := float64(len(s))
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := float64(f) / n
		h -= p * math.Log2(p)
	}
	return h > 3.5
}
