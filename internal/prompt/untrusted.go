package prompt

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// closeUntrusted is the only sequence that could end an untrusted block
// early; WrapUntrusted rewrites it so content can never escape its fence.
const closeUntrusted = "</untrusted"

// WrapUntrusted fences body as data from source ("bash", "web_fetch", an MCP
// server…) so the model treats it as content, not as instructions. A literal
// "</untrusted" inside body is escaped as "<\/untrusted"; the source
// attribute is quoted so it cannot inject attributes or close the tag.
func WrapUntrusted(source, body string) string {
	escaped := strings.ReplaceAll(body, closeUntrusted, `<\/untrusted`)
	return "<untrusted source=" + strconv.Quote(source) + ">\n" + escaped + "\n</untrusted>"
}

// Signal is one prompt-injection indicator found by ScanInjection.
type Signal struct {
	Kind    string
	Snippet string
	Offset  int // byte offset of the match in the scanned text
}

// Signal kinds, stable for audit events and the UI badge.
const (
	KindIgnorePrevious = "ignore-previous-instructions"
	KindRoleReassign   = "role-reassignment"
	KindSystemPrompt   = "system-prompt-override"
	KindConceal        = "conceal-from-user"
	KindRoleMarker     = "assistant-role-marker"
	KindTemplateMarker = "chat-template-marker"
	KindUnicodeTags    = "unicode-tag-characters"
	KindZeroWidth      = "zero-width-characters"
	KindHiddenHTML     = "hidden-html"
	KindBase64Exec     = "base64-then-exec"
	KindPipeToShell    = "pipe-to-shell"
)

// snippetLen bounds Signal.Snippet so an audit line stays readable.
const snippetLen = 80

// regexSignals are the textual patterns. They are deliberately narrow: a
// false positive only shows a badge, but a noisy scanner gets ignored.
var regexSignals = []struct {
	kind string
	re   *regexp.Regexp
}{
	{KindIgnorePrevious, regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\s+(all\s+|any\s+|the\s+|your\s+)?(previous|prior|above|earlier|preceding)\s+(instructions?|prompts?|directions?|rules?)`)},
	{KindRoleReassign, regexp.MustCompile(`(?i)\byou\s+are\s+now\s+(a|an|the|in|no\s+longer)\b`)},
	{KindSystemPrompt, regexp.MustCompile(`(?i)\b(new|updated|revised|real|actual)\s+system\s+(prompt|instructions?|message)\b`)},
	{KindConceal, regexp.MustCompile(`(?i)\b(do\s+not|don't|never)\s+(tell|inform|mention\s+(this\s+)?to|reveal\s+(this\s+)?to|show)\s+(the\s+)?(user|human|operator)\b`)},
	{KindRoleMarker, regexp.MustCompile(`(?im)^\s*(assistant|system)\s*:\s`)},
	{KindTemplateMarker, regexp.MustCompile(`<\|im_start\|>|<\|im_end\|>|<\|system\|>|<\|user\|>|<\|assistant\|>|\[INST\]|\[/INST\]|<<SYS>>|<</SYS>>|(?i)<system>`)},
	{KindHiddenHTML, regexp.MustCompile(`(?is)<[a-z][^>]*(display\s*:\s*none|font-size\s*:\s*0(px|em|rem)?\s*[;"']|aria-hidden\s*=\s*["']?true|visibility\s*:\s*hidden)[^>]*>[^<]{41,}`)},
	{KindPipeToShell, regexp.MustCompile(`(?i)\b(curl|wget)\b[^|\n]{0,300}\|\s*(sudo\s+(-\S+\s+)?)?(ba|z|da|k)?sh\b`)},
}

var (
	base64Run   = regexp.MustCompile(`[A-Za-z0-9+/]{400,}={0,2}`)
	decodeAfter = regexp.MustCompile(`(?i)\b(decode|eval|exec|source)\b`)
)

// zeroWidth are the invisible code points used to hide text from a reader
// while a tokenizer still sees it.
var zeroWidth = map[rune]bool{0x200B: true, 0x200C: true, 0x200D: true, 0x2060: true, 0xFEFF: true, 0x180E: true}

// ScanInjection returns the injection signals found in s, in offset order.
// It never panics on arbitrary input (fuzzed) and treats s as bytes when it
// is not valid UTF-8, so the rune scans simply see replacement characters.
func ScanInjection(s string) []Signal {
	var out []Signal
	for _, rs := range regexSignals {
		for _, loc := range rs.re.FindAllStringIndex(s, -1) {
			out = append(out, Signal{Kind: rs.kind, Snippet: snippet(s, loc[0], loc[1]), Offset: loc[0]})
		}
	}
	out = append(out, scanBase64(s)...)
	out = append(out, scanRunes(s)...)
	sortSignals(out)
	return out
}

func scanBase64(s string) []Signal {
	var out []Signal
	for _, loc := range base64Run.FindAllStringIndex(s, -1) {
		end := min(loc[1]+200, len(s))
		if decodeAfter.MatchString(s[loc[1]:end]) {
			out = append(out, Signal{Kind: KindBase64Exec, Snippet: snippet(s, loc[0], loc[1]), Offset: loc[0]})
		}
	}
	return out
}

// scanRunes finds Unicode tag characters (U+E0000–U+E007F, invisible and
// used to smuggle ASCII) and runs of more than five zero-width characters.
func scanRunes(s string) []Signal {
	var out []Signal
	zw, zwStart, tagged := 0, -1, false
	for i, r := range s {
		if r >= 0xE0000 && r <= 0xE007F && !tagged {
			tagged = true
			out = append(out, Signal{Kind: KindUnicodeTags, Snippet: snippet(s, i, i), Offset: i})
		}
		if zeroWidth[r] {
			if zw == 0 {
				zwStart = i
			}
			zw++
			continue
		}
	}
	if zw > 5 {
		out = append(out, Signal{Kind: KindZeroWidth, Snippet: snippet(s, zwStart, zwStart), Offset: zwStart})
	}
	return out
}

// snippet returns up to snippetLen bytes around [from,to), cut on rune
// boundaries and with control characters made visible.
func snippet(s string, from, to int) string {
	end := min(max(to, from+snippetLen), len(s))
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	if end-from > snippetLen*2 {
		end = from + snippetLen*2
		for end > from && !utf8.RuneStart(s[end]) {
			end--
		}
	}
	return strconv.Quote(s[from:end])
}

// SignalKinds lists the distinct signal kinds in sig, first occurrence
// first. It is what the prompt's warning attribute and the app's notice both
// show: the kinds name what was seen without repeating the content.
func SignalKinds(sig []Signal) []string {
	seen := make(map[string]bool, len(sig))
	out := make([]string, 0, len(sig))
	for _, s := range sig {
		if seen[s.Kind] {
			continue
		}
		seen[s.Kind] = true
		out = append(out, s.Kind)
	}
	return out
}

func sortSignals(sig []Signal) {
	for i := 1; i < len(sig); i++ {
		for j := i; j > 0 && sig[j].Offset < sig[j-1].Offset; j-- {
			sig[j], sig[j-1] = sig[j-1], sig[j]
		}
	}
}
