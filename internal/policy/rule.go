package policy

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ruleKind is how a rule's Pattern is interpreted, decided by the tool.
type ruleKind int

const (
	kindBare   ruleKind = iota // tool with no pattern: matches every call
	kindPath                   // doublestar glob over resolved paths
	kindArgv                   // bash argv prefix, optional trailing *
	kindRegex                  // bash re:RE2 over the joined argv
	kindDomain                 // web_fetch host pattern
)

// Rule is one parsed permission rule.
//
//	rule    := tool [ "(" spec ")" ] [ "+net" ]
//	tool    := name | "mcp:" server [ ":" toolglob ] | "*"
//	spec    := pathglob | argv-prefix [ "*" ] | "re:" RE2 | "domain:" host | "*.host"
type Rule struct {
	Decision Decision
	Tool     string
	Pattern  string
	Source   Source

	kind     ruleKind
	net      bool
	argv     []string
	wildcard bool
	re       *regexp.Regexp
	mcp      bool
	server   string
	toolGlob string
}

// pathTools take a path glob; every other name (and future tools) does too
// unless listed in argvTools or webTools.
var (
	argvTools = map[string]bool{"bash": true}
	webTools  = map[string]bool{"web_fetch": true, "web_search": true}
)

// ParseRule parses a rule with a given source. The decision is set by the
// caller (usually ParseRules) because it comes from the list the text sat in.
func ParseRule(text string, src Source) (Rule, error) {
	r := Rule{Source: src}
	text = strings.TrimSpace(text)
	if strings.HasSuffix(text, "+net") {
		r.net = true
		text = strings.TrimSpace(strings.TrimSuffix(text, "+net"))
	}
	tool, spec, err := splitRule(text)
	if err != nil {
		return Rule{}, err
	}
	r.Tool, r.Pattern = tool, spec
	if err := r.parseTool(); err != nil {
		return Rule{}, err
	}
	if err := r.parseSpec(); err != nil {
		return Rule{}, err
	}
	if r.net && !argvTools[r.Tool] {
		return Rule{}, fmt.Errorf("policy: +net only applies to bash rules: %q", text)
	}
	return r, nil
}

// splitRule separates "tool(spec)" into its parts.
func splitRule(text string) (tool, spec string, err error) {
	if text == "" {
		return "", "", errors.New("policy: empty rule")
	}
	open := strings.IndexByte(text, '(')
	if open < 0 {
		if strings.ContainsAny(text, ") \t") {
			return "", "", fmt.Errorf("policy: malformed rule %q", text)
		}
		return text, "", nil
	}
	if !strings.HasSuffix(text, ")") {
		return "", "", fmt.Errorf("policy: missing closing parenthesis in %q", text)
	}
	tool, spec = text[:open], text[open+1:len(text)-1]
	if tool == "" || strings.TrimSpace(spec) == "" {
		return "", "", fmt.Errorf("policy: malformed rule %q", text)
	}
	// The spec is returned untrimmed: a bash regex may legitimately end in
	// a space. Each kind trims what it needs.
	return tool, spec, nil
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseTool validates the tool part and decodes mcp:server[:toolglob].
func (r *Rule) parseTool() error {
	switch {
	case r.Tool == "*":
		return nil
	case strings.HasPrefix(r.Tool, "mcp:"):
		rest := strings.TrimPrefix(r.Tool, "mcp:")
		server, glob, _ := strings.Cut(rest, ":")
		if server == "" {
			return fmt.Errorf("policy: mcp rule needs a server name: %q", r.Tool)
		}
		if r.Pattern != "" {
			return fmt.Errorf("policy: mcp rules use mcp:server:toolglob, not parentheses: %q", r.Tool)
		}
		if glob != "" && !doublestar.ValidatePattern(glob) {
			return fmt.Errorf("policy: bad mcp tool glob %q", glob)
		}
		r.mcp, r.server, r.toolGlob = true, server, glob
		return nil
	case toolName.MatchString(r.Tool):
		return nil
	}
	return fmt.Errorf("policy: bad tool name %q", r.Tool)
}

// parseSpec interprets Pattern according to the tool.
func (r *Rule) parseSpec() error {
	if r.Pattern == "" {
		r.kind = kindBare
		return nil
	}
	switch {
	case argvTools[r.Tool]:
		return r.parseBash()
	case webTools[r.Tool]:
		return r.parseDomain()
	default:
		r.Pattern = strings.TrimSpace(r.Pattern)
		if !doublestar.ValidatePathPattern(r.Pattern) {
			return fmt.Errorf("policy: bad path glob %q", r.Pattern)
		}
		r.kind = kindPath
		return nil
	}
}

func (r *Rule) parseBash() error {
	if re, ok := strings.CutPrefix(strings.TrimLeft(r.Pattern, " \t"), "re:"); ok {
		compiled, err := regexp.Compile(re)
		if err != nil {
			return fmt.Errorf("policy: bad bash regex: %w", err)
		}
		r.kind, r.re = kindRegex, compiled
		return nil
	}
	r.Pattern = strings.TrimSpace(r.Pattern)
	words := strings.Fields(r.Pattern)
	if len(words) == 0 {
		return errors.New("policy: empty bash pattern")
	}
	if words[len(words)-1] == "*" {
		r.wildcard = true
		words = words[:len(words)-1]
	}
	if len(words) == 0 {
		return errors.New("policy: bash(*) is too broad; use a bare bash rule")
	}
	for _, w := range words {
		if strings.ContainsAny(w, "*?[") {
			return fmt.Errorf("policy: wildcards are only allowed as the last word: %q", r.Pattern)
		}
	}
	r.kind, r.argv = kindArgv, words
	return nil
}

func (r *Rule) parseDomain() error {
	host := strings.TrimPrefix(r.Pattern, "domain:")
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || strings.ContainsAny(host, "/ \t") {
		return fmt.Errorf("policy: bad domain pattern %q", r.Pattern)
	}
	r.kind, r.Pattern = kindDomain, host
	return nil
}

// ParseRules parses a list from one settings layer. A bare "*" (or "*" with
// a pattern) is refused in allow lists: nothing may be blanket-allowed.
func ParseRules(texts []string, decision Decision, src Source) ([]Rule, error) {
	out := make([]Rule, 0, len(texts))
	for _, t := range texts {
		r, err := ParseRule(t, src)
		if err != nil {
			return nil, err
		}
		if decision == Allow && r.Tool == "*" {
			return nil, fmt.Errorf("policy: %q cannot be in an allow list", t)
		}
		r.Decision = decision
		out = append(out, r)
	}
	return out, nil
}

// MustParseRules is ParseRules for compiled-in lists.
func MustParseRules(texts []string, decision Decision, src Source) []Rule {
	rules, err := ParseRules(texts, decision, src)
	if err != nil {
		panic(err)
	}
	return rules
}

// String renders the rule in its canonical text form (without decision).
func (r Rule) String() string {
	var b strings.Builder
	b.WriteString(r.Tool)
	switch r.kind {
	case kindBare:
	case kindArgv:
		b.WriteString("(")
		b.WriteString(strings.Join(r.argv, " "))
		if r.wildcard {
			b.WriteString(" *")
		}
		b.WriteString(")")
	case kindRegex:
		b.WriteString("(re:")
		b.WriteString(r.re.String())
		b.WriteString(")")
	case kindDomain:
		b.WriteString("(domain:")
		b.WriteString(r.Pattern)
		b.WriteString(")")
	default:
		b.WriteString("(")
		b.WriteString(r.Pattern)
		b.WriteString(")")
	}
	if r.net {
		b.WriteString(" +net")
	}
	return b.String()
}

// Net reports whether the rule also grants sandbox network access.
func (r Rule) Net() bool { return r.net }

// MatchesTool reports whether the rule's tool part applies to tool. "*"
// matches everything; mcp rules match "mcp:server:tool" names.
func (r Rule) MatchesTool(tool string) bool {
	if r.Tool == "*" {
		return true
	}
	if !r.mcp {
		return r.Tool == tool
	}
	rest, ok := strings.CutPrefix(tool, "mcp:")
	if !ok {
		return false
	}
	server, name, _ := strings.Cut(rest, ":")
	if r.server != "*" && r.server != server {
		return false
	}
	if r.toolGlob == "" {
		return true
	}
	return doublestar.MatchUnvalidated(r.toolGlob, name)
}

// MatchesPath reports whether a path-kind rule matches abs. Patterns starting
// with "**" match anywhere; "~/" and "$WORKSPACE/" expand; other relative
// patterns are workspace-relative.
func (r Rule) MatchesPath(abs, home, root string) bool {
	if r.kind != kindPath {
		return false
	}
	pat := expandPattern(r.Pattern, home, root)
	return doublestar.PathMatchUnvalidated(pat, abs)
}

// expandPattern resolves the rule-pattern prefixes.
func expandPattern(p, home, root string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case p == "$WORKSPACE":
		return root
	case strings.HasPrefix(p, "$WORKSPACE/"):
		return filepath.Join(root, p[len("$WORKSPACE/"):])
	case strings.HasPrefix(p, "**"), filepath.IsAbs(p):
		return p
	default:
		return filepath.Join(root, p)
	}
}

// MatchesCommand reports whether a bash rule matches one simple command.
func (r Rule) MatchesCommand(argv []string) bool {
	switch r.kind {
	case kindBare:
		return true
	case kindRegex:
		return r.re.MatchString(strings.Join(argv, " "))
	case kindArgv:
		if len(argv) < len(r.argv) {
			return false
		}
		for i, w := range r.argv {
			if argv[i] != w {
				return false
			}
		}
		return r.wildcard || len(argv) == len(r.argv)
	}
	return false
}

// MatchesHost reports whether a web rule matches host. "*.example.com"
// matches the domain and every subdomain; other patterns use path.Match.
func (r Rule) MatchesHost(host string) bool {
	switch r.kind {
	case kindBare:
		return true
	case kindDomain:
		host = strings.ToLower(host)
		if suffix, ok := strings.CutPrefix(r.Pattern, "*."); ok {
			return host == suffix || strings.HasSuffix(host, "."+suffix)
		}
		ok, _ := path.Match(r.Pattern, host)
		return ok
	}
	return false
}

// IsBare reports a tool-only rule.
func (r Rule) IsBare() bool { return r.kind == kindBare }

// IsPath reports a path-glob rule.
func (r Rule) IsPath() bool { return r.kind == kindPath }

// IsBash reports an argv or regex rule.
func (r Rule) IsBash() bool { return r.kind == kindArgv || r.kind == kindRegex }

// IsDomain reports a web host rule.
func (r Rule) IsDomain() bool { return r.kind == kindDomain }

// ArgvWords returns the prefix words of an argv rule (nil otherwise).
func (r Rule) ArgvWords() []string { return append([]string(nil), r.argv...) }
